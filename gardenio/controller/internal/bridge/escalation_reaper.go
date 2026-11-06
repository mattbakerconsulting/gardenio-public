// Package bridge — escalation auto-reaper (ka-0000004105).
//
// EscalationReaper periodically sweeps OPEN escalations and auto-closes the
// ones whose ask a human already satisfied OUTSIDE the beads data plane — the
// exact failure mode the bug measured: an operator merges/approves an MR in the
// GitLab UI, the escalation that requested it emits no reverse event, and it
// sits open at P0 forever. Because the dead entries are the oldest, age-ordering
// sorts them to the TOP of the operator queue, training the operator to distrust
// the whole queue (~15% phantom rate, verified 2026-07-26).
//
// THE LOAD-BEARING CORRECTNESS RULE (do not weaken this):
//
// Auto-reap ONLY when operation_spec.action is a pure approve/merge verb —
// {approve_and_merge_mr, approve_only_do_not_merge, merge_mr}. NEVER reap on an
// apply/deploy/provision/mint verb even when the escalation references a merged
// MR. The named counterexample is ka-0000001987: it references site-deployment
// !1168, which IS merged — but the ask is to `terragrunt apply` the merged code
// in a prod-class account, which no MR state can attest to. Its operation_spec
// carries no `action` field (operation_type=infrastructure, commands=[terragrunt
// plan/apply]), so the verb allowlist excludes it and it stays open. A naive
// MR-state reaper would false-reap it and silently drop a live P0 — strictly
// worse than the phantom, because a phantom wastes attention while a false-reap
// loses work. The verb is the discriminator, NOT the presence of an MR.
//
// COMPLETION CRITERION DEPENDS ON THE VERB (another naive-implementation trap):
//   - merge_mr / approve_and_merge_mr → MR state=merged (or closed) is the
//     completion signal.
//   - approve_only_do_not_merge → MR state is the WRONG signal. ka-0000000844
//     explicitly requires !1181 to REMAIN draft and NOT be merged (its
//     expected_outputs say so), so a state=merged reaper never closes it. We
//     read the approvals endpoint (approvals_left==0) instead.
//
// COVERAGE: the reaper resolves the MR reference from structured
// operation_spec fields first (iid/mr_iid + project, or mr_url), then falls
// back to parsing a merge_requests/<iid> URL out of mr_url / title /
// description. Field-only matching covers a minority of MR-shaped asks (the
// field is often unpopulated), so the prose fallback is what makes coverage
// track the real phantom rate. The verb allowlist stays authoritative over
// however the iid was discovered.
//
// This is the reverse edge the bug asked for (fix #1), keyed on the verb. It
// lives in gitlab-bridge because that is where the GitLab client and the
// existing MR watchers already are.
//
// ─────────────────────────────────────────────────────────────────────────
// SECOND DETECTOR — the ORCHESTRATION-LAYER witness (ka-0000007203).
//
// The MR-witness detector above INFERS satisfaction from GitLab MR state, so it
// only covers pure approve/merge asks with a resolvable MR. It is structurally
// blind to the other half of the bug: an escalation the operator satisfied
// out-of-band whose completion NO MR state can attest to — an apply/deploy/
// provision/mint ask (the ka-0000001987 shape), or a merge the operator
// recorded as done in the orchestration layer without the MR reaching a state
// the merge-detector reads. Those escalations sit open at P0 forever exactly
// like the MR phantoms did.
//
// The orchestration-layer detector closes that gap by reading a witness the
// operator already left ON THE BEAD ITSELF — no GitLab call, so it works for
// EVERY operation_type. It reaps on either of two explicit, human-attributable
// signals (checkOrchestrationSatisfied):
//
//  1. A terminal result_status ({success, partial}) recorded on a bead that is
//     STILL OPEN. The normal close path (CaptureEscalationResult /
//     `gb escalation close`) writes result_status AND closes atomically, so a
//     terminal result_status on an OPEN escalation means the result was
//     recorded out-of-band (a `kd update -f result_status=…`, or a close whose
//     bead-close half did not land). The ask is done; only the close is
//     missing — precisely what the reaper exists to finish.
//
//  2. An OPERATOR resolve-comment: a comment whose AUTHOR is a human operator
//     (isHumanCommentAuthor — a typed slack:/oidc: id or a bare email, NOT a
//     bare agent name), whose text carries an explicit resolve directive
//     (/reap, /resolve, "resolved out-of-band", "done out of band", …), AND
//     which is NOT vetoed by a "leave this open / do not execute" phrase. This
//     is the "operator closes it in Slack" path: the bridge forwards operator
//     Slack replies into bead comments, so a human typing "resolved out of band"
//     on the escalation thread lands as a human-authored comment the sweep reads.
//
// LOAD-BEARING SAFETY (mirrors the MR detector's verb guard):
//   - NEVER reaps on result_status=failure — a failed ask is the OPPOSITE of
//     satisfied; closing it would bury unfinished work (same principle as the
//     wish-close triage guard).
//   - The comment signal requires a HUMAN-AUTHORED comment, judged on the
//     comment's AUTHOR — not its operated_by. Agents carry a human operated_by
//     (their spawning operator), so keying on operated_by read an agent comment
//     as the operator's own and false-reaped a live merge handle whose text
//     asked to keep it open (ka-0000002488). An AGENT comment — even one saying
//     "resolved out of band" — is NOT an operator resolution. The directive must
//     ALSO be present (so "should I reap this?" does not fire), and a veto
//     phrase ("leave this open", "do not execute/merge") hard-blocks the reap
//     even when a resolve-looking phrase co-occurs — the fail-closed direction.
//   - Both signals stamp reaped_signal ∈ {result_status, operator_comment} and
//     an audit comment, identical in shape to the MR detector, so every reap is
//     attributable no matter which detector fired.
//
// ORDER: the orchestration detector runs BEFORE the verb/MR path in
// evaluateBead. It needs no GitLab round-trip and no verb allowlist (the
// operator's explicit witness IS the authority), so checking it first also
// reaps apply/deploy asks the verb guard deliberately refuses.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// defaultReapSweepInterval is how often the reaper re-evaluates open
	// escalations. Coarse: the phantoms it clears sat open for days, so a tight
	// sweep buys nothing and just spends GitLab API budget.
	defaultReapSweepInterval = 10 * time.Minute

	// reapActor is the author recorded on the audit comment and the close.
	reapActor = "escalation-reaper"

	// defaultMergeEscalationGroupNamespace expands a short repo name found in a
	// merge escalation's title (e.g. "gardenio !7656") into a full GitLab project
	// path ("example-org/gardenio") so the reaper can GET the MR. All
	// first-party repos live under this group (matches releaseDroughtDefaultProject
	// and helm's gitlabGroup default). Overridable via ESCALATION_MR_GROUP_NAMESPACE
	// for a self-hosted / re-namespaced instance (bug ka-0000006851).
	defaultMergeEscalationGroupNamespace = "example-org"
)

// reapableActions is the verb allowlist — the ONLY operation_spec.action values
// an escalation may be auto-reaped on. Everything else (apply/deploy/provision/
// mint/…, or a missing action) is never reaped, even when an MR is referenced.
// This map is the load-bearing correctness guard; see the package doc.
var reapableActions = map[string]bool{
	"approve_and_merge_mr":      true,
	"approve_only_do_not_merge": true,
	"merge_mr":                  true,
}

// satisfiedResultStatuses is the set of terminal result_status values that,
// when found on a STILL-OPEN escalation, signal an out-of-band satisfaction the
// orchestration detector reaps (ka-0000007203). "failure" is DELIBERATELY
// excluded — a failed ask is unfinished work, not a satisfied one; auto-closing
// it would bury it (the wish-close triage-guard principle).
var satisfiedResultStatuses = map[string]bool{
	"success": true,
	"partial": true,
}

// operatorResolveDirective matches an explicit operator resolve directive in a
// comment body — the second orchestration-layer witness. It is intentionally
// narrow: a bare "done" or "ok" must NOT fire (agents and operators say those
// constantly), so the directive is a slash-command or an unambiguous
// "resolved/satisfied/handled … out of band / out-of-band / by hand /
// manually / already" phrase. Matching is case-insensitive. This gates ONLY the
// comment signal; it is paired with an IsHumanOperator author check so an agent
// comment can never trip it (see checkOrchestrationSatisfied).
var operatorResolveDirective = regexp.MustCompile(
	`(?i)(^|\s)(/reap\b|/resolve\b|/done\b|(resolved|satisfied|handled|completed|closed|merged|approved|applied|done)\b[^.\n]{0,40}\b(out[ -]?of[ -]?band|by hand|manually|already|myself|out of band))`)

// operatorReapVeto matches a comment that expresses the OPPOSITE of
// satisfaction — an explicit request to keep the escalation OPEN or to NOT
// execute its ask. It is a HARD VETO on the comment witness (ka-0000002488):
// checked before operatorResolveDirective, it wins even when a resolve-looking
// phrase also appears in the same comment ("resolved the other one out of band;
// leave THIS one open"). This is the "fail closed" direction the bug demands —
// a false "satisfied" hides unfinished privileged work, whereas a stale open
// handle merely nags. Matching is case-insensitive. It gates ONLY the comment
// witness; the result_status field witness is a structured signal, not prose.
var operatorReapVeto = regexp.MustCompile(
	`(?i)(do\s*n['’]?t|don['’]?t|do not|never)\s+(execute|merge|reap|close|resolve|approve|apply|deploy|run|touch|land)` +
		`|(leave|keep)\b[^.\n]{0,40}\bopen\b` +
		`|\b(stay|stays|remain|remains|staying|remaining|still)\s+open\b` +
		`|\bnot\s+(yet\s+)?(satisfied|done|resolved|complete|completed|merged|ready)\b` +
		`|\bstill\s+(needed|blocked|pending|unmerged|open)\b`)

// mrURLInText finds an MR web URL anywhere in a string (not anchored, unlike
// mrURLPattern). Used by the prose fallback to pull a full merge_requests/<iid>
// URL out of mr_url / title / description so ParseMRURL can yield host + path +
// iid in one shot.
var mrURLInText = regexp.MustCompile(`https?://[^\s)]+/-/merge_requests/\d+`)

// mrIIDInText matches a bare merge_requests/<iid> path fragment, for the case
// where an iid is present in prose but not as a full URL (project path then
// comes from operation_spec.project).
var mrIIDInText = regexp.MustCompile(`merge_requests/(\d+)`)

// mrShortNameBangRef matches a "<repo> !<iid>" or "<repo>!<iid>" reference where
// <repo> is a SHORT repo name (no slash), the shape the real self-exec merge
// escalations use in their TITLE (bug ka-0000006851): "gardenio !7656",
// "Approve+merge site-deployment!1228", "Merge acme-helm-chart !450",
// "fluentbit MR !7". Group 1 is the short repo name, group 2 the iid. The
// leading (?:^|[\s(:,]) anchors the repo token to a word boundary so a stray
// "ka-x!5" or a mid-word capture cannot masquerade as a repo; an optional
// "MR " between the name and "!" absorbs the "fluentbit MR !7" spelling. A
// path-qualified ref (contains '/') is intentionally NOT matched here — those
// resolve through resolveMRRef's URL/structured paths, which carry the full
// authoritative path already.
var mrShortNameBangRef = regexp.MustCompile(`(?:^|[\s(:,])([A-Za-z][A-Za-z0-9._-]*?)\s*(?:MR\s+)?!(\d+)\b`)

// mrShortNameStopwords are common title words that can be captured as the token
// immediately before a BARE "!<iid>" ("Approve + merge !2578", "Merge !2490")
// but are NOT a repo name. Expanding one into a project path
// ("example-org/merge") would just 404 on the live GET — safe, but a wasted
// round trip and a theoretical false-reap were such a repo ever to exist. When
// the captured token is a stopword the fallback declines: a genuinely bare-iid
// title carries no resolvable repo, so it correctly stays open for a human.
// Matched case-insensitively.
var mrShortNameStopwords = map[string]bool{
	"merge":   true,
	"approve": true,
	"review":  true,
	"close":   true,
	"decline": true,
	"deploy":  true,
	"apply":   true,
	"and":     true,
	"or":      true,
	"then":    true,
	"the":     true,
	"plus":    true,
	"fix":     true,
	"squash":  true,
}

// resolveMergeShortNameRef expands a short-name bang-reference in a merge
// escalation's title ("gardenio !7656") into a full (project, iid) ref by
// prefixing the configured group namespace ("example-org/gardenio"). It is
// the last-resort resolver on the ALREADY-REAPABLE merge path (bug
// ka-0000006851) — reached only after resolveMRRef found no structured or URL
// ref. Returns ok=false when the namespace is unset or no short-name ref is
// present, so the reaper falls through to leaving the escalation open. Because
// the caller re-verifies the MR live against GitLab before closing, a wrong repo
// name simply 404s and no-ops — it can never manufacture a merge from a bad guess
// (advice ka-0000001276: NoSuchEntity is not a credential problem).
func (r *EscalationReaper) resolveMergeShortNameRef(title string) (reapRef, bool) {
	if r.groupNamespace == "" {
		return reapRef{}, false
	}
	m := mrShortNameBangRef.FindStringSubmatch(title)
	if m == nil {
		return reapRef{}, false
	}
	iid, err := strconv.Atoi(m[2])
	if err != nil || iid <= 0 {
		return reapRef{}, false
	}
	repo := strings.TrimRight(m[1], ".-_")
	if repo == "" || mrShortNameStopwords[strings.ToLower(repo)] {
		return reapRef{}, false
	}
	return reapRef{
		ProjectPath: fmt.Sprintf("%s/%s", strings.TrimRight(r.groupNamespace, "/"), repo),
		IID:         iid,
	}, true
}

// ReaperGitLabClient is the narrow GitLab surface the reaper needs. Satisfied by
// *GitLabClient; kept small so tests can supply a tiny fake.
type ReaperGitLabClient interface {
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
	ListMergeRequestApprovals(ctx context.Context, projectPath string, mrIID int) (*GitLabMRApprovals, error)
}

// Compile-time assertion that the production GitLab client satisfies the narrow
// reaper surface.
var _ ReaperGitLabClient = (*GitLabClient)(nil)

// ReaperBeadClient is the narrow bead-daemon surface the reaper needs.
// Satisfied by *beadsapi.Client. GetComments backs the orchestration-layer
// detector (ka-0000007203): it reads operator resolve-comments left on an
// escalation the operator satisfied out-of-band. UpdateBeadFields + AddLabel
// back the MR-landed review flag (ka-0000002563): it flips a non-reapable
// escalation to needs-review instead of closing it.
type ReaperBeadClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	AddComment(ctx context.Context, beadID, author, text string) error
	// CaptureEscalationResult closes an escalation through its dedicated result
	// verb (POST /v1/escalations/{id}/result). The reaper MUST NOT use the
	// generic CloseBead path: since ka-0000005516 the daemon 409s any generic
	// close of an OPEN escalation, demanding this verb instead (it skips the
	// operation_spec replay the reaper must not run, and releases
	// close-on-escalation wrappers only on a success result). CloseBead is
	// deliberately absent from this interface so a regression cannot silently
	// reintroduce the 409-dead path.
	CaptureEscalationResult(ctx context.Context, escalationID string, req beadsapi.CaptureEscalationResultRequest) error
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddLabel(ctx context.Context, beadID, label string) error
}

// Compile-time assertion that the production beads client satisfies the narrow
// reaper surface.
var _ ReaperBeadClient = (*beadsapi.Client)(nil)

// EscalationReaper drives the periodic out-of-band-satisfaction sweep.
type EscalationReaper struct {
	gitlab   ReaperGitLabClient
	daemon   ReaperBeadClient
	logger   *slog.Logger
	interval time.Duration
	// host, when set, scopes reaping to escalations whose resolved MR lives on
	// this GitLab host — the dual-GitLab guard (one beads daemon shared by a
	// dark-pointed and an upstream-pointed bridge). Mirrors GitLabUpstreamPoller.
	// Only applied when the resolved ref carries a host (i.e. came from a full
	// MR URL); a structured project+iid ref has no host and is never skipped.
	host string
	// dryRun logs the intended reap without commenting or closing. Lets an
	// operator watch what the sweep WOULD close before enabling the closes.
	dryRun bool

	// reviewFlag enables the MR-landed review flag (ka-0000002563): the third
	// detector that flips a NON-reapable escalation whose referenced MR has
	// merged/closed into a needs-review posture instead of closing it. On by
	// default; the reaper's dryRun is honored by the flag too.
	reviewFlag bool

	// groupNamespace expands a bare short repo name in a merge escalation's title
	// (e.g. "gardenio !7656") into a full GitLab project path so the reaper can GET
	// the MR (bug ka-0000006851). Empty disables the short-name fallback — the
	// reaper still resolves structured project+iid and full-URL refs, so a
	// mis-configured namespace only reverts to the pre-fix coverage, never
	// mis-targets a project.
	groupNamespace string

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// EscalationReaperConfig configures an EscalationReaper.
type EscalationReaperConfig struct {
	GitLab   ReaperGitLabClient
	Daemon   ReaperBeadClient
	Logger   *slog.Logger
	Interval time.Duration // ≤ 0 → defaultReapSweepInterval
	Host     string
	DryRun   bool
	// ReviewFlag enables the MR-landed review flag (ka-0000002563). Defaults to
	// ON when NewEscalationReaper is called via the config below (the caller sets
	// it explicitly; a zero-value Config disables it, which tests rely on to keep
	// the pre-existing reap tests scoped to the reap detectors).
	ReviewFlag bool
	// GroupNamespace expands a bare short repo name in a merge escalation's title
	// into a full GitLab project path (bug ka-0000006851). Empty →
	// defaultMergeEscalationGroupNamespace, so first-party repos resolve without
	// per-deployment config; set "" explicitly cannot happen (empty falls to the
	// default). To hard-disable the short-name fallback, tests may set it via the
	// unexported field directly.
	GroupNamespace string
}

// NewEscalationReaper constructs the reaper loop.
func NewEscalationReaper(cfg EscalationReaperConfig) *EscalationReaper {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultReapSweepInterval
	}
	groupNamespace := cfg.GroupNamespace
	if groupNamespace == "" {
		groupNamespace = defaultMergeEscalationGroupNamespace
	}
	return &EscalationReaper{
		gitlab:         cfg.GitLab,
		daemon:         cfg.Daemon,
		logger:         logger,
		interval:       interval,
		host:           cfg.Host,
		dryRun:         cfg.DryRun,
		reviewFlag:     cfg.ReviewFlag,
		groupNamespace: groupNamespace,
		now:            time.Now,
	}
}

// Run sweeps immediately, then on every tick until ctx is canceled.
func (r *EscalationReaper) Run(ctx context.Context) error {
	r.logger.Info("escalation reaper starting",
		"interval", r.interval, "host", r.host, "dry_run", r.dryRun)
	r.sweepOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("escalation reaper stopping")
			return ctx.Err()
		case <-ticker.C:
			r.sweepOnce(ctx)
		}
	}
}

// sweepOnce evaluates every open/in_progress escalation. Listing only the open
// set is what makes the sweep idempotent: a bead the reaper closed is gone from
// the next sweep's result, so it is never re-processed or double-commented.
func (r *EscalationReaper) sweepOnce(ctx context.Context) {
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open", "in_progress"},
		Limit:    200,
	})
	if err != nil {
		r.logger.Warn("escalation reaper sweep: list failed", "error", err)
		return
	}
	checked, reaped := 0, 0
	for _, b := range res.Beads {
		acted := r.evaluateBead(ctx, b)
		checked++
		if acted {
			reaped++
		}
	}
	r.logger.Debug("escalation reaper sweep complete",
		"open", len(res.Beads), "checked", checked, "reaped", reaped)
}

// evaluateBead runs one escalation through the verb allowlist, MR resolution,
// and verb-specific completion check, reaping it when the ask is verifiably
// satisfied. Returns true when it closed (or would have, in dry-run) the bead.
func (r *EscalationReaper) evaluateBead(ctx context.Context, b *beadsapi.BeadDetail) bool {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	// E2E fixtures must never be auto-reaped against the real GitLab API.
	if isE2EEscalation(b.CreatedBy, b.Labels) {
		return false
	}
	// Belt-and-suspenders idempotency: never re-act on one already stamped
	// reaped (it should already be closed and thus out of the sweep set).
	if fields["reaped"] == "true" {
		return false
	}

	// ORCHESTRATION-LAYER detector (ka-0000007203), checked FIRST: reap on an
	// operator's explicit out-of-band resolution recorded on the bead itself —
	// a terminal result_status on a still-open escalation, or a human-operator
	// resolve-comment. It needs no GitLab call and no verb allowlist (the
	// operator's explicit witness IS the authority), so it also catches
	// apply/deploy/mint asks the verb guard below deliberately refuses.
	if satisfied, audit := r.checkOrchestrationSatisfied(ctx, b); satisfied {
		return r.reap(ctx, b, "", reapRef{}, audit)
	}

	spec := parseOperationSpec(fields["operation_spec"])

	// THE GUARD: reap only on a pure approve/merge verb. This runs FIRST and
	// short-circuits — an apply/deploy/provision/mint verb (or a missing action,
	// as on the ka-0000001987 counterexample) is never reaped, no matter what MR
	// it references.
	//
	// PRIMARY signal: operation_spec.action ∈ reapableActions (the strict verb
	// allowlist). FALLBACK (bug ka-0000006698): a `gitlab_mr_merge`-family
	// escalation whose operation_spec carries NO canonical action. Measured
	// 2026-08-16: 17 of 19 open merge escalations had no operation_spec.action at
	// all (the rest used non-canonical spellings like "merge"), so keying reap
	// SOLELY on the action verb left every one of them open forever after its MR
	// merged — the exact "56 dead merge escalations, nothing closed them" symptom
	// the bug reports. When the operation_type names a merge the bot performs
	// itself (isMergeFamilyEscalation, the same free-text merge-family set the
	// dispatch-gate uses), the ask IS "merge this MR", so its completion is MR
	// state=merged/closed — identical to an explicit `merge_mr` verb. Synthesize
	// that verb so the merge-state check below fires. This NEVER widens reaping to
	// apply/deploy/mint asks: those carry a non-merge operation_type and no
	// reapable action, so they still fall through to flagMRLandedApplyPending.
	action := stringField(spec, "action")
	if !reapableActions[action] && isMergeFamilyEscalation(b) {
		action = "merge_mr"
	}
	if !reapableActions[action] {
		// NON-reapable ask (apply/deploy/mint verb, or a bare approve/merge verb
		// the allowlist does not cover). It must never be auto-closed — but if
		// its referenced MR has landed, flip it to needs-review so the operator
		// gets one signal instead of the endless stale nag (ka-0000002563). This
		// is the LAST detector: it only ever sees asks the reap detectors above
		// declined.
		return r.flagMRLandedApplyPending(ctx, b)
	}

	ref, ok := resolveMRRef(spec, b.Title, b.Description)
	if !ok {
		// FALLBACK (bug ka-0000006851): the real self-exec merge escalations carry
		// operation_spec={} and name their MR only as a short-name bang-reference in
		// the TITLE — "gardenio !7656", "Merge acme-helm-chart !450". resolveMRRef
		// cannot resolve those: there is no structured project+iid, no full URL, and
		// a short repo name is not a GitLab project path. So every one of them
		// (measured 2026-08-25: the entire open gardenio merge-escalation queue with
		// empty specs) stayed open forever after its MR merged — the exact symptom
		// this bug reports. Expand the short repo name to a full project path under
		// the configured group namespace ONLY on the merge path (a reapable verb is
		// already proven), so the merge-state check below can fire. This never
		// widens WHAT is reapable — the verb guard above is authoritative; it only
		// finds the MR for an already-reapable merge ask.
		ref, ok = r.resolveMergeShortNameRef(b.Title)
		if !ok {
			// Reapable verb but no machine-checkable MR reference — leave it open
			// for a human. Under-reaping here is the safe direction.
			r.logger.Debug("escalation reaper: reapable verb but unresolved MR ref",
				"id", b.ID, "action", action)
			return false
		}
	}

	// Dual-GitLab safety: never GET a foreign-host MR against this bridge's API.
	// Only applies when the ref carries a host (came from a full MR URL); a
	// structured project+iid ref has no host and is always considered in-scope.
	if r.host != "" && ref.Host != "" && ref.Host != r.host {
		return false
	}

	satisfied, audit, err := r.checkSatisfied(ctx, action, ref)
	if err != nil {
		r.logger.Warn("escalation reaper: MR check failed — leaving open",
			"id", b.ID, "action", action, "project", ref.ProjectPath, "iid", ref.IID, "error", err)
		return false
	}
	if !satisfied {
		// Reapable verb but the MR is not yet merged/approved. Do NOT flag it —
		// an unmerged MR is not "landed", and the reap detector already owns this
		// ask. Leave it for the operator (and the pingback loop) unchanged.
		return false
	}

	return r.reap(ctx, b, action, ref, audit)
}

// reapRef is a resolved MR reference: the components needed to query GitLab.
type reapRef struct {
	Host        string // "" when resolved from a bare project+iid (no URL)
	ProjectPath string
	IID         int
}

// reapAudit carries the human-readable + machine attribution recorded on the
// reap, so the close is auditable (bug requirement).
type reapAudit struct {
	// reason is the one-line human-readable justification.
	reason string
	// actor is the out-of-band satisfier, recorded as reaped_satisfier — the
	// operator DID approve/merge, just outside beads. Empty falls back to
	// "out-of-band".
	actor string
	// fields are extra audit fields stamped on the close (merged_by/merged_at,
	// approval counts, mr_url, mr_state).
	fields map[string]string
	// mrURL is the MR web URL for the comment/link (best effort).
	mrURL string
}

// checkSatisfied applies the verb-specific completion criterion.
//
//   - merge verbs: MR state=merged (or closed) satisfies. merged_by/merged_at
//     recorded when present.
//   - approve_only: the approvals endpoint (approvals_left==0 with
//     approvals_required>=1) is the signal — MR state is deliberately ignored,
//     because the ask requires the MR to stay draft and unmerged.
func (r *EscalationReaper) checkSatisfied(ctx context.Context, action string, ref reapRef) (bool, reapAudit, error) {
	switch action {
	case "approve_only_do_not_merge":
		appr, err := r.gitlab.ListMergeRequestApprovals(ctx, ref.ProjectPath, ref.IID)
		if err != nil {
			return false, reapAudit{}, err
		}
		// approvals_required>=1 guards against a Community-Edition 404, which
		// ListMergeRequestApprovals maps to a zero-value struct (required==0):
		// that must NOT read as "approved". Under-reaping on CE is the safe
		// direction; site-deployment (the real approve_only target) is on a
		// Premium instance where the endpoint is live.
		if appr.ApprovalsRequired < 1 || appr.ApprovalsLeft != 0 {
			return false, reapAudit{}, nil
		}
		approvers := strings.Join(appr.ApprovedBy, ", ")
		actor := approvers
		if actor == "" {
			actor = "out-of-band"
		}
		audit := reapAudit{
			reason: fmt.Sprintf("MR !%d approved out-of-band (approvals_left=0, required=%d, approved_by=%s); the approve-only ask is satisfied while the MR remains unmerged",
				ref.IID, appr.ApprovalsRequired, orNone(approvers)),
			actor: actor,
			fields: map[string]string{
				"reaped_signal":      "approvals",
				"approvals_required": strconv.Itoa(appr.ApprovalsRequired),
				"approvals_left":     strconv.Itoa(appr.ApprovalsLeft),
				"mr_approved_by":     approvers,
			},
			mrURL: mrURLFor(ref),
		}
		return true, audit, nil

	case "merge_mr", "approve_and_merge_mr":
		mr, err := r.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
		if err != nil {
			return false, reapAudit{}, err
		}
		if mr.State != "merged" && mr.State != "closed" {
			return false, reapAudit{}, nil
		}
		mergedBy, mergedAt := "", ""
		if mr.MergedBy != nil {
			mergedBy = mr.MergedBy.Username
		}
		if mr.MergedAt != nil {
			mergedAt = *mr.MergedAt
		}
		mrURL := mr.WebURL
		if mrURL == "" {
			mrURL = mrURLFor(ref)
		}
		var reason string
		if mr.State == "merged" {
			reason = fmt.Sprintf("MR !%d merged out-of-band by %s at %s; the %s ask is satisfied",
				ref.IID, orNone(mergedBy), orNone(mergedAt), action)
		} else {
			reason = fmt.Sprintf("MR !%d closed/declined out-of-band; the %s ask is moot",
				ref.IID, action)
		}
		audit := reapAudit{
			reason: reason,
			actor:  mergedBy, // "" for closed-not-merged → falls back to out-of-band
			fields: map[string]string{
				"reaped_signal": "mr_state",
				"mr_state":      mr.State,
				"mr_merged_by":  mergedBy,
				"mr_merged_at":  mergedAt,
			},
			mrURL: mrURL,
		}
		return true, audit, nil

	default:
		// Unreachable: evaluateBead already gated on reapableActions.
		return false, reapAudit{}, nil
	}
}

// checkOrchestrationSatisfied is the orchestration-layer detector
// (ka-0000007203): it reaps an escalation the operator resolved out-of-band and
// recorded ON THE BEAD, without any GitLab call. Two witnesses, either
// sufficient:
//
//  1. A terminal result_status ∈ {success, partial} on a still-OPEN escalation
//     (the close half never landed). Checked first — it is a single field read.
//  2. A HUMAN-operator comment carrying an explicit resolve directive.
//
// Returns satisfied=false with a zero audit when neither witness is present, so
// evaluateBead falls through to the MR-witness path. It NEVER errors: a comment
// fetch failure just means "no comment witness this sweep", and the bead is
// re-evaluated next sweep — the same under-reap-is-safe stance as the MR path.
func (r *EscalationReaper) checkOrchestrationSatisfied(ctx context.Context, b *beadsapi.BeadDetail) (bool, reapAudit) {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	// Witness 1: a terminal result_status on a still-open escalation. The normal
	// close path writes result_status AND closes atomically, so a terminal
	// status on an OPEN bead means the result was recorded out-of-band and only
	// the close is missing. "failure" is excluded (satisfiedResultStatuses) — a
	// failed ask is unfinished, not satisfied.
	if rs := strings.TrimSpace(fields["result_status"]); satisfiedResultStatuses[rs] {
		actor := firstNonEmptyField(fields, "executor", "approved_by", "resolved_by")
		return true, reapAudit{
			reason: fmt.Sprintf("result_status=%s was recorded out-of-band on a still-open escalation (executor=%s); the ask is complete and only the close never landed",
				rs, orNone(actor)),
			actor: actor,
			fields: map[string]string{
				"reaped_signal":        "result_status",
				"reaped_result_status": rs,
				"reaped_orchestration": "true",
			},
		}
	}

	// Witness 2: a human-operator resolve-comment. Only fetched when the field
	// witness is absent, so the common case costs one field read, not a round
	// trip. An AGENT comment can never trip this even if it carries the directive
	// (ka-0000002488).
	comments, err := r.daemon.GetComments(ctx, b.ID)
	if err != nil {
		r.logger.Debug("escalation reaper: comment fetch failed — no comment witness this sweep",
			"id", b.ID, "error", err)
		return false, reapAudit{}
	}
	for _, c := range comments {
		// WHO — the comment must be AUTHORED by a human, judged on its Author (the
		// audit actor), NOT on operated_by. Agents carry a human operated_by (their
		// spawning operator), so operated_by alone cannot tell an agent comment from
		// a human one — reading it as authorship let a gardener-agent comment reap a
		// live merge handle whose own text asked to keep it open (ka-0000002488,
		// closing ka-0000006977 against a stale head_sha). isHumanCommentAuthor is the
		// same discriminator comment_forward.go already uses for exactly this reason.
		if !isHumanCommentAuthor(c.Author) {
			continue
		}
		// WHETHER (veto first) — a "leave this open / do not execute / do not merge"
		// comment is the OPPOSITE of satisfaction and hard-vetoes the reap, even for
		// a human author and even if a resolve-looking phrase also appears in the
		// same comment. Fail closed: a false "satisfied" hides unfinished privileged
		// work; a stale open handle merely nags.
		if operatorReapVeto.MatchString(c.Text) {
			continue
		}
		// WHETHER (directive) — an explicit resolve directive must be present, so a
		// human musing "should I reap this?" is not read as a resolution.
		if !operatorResolveDirective.MatchString(c.Text) {
			continue
		}
		author := commentOperator(c)
		return true, reapAudit{
			reason: fmt.Sprintf("operator %s resolved this out-of-band in a comment: %q",
				author, truncateForAudit(c.Text, 160)),
			actor: author,
			fields: map[string]string{
				"reaped_signal":        "operator_comment",
				"reaped_orchestration": "true",
			},
		}
	}

	return false, reapAudit{}
}

// commentOperator returns the human-operator id attributed to a comment,
// preferring the resolved operated_by over the raw author (an agent posting on
// behalf of an operator sets operated_by; a human commenting directly has it in
// author). Returns "" when neither is a human-operator shape.
func commentOperator(c beadsapi.Comment) string {
	if IsHumanOperator(c.OperatedBy) {
		return c.OperatedBy
	}
	if IsHumanOperator(c.Author) {
		return c.Author
	}
	// operated_by wins even if it is not a strict human shape — the IsHumanOperator
	// gate in the caller makes the final call; return the more-specific field.
	if c.OperatedBy != "" {
		return c.OperatedBy
	}
	return c.Author
}

// firstNonEmptyField returns the first non-empty value among keys in m.
func firstNonEmptyField(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

// truncateForAudit shortens s to at most n runes for an audit comment/log,
// appending an ellipsis when it cut.
func truncateForAudit(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// reap records an audit comment and closes the escalation (or logs the intent
// in dry-run). Returns true when it acted.
func (r *EscalationReaper) reap(ctx context.Context, b *beadsapi.BeadDetail, action string, ref reapRef, audit reapAudit) bool {
	nowStr := r.now().UTC().Format(time.RFC3339)
	actor := audit.actor
	if actor == "" {
		actor = "out-of-band"
	}

	if r.dryRun {
		r.logger.Info("escalation reaper: WOULD reap (dry-run)",
			"id", b.ID, "action", action, "project", ref.ProjectPath, "iid", ref.IID,
			"reason", audit.reason)
		return true
	}

	// Audit comment first, so the reason survives even if the close hits a
	// transient error and the bead is retried next sweep. The provenance line
	// differs by detector: the MR-witness path cites the verb allowlist; the
	// orchestration-layer path (empty action) cites the operator witness.
	var provenance string
	if action != "" {
		provenance = fmt.Sprintf("Verb `%s` is on the approve/merge allowlist; %s was the completion signal.", action, auditSignal(audit))
	} else {
		provenance = fmt.Sprintf("Orchestration-layer signal `%s` recorded the out-of-band resolution.", auditSignal(audit))
	}
	comment := fmt.Sprintf(
		"🪝 Auto-reaped by %s: satisfied out-of-band.\n\n%s\n\n%s Reaped at %s.",
		reapActor, audit.reason, provenance, nowStr)
	if audit.mrURL != "" {
		comment += "\nMR: " + audit.mrURL
	}
	if err := r.daemon.AddComment(ctx, b.ID, reapActor, comment); err != nil {
		// Non-fatal: proceed to close anyway; the close fields still carry the
		// machine-readable audit trail.
		r.logger.Warn("escalation reaper: failed to add audit comment",
			"id", b.ID, "error", err)
	}

	// P7 Task 10: only render/witness keys ride along (the daemon refuses
	// anything else). result_status=success already routes handleClosed to the
	// "Complete" render; the daemon stamps executor and closed_by itself.
	closeFields := map[string]string{
		"reaped":           "true",
		"reaped_by":        reapActor,
		"reaped_at":        nowStr,
		"reaped_reason":    audit.reason,
		"reaped_satisfier": actor,
	}
	// reaped_action is the MR-detector verb; the orchestration detector passes
	// "" (no verb), so only stamp it when present.
	if action != "" {
		closeFields["reaped_action"] = action
	}
	if audit.mrURL != "" {
		closeFields["reaped_mr_url"] = audit.mrURL
	}
	for k, v := range audit.fields {
		if v != "" {
			closeFields[k] = v
		}
	}

	// Close via the dedicated escalation-result verb, NOT the generic CloseBead
	// path — since ka-0000005516 the daemon 409s any generic close of an OPEN
	// escalation (it would skip operation_spec replay and unconditionally
	// release close-on-escalation wrappers). The result verb has exactly the
	// semantics the reaper wants: no replay, and wrappers released only on a
	// success result. The render/audit trail (reaped*, MR witness mr_*,
	// approvals_*) rides along as Fields so it lands as top-level bead fields atomically with
	// the close — a single beads.bead.closed event, never a preceding update
	// that would re-trigger execution of the already-satisfied ask.
	if err := r.daemon.CaptureEscalationResult(ctx, b.ID, beadsapi.CaptureEscalationResultRequest{
		ResultStatus: "success",
		Fields:       closeFields,
	}); err != nil {
		r.logger.Error("escalation reaper: failed to close reaped escalation",
			"id", b.ID, "action", action, "error", err)
		return false
	}

	r.logger.Info("escalation reaped: satisfied out-of-band",
		"id", b.ID, "action", action, "project", ref.ProjectPath, "iid", ref.IID,
		"signal", auditSignal(audit), "actor", actor)
	return true
}

// resolveMRRef finds the (host, projectPath, iid) for an escalation's MR-shaped
// ask. The authoritative structured operation_spec project+iid wins FIRST; a
// full MR URL (structured field, then prose) is the fallback used only when no
// structured project reference exists. Returns ok=false when no MR reference
// can be resolved (the reaper then leaves the escalation open).
//
// GitLab IIDs are per-project, so the (project, iid) pair — not the bare iid —
// is what identifies an MR. Bug ka-0000000198: when the URL scrape ran first, a
// merge_requests/<iid> URL for a DIFFERENT project sitting in the escalation's
// title/description (e.g. "orthogonal to monorepo!965", or a colliding IID that
// happens to name a closed MR) overrode the authoritative operation_spec
// project+iid. That mis-resolved the state check to the wrong project's MR and
// falsely reaped the handle — site-deployment!1230 was killed 3x by the closed
// monorepo!1230, then again on site-deployment!1229. Keying on the structured
// (project_id, iid) first makes prose incapable of redirecting the check.
//
// The verb allowlist is checked by the caller BEFORE this — discovery never
// widens what is reapable, it only finds the MR for an already-reapable verb.
func resolveMRRef(spec map[string]any, title, description string) (reapRef, bool) {
	// 1. AUTHORITATIVE: structured iid + project path from operation_spec (no
	//    host — treated as in-scope). This must win over any URL scraped from
	//    prose, which can name a different project entirely (see doc comment).
	proj := firstStringField(spec, "project", "mr_project", "project_path")
	iid := firstIntField(spec, "iid", "mr_iid", "merge_request_iid")

	// 1a. Prose fallback for the iid ONLY: a bare merge_requests/<iid> fragment
	//     in the structured mr_url / title / description, paired with the
	//     authoritative structured project path. The project is never taken from
	//     prose here — only the numeric iid, scoped to the structured project.
	if iid == 0 && proj != "" {
		iid = parseMRIIDFromText(stringField(spec, "mr_url"), title, description)
	}
	if iid > 0 && proj != "" {
		return reapRef{ProjectPath: proj, IID: iid}, true
	}

	// 2. FALLBACK: a full MR URL in a structured field, then in prose → host +
	//    path + iid. Reached only when the structured project reference above is
	//    absent, so it cannot override an authoritative operation_spec target.
	for _, s := range []string{
		stringField(spec, "mr_url"),
		stringField(spec, "merge_request_url"),
		title,
		description,
	} {
		if u := mrURLInText.FindString(s); u != "" {
			if m := ParseMRURL(u); m != nil {
				return reapRef{Host: m.Host, ProjectPath: m.ProjectPath, IID: m.IID}, true
			}
		}
	}

	return reapRef{}, false
}

// parseMRIIDFromText returns the first merge_requests/<iid> fragment found
// across the given strings, or 0.
func parseMRIIDFromText(sources ...string) int {
	for _, s := range sources {
		if m := mrIIDInText.FindStringSubmatch(s); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// parseOperationSpec decodes the operation_spec field (a JSON string on the
// bead) into a map. Returns an empty map on any parse failure or non-object.
func parseOperationSpec(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var m map[string]any
	// A JSON `null` unmarshals with err==nil but leaves m nil; a non-object
	// (array/number/string) errors. Both collapse to an empty map so callers
	// never dereference nil.
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// stringField returns the string value at key, or "". JSON numbers/bools are
// not coerced — only genuine string values are returned.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// firstStringField returns the first non-empty string value among keys.
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := stringField(m, k); v != "" {
			return v
		}
	}
	return ""
}

// firstIntField returns the first key whose value parses as a positive int.
// JSON numbers decode to float64; a numeric string is also accepted.
func firstIntField(m map[string]any, keys ...string) int {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if v > 0 {
				return int(v)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// mrURLFor synthesizes an MR web URL from a ref that carries a host, else "".
func mrURLFor(ref reapRef) string {
	if ref.Host == "" || ref.ProjectPath == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", ref.Host, ref.ProjectPath, ref.IID)
}

// auditSignal names the completion signal used, for the comment/log.
func auditSignal(a reapAudit) string {
	if a.fields != nil {
		if s := a.fields["reaped_signal"]; s != "" {
			return s
		}
	}
	return "out-of-band"
}

// orNone returns s, or "(unknown)" when empty — for readable audit prose.
func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
