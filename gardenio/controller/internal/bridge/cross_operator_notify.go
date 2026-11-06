// cross_operator_notify.go implements the DETECTIVE accountability control the
// operator chose over a preventive gate (bug ka-0000004303, task ka-0000002131).
//
// BACKGROUND — read this before "fixing" anything here:
//
// requireRole short-circuits every non-OIDC caller (kbeads
// internal/server/interceptors.go:105-110) and fleet tokens are stamped
// authMethodStatic (:658), so any agent fleet token already satisfies
// requireRole(RoleOperator) and can terminate a decision or an escalation
// addressed to a human. ka-0000000798 then widened the ownership carve-out
// (write_authz.go:243) to type=decision, so the generic CloseBead path is open
// too. The operator ruled (2026-08-24/25) to KEEP that capability:
//
//	"i kind of like that ability tbh; what we could do is add a slack message
//	 when a decision is resolved by someone other than its operator so a slack
//	 mention hits them"
//
// So this file does NOT gate anything. It observes terminations and tells the
// addressed human when somebody else answered for them. The caveat is on the
// record and accepted: resolving a decision SPAWNS an agent to act on the
// chosen option (DecisionRespawner), so by the time this message lands the work
// has already started. This is detective, not preventive.
//
// WHY IT EARNS ITS KEEP BEYOND THE ADVERSARIAL CASE: the common case is benign.
// On 2026-08-24 the operator answered ka-0000005256 — a CVE-gate policy call
// addressed to alex.novak@contractor-b.example — in good faith, because it
// was presented as his. Nobody did anything wrong and Alex never found
// out. Three sibling decisions in the same batch are addressed to Terry Vance
// (ka-0000007995), Sky Bennett (ka-0000007743) and Pat Stone
// (ka-0000007633). Those are the messages this would have sent.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// operatorAliasLister is the slice of BeadClient this file needs: the
// operators slack-alias feed (GET /v1/admin/operators/slack-aliases).
type operatorAliasLister interface {
	ListSlackOperatorAliases(ctx context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error)
}

// CrossOperatorSender delivers the notification. *Bot satisfies it via SendDM.
type CrossOperatorSender interface {
	SendDM(ctx context.Context, userID, text string) (channelID, ts string, err error)
}

// CrossOperatorThreadPoster posts the notice as a reply in the Slack thread the
// decision/escalation card already lives in. *Bot satisfies it via
// ReplyInBeadThread.
//
// This is the PREFERRED route (operator ask, Pat Stone 2026-08-26: "I
// really don't like getting DM's to say that one of your threads has updated a
// decision. That should just be captured in the actual slack conversation.").
// The DM below is retained only as the fallback for the case that motivated the
// original routing choice — a bead with no Slack message ref at all, where a DM
// is the difference between delivery and silence.
type CrossOperatorThreadPoster interface {
	// ReplyInBeadThread posts text under the Slack message the bridge recorded
	// for beadID. The bool reports whether such a message existed; false means
	// nothing was posted and the caller should fall back.
	ReplyInBeadThread(ctx context.Context, beadID, text string) (bool, error)
}

// slackUserIDShape is the real Slack user-id shape. It is deliberately stricter
// than a prefix strip: the live alias feed contains kind=slack rows whose
// subject is NOT an id at all — operator 229 carries `matt.baker` and
// `Matt Baker` alongside the real `U0OPERATOR1`. Mentioning a non-id subject
// renders as literal junk, so non-conforming subjects are dropped from the
// index rather than mentioned.
//
// `W` is admitted as well as `U`: enterprise-grid workspaces issue W-prefixed
// ids, which extractSlackUserIDOperator rejects (it predates grid support) but
// the beads-viewer mention parser accepts (cmd/beads-viewer/people_mentions.go).
var slackUserIDShape = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// sentinelEmailSuffix marks a derived placeholder address the daemon mints for
// an operator with no real email (kbeads internal/store/postgres/operators.go
// deriveOperatorEmail). Indexing these would let `oidc:x@y` collide with an
// unrelated operator, so they are skipped.
const sentinelEmailSuffix = "@noreply.kbeads.local"

// operatorAliasTTL bounds how stale the cached alias index may be. The feed is
// small (tens of rows) and changes only when the resync jobs run (6h cadence),
// so 15m is generous while still bounding the blast radius of a bad row.
const operatorAliasTTL = 15 * time.Minute

// operatorSlackIndex is a TTL-cached view of the operators slack-alias feed,
// used to resolve an `operated_by` value to the one Slack user id that may be
// @-mentioned for it.
//
// The alias table is preferred over type=user beads on purpose. User beads are
// duplicated and drift: `gb identity list` currently shows two beads for Matt
// Baker, two for Terry Vance ("Terry Vance" and "Tim") and two for Hema Bhatia,
// so a LookupByEmail hit is order-dependent, and the only bead carrying
// Alex's id (ka-0000005809, "Slack U0PERSON003") has no email at all so
// an email lookup misses him entirely. The alias feed joins operators.display /
// operators.email, which slack_operator_resync.go refreshes from Slack
// users.info — an authoritative source — and it resolves Alex correctly.
type operatorSlackIndex struct {
	daemon operatorAliasLister
	logger *slog.Logger
	ttl    time.Duration

	mu        sync.Mutex
	fetchedAt time.Time
	// byEmail maps lower(email) → the set of DISTINCT well-shaped Slack ids
	// seen for that email. A set, not a single value, because ambiguity must
	// be detectable: mentioning the wrong human is worse than not mentioning.
	byEmail map[string]map[string]bool
	// byOperator maps operators(id) → the same kind of set.
	byOperator map[int64]map[string]bool
	// display maps a Slack id → the operator display name, for message text.
	display map[string]string
}

func newOperatorSlackIndex(daemon operatorAliasLister, logger *slog.Logger) *operatorSlackIndex {
	return &operatorSlackIndex{daemon: daemon, logger: logger, ttl: operatorAliasTTL}
}

// refreshLocked repopulates the index when the cached copy is older than ttl.
func (x *operatorSlackIndex) refreshLocked(ctx context.Context) error {
	if x.byEmail != nil && time.Since(x.fetchedAt) < x.ttl {
		return nil
	}
	resp, err := x.daemon.ListSlackOperatorAliases(ctx)
	if err != nil {
		return err
	}
	byEmail := make(map[string]map[string]bool)
	byOperator := make(map[int64]map[string]bool)
	display := make(map[string]string)
	for _, a := range resp.Aliases {
		subject := strings.TrimSpace(a.Subject)
		if !slackUserIDShape.MatchString(subject) {
			continue
		}
		if d := strings.TrimSpace(a.Display); d != "" {
			display[subject] = d
		}
		if byOperator[a.OperatorID] == nil {
			byOperator[a.OperatorID] = make(map[string]bool)
		}
		byOperator[a.OperatorID][subject] = true

		email := normalizeEmail(a.Email)
		if email == "" || strings.HasSuffix(email, sentinelEmailSuffix) {
			continue
		}
		if byEmail[email] == nil {
			byEmail[email] = make(map[string]bool)
		}
		byEmail[email][subject] = true
	}
	x.byEmail, x.byOperator, x.display = byEmail, byOperator, display
	x.fetchedAt = time.Now()
	return nil
}

// soleSlackID returns the single member of set, or ("", false) when the set is empty
// or ambiguous. Ambiguity is a refusal, not a coin flip: the live feed really
// does map matthew.baker@acme.example to two distinct ids (U0OPERATOR1 and
// U0OPERATOR2), and picking arbitrarily would sometimes DM a stale account.
func soleSlackID(set map[string]bool) (string, bool) {
	if len(set) != 1 {
		return "", false
	}
	for k := range set {
		return k, true
	}
	return "", false
}

// resolvedOperator is the addressed human, resolved far enough to mention.
type resolvedOperator struct {
	SlackID string // "" when unmentionable
	Display string
	Email   string
	Raw     string // operated_by verbatim
}

// Mention renders the Slack mention for the operator, falling back to a plain
// escaped identity when no id resolved (so the message still names a human).
func (r resolvedOperator) Mention() string {
	if r.SlackID != "" {
		return fmt.Sprintf("<@%s>", r.SlackID)
	}
	return formatOwnerMention(r.Raw)
}

// Resolve maps an addressed operator to a Slack id.
//
// Order matters and is deliberate:
//
//  1. `slack:U…` needs no lookup — the id IS the value.
//  2. Email match against the alias feed, case-insensitively. This is the
//     load-bearing path and it must be email-keyed, NOT operator_id-keyed:
//     ka-0000005256 carries operated_by_id=370, but Alex's slack-aliased
//     operator row is 364 — his oidc row and his slack row are separate,
//     unlinked operators. An operator_id lookup therefore misses exactly the
//     case that motivated this feature. Case-insensitivity is equally
//     load-bearing: the feed stores `Matthew.Baker@acme.example` and
//     `terry.vance@acme.example` while operated_by carries the lowercase forms.
//  3. operator_id, as a fallback for a `slack:`-rooted operator whose email is
//     blank.
func (x *operatorSlackIndex) Resolve(ctx context.Context, operatedBy string, operatorID *int64, beadEmail string) resolvedOperator {
	out := resolvedOperator{Raw: strings.TrimSpace(operatedBy)}
	if uid, ok := extractSlackUserIDOperator(out.Raw); ok {
		out.SlackID = uid
	}
	out.Email = normalizeEmail(emailFromOperator(out.Raw))
	if out.Email == "" {
		out.Email = normalizeEmail(beadEmail)
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.refreshLocked(ctx); err != nil {
		// Degrade, do not drop: a mention-less message that names the operator
		// by email still tells a human something happened.
		x.logger.Warn("cross-operator notify: operator alias feed unavailable",
			"operated_by", out.Raw, "error", err)
		if out.SlackID != "" {
			out.Display = strings.TrimSpace(x.display[out.SlackID])
		}
		return out
	}

	if out.SlackID == "" && out.Email != "" {
		if uid, ok := soleSlackID(x.byEmail[out.Email]); ok {
			out.SlackID = uid
		} else if len(x.byEmail[out.Email]) > 1 {
			x.logger.Warn("cross-operator notify: refusing ambiguous slack alias",
				"operated_by", out.Raw, "email", out.Email,
				"candidates", len(x.byEmail[out.Email]))
		}
	}
	if out.SlackID == "" && operatorID != nil {
		if uid, ok := soleSlackID(x.byOperator[*operatorID]); ok {
			out.SlackID = uid
		}
	}
	if out.SlackID != "" {
		out.Display = strings.TrimSpace(x.display[out.SlackID])
		if out.Email == "" {
			// Recover the email for the same-person comparison below.
			for email, ids := range x.byEmail {
				if ids[out.SlackID] {
					out.Email = email
					break
				}
			}
		}
	}
	return out
}

// normalizeEmail lowercases and trims an address for comparison.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// emailFromOperator extracts an address from `oidc:<email>` or from a bare
// address. Returns "" for slack:/fleet:/system: ids and for anything without
// an "@".
func emailFromOperator(operatedBy string) string {
	v := strings.TrimSpace(operatedBy)
	if v == "" {
		return ""
	}
	if kind, rest, ok := strings.Cut(v, ":"); ok {
		switch kind {
		case "oidc":
			v = rest
		case "slack", "fleet", "system", "jira", "gitlab", "agent":
			return ""
		}
	}
	if !strings.Contains(v, "@") {
		return ""
	}
	return v
}

// sameOperator reports whether resolver and the addressed operator are the same
// human. It is the "do NOT fire" gate — the normal case is the addressed
// operator answering their own decision, and firing there would be pure noise
// on a queue 165 escalations deep.
//
// It compares generously (a false "same" is silence; a false "different" DMs a
// human about their own click) across the shapes the resolving identity arrives
// in: an OIDC email (BeadEvent.Actor, DecisionAnswer.RespondedBy on the edge
// path), a `slack:U…`/bare Slack id (Slack modal submit), an `oidc:`-prefixed
// id, and a bare local part.
//
// The resolving identity is NOT always an email or a Slack id. Measured on 300
// recently-closed escalations, approved_by carries bare GITLAB USERNAMES for
// real humans — `matthewbaker` (4 of them), `fshot`, `gardeniobot`. None of those
// string-compare against `slack:U0OPERATOR1` or
// `oidc:matthew.baker@acme.example`, so without this the control would DM those
// operators about escalations they actioned THEMSELVES. (Note the handle is
// `matthewbaker`, not `matthew.baker` — a dotted handle is a different GitLab
// account entirely, so this must be a real lookup, not string munging.)
//
// The UserDirectory is used here and ONLY here — on the resolver side, to ask
// "is this the same human?". That asymmetry is deliberate: user beads are
// duplicated and can carry a wrong slack_id, and a wrong answer HERE degrades
// to a false "same" ⇒ silence, which is the safe direction. The MENTION target
// is resolved from the operators alias table instead, because a wrong answer
// there would DM the wrong human.
func sameOperator(ctx context.Context, op resolvedOperator, resolver string, users *UserDirectory) bool {
	r := strings.TrimSpace(resolver)
	if r == "" {
		return true // unknown resolver: stay quiet rather than guess
	}
	if r == op.Raw {
		return true
	}
	rStripped := strings.ToLower(stripOperatorPrefix(r))
	if opStripped := strings.ToLower(stripOperatorPrefix(op.Raw)); opStripped != "" && rStripped == opStripped {
		return true
	}
	if op.Email != "" && normalizeEmail(emailFromOperator(r)) == op.Email {
		return true
	}
	if op.Email != "" && normalizeEmail(r) == op.Email {
		return true
	}
	if op.SlackID != "" {
		if uid, ok := extractSlackUserIDOperator(r); ok && uid == op.SlackID {
			return true
		}
		if r == op.SlackID {
			return true
		}
	}

	// Bare provider handle (GitLab / GitHub / Slack id / email) → the human.
	if users != nil && !strings.Contains(r, ":") {
		for _, u := range []*UserIdentity{
			users.LookupByGitLabUsername(ctx, r),
			users.LookupByGitHubUsername(ctx, r),
		} {
			if u == nil {
				continue
			}
			if op.Email != "" && normalizeEmail(u.Email) == op.Email {
				return true
			}
			if op.SlackID != "" && u.SlackID == op.SlackID {
				return true
			}
		}
	}
	return false
}

// mechanicalResolvers are terminator identities that carry no accountability
// signal — they are the machinery finishing a termination a human already
// authorized, not a party who answered for somebody else.
//
// `auto-replay` is the literal closed_by the daemon stamps when an approved
// escalation's replay succeeds (kbeads http_escalations.go:334); the human who
// decided is in approved_by, which is preferred over closed_by upstream of
// this check.
//
// `static` is load-bearing and NOT obvious. The daemon derives responder
// attribution in decisionAttribution (kbeads http_decisions.go:330): a
// static/fleet-token caller's actor is the literal "static", which
// isServiceActor treats as a service identity and yields to the human threaded
// via X-Operated-By. That is why the Slack-modal relay — which runs under an
// ordinary bridge fleet token — correctly stamps responded_by with the HUMAN
// who clicked, and why an operator answering their own decision in Slack is
// silent here. But when no human resolves, decisionAttribution deliberately
// keeps "static" rather than inventing one. Treating that as a resolving party
// would DM the operator "resolved by `static`" on every unattributed service
// resolve — noise with no accountability value, and the failure mode most
// likely to spam a real human.
//
// Agent names are deliberately NOT listed: an agent answering an operator's
// decision is precisely the event this control exists to surface.
var mechanicalResolvers = map[string]bool{
	"auto-replay": true,
	"static":      true,
	"system":      true,
	"anonymous":   true,
	"unknown":     true,
}

// gitlabBotToken matches the identity a GitLab group/project access token
// writes under, e.g. `group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa`.
//
// This is measured, not hypothetical: of 300 recently-closed escalations, 33
// carry exactly this shape in approved_by — the escalation reaper stamping the
// out-of-band satisfier when a GitLab automation merged the MR the escalation
// was asking about (escalation_reaper.go). "A group access token merged it"
// carries no accountability signal about a party answering for the addressed
// operator, so it must not generate a DM.
var gitlabBotToken = regexp.MustCompile(`^(group|project)_\d+_bot_[0-9a-f]{8,}$`)

func isMechanicalResolver(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return true
	}
	if mechanicalResolvers[v] {
		return true
	}
	if gitlabBotToken.MatchString(v) {
		return true
	}
	// system:timeout, system:machine, fleet:bridge:jira, …
	if kind, _, ok := strings.Cut(v, ":"); ok {
		switch kind {
		case "system", "fleet":
			return true
		}
	}
	return false
}

// decisionResolver picks the identity that answered a decision, most
// trustworthy first. RespondedBy is stamped by the daemon on the resolve path;
// Actor is the server-resolved subject of the mutation; ClosedBy is the
// caller-supplied last resort.
func decisionResolver(bead BeadEvent) string {
	if bead.DecisionAnswer != nil {
		if v := strings.TrimSpace(bead.DecisionAnswer.RespondedBy); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(bead.Fields["responded_by"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(bead.Actor); v != "" {
		return v
	}
	return strings.TrimSpace(bead.ClosedBy)
}

// escalationResolver picks the identity that decided an escalation.
//
// For an escalation the meaningful "resolved by" is the APPROVER or REJECTER —
// the human (or agent) who made the privileged call — not the executor that
// mechanically ran it and captured the result. Escalations are approved by a
// person and then executed by an agent by design, so keying on the executor
// would fire on essentially every one of the 165 open escalations and say
// nothing. approved_by_email is preferred because only the web-UI path stamps
// it and it is a real OIDC address (bridge api_escalations.go:290).
func escalationResolver(bead BeadEvent) string {
	for _, k := range []string{"approved_by_email", "approved_by", "rejected_by"} {
		if v := strings.TrimSpace(bead.Fields[k]); v != "" && !isMechanicalResolver(v) {
			return v
		}
	}
	if v := strings.TrimSpace(bead.Actor); v != "" && !isMechanicalResolver(v) {
		return v
	}
	if v := strings.TrimSpace(bead.ClosedBy); v != "" && !isMechanicalResolver(v) {
		return v
	}
	return ""
}

// escalationPhase distinguishes the two moments worth telling the addressed
// operator about.
type escalationPhase string

const (
	// escalationApproved is the moment the privileged call is MADE. It is the
	// higher-value notification: approval is what authorizes the merge / apply
	// / credential mint, and an approved escalation whose replay fails is left
	// OPEN by the daemon (kbeads http_escalations.go:319) so it may never emit
	// a close event at all.
	escalationApproved escalationPhase = "approved"
	// escalationClosed is the termination — a rejection, a captured result, or
	// an out-of-band reap.
	escalationClosed escalationPhase = "closed"
)

// escalationOutcome renders the state of an escalation at the given phase.
// Rejection is inferred, not stamped: escalations.go treats a close with no
// approved_by as a rejection.
func escalationOutcome(bead BeadEvent, phase escalationPhase) string {
	if phase == escalationApproved {
		out := "approved"
		if op := strings.TrimSpace(bead.Fields["operation_type"]); op != "" {
			out += fmt.Sprintf(" — privileged operation `%s`", truncateText(op, 120))
		}
		if t := strings.TrimSpace(bead.Fields["execution_target"]); t != "" {
			out += fmt.Sprintf(" (target `%s`)", truncateText(t, 60))
		}
		return out
	}
	if status := strings.TrimSpace(bead.Fields["result_status"]); status != "" {
		if strings.TrimSpace(bead.Fields["approved_by"]) == "" {
			return "rejected (" + status + ")"
		}
		return status
	}
	if strings.TrimSpace(bead.Fields["approved_by"]) == "" {
		return "rejected"
	}
	return "closed"
}

// decisionOutcome renders the chosen option as `id` plus its human label, which
// is the whole point of the message: "resolved as fail-on-fixable — Option B,
// gate blocks only on fixable findings" tells the addressed operator what was
// decided in their name. "Your decision was resolved" does not.
func decisionOutcome(bead BeadEvent) string {
	chosen := strings.TrimSpace(bead.Fields["chosen"])
	if chosen == "" {
		if txt := strings.TrimSpace(bead.Fields["response_text"]); txt != "" {
			return "free-text answer: " + truncateText(txt, 240)
		}
		return ""
	}
	opts := parseDecisionOptionRows(bead.Fields["options"])
	if id, ok := matchOptionID(opts, chosen); ok {
		for _, o := range opts {
			if o.ID == id {
				if label := optionDisplayLabel(o); label != "" && label != id {
					return fmt.Sprintf("`%s` — %s", id, truncateText(label, 240))
				}
				return fmt.Sprintf("`%s`", id)
			}
		}
	}
	return fmt.Sprintf("`%s`", truncateText(chosen, 240))
}

// CrossOperatorNotifyConfig configures the watcher-side notifier.
type CrossOperatorNotifyConfig struct {
	Daemon BeadClient
	Sender CrossOperatorSender
	// Thread posts the notice into the bead's own Slack thread. When set it is
	// tried FIRST and Sender is used only if the bead has no Slack message.
	// Optional; without it every notice is a DM (the pre-ka-0000000294
	// behaviour).
	Thread CrossOperatorThreadPoster
	Logger *slog.Logger
	// Users resolves a bare provider handle (a GitLab/GitHub username) in the
	// RESOLVER position back to a human, so an operator who actioned their own
	// escalation under their GitLab identity is not told a stranger did it.
	// Optional; without it those cases produce a spurious notification.
	Users *UserDirectory
	// LinkBead renders a Slack link for a bead id + title. Optional; when nil
	// the message falls back to a bare id.
	LinkBead func(id, title string) string
}

// CrossOperatorNotify posts one DM to the addressed operator when somebody else
// terminates their decision or escalation.
type CrossOperatorNotify struct {
	daemon BeadClient
	sender CrossOperatorSender
	thread CrossOperatorThreadPoster
	logger *slog.Logger
	link   func(id, title string) string
	users  *UserDirectory
	index  *operatorSlackIndex

	// seen guarantees at most ONE message per bead for the process lifetime,
	// which is what makes it safe to observe both beads.bead.closed and the
	// PATCH-to-closed path (beads.bead.updated with changes.status=closed).
	// That second path is a real hole in closed-event coverage: updateBead
	// (kbeads beads.go:1400) publishes only TopicBeadUpdated, so a
	// `kd update <id> --status closed` terminates an escalation invisibly to
	// any closed-only watcher.
	seen *seenSet
}

// NewCrossOperatorNotify builds the notifier. A nil Sender or Daemon disables
// it, so the slack-bridge running without a Slack bot is unaffected.
func NewCrossOperatorNotify(cfg CrossOperatorNotifyConfig) *CrossOperatorNotify {
	if cfg.Daemon == nil || cfg.Sender == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &CrossOperatorNotify{
		daemon: cfg.Daemon,
		sender: cfg.Sender,
		thread: cfg.Thread,
		logger: logger,
		link:   cfg.LinkBead,
		users:  cfg.Users,
		index:  newOperatorSlackIndex(cfg.Daemon, logger),
		seen:   newSeenSet(seenTTL, seenMaxEntries),
	}
}

// Run sweeps the dedup set, mirroring the other bridge watchers.
func (c *CrossOperatorNotify) Run(ctx context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.seen.gc()
		}
	}
}

// DecisionResolved is called from Decisions.handleClosed after the existing
// notifier has updated the in-channel card.
func (c *CrossOperatorNotify) DecisionResolved(ctx context.Context, bead BeadEvent) {
	if c == nil {
		return
	}
	c.handle(ctx, bead, "decision", decisionResolver(bead), decisionOutcome(bead))
}

// EscalationApproved is called from Escalations.handleUpdated the moment an
// escalation gains an approved_by. Approving is the privileged act; notifying
// here rather than only at close means the addressed operator hears about a
// merge/apply authorized in their name even when the replay then fails and the
// bead is left open.
func (c *CrossOperatorNotify) EscalationApproved(ctx context.Context, bead BeadEvent) {
	if c == nil {
		return
	}
	c.handle(ctx, bead, "escalation", escalationResolver(bead), escalationOutcome(bead, escalationApproved))
}

// EscalationResolved is called from Escalations.handleClosed, and from the
// PATCH-to-closed path in handleUpdated. Covers rejections, captured results
// and out-of-band reaps. Deduped against EscalationApproved by bead id, so an
// approve-then-close pair yields exactly one message.
func (c *CrossOperatorNotify) EscalationResolved(ctx context.Context, bead BeadEvent) {
	if c == nil {
		return
	}
	c.handle(ctx, bead, "escalation", escalationResolver(bead), escalationOutcome(bead, escalationClosed))
}

func (c *CrossOperatorNotify) handle(ctx context.Context, bead BeadEvent, kind, resolver, outcome string) {
	if bead.ID == "" {
		return
	}
	if isMechanicalResolver(resolver) {
		// Timeouts, expiry sweeps and the approve→replay auto-close are not a
		// party answering for someone else.
		return
	}

	// The addressed operator lives on the bead row (operated_by /
	// operated_by_id / operated_by_email), NOT in fields and NOT on the SSE
	// payload the bridge parses — sseBeadData drops it. One GetBead is
	// therefore required, and it also backfills a sparse close event.
	detail, err := c.daemon.GetBead(ctx, bead.ID)
	if err != nil {
		c.logger.Warn("cross-operator notify: cannot read bead",
			"bead", bead.ID, "error", err)
		return
	}
	if kind == "escalation" && !beadsapi.EscalationForOwner(escalationAudienceFields(detail.Fields, bead.Fields)) {
		// A desk or operator escalation is not the owner's to follow
		// (gd-bmOiteQi36): its own channel card records who answered it.
		return
	}
	operatedBy := strings.TrimSpace(detail.OperatedBy)
	if operatedBy == "" || !IsHumanOperator(operatedBy) {
		// No addressed human — e.g. the 153 open escalations stamped
		// fleet:bridge:jira. Nobody to tell.
		return
	}

	op := c.index.Resolve(ctx, operatedBy, detail.OperatedByID, detail.OperatedByEmail)
	if sameOperator(ctx, op, resolver, c.users) {
		// THE NORMAL CASE. The addressed operator answered their own
		// decision; saying so would be noise.
		return
	}
	if op.SlackID == "" {
		c.logger.Warn("cross-operator notify: no mentionable slack id for addressed operator",
			"bead", bead.ID, "type", kind, "operated_by", operatedBy, "resolver", resolver)
		return
	}
	// CLAIM the bead before sending so a concurrent redelivery cannot
	// double-post, and hand the claim back below if the send fails — the whole
	// point of this control is that a cross-operator resolution is never
	// silent, so a transient Slack error must not become a permanent drop.
	if c.seen.markSeen(bead.ID) {
		return
	}

	title := detail.Title
	if title == "" {
		title = bead.Title
	}
	if outcome == "" {
		outcome = c.outcomeFromDetail(kind, detail)
	}
	text := buildCrossOperatorMessage(crossOperatorMessage{
		Kind:     kind,
		BeadLink: c.beadLink(bead.ID, title),
		Mention:  op.Mention(),
		Resolver: resolver,
		Outcome:  outcome,
	})

	route, err := c.deliver(ctx, bead.ID, op.SlackID, text)
	if err != nil {
		c.seen.release(bead.ID)
		c.logger.Error("cross-operator notify: delivery failed, claim released for retry",
			"bead", bead.ID, "type", kind, "slack_id", op.SlackID,
			"route", route, "error", err)
		return
	}
	c.logger.Info("cross-operator notify: mentioned addressed operator",
		"bead", bead.ID, "type", kind, "operated_by", operatedBy,
		"slack_id", op.SlackID, "resolver", resolver, "outcome", outcome,
		"route", route)
}

// deliver posts the notice into the bead's own Slack thread when the bridge has
// a message ref for it, and falls back to a DM when it does not.
//
// THREAD FIRST is the operator's explicit preference (ka-0000000294): the
// notice is a fact about a decision that already has a public card, so it
// belongs under that card where everyone following the conversation sees it —
// not in a private DM that pulls the reader out of the thread they were already
// in. The @-mention inside the text still notifies the addressed operator.
//
// THE DM FALLBACK IS NOT VESTIGIAL. It covers the case that motivated the
// original routing: a decision whose card the bridge never posted or no longer
// tracks — for an external collaborator with no channel presence, an untracked
// bead means the in-thread notice reaches nobody. Silence is the one outcome
// this control exists to prevent, so a missing ref degrades to a DM rather than
// to nothing.
func (c *CrossOperatorNotify) deliver(ctx context.Context, beadID, slackID, text string) (string, error) {
	if c.thread != nil {
		posted, err := c.thread.ReplyInBeadThread(ctx, beadID, text)
		if err != nil {
			return "thread", err
		}
		if posted {
			return "thread", nil
		}
		c.logger.Info("cross-operator notify: no Slack message for bead, falling back to DM",
			"bead", beadID, "slack_id", slackID)
	}
	if _, _, err := c.sender.SendDM(ctx, slackID, text); err != nil {
		return "dm", err
	}
	return "dm", nil
}

// outcomeFromDetail recovers the outcome from the full bead when the SSE close
// event was too sparse to carry it.
func (c *CrossOperatorNotify) outcomeFromDetail(kind string, detail *beadsapi.BeadDetail) string {
	synth := BeadEvent{ID: detail.ID, Type: detail.Type, Status: detail.Status, Fields: detail.Fields}
	if kind == "escalation" {
		return escalationOutcome(synth, escalationClosed)
	}
	return decisionOutcome(synth)
}

func (c *CrossOperatorNotify) beadLink(id, title string) string {
	if c.link != nil {
		if s := c.link(id, title); s != "" {
			return s
		}
	}
	if title == "" {
		return id
	}
	return fmt.Sprintf("%s — %s", id, escapeSlackMrkdwn(title))
}

type crossOperatorMessage struct {
	Kind     string // "decision" | "escalation"
	BeadLink string
	Mention  string
	Resolver string
	Outcome  string
}

// buildCrossOperatorMessage renders the notice. Kept pure so the exact wording
// is asserted by tests rather than discovered in production.
//
// Every element the ruling asked for is named: which bead, WHO resolved it,
// WHICH option was chosen, and an @-mention of the addressed operator.
//
// ONE TEXT FOR BOTH ROUTES. It reads correctly in the bead's thread and as a
// DM, so the wording is not a second thing to keep in sync with the routing.
// The bead link stays even in-thread: a thread-bound agent's Slack conversation
// carries every decision that agent filed, so "which one" is not implied by
// position there.
func buildCrossOperatorMessage(m crossOperatorMessage) string {
	noun := "decision"
	verb := "resolved"
	if m.Kind == "escalation" {
		noun = "escalation"
		verb = "actioned"
	}
	var b strings.Builder
	fmt.Fprintf(&b, ":eyes: %s — your %s was %s by someone else.\n", m.Mention, noun, verb)
	fmt.Fprintf(&b, "*%s*\n", m.BeadLink)
	fmt.Fprintf(&b, "Resolved by: `%s`\n", escapeSlackMrkdwn(strings.TrimSpace(m.Resolver)))
	if m.Outcome != "" {
		fmt.Fprintf(&b, "Resolved as: %s\n", m.Outcome)
	}
	b.WriteString("_You are the operator this was addressed to. " +
		"Answering another operator's item is allowed by design (ka-0000004303) — " +
		"this note exists so it is never silent. Work on the chosen option may already have started._")
	return b.String()
}

// statusChangedToClosed reports whether an update event carries a status
// transition into a terminal state. It exists because the daemon's PATCH path
// (kbeads updateBead) publishes only TopicBeadUpdated even when it flips a bead
// to closed, so a closed-only watcher never sees that termination.
func statusChangedToClosed(bead *BeadEvent) bool {
	if bead == nil || len(bead.Changes) == 0 {
		return false
	}
	v, ok := bead.Changes["status"]
	if !ok {
		return false
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "closed", "resolved", "done":
		return true
	}
	return false
}
