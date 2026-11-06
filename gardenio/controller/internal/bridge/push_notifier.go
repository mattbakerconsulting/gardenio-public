package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/payload"
	"github.com/sideshow/apns2/token"
)

// PushNotifier sends APNs alerts for decision lifecycle events to the
// iOS devices of the humans each alert is addressed to. It implements the
// Notifier interface so the existing decisions watcher can wrap it
// alongside the Slack notifier.
//
// Recipients (Lands 4.7b, gd-a7U4AgwgCA): an alert goes to the bead's
// addressed human, resolved by alertRecipients, and only to the devices
// that human registered (kbeads /v1/devices is per person). An alert with
// no addressed human sends nothing; it never falls back to every device.
//
// Delivery model: the notifier does not cache the device list —
// instead, every dispatch queries the beads daemon's /v1/devices
// endpoint for the recipients. That keeps freshly-registered devices
// visible immediately, and the N is small enough (a few rows per human)
// that the extra round-trip is trivial compared to the APNs latency.
//
// When APNs reports a token is no longer valid (HTTP 410 Gone) the
// notifier removes it from the registry so subsequent decisions don't
// waste cycles trying to reach a dead install.
//
// Deep link (Lands, Commons on a phone M6, gd-JSRReAJLRL): with GardenURL set,
// every alert carries "url", the bead's place in the Commons
// (/garden/c/bead/{id}, internal/commons/place.go). The viewer resolves it when
// the alert is tapped, to the thread holding the bead's card, so the link works
// even though the card is posted at the same moment as the push.
type PushNotifier struct {
	cfg       PushNotifierConfig
	logger    *slog.Logger
	daemon    PushDeviceSource
	apns      apnsPusher
	bundleID  string
	gardenURL string
}

// PushDeviceSource is the subset of beadsapi.Client that the push
// notifier depends on — defined as an interface for testability. GetBead
// and FindAgentBead resolve who an alert is addressed to.
type PushDeviceSource interface {
	ListDevices(ctx context.Context, userEmails []string, platform string) ([]*beadsapi.Device, error)
	DeleteDevice(ctx context.Context, token string) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
}

// apnsPusher is the subset of *apns2.Client the notifier uses, so tests can
// stand in for APNs.
type apnsPusher interface {
	PushWithContext(ctx apns2.Context, n *apns2.Notification) (*apns2.Response, error)
}

// PushNotifierConfig holds APNs credentials + mode.
type PushNotifierConfig struct {
	// Path to the APNs authentication .p8 file (preferred). When
	// empty, KeyPEM is read instead.
	KeyPath string
	// Raw PEM contents of the .p8. Useful when the key is mounted
	// as a K8s secret env var rather than a file.
	KeyPEM string
	// 10-character APNs key id (from developer.apple.com → Keys).
	KeyID string
	// Apple developer team id.
	TeamID string
	// App bundle id — used as the APNs topic.
	BundleID string
	// true = production APNs host; false = sandbox (development builds).
	// Must match the aps-environment entitlement in the shipped app.
	Production bool
	// GardenURL is the viewer's public URL, origin and base path
	// (BEADS_VIEWER_URL), where an alert's deep link points. Empty sends no link.
	GardenURL string
}

// Enabled reports whether the notifier has enough config to do
// anything useful. Callers should skip wiring the notifier entirely
// when this returns false.
func (c PushNotifierConfig) Enabled() bool {
	if c.KeyPath == "" && c.KeyPEM == "" {
		return false
	}
	return c.KeyID != "" && c.TeamID != "" && c.BundleID != ""
}

// NewPushNotifier constructs a notifier. Returns nil + error when
// config is invalid or the key fails to parse. When cfg.Enabled() is
// false, returns nil, nil — callers treat that as "push disabled".
func NewPushNotifier(cfg PushNotifierConfig, daemon PushDeviceSource, logger *slog.Logger) (*PushNotifier, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	pem := []byte(cfg.KeyPEM)
	if len(pem) == 0 {
		data, err := os.ReadFile(cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("apns: read key file: %w", err)
		}
		pem = data
	}
	authKey, err := token.AuthKeyFromBytes(pem)
	if err != nil {
		return nil, fmt.Errorf("apns: parse .p8 key: %w", err)
	}
	tok := &token.Token{
		AuthKey: authKey,
		KeyID:   cfg.KeyID,
		TeamID:  cfg.TeamID,
	}

	client := apns2.NewTokenClient(tok)
	if cfg.Production {
		client.Production()
	} else {
		client.Development()
	}
	client.HTTPClient.Timeout = 10 * time.Second

	return &PushNotifier{
		cfg:       cfg,
		logger:    logger,
		daemon:    daemon,
		apns:      client,
		bundleID:  cfg.BundleID,
		gardenURL: strings.TrimRight(cfg.GardenURL, "/"),
	}, nil
}

// NotifyDecision alerts the decision's addressed human. The payload includes the bead id so the iOS app can deep-link to the
// decision when the user taps the notification.
func (p *PushNotifier) NotifyDecision(ctx context.Context, bead BeadEvent) error {
	title := "Decision requested"
	body := decisionSummary(bead)
	pl := payload.NewPayload().
		AlertTitle(title).
		AlertBody(body).
		Sound("default").
		Custom("bead_id", bead.ID).
		Custom("kind", "decision").
		ThreadID("decision:" + bead.ID)
	p.link(pl, bead.ID)
	if bead.Priority <= 1 {
		// High-priority decision: bump the interruption level so iOS
		// shows it on the lock screen even when DND is on.
		pl = pl.InterruptionLevel(payload.InterruptionLevelTimeSensitive)
	}
	return p.dispatch(ctx, bead, pl)
}

// UpdateDecision is a no-op for push — the user doesn't need a banner
// every time an agent picks an option. Could be a silent content-
// available push later to refresh the app.
func (p *PushNotifier) UpdateDecision(_ context.Context, _, _, _ string) error { return nil }

// NotifyEscalation is treated the same as a fresh decision for now —
// the user wants attention.
func (p *PushNotifier) NotifyEscalation(ctx context.Context, bead BeadEvent) error {
	pl := payload.NewPayload().
		AlertTitle("Escalation").
		AlertBody(alertLine(bead.Title)).
		Sound("default").
		Custom("bead_id", bead.ID).
		Custom("kind", "escalation").
		ThreadID("escalation:" + bead.ID)
	p.link(pl, bead.ID)
	return p.dispatch(ctx, bead, pl)
}

// link adds the alert's deep link: where bead id is in the Commons.
func (p *PushNotifier) link(pl *payload.Payload, id string) {
	if p.gardenURL != "" && id != "" {
		pl.Custom("url", p.gardenURL+"/garden/c/bead/"+url.PathEscape(id))
	}
}

// DismissDecision is a no-op: iOS has no clean API to retract a
// delivered banner. If/when we want to remove stale notifications
// from the Notification Center we'd send a silent push carrying a
// "dismiss" payload the app could act on on foreground.
func (p *PushNotifier) DismissDecision(_ context.Context, _ string) error { return nil }

// PostReport is a no-op for push — reports land in the app's activity
// feed via SSE, no separate alert needed.
func (p *PushNotifier) PostReport(_ context.Context, _, _, _, _ string) error { return nil }

// dispatch fans a payload out to the iOS devices of the bead's addressed
// humans, running sends concurrently so one slow device doesn't stall the
// rest. Invalid tokens (410 Gone) are pruned from the daemon's registry so
// subsequent decisions skip them.
func (p *PushNotifier) dispatch(ctx context.Context, bead BeadEvent, pl *payload.Payload) error {
	recipients := p.alertRecipients(ctx, bead)
	if len(recipients) == 0 {
		// ListDevices with no owners would return every device, which is
		// exactly the fleet-wide fan-out this replaced.
		p.logger.Debug("apns: no addressed human, nothing to push", "bead", bead.ID)
		return nil
	}
	listed, err := p.daemon.ListDevices(ctx, recipients, "ios")
	if err != nil {
		return fmt.Errorf("apns: list devices: %w", err)
	}
	// Keep only the recipients' rows even if the daemon returned more (a
	// daemon that ignored the filter must not turn into a broadcast).
	want := make(map[string]bool, len(recipients))
	for _, r := range recipients {
		want[r] = true
	}
	var devices []*beadsapi.Device
	for _, d := range listed {
		if d != nil && want[strings.ToLower(strings.TrimSpace(d.UserEmail))] {
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(devices))
	for _, d := range devices {
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.sendOne(ctx, d, pl); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// alertRecipients returns the lowercase emails of the humans an alert about
// bead is addressed to, the same addressee the Slack DM paths use
// (bot_decisions_operated_by.go, escalation_operator_dm.go):
//
//  1. the bead's own operated_by: the decision field, else the bead row
//     (escalations carry it on the row, and the SSE payload drops it);
//  2. only when the bead names nobody, the requesting agent's operator.
//
// A bead whose operated_by names a non-human (fleet:bridge:jira) is
// addressed to no one and gets no push. Only oidc: humans resolve: a device
// row is keyed by the email of the OIDC token that registered it.
func (p *PushNotifier) alertRecipients(ctx context.Context, bead BeadEvent) []string {
	operatedBy := strings.TrimSpace(bead.Fields["operated_by"])
	if e := pushRecipientEmail(operatedBy, ""); e != "" {
		return []string{e}
	}
	// The row carries the daemon-resolved operator email (so a linked
	// slack:U… operator still resolves) and, for escalations, operated_by
	// itself. Its email is used only when it describes the same operator.
	var operatedByEmail string
	if bead.ID != "" {
		if detail, err := p.daemon.GetBead(ctx, bead.ID); err != nil {
			p.logger.Warn("apns: cannot read bead for recipients", "bead", bead.ID, "error", err)
		} else if detail != nil {
			rowOp := strings.TrimSpace(detail.OperatedBy)
			if operatedBy == "" {
				operatedBy = rowOp
			}
			if rowOp == operatedBy {
				operatedByEmail = detail.OperatedByEmail
			}
		}
	}
	if operatedBy != "" {
		if e := pushRecipientEmail(operatedBy, operatedByEmail); e != "" {
			return []string{e}
		}
		return nil
	}

	agent := p.requestingAgent(ctx, bead)
	if agent == nil {
		return nil
	}
	agentOp := strings.TrimSpace(agent.OperatedBy)
	if agentOp == "" {
		agentOp = strings.TrimSpace(agent.Fields["operated_by"])
	}
	if e := pushRecipientEmail(agentOp, agent.OperatedByEmail); e != "" {
		return []string{e}
	}
	return nil
}

// requestingAgent returns the agent bead that filed bead, or nil.
// Escalations name it by bead id (requesting_agent); decisions by bead id
// (requesting_agent_bead_id) or by name (assignee, requested_by).
func (p *PushNotifier) requestingAgent(ctx context.Context, bead BeadEvent) *beadsapi.BeadDetail {
	for _, key := range []string{"requesting_agent", "requesting_agent_bead_id"} {
		if id := strings.TrimSpace(bead.Fields[key]); id != "" {
			if detail, err := p.daemon.GetBead(ctx, id); err == nil && detail != nil {
				return detail
			}
		}
	}
	for _, name := range []string{bead.Assignee, bead.Fields["requested_by"]} {
		if name = extractAgentName(strings.TrimSpace(name)); name != "" {
			if detail, err := p.daemon.FindAgentBead(ctx, name); err == nil && detail != nil {
				return detail
			}
		}
	}
	return nil
}

// pushRecipientEmail returns the lowercase email of a human operator, from
// the resolved operator email when the daemon supplied one, else from an
// oidc:<email> operated_by. Anything else (slack:U…, fleet:…, agent names)
// is "".
func pushRecipientEmail(operatedBy, operatedByEmail string) string {
	if e := strings.ToLower(strings.TrimSpace(operatedByEmail)); e != "" && IsHumanOperator("oidc:"+e) {
		return e
	}
	if rest, ok := strings.CutPrefix(operatedBy, "oidc:"); ok && IsHumanOperator(operatedBy) {
		return strings.ToLower(rest)
	}
	return ""
}

func (p *PushNotifier) sendOne(ctx context.Context, d *beadsapi.Device, pl *payload.Payload) error {
	n := &apns2.Notification{
		DeviceToken: d.Token,
		Topic:       p.bundleID,
		Payload:     pl,
		PushType:    apns2.PushTypeAlert,
	}
	resp, err := p.apns.PushWithContext(ctx, n)
	if err != nil {
		return fmt.Errorf("apns: push %s: %w", shortToken(d.Token), err)
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// 410 Gone means the token is no longer registered (user uninstalled
	// or disabled notifications). Prune it.
	if resp.StatusCode == http.StatusGone || resp.Reason == "BadDeviceToken" || resp.Reason == "Unregistered" {
		p.logger.Info("apns: pruning dead token",
			"token", shortToken(d.Token),
			"reason", resp.Reason,
			"user", d.UserEmail)
		if err := p.daemon.DeleteDevice(ctx, d.Token); err != nil {
			p.logger.Warn("apns: failed to prune token", "error", err)
		}
		return nil
	}
	return fmt.Errorf("apns: push %s: %d %s", shortToken(d.Token), resp.StatusCode, resp.Reason)
}

// decisionSummary returns the body of a decision alert. Priority
// prefixes help the user triage at a glance.
func decisionSummary(b BeadEvent) string {
	prefix := ""
	switch b.Priority {
	case 0:
		prefix = "[critical] "
	case 1:
		prefix = "[high] "
	}
	title := alertLine(b.Title)
	if len(title) > 160 {
		title = title[:157] + "…"
	}
	return prefix + title
}

// alertLine is a title's first line: an alert's body is one line, as the
// Commons list and Slack's own notification show a message.
func alertLine(s string) string { return strings.TrimSpace(firstLine(strings.TrimSpace(s))) }

// shortToken returns the first 10 chars of an APNs token for logging —
// the full 64-char hex is noise.
func shortToken(t string) string {
	if len(t) <= 10 {
		return t
	}
	return t[:10] + "…"
}
