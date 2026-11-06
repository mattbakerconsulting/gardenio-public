package bridge

// ScifGateAlert watches investigation-request beads for a human gate becoming
// OPEN and posts a pointer-only Slack alert to the acme-scif gate-alert channel.
//
// Task ka-0000002770 (Gate-2 OPENING alert). The two acme-scif human gates are
// both recorded as fields on the ONE investigation-request bead:
//
//   - Gate-1 (approve the RUN):     approval_status ∈ {pending, approved, denied}
//   - Gate-2 (approve the RELEASE): release_status  ∈ {pending, released, denied}
//
// Gate-2 fires when release_status TRANSITIONS to "pending" — i.e. a finished
// run is awaiting human release. The signal is written onto the bead by the
// gate's writer (originally the scif-portal reconcile sweep; that in-repo
// portal was deleted 2026-10-31, gd-Y4ZCGr2m2t); this watcher is the alert half.
//
// Task ka-0000006007 (Gate-1 OPENING alert + close-the-loop). The same watcher
// also announces a Gate-1 opening (approval_status=pending, set at CREATE by any
// requester) and threads the approve/deny outcome onto the opening message so
// the channel reads as a ledger. Gate-1 and Gate-2 key off different fields on
// the one bead and dedup under distinct composite keys, so neither suppresses
// the other. Both created + updated events are wired for both gates.
//
// PHI MINIMISATION (the load-bearing invariant of the whole acme-scif design):
// the alert carries ONLY pointers/provenance — the request bead id, the
// data_scope tenant, the requester, a hard-truncated excerpt of the author's
// ask, and a link to the portal. It NEVER carries the redacted report body,
// query results, or anything derived from the run's data. The human clicks
// through to the portal to review under four-eyes.
//
// FAIL-SAFE: a Slack outage MUST NEVER block or auto-decide a gate. A post
// failure is logged and dropped; the gate state on the bead is untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	slackapi "github.com/slack-go/slack"
)

// scifGateAlertBeadType is the only bead type this watcher acts on.
const scifGateAlertBeadType = "investigation-request"

// Gate-1 approval_status values (task ka-0000006007). The request writer stamps
// approval_status=pending at CREATE (originally the scif-portal, deleted
// 2026-10-31, gd-Y4ZCGr2m2t) and flips it to approved/denied when a human decides. Gate-1 is the RUN gate:
// approve before the launcher may run acme-scif against real clinical data.
const (
	scifApprovalStatusField = "approval_status"
	scifApprovalPending     = "pending"
	scifApprovalApproved    = "approved"
	scifApprovalDenied      = "denied"
)

// scifPromptExcerptMax caps the author's-ask excerpt included in the alert. The
// prompt is author-written free text (the investigation ask), not report/PHI
// content, but it is hard-truncated anyway so the alert stays a pointer, not a
// payload — a long prompt is never reproduced in Slack.
const scifPromptExcerptMax = 140

// ScifGateAlertConfig configures the ScifGateAlert watcher. This is the shared
// config the sibling RELEASED-alert watcher (ka-0000003991) reuses — one
// channel, one Slack client, plumbed once from slackBridge.scifGateAlert.channel.
type ScifGateAlertConfig struct {
	API NoticeSlackAPI
	// Channel is the acme-scif gate-alert Slack channel id
	// (slackBridge.scifGateAlert.channel, recorded under ka-0000007187). Empty
	// disables the watcher entirely (NewScifGateAlert returns nil) — a portal
	// with no gate-alert channel wired simply does not post, rather than
	// crashing or posting to a default.
	Channel string
	// PortalURL is the base URL of the SCIF review surface the alert links to
	// (e.g. https://gardenio.example.com/scif). Empty omits the link.
	PortalURL string
	// BeadsViewerURL, when set, deep-links the request bead id in the message.
	BeadsViewerURL string
	// ThemeFor resolves the gate-alert channel's card theme. Nil renders Garden.
	ThemeFor ThemeResolver
	Logger   *slog.Logger
}

// ScifGateAlert posts a Slack alert when a acme-scif human gate opens. It
// deduplicates by bead id so an SSE reconnect (or a redundant same-value stamp)
// cannot double-post the same opening.
type ScifGateAlert struct {
	api            NoticeSlackAPI
	channel        string
	portalURL      string
	beadsViewerURL string
	themeFor       ThemeResolver
	logger         *slog.Logger

	mu   sync.Mutex
	seen map[string]bool
	// ts remembers the opening-alert message ts per bead id so a Gate-1 outcome
	// (approve/deny) can thread onto the original opening — the channel reads as
	// a ledger, pending pings resolving in place. Lost on a bridge restart, in
	// which case the outcome posts standalone (still deduped, still advisory).
	ts map[string]string
}

// NewScifGateAlert returns a watcher, or nil if the Slack API or the gate-alert
// channel is unset (the watcher ships dark until both are wired).
func NewScifGateAlert(cfg ScifGateAlertConfig) *ScifGateAlert {
	if cfg.API == nil || strings.TrimSpace(cfg.Channel) == "" {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ScifGateAlert{
		api:            cfg.API,
		channel:        strings.TrimSpace(cfg.Channel),
		portalURL:      strings.TrimRight(strings.TrimSpace(cfg.PortalURL), "/"),
		beadsViewerURL: strings.TrimRight(strings.TrimSpace(cfg.BeadsViewerURL), "/"),
		themeFor:       cfg.ThemeFor,
		logger:         logger,
		seen:           make(map[string]bool),
		ts:             make(map[string]string),
	}
}

// RegisterHandlers wires the watcher onto an SSE stream. It listens on BOTH
// created and updated: Gate-2's release_status can only ever become "pending"
// via an update (a fresh request is born with release_status unset — the
// born-inert guard forbids it at create), while a future Gate-1 alert keys off
// approval_status=pending which IS present at create. handleCreated /
// handleUpdated funnel into the same matcher so adding Gate-1 is one predicate.
func (a *ScifGateAlert) RegisterHandlers(stream *SSEStream) {
	if a == nil {
		return
	}
	stream.On("beads.bead.created", a.handleCreated)
	stream.On("beads.bead.updated", a.handleUpdated)
	a.logger.Info("scif-gate-alert watcher registered SSE handlers", "channel", a.channel)
}

func (a *ScifGateAlert) handleCreated(ctx context.Context, data []byte) {
	// A created bead has no "changes" set; every field present is present at
	// birth. release_status is forbidden at create, so Gate-2 never matches
	// here — but Gate-1 (approval_status=pending at birth) does.
	a.handle(ctx, data, false)
	a.handleGate1(ctx, data, false)
}

func (a *ScifGateAlert) handleUpdated(ctx context.Context, data []byte) {
	// An updated bead carries "changes"; we only fire when the gate field
	// actually transitioned in THIS event, so an unrelated field update on an
	// already-open gate does not re-alert (belt-and-braces with id dedup).
	a.handle(ctx, data, true)
	a.handleGate1(ctx, data, true)
}

func (a *ScifGateAlert) handle(ctx context.Context, data []byte, requireChange bool) {
	if a == nil {
		return
	}
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != scifGateAlertBeadType {
		return
	}
	// Gate-2: release_status just became "pending" (a finished run awaiting
	// release). requireChange guards the updated path so only the transition
	// fires, never a later touch of an already-pending bead.
	if bead.Fields["release_status"] != "pending" {
		return
	}
	if requireChange && !changedTo(bead.Changes, "release_status", "pending") {
		return
	}
	if a.alreadySeen(bead.ID) {
		return
	}
	text := a.formatGate2(themeOf(ctx, a.themeFor, a.channel), bead)
	if _, _, err := a.api.PostMessageContext(ctx, a.channel,
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionDisableLinkUnfurl(),
	); err != nil {
		// FAIL-SAFE: log and drop. The gate state on the bead is untouched; a
		// human can still find the run in the portal queue. Never block a gate
		// on a Slack outage. We deliberately do NOT mark the bead seen here, so a
		// duplicate event (SSE reconnect replay) can retry a transiently-failed
		// post rather than swallowing the only alert.
		a.logger.Warn("scif-gate-alert post failed", "id", bead.ID, "channel", a.channel, "error", err)
		return
	}
	a.markSeen(bead.ID)
	a.logger.Info("scif-gate-alert posted (Gate-2 opening)", "id", bead.ID, "channel", a.channel)
}

// handleGate1 announces a Gate-1 opening (investigation-request created
// approval_status=pending) and threads the approve/deny outcome onto the same
// message (task ka-0000006007). It runs alongside — never replacing — the Gate-2
// path above; the two gates key off different fields on the same bead and dedup
// under distinct composite keys, so neither suppresses the other.
func (a *ScifGateAlert) handleGate1(ctx context.Context, data []byte, requireChange bool) {
	if a == nil {
		return
	}
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != scifGateAlertBeadType {
		return
	}
	status := strings.TrimSpace(bead.Fields[scifApprovalStatusField])
	switch status {
	case scifApprovalPending:
		a.gate1Opening(ctx, bead, requireChange)
	case scifApprovalApproved, scifApprovalDenied:
		a.gate1Outcome(ctx, bead, status)
	}
}

// gate1Opening posts the pending-request alert. On the updated path it fires
// only when approval_status actually transitioned to pending in THIS event, so
// an unrelated write to an already-pending bead does not re-alert.
func (a *ScifGateAlert) gate1Opening(ctx context.Context, bead *BeadEvent, requireChange bool) {
	if requireChange && !changedTo(bead.Changes, scifApprovalStatusField, scifApprovalPending) {
		return
	}
	key := bead.ID + ":approval:pending"
	if a.alreadySeen(key) {
		return
	}
	text := a.formatGate1Opening(themeOf(ctx, a.themeFor, a.channel), bead)
	_, ts, err := a.api.PostMessageContext(ctx, a.channel,
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		// FAIL-SAFE: log and drop (see handle above). Leave the key un-seen so a
		// duplicate event can retry a transiently-failed post.
		a.logger.Warn("scif-gate-alert post failed (Gate-1 opening)", "id", bead.ID, "channel", a.channel, "error", err)
		return
	}
	a.markSeen(key)
	a.rememberTS(bead.ID, ts)
	a.logger.Info("scif-gate-alert posted (Gate-1 opening)", "id", bead.ID, "channel", a.channel, "ts", ts)
}

// gate1Outcome threads the decision follow-up onto the opening message. The
// outcome always requires the approval_status transition in the event's changes
// map, so it fires exactly once, only on the update that decided the gate, and
// never phantom-reposts on a bridge restart replay.
func (a *ScifGateAlert) gate1Outcome(ctx context.Context, bead *BeadEvent, status string) {
	if !changedTo(bead.Changes, scifApprovalStatusField, status) {
		return
	}
	key := bead.ID + ":approval:" + status
	if a.alreadySeen(key) {
		return
	}
	threadTS := a.recallTS(bead.ID)
	text := a.formatGate1Outcome(themeOf(ctx, a.themeFor, a.channel), bead, status, threadTS == "")
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionDisableLinkUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := a.api.PostMessageContext(ctx, a.channel, opts...); err != nil {
		a.logger.Warn("scif-gate-alert post failed (Gate-1 outcome)", "id", bead.ID, "channel", a.channel, "status", status, "error", err)
		return
	}
	a.markSeen(key)
	a.logger.Info("scif-gate-alert posted (Gate-1 outcome)", "id", bead.ID, "channel", a.channel, "status", status, "threaded", threadTS != "")
}

// formatGate1Opening builds the pointer-only alert for a Gate-1 opening — bead
// id, tenant, requester, a hard-truncated ask excerpt, and the portal link.
// Never the full prompt, never run-derived data (the load-bearing invariant).
func (a *ScifGateAlert) formatGate1Opening(th Theme, bead *BeadEvent) string {
	f := bead.Fields
	var sb strings.Builder
	sb.WriteString(semLabel(th, SemReview, "*acme-scif Gate-1: an investigation-request is awaiting approval*") + "\n")
	fmt.Fprintf(&sb, "• Request: %s\n", a.beadLink(bead.ID))
	if tenant := tenantFromDataScopeJSON(f["data_scope"]); tenant != "" {
		fmt.Fprintf(&sb, "• Tenant: `%s`\n", tenant)
	}
	if rb := firstNonEmpty(f["requested_by"], bead.CreatedBy); rb != "" {
		fmt.Fprintf(&sb, "• Requested by: %s\n", rb)
	}
	if excerpt := truncateExcerpt(f["prompt"], scifPromptExcerptMax); excerpt != "" {
		fmt.Fprintf(&sb, "• Ask: %s\n", excerpt)
	}
	if a.portalURL != "" {
		fmt.Fprintf(&sb, "• Review & decide: <%s|open the SCIF portal>", a.portalURL)
	} else {
		sb.WriteString("• Review & decide in the SCIF portal")
	}
	return sb.String()
}

// formatGate1Outcome renders the decision follow-up. standalone=true means the
// original opening ts was lost (bridge restart) so the message must name the
// request itself; a threaded reply inherits that context from the thread.
func (a *ScifGateAlert) formatGate1Outcome(th Theme, bead *BeadEvent, status string, standalone bool) string {
	f := bead.Fields
	var sb strings.Builder
	if status == scifApprovalApproved {
		sb.WriteString(semLabel(th, SemOK, "*Approved*"))
		if by := strings.TrimSpace(f["approved_by"]); by != "" {
			fmt.Fprintf(&sb, " by %s", by)
		}
		if at := strings.TrimSpace(f["approved_at"]); at != "" {
			fmt.Fprintf(&sb, " at %s", at)
		}
	} else {
		sb.WriteString(semLabel(th, SemDenied, "*Denied*"))
		if by := strings.TrimSpace(f["denied_by"]); by != "" {
			fmt.Fprintf(&sb, " by %s", by)
		}
		if at := strings.TrimSpace(f["denied_at"]); at != "" {
			fmt.Fprintf(&sb, " at %s", at)
		}
		if reason := truncateExcerpt(f["denial_reason"], scifPromptExcerptMax); reason != "" {
			fmt.Fprintf(&sb, " — %s", reason)
		}
	}
	if standalone {
		fmt.Fprintf(&sb, "\n• Request: %s", a.beadLink(bead.ID))
	}
	return sb.String()
}

// rememberTS records the opening-alert ts so a later outcome can thread onto it.
func (a *ScifGateAlert) rememberTS(beadID, ts string) {
	if ts == "" {
		return
	}
	a.mu.Lock()
	a.ts[beadID] = ts
	a.mu.Unlock()
}

// recallTS returns the remembered opening-alert ts for beadID, or "" if none
// (never posted, or lost to a bridge restart).
func (a *ScifGateAlert) recallTS(beadID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ts[beadID]
}

// formatGate2 builds the pointer-only Slack message for a Gate-2 opening. It
// carries provenance identifiers and a hard-truncated ask excerpt — never the
// report body or any run-derived data.
func (a *ScifGateAlert) formatGate2(th Theme, bead *BeadEvent) string {
	f := bead.Fields
	var sb strings.Builder
	sb.WriteString(semLabel(th, SemReview, "*acme-scif: a finished run is awaiting release*") + "\n")
	fmt.Fprintf(&sb, "• Request: %s\n", a.beadLink(bead.ID))
	if tenant := tenantFromDataScopeJSON(f["data_scope"]); tenant != "" {
		fmt.Fprintf(&sb, "• Tenant: `%s`\n", tenant)
	}
	if rb := firstNonEmpty(f["requested_by"], bead.CreatedBy); rb != "" {
		fmt.Fprintf(&sb, "• Requested by: %s\n", rb)
	}
	if excerpt := truncateExcerpt(f["prompt"], scifPromptExcerptMax); excerpt != "" {
		fmt.Fprintf(&sb, "• Ask: %s\n", excerpt)
	}
	if a.portalURL != "" {
		fmt.Fprintf(&sb, "• Review & release: <%s|open the SCIF portal>", a.portalURL)
	} else {
		sb.WriteString("• Review & release in the SCIF portal")
	}
	return sb.String()
}

// beadLink renders a bead id as a beads-viewer deep link when configured, else
// the bare id.
func (a *ScifGateAlert) beadLink(id string) string {
	if a.beadsViewerURL != "" && id != "" {
		return fmt.Sprintf("<%s/%s|%s>", a.beadsViewerURL, id, id)
	}
	return id
}

// alreadySeen is a pure read: it reports whether beadID has already been
// alerted, WITHOUT recording it. The bead is recorded (via markSeen) only after
// a successful post, so a failed post leaves it un-seen and retryable.
func (a *ScifGateAlert) alreadySeen(beadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[beadID]
}

// markSeen records beadID as alerted so a duplicate event (e.g. an SSE
// reconnect replay) does not double-post.
func (a *ScifGateAlert) markSeen(beadID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen[beadID] = true
}

// changedTo reports whether the SSE "changes" map records field transitioning
// to want in this event. A change entry may be the new value directly or an
// {old,new} object; both shapes are tolerated. When changes is absent (nil),
// returns false — the caller only uses this on the updated path where changes
// is always present for a real field write.
func changedTo(changes map[string]any, field, want string) bool {
	if changes == nil {
		return false
	}
	v, ok := changes[field]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == want
	case map[string]any:
		if nv, ok := t["new"].(string); ok {
			return nv == want
		}
		if nv, ok := t["to"].(string); ok {
			return nv == want
		}
	}
	// Unknown shape but the field is named in changes — treat as changed so we
	// fail toward alerting rather than silently missing an opening.
	return true
}

// tenantFromDataScopeJSON extracts data_scope.tenant from the JSON-encoded
// data_scope field (ParseFieldsJSON re-marshals the nested object to a string).
// Returns "" on any parse failure or absent tenant — the alert omits the line
// rather than failing.
func tenantFromDataScopeJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var ds struct {
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal([]byte(raw), &ds); err != nil {
		return ""
	}
	return strings.TrimSpace(ds.Tenant)
}

// truncateExcerpt returns s collapsed to a single line and hard-capped at max
// runes (with an ellipsis when cut), so a long author ask is never reproduced
// in full in Slack.
func truncateExcerpt(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}
