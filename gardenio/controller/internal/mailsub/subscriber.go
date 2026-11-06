// Package mailsub is the agent-side mail subscriber (spec §3, "mail is the
// inbox"). It runs beside coop inside `gb init` (agent pods), `gb agent
// start --k8s` and `gb agent start`, subscribes to the agent's own mail over
// the kbeads SSE stream with the daemon's `agent` scope, and delivers each
// mail to coop on loopback, so nothing has to dial in to the agent to reach
// it. Through a land door, which serves no event stream, it polls instead
// (Config.Poll).
package mailsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/mailproto"
)

// Tunables are package variables so tests can shrink them (spec §10: three
// attempts two seconds apart; reconnect backoff from one second, capped at
// 60 seconds). A delivery that still fails is re-drained from RedrainInitial,
// doubling to RedrainMax, until coop takes it: mail can reach a pod before
// its coop is up (a spawning agent, or coop restarting inside `gb init`), and
// the bridge respawns a pod agent whose mail stays unstamped for two minutes
// (bridge.MailRespawnAfter), so the cap must sit well inside that.
var (
	DeliverAttempts   = 3
	DeliverRetryDelay = 2 * time.Second
	InitialBackoff    = time.Second
	MaxBackoff        = 60 * time.Second
	RedrainInitial    = 2 * time.Second
	RedrainMax        = 15 * time.Second
)

// Daemon is the slice of the beads daemon client the subscriber uses.
// *beadsapi.Client satisfies it.
type Daemon interface {
	EventStreamScoped(ctx context.Context, topics, agentBead, lastEventID string) (<-chan beadsapi.SSEEvent, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// Reassign sets the bead's assignee: Drain re-addresses parked
	// runner:<id> mail to this agent with it.
	Reassign(ctx context.Context, beadID, assignee string) error
}

// Coop is the slice of the coop loopback client the subscriber uses.
// *coopapi.Client satisfies it.
type Coop interface {
	Nudge(ctx context.Context, message string) (*coopapi.NudgeResponse, error)
	Shutdown(ctx context.Context) error
}

type Config struct {
	Daemon Daemon
	Coop   Coop
	// AgentBead scopes the stream (`?agent=`); AgentName is the mail
	// assignee this agent answers to.
	AgentBead string
	AgentName string
	Logger    *slog.Logger
	// OnControl handles control:<verb> mail other than kill. nil = log and
	// ignore.
	OnControl func(ctx context.Context, verb string, mail beadsapi.BeadDetail)
	// Exit is called after a control:kill has been handed to coop. The
	// default exits the process.
	Exit func()
	// Poll, when set, drains the agent's mail on this interval instead of
	// streaming it: a land door serves no event stream and the land feed
	// carries no mail (gd-2iqSr9ANOH).
	Poll time.Duration
	// RunnerID is the runner this agent joined on (gb agent start --runner),
	// or "". When set, Drain also claims mail the daemon parked for the
	// runner (assignee runner:<id>) because no agent was joined on it when
	// the mail was sent (spec §10): it re-addresses each to AgentName, then
	// delivers it.
	RunnerID string
}

type Subscriber struct {
	cfg Config

	mu sync.Mutex
	// delivered dedups by mail id: mail stays open until the agent runs
	// `gb mail read`, so a drain sees it again on every reconnect.
	delivered map[string]bool
	lastID    string
	// baselined is set once a drain has recorded the kill mail that was
	// already closed when this subscriber started (see drainClosedKills).
	baselined bool
}

func New(cfg Config) *Subscriber {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Exit == nil {
		cfg.Exit = func() { os.Exit(0) }
	}
	return &Subscriber{cfg: cfg, delivered: make(map[string]bool)}
}

// Run subscribes, drains, and delivers until ctx ends. A lost stream is
// reopened with exponential backoff and the last seen event id, and every
// connect drains unread mail, so mail that fell out of the daemon's replay
// ring is still delivered.
func (s *Subscriber) Run(ctx context.Context) error {
	if s.cfg.Poll > 0 {
		return s.poll(ctx)
	}
	backoff := InitialBackoff
	for {
		connected, err := s.stream(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = InitialBackoff
		}
		s.cfg.Logger.Warn("mail subscriber: stream lost, reconnecting", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, MaxBackoff)
	}
}

// poll drains on every tick of Config.Poll until ctx ends. A drain leaves
// mail coop did not take open, so the next tick re-drains it.
func (s *Subscriber) poll(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Poll)
	defer ticker.Stop()
	for {
		if err := s.Drain(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("mail subscriber: drain failed, retrying on the next poll", "error", err, "poll", s.cfg.Poll)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// stream runs one connection. connected reports whether the subscribe
// succeeded, which resets the reconnect backoff.
func (s *Subscriber) stream(ctx context.Context) (connected bool, err error) {
	s.mu.Lock()
	last := s.lastID
	s.mu.Unlock()
	events, err := s.cfg.Daemon.EventStreamScoped(ctx, mailproto.Topic, s.cfg.AgentBead, last)
	if err != nil {
		return false, fmt.Errorf("subscribe: %w", err)
	}
	// redrain is armed while some mail is still undelivered: a drain retries
	// it on a doubling delay until coop takes it, since nothing else would
	// until the stream reconnects.
	var redrain <-chan time.Time
	delay := RedrainInitial
	settle := func(err error) {
		if err == nil {
			redrain, delay = nil, RedrainInitial
			return
		}
		if redrain == nil {
			s.cfg.Logger.Warn("mail subscriber: mail left undelivered, re-draining", "error", err, "after", delay)
			redrain = time.After(delay)
			delay = min(delay*2, RedrainMax)
		}
	}
	settle(s.Drain(ctx))
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-redrain:
			redrain = nil
			settle(s.Drain(ctx))
		case evt, ok := <-events:
			if !ok {
				return true, errors.New("event stream closed")
			}
			if evt.ID != "" {
				s.mu.Lock()
				s.lastID = evt.ID
				s.mu.Unlock()
			}
			switch err := s.handleEvent(ctx, evt); {
			case errors.Is(err, errNotDelivered):
				settle(s.Drain(ctx))
			case err != nil:
				settle(err)
			}
		}
	}
}

// eventBead is the bead of a beads.bead.created event. Fields is decoded the
// way the REST client decodes it (beadsapi.ParseFieldsJSON): the daemon's
// fields are any JSON, and one non-string value must not fail the whole
// event (gd-D8jkm7eyEO).
type eventBead struct {
	beadsapi.BeadDetail
	Fields json.RawMessage `json:"fields"`
}

// errNotDelivered marks a stream event the subscriber could not deliver
// itself: the caller re-drains, which reads the mail over REST.
var errNotDelivered = errors.New("mail event not delivered from the stream")

// handleEvent delivers a live mail event. The stream is scoped to this agent,
// so every mail on it is addressed here in the daemon's view: one that does
// not decode, or names another assignee, is logged and returned as
// errNotDelivered so a drain picks it up (gd-D8jkm7eyEO). Any other error is a
// failed delivery, which leaves the mail open for a re-drain.
func (s *Subscriber) handleEvent(ctx context.Context, evt beadsapi.SSEEvent) error {
	var p struct {
		Bead *eventBead `json:"bead"`
	}
	if err := json.Unmarshal(evt.Data, &p); err != nil || p.Bead == nil || p.Bead.ID == "" {
		s.cfg.Logger.Warn("mail subscriber: undecodable event, draining instead", "event", evt.ID, "error", err)
		return errNotDelivered
	}
	m := p.Bead.BeadDetail
	m.Fields = beadsapi.ParseFieldsJSON(p.Bead.Fields)
	if m.Type != "mail" {
		return nil
	}
	if m.Assignee != s.cfg.AgentName {
		s.cfg.Logger.Warn("mail subscriber: streamed mail names another assignee, draining instead",
			"mail", m.ID, "assignee", m.Assignee, "agent", s.cfg.AgentName)
		return errNotDelivered
	}
	s.cfg.Logger.Info("mail subscriber: mail received", "mail", m.ID, "event", evt.ID)
	if err := s.deliver(ctx, m); err != nil {
		s.cfg.Logger.Warn("mail subscriber: delivery failed, mail left open", "mail", m.ID, "error", err)
		return err
	}
	return nil
}

// Drain delivers every open mail addressed to this agent, oldest first, that
// this process has not delivered yet (`gb mail inbox` semantics), after
// honouring any control:kill mail that was closed without this subscriber
// acting on it. A mail coop did not take is left open and its error is
// returned, so the caller re-drains.
func (s *Subscriber) Drain(ctx context.Context) error {
	if err := s.drainClosedKills(ctx); err != nil {
		s.cfg.Logger.Warn("mail subscriber: closed kill mail check failed", "error", err)
	}
	res, err := s.cfg.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"mail"},
		Statuses: []string{"open"},
		Assignee: s.cfg.AgentName,
		Sort:     "created_at",
		Limit:    100,
	})
	if err != nil {
		return err
	}
	var failed []error
	for _, b := range res.Beads {
		if err := s.deliver(ctx, *b); err != nil {
			s.cfg.Logger.Warn("mail subscriber: drain delivery failed, mail left open", "mail", b.ID, "error", err)
			failed = append(failed, err)
		}
	}
	if err := s.drainParkedRunnerMail(ctx); err != nil {
		failed = append(failed, err)
	}
	return errors.Join(failed...)
}

// drainParkedRunnerMail claims the open mail addressed to runner:<RunnerID>.
// Each is re-addressed to this agent before it is delivered, so a second
// agent joined on the same runner cannot also act on it, and the mail then
// reads as this agent's own (the daemon's control: guard admits the runner
// re-addressing mail to its own agent). A mail whose re-address fails is
// left parked and not delivered.
func (s *Subscriber) drainParkedRunnerMail(ctx context.Context) error {
	if s.cfg.RunnerID == "" {
		return nil
	}
	res, err := s.cfg.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"mail"},
		Statuses: []string{"open"},
		Assignee: "runner:" + s.cfg.RunnerID,
		Sort:     "created_at",
		Limit:    100,
	})
	if err != nil {
		return err
	}
	var failed []error
	for _, b := range res.Beads {
		if err := s.cfg.Daemon.Reassign(ctx, b.ID, s.cfg.AgentName); err != nil {
			s.cfg.Logger.Warn("mail subscriber: claiming parked runner mail failed, mail left parked", "mail", b.ID, "runner", s.cfg.RunnerID, "error", err)
			failed = append(failed, err)
			continue
		}
		m := *b
		m.Assignee = s.cfg.AgentName
		s.cfg.Logger.Info("mail subscriber: claimed parked runner mail", "mail", m.ID, "runner", s.cfg.RunnerID)
		if err := s.deliver(ctx, m); err != nil {
			s.cfg.Logger.Warn("mail subscriber: drain delivery failed, mail left open", "mail", m.ID, "error", err)
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// drainClosedKills makes closing a kill mail useless as a way to cancel it.
// The agent's session runs under the same uid as this subscriber and, on
// --local, holds the owner's token, so nothing stops it closing (or stamping
// control_acted on) its own kill mail before a drain reads it. The kill is
// therefore honoured whether the mail is open or closed. The first drain only
// records the kill mail already closed at that point as handled: it was
// acted on before this subscriber started (a resumed session must not die on
// an old kill), and a baseline needs no clock shared with the daemon. Newest
// first, so a long history cannot push a fresh kill past the limit.
func (s *Subscriber) drainClosedKills(ctx context.Context) error {
	res, err := s.cfg.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"mail"},
		Statuses: []string{"closed"},
		Labels:   []string{mailproto.ControlLabel(mailproto.ControlKill)},
		Assignee: s.cfg.AgentName,
		Sort:     "-created_at",
		Limit:    100,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	baseline := !s.baselined
	s.baselined = true
	s.mu.Unlock()
	for _, b := range res.Beads {
		if baseline {
			s.markDelivered(b.ID)
			continue
		}
		if err := s.deliver(ctx, *b); err != nil {
			return err
		}
	}
	return nil
}

// errRefused marks a mail coop answered but did not type in.
var errRefused = errors.New("coop refused the nudge")

func (s *Subscriber) deliver(ctx context.Context, m beadsapi.BeadDetail) error {
	s.mu.Lock()
	done := s.delivered[m.ID]
	s.mu.Unlock()
	if done {
		return nil
	}

	if verb, ok := mailproto.ControlVerb(m.Labels); ok {
		s.markDelivered(m.ID)
		if verb == mailproto.ControlKill {
			s.kill(ctx, m)
			return nil
		}
		if s.cfg.OnControl != nil {
			s.cfg.OnControl(ctx, verb, m)
		} else {
			s.cfg.Logger.Warn("mail subscriber: unhandled control mail", "verb", verb, "mail", m.ID)
		}
		return nil
	}

	msg := FormatNudge(m)
	var lastErr error
	for attempt := 1; attempt <= DeliverAttempts; attempt++ {
		resp, err := s.cfg.Coop.Nudge(ctx, msg)
		if err == nil && resp != nil && !resp.Delivered {
			// coop answers a nudge it did not type in (agent in a prompt or
			// starting) with delivered=false and a reason. The mail is left
			// undelivered and unstamped, so the caller's re-drain offers it
			// again once the agent can take it (gd-inbwLZfEYe).
			s.cfg.Logger.Warn("mail refused by coop, left undelivered", "mail", m.ID, "coop_reason", resp.Reason)
			return fmt.Errorf("deliver %s: %w: %s", m.ID, errRefused, resp.Reason)
		}
		if err == nil {
			s.markDelivered(m.ID)
			s.cfg.Logger.Info("mail delivered to coop", "mail", m.ID, "attempt", attempt)
			s.stampDelivered(ctx, m)
			return nil
		}
		lastErr = err
		if attempt < DeliverAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(DeliverRetryDelay):
			}
		}
	}
	return fmt.Errorf("deliver %s after %d attempts: %w", m.ID, DeliverAttempts, lastErr)
}

// kill closes the kill mail, asks coop to shut down, then exits. The mail is
// closed first because nobody runs `gb mail read` on it: left open, the next
// session started under the same agent name would drain it and stop at once.
// The close stamps control_acted, without which the daemon refuses this
// agent's token a close of control mail. Mail someone else already closed is
// left as it is.
func (s *Subscriber) kill(ctx context.Context, m beadsapi.BeadDetail) {
	s.cfg.Logger.Info("mail subscriber: control:kill received, stopping coop", "mail", m.ID, "from", mailproto.Sender(m.Labels), "status", m.Status)
	if m.Status != "closed" {
		acted := map[string]string{mailproto.FieldControlActed: time.Now().UTC().Format(time.RFC3339)}
		if err := s.cfg.Daemon.CloseBead(ctx, m.ID, acted); err != nil {
			s.cfg.Logger.Warn("mail subscriber: closing kill mail failed", "mail", m.ID, "error", err)
		}
	}
	if err := s.cfg.Coop.Shutdown(ctx); err != nil {
		s.cfg.Logger.Warn("mail subscriber: coop shutdown failed", "error", err)
	}
	s.cfg.Exit()
}

// stampDelivered records on the mail that coop took it, so the bridge does
// not mistake mail a live pod has but has not read for mail no pod received.
// Best-effort: a failed stamp only risks a respawn the bridge did not need.
func (s *Subscriber) stampDelivered(ctx context.Context, m beadsapi.BeadDetail) {
	if m.Fields[mailproto.FieldDeliveredAt] != "" {
		return
	}
	stamp := map[string]string{mailproto.FieldDeliveredAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.cfg.Daemon.UpdateBeadFields(ctx, m.ID, stamp); err != nil {
		s.cfg.Logger.Warn("mail subscriber: stamping delivered_at failed", "mail", m.ID, "error", err)
	}
}

func (s *Subscriber) markDelivered(id string) {
	s.mu.Lock()
	s.delivered[id] = true
	s.mu.Unlock()
}

// FormatNudge renders the coop message for a mail. Interrupt mail carries its
// body inline; other mail is the one-line pointer the bridge sends today.
func FormatNudge(m beadsapi.BeadDetail) string {
	from := mailproto.Sender(m.Labels)
	if slices.Contains(m.Labels, mailproto.LabelDeliveryInterrupt) {
		return fmt.Sprintf("Mail from %s [%s]: %s\n\n%s", from, m.ID, m.Title, m.Description)
	}
	return fmt.Sprintf("New mail from %s: %s — run 'kd show %s' to read", from, m.Title, m.ID)
}
