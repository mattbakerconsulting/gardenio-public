// lands_support.go turns a message in a tenant land's #support into a support
// bead in that land and tells the operator (Lands Track B spec §6.1, step 11.1,
// gd-UBeJ3kVicQ).
//
// A tenant cannot see the operator's land, so support lives in each tenant
// land: the provisioner seeds #support there (internal/lands/provision,
// DefaultChannels), and this watcher is what makes a post in it reach a human.
//
//   - A top-level post creates ONE support bead per thread: a type:task in the
//     land's project, labelled support, assigned to the support operator, so the
//     tenant can see their request and its state in their own land and no agent
//     takes it for work. The create carries the idempotency key
//     lands-support:<root message id>, so a redelivered event, the sweep and a
//     second bridge replica all converge on the same bead.
//   - A reply in that thread is added to the bead as a comment.
//   - Each one DMs the support operator (LANDS_SUPPORT_OPERATOR), the way an
//     escalation DMs the human it is addressed to. When no DM can be sent
//     (operator unset or not resolvable to a Slack id), a pointer with the bead
//     link and no tenant text goes to the fallback channel: a tenant's words
//     reach the operator's Slack only in a DM to the operator (B-D11; Privacy
//     Policy, "Slack Technologies").
//   - support_notified_at / support_notified_via on the bead record that the
//     operator was told about the request. The sweep (every SweepInterval)
//     re-runs top-level posts of the last day whose bead lacks the stamp, so a
//     post made while the bridge was down, or a DM that failed, is not lost.
//     Replies are delivered from SSE only; one missed while the bridge was down
//     is still in the thread, where the operator reads the request.
//
// Messages written by the support operator, and the Commons' own notices, are
// not requests. Everything else in #support is, the steward's posts included:
// #support is the steward's only way to the operator (spec §4.2).
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// SupportLabel marks a support bead (spec §6.1).
	SupportLabel = "support"
	// SupportChannelName is the seeded channel's name (provision.DefaultChannels).
	SupportChannelName = "support"

	// Fields on the support bead (kbeads type:task declares them).
	SupportMessageField    = "support_message_id"
	SupportChannelField    = "support_channel_id"
	SupportNotifiedAtField = "support_notified_at"
	SupportNotifiedVia     = "support_notified_via"

	supportAuthor        = "gardenio-bridge"
	supportTitleLimit    = 80
	supportQuoteLimit    = 500
	supportSweepLookback = 24 * time.Hour
	// supportLandsTTL bounds how stale the tenant-land list may be. Every
	// Commons message in every land passes through handleEvent, so the list
	// is not read per message.
	supportLandsTTL = time.Minute
	// SupportSweepInterval is how often the sweep re-runs un-notified requests.
	SupportSweepInterval = 5 * time.Minute
)

// LandsSupportDaemon is the slice of the daemon client the watcher needs.
// *beadsapi.Client satisfies it.
type LandsSupportDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddComment(ctx context.Context, beadID, author, text string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
}

// LandsSupportConfig configures the watcher.
type LandsSupportConfig struct {
	Daemon LandsSupportDaemon
	// Sender DMs the operator. *Bot satisfies it. nil: pointers only.
	Sender EscalationDMSender
	// Operator is who answers support: an email or oidc:<email>
	// (LANDS_SUPPORT_OPERATOR). It is the support bead's assignee and the DM's
	// addressee. Empty: beads are still made, and the pointer goes to the
	// fallback channel.
	Operator string
	// ResolveSlackID maps the operator to a Slack user id ("" when it cannot).
	ResolveSlackID func(ctx context.Context, operator string) string
	// PostPointer posts a line with no tenant text to the fallback channel.
	// Optional.
	PostPointer func(ctx context.Context, text string) error
	// LinkBead renders a Slack link for a bead id + title. Optional.
	LinkBead func(id, title string) string
	Logger   *slog.Logger
	Now      func() time.Time
}

// LandsSupport is the #support watcher.
type LandsSupport struct {
	daemon   LandsSupportDaemon
	sender   EscalationDMSender
	operator string
	resolve  func(ctx context.Context, operator string) string
	pointer  func(ctx context.Context, text string) error
	link     func(id, title string) string
	logger   *slog.Logger
	now      func() time.Time
	seen     *seenSet

	mu        sync.Mutex
	tenants   map[string]bool
	tenantsAt time.Time
}

// NewLandsSupport builds the watcher; nil without a daemon.
func NewLandsSupport(cfg LandsSupportConfig) *LandsSupport {
	if cfg.Daemon == nil {
		return nil
	}
	s := &LandsSupport{
		daemon: cfg.Daemon, sender: cfg.Sender, operator: strings.TrimSpace(cfg.Operator),
		resolve: cfg.ResolveSlackID, pointer: cfg.PostPointer, link: cfg.LinkBead,
		logger: cfg.Logger, now: cfg.Now, seen: newSeenSet(seenTTL, seenMaxEntries),
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.link == nil {
		s.link = func(id, title string) string { return escapeSlackMrkdwn(title) + " (" + id + ")" }
	}
	return s
}

// RegisterHandlers wires the watcher onto the SSE stream. The Commons creates
// its messages already closed, so both topics can carry a new one; the seen
// set keeps it to one pass per message.
func (s *LandsSupport) RegisterHandlers(stream *SSEStream) {
	if s == nil {
		return
	}
	stream.On("beads.bead.created", s.handleEvent)
	stream.On("beads.bead.closed", s.handleEvent)
	s.logger.Info("lands support watcher registered", "operator_set", s.operator != "")
}

// Run sweeps for un-notified requests until ctx ends.
func (s *LandsSupport) Run(ctx context.Context) {
	if s == nil {
		return
	}
	ticker := time.NewTicker(SupportSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.seen.gc()
			s.Sweep(ctx)
		}
	}
}

func (s *LandsSupport) handleEvent(ctx context.Context, data []byte) {
	ev := ParseBeadEvent(data)
	if ev == nil || ev.Type != "message" || ev.Fields["channel_id"] == "" {
		return
	}
	if s.seen.markSeen("support-msg:" + ev.ID) {
		return
	}
	msg := supportMessage{ID: ev.ID, Labels: ev.Labels, Fields: ev.Fields, CreatedBy: ev.CreatedBy}
	if err := s.handle(ctx, msg, false); err != nil {
		s.seen.release("support-msg:" + ev.ID)
		s.logger.Warn("lands support: message not handled", "message", ev.ID, "error", err)
	}
}

// supportMessage is the part of a message bead the watcher reads.
type supportMessage struct {
	ID        string
	Labels    []string
	Fields    map[string]string
	CreatedBy string
}

func (m supportMessage) author() string {
	if a := strings.TrimSpace(m.Fields["author"]); a != "" {
		return a
	}
	return strings.TrimSpace(m.CreatedBy)
}

// supportLand returns the land of a message in a tenant land's #support, or "".
func (s *LandsSupport) supportLand(ctx context.Context, m supportMessage) (string, error) {
	land := projectFromLabels(m.Labels)
	if land == "" {
		return "", nil
	}
	tenants, err := s.tenantLands(ctx)
	if err != nil {
		return "", err
	}
	if !tenants[land] {
		return "", nil
	}
	ch, err := s.daemon.GetBead(ctx, m.Fields["channel_id"])
	if err != nil {
		return "", fmt.Errorf("reading channel %s: %w", m.Fields["channel_id"], err)
	}
	if ch == nil || ch.Type != "channel" || ch.Fields["name"] != SupportChannelName ||
		!hasLabel(ch.Labels, "project:"+land) {
		return "", nil
	}
	return land, nil
}

// tenantLands returns the set of tenant lands, read at most once a minute.
func (s *LandsSupport) tenantLands(ctx context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenants != nil && s.now().Sub(s.tenantsAt) < supportLandsTTL {
		return s.tenants, nil
	}
	projects, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading lands: %w", err)
	}
	tenants := map[string]bool{}
	for land, p := range projects {
		if !p.IsOperatorLand() {
			tenants[land] = true
		}
	}
	s.tenants, s.tenantsAt = tenants, s.now()
	return tenants, nil
}

// fromOperator reports whether the support operator wrote the message.
func (s *LandsSupport) fromOperator(author string) bool {
	if s.operator == "" || author == "" {
		return false
	}
	op := normalizeEmail(emailFromOperator(s.operator))
	return op != "" && normalizeEmail(emailFromOperator(author)) == op
}

// handle files or extends the support bead for one message. sweep is true
// when the sweep (not SSE) found it: then only an un-notified request is told.
func (s *LandsSupport) handle(ctx context.Context, m supportMessage, sweep bool) error {
	if k := m.Fields["kind"]; k == "notice" || k == "card" {
		return nil
	}
	author := m.author()
	if s.fromOperator(author) || author == supportAuthor {
		return nil
	}
	land, err := s.supportLand(ctx, m)
	if err != nil || land == "" {
		return err
	}
	root := strings.TrimSpace(m.Fields["thread_id"])
	reply := root != "" && root != m.ID
	if !reply {
		root = m.ID
	}
	text := strings.TrimSpace(m.Fields["text"])

	bead, err := s.supportBead(ctx, land, root, m, author, text, reply)
	if err != nil {
		return err
	}
	if reply {
		if sweep {
			return nil
		}
		if err := s.daemon.AddComment(ctx, bead.ID, supportAuthor,
			fmt.Sprintf("%s replied in #support (message %s):\n\n%s", author, m.ID, text)); err != nil {
			return fmt.Errorf("commenting on %s: %w", bead.ID, err)
		}
		s.notify(ctx, land, bead, author, text, true)
		return nil
	}
	if bead.Fields[SupportNotifiedAtField] != "" {
		return nil
	}
	s.notify(ctx, land, bead, author, text, false)
	return nil
}

// supportBead gets or creates the thread's support bead.
func (s *LandsSupport) supportBead(ctx context.Context, land, root string, m supportMessage, author, text string, reply bool) (*beadsapi.BeadDetail, error) {
	firstLine := text
	if reply {
		// Titled by the thread's first post when the bead is made from a
		// reply (the first post was the operator's, or the bead was lost).
		if r, err := s.daemon.GetBead(ctx, root); err == nil && r != nil {
			firstLine = strings.TrimSpace(r.Fields["text"])
		}
	}
	if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	if firstLine = strings.TrimSpace(firstLine); firstLine == "" {
		firstLine = "(no text)"
	}
	desc := fmt.Sprintf("Support request from %s in #support of land %s (message %s).\n\n"+
		"The thread is the conversation: answer there. This bead is the record: close it when the request is answered "+
		"(gardenio/docs/lands/support-runbook.md).\n\n---\n\n%s", author, land, root, text)
	fields, err := json.Marshal(map[string]string{
		SupportMessageField: root,
		SupportChannelField: m.Fields["channel_id"],
	})
	if err != nil {
		return nil, err
	}
	req := beadsapi.CreateBeadRequest{
		Title:          truncateRunes(fmt.Sprintf("Support (%s): %s", land, firstLine), supportTitleLimit+len(land)+12),
		Type:           "task",
		Kind:           "issue",
		Priority:       2,
		Description:    desc,
		Labels:         []string{SupportLabel, "project:" + land},
		Assignee:       emailFromOperator(s.operator),
		CreatedBy:      supportAuthor,
		Fields:         fields,
		IdempotencyKey: "lands-support:" + root,
	}
	id, err := s.daemon.CreateBead(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("filing the support bead in %s: %w", land, err)
	}
	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil || bead == nil {
		return nil, fmt.Errorf("reading support bead %s: %w", id, err)
	}
	return bead, nil
}

// notify DMs the operator, or posts a pointer when it cannot, and stamps the
// bead. A failure is logged; the sweep retries an un-stamped request.
func (s *LandsSupport) notify(ctx context.Context, land string, bead *beadsapi.BeadDetail, author, text string, reply bool) {
	link := s.link(bead.ID, bead.Title)
	via := ""
	if slackID := s.slackID(ctx); slackID != "" && s.sender != nil {
		// A DM has no channel theme; Garden is the default every theme falls back to.
		head := fmt.Sprintf("%s *Support request* in land `%s` from %s: %s", Garden.Semantic(SemQuestion), land, escapeSlackMrkdwn(author), link)
		if reply {
			head = fmt.Sprintf("%s *Reply* in land `%s` from %s on %s", Garden.Semantic(SemNote), land, escapeSlackMrkdwn(author), link)
		}
		if _, _, err := s.sender.SendDM(ctx, slackID, head+"\n"+quoteMrkdwn(truncateRunes(text, supportQuoteLimit))); err != nil {
			s.logger.Warn("lands support: DM failed", "bead", bead.ID, "error", err)
		} else {
			via = "dm"
		}
	}
	if via == "" && s.pointer != nil {
		what := "New support request"
		if reply {
			what = "Reply on support request"
		}
		// The bead id only: the title quotes the tenant's first line.
		if err := s.pointer(ctx, fmt.Sprintf("%s %s in land `%s`: %s (no DM to the support operator: "+
			"LANDS_SUPPORT_OPERATOR is unset or has no Slack id, or the DM failed)",
			Garden.Semantic(SemQuestion), what, land, s.link(bead.ID, bead.ID))); err != nil {
			s.logger.Warn("lands support: pointer failed", "bead", bead.ID, "error", err)
		} else {
			via = "channel"
		}
	}
	if via == "" {
		s.logger.Error("lands support: operator not told of a support request", "bead", bead.ID, "land", land)
		return
	}
	if reply {
		return
	}
	if err := s.daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{
		SupportNotifiedAtField: s.now().UTC().Format(time.RFC3339),
		SupportNotifiedVia:     via,
	}); err != nil {
		s.logger.Warn("lands support: stamp failed", "bead", bead.ID, "error", err)
	}
	s.logger.Info("lands support: operator told", "bead", bead.ID, "land", land, "via", via)
}

func (s *LandsSupport) slackID(ctx context.Context) string {
	if s.operator == "" || s.resolve == nil {
		return ""
	}
	return s.resolve(ctx, s.operator)
}

// Sweep re-runs the top-level #support posts of the last day in every tenant
// land, telling the operator about any whose bead is not stamped.
func (s *LandsSupport) Sweep(ctx context.Context) {
	if s == nil {
		return
	}
	tenants, err := s.tenantLands(ctx)
	if err != nil {
		s.logger.Warn("lands support sweep: reading lands", "error", err)
		return
	}
	since := s.now().Add(-supportSweepLookback)
	for land := range tenants {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types: []string{"message"}, Labels: []string{"project:" + land}, CreatedAfter: since, Limit: 500,
		})
		if err != nil {
			s.logger.Warn("lands support sweep: reading messages", "land", land, "error", err)
			continue
		}
		for _, b := range res.Beads {
			if b.Fields["channel_id"] == "" || (b.Fields["thread_id"] != "" && b.Fields["thread_id"] != b.ID) {
				continue
			}
			m := supportMessage{ID: b.ID, Labels: b.Labels, Fields: b.Fields, CreatedBy: b.CreatedBy}
			if err := s.handle(ctx, m, true); err != nil {
				s.logger.Warn("lands support sweep: message not handled", "message", b.ID, "error", err)
			}
		}
	}
}

// quoteMrkdwn renders text as a Slack block quote, escaped.
func quoteMrkdwn(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(escapeSlackMrkdwn(text), "\n")
	for i, l := range lines {
		lines[i] = ">" + l
	}
	return strings.Join(lines, "\n")
}
