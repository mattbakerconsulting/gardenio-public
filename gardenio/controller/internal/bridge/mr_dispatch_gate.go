package bridge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mr_dispatch_gate.go implements the deterministic suppress-dispatch decision
// the gardener-delivery MR walk needs before it spawns a review-merge crew onto
// a green MR (bug ka-0000004390).
//
// # The leak this closes
//
// gardener-delivery is a cheap Haiku ORCHESTRATOR that walks open MRs every 20m
// and dispatches an Opus review-merge crew per eligible orphan. Its DEDUP was a
// prompt-driven bash/jq heuristic — fragile enough that a Haiku model runs it
// unreliably — and it only scanned `type=decision`, never escalations. So an MR
// whose task is parked on an OPEN operator merge-gate (a `gb decision` awaiting
// a human merge/hold call, and/or the `gb escalation` holding the raw approve+
// merge ask) got a fresh Opus crew every cycle: SIX+ redundant crews in ~18h on
// MR !6218 alone, each re-reaching the identical "held for operator" verdict.
//
// `gb ready` already suppresses that same task correctly — it carries forward
// `blocks` edges to both the open decision and open escalation, so the daemon's
// NoOpenDeps filter drops it. But the MR walk lists MRs via `glab mr list`, NOT
// `gb ready`, so it never inherited that suppression. This gate restores the
// symmetry deterministically: it reads the task's forward blocking-dep chain
// (the exact edges NoOpenDeps keys on) and, when an open decision/escalation
// blocker is present, tells the gardener to comment-only ("no new crew (dedup)")
// instead of spawning a pod.
//
// # The second signal: an open decision/escalation that only REFERENCES the MR
//
// The blocking-edge scan above catches only handles wired to the task with a
// `blocks`/`depends-on`/`blocked-by` edge. But a review-merge decision is
// frequently filed by a DIFFERENT agent that names the MR by `!iid` / URL in its
// title/description without ever adding a blocks edge to the producing task —
// and an ORPHAN MR (no linked task at all) can carry one too. Those slip past
// the edge scan, so gardener-delivery re-discovers the MR and re-dispatches an
// Opus crew every cycle (the durable-gate gap of R1, bug ka-0000001050; live
// repro: decision ka-0000004748 on MR !6428 re-crewed 27/50 cycles). The old
// prompt-level guard against this was a hand-rolled `kd list --type=decision
// --status=open --json | jq contains(<iid>)` scan — which returns only the
// daemon's first PAGE, so it is a SILENT FALSE-EMPTY on any fleet with more than
// a page of open decisions (advice ka-0000005023, ka-0000002706) and never
// looked at escalations. This gate folds that reference signal in
// deterministically: the caller supplies the candidate open decision/escalation
// beads (gathered via the server-side mentions index on the linked bead PLUS a
// bounded, explicitly-limited decision/escalation listing — never a one-page
// jq scan), and EvaluateDispatchGate matches each candidate's text against the
// MR's URL / `!iid` bang-reference using the same tested matchers the escalation
// reaper uses (mrURLInText + parseMRBangRef).
//
// The evaluation is a PURE function (no daemon I/O) so it is fully
// table-testable, mirroring EvaluateTicketResolvedCloseGate in
// mr_relevance_gate.go. The caller (gb mr dispatch-gate) fetches the task's
// dependencies, the blocker beads, and the reference candidates and hands them
// in.

// # R3 (bug ka-0000007496): one aging nudge instead of silent re-suppression
//
// R1 (above) stops the gardener re-CREWING a parked MR every cycle — but it does
// so SILENTLY: the gate returns MayDispatch=false, the gardener writes a "no new
// crew (dedup)" comment on the handle, and nothing ever tells the OPERATOR the
// MR has been sitting green + mergeable for hours awaiting their call. MR !6428
// (decision ka-0000004748) sat green+gated 2+ days while 27 crews silently
// re-confirmed it — the operator never got a single ping that their attention
// was the only thing missing.
//
// R3 closes that loop. The gate already knows which handle parks the MR and when
// that handle was filed, so it can also decide — purely — whether the park has
// aged past a threshold (~6 cycles at the 20m cadence == 2h) and no nudge has yet
// been sent. When so, EvaluateDispatchGate flags Nudge{...} alongside the
// suppression; the caller posts ONE comment on the EXISTING decision/escalation
// bead (MR URL + decision id + how long it has been gated) and stamps a one-shot
// marker field on that bead so the nudge never repeats. Crucially R3 does NOT
// file a new decision and does NOT change the suppress verdict — a persistently
// gated MR is still not re-crewed; the operator simply gets one aging ping.
//
// The one-shot is idempotent across cycles because the marker lives on the
// durable handle bead (fields.gardener_delivery_gate_nudged_at), NOT in the
// gardener's ephemeral per-cycle memory: the pure evaluator is told the handle's
// existing marker value and refuses a second nudge whenever it is already set.

// dispatchGateBlockingDepTypes mirrors the daemon's FilterNoOpenDeps allow-list
// (kbeads internal/store/postgres/queries.go) and the scheduler conditions
// package's blockingDepTypes: dep types that mean the SOURCE bead is genuinely
// waiting on the target. Structural/relational edges (parent-child, related,
// duplicates, supersedes, produces, …) link beads but do NOT block, so they are
// excluded. Kept in lockstep with `gb ready` / `kd list --no-blockers` so the
// gate's notion of "blocked by" matches the queue the MR walk bypasses.
var dispatchGateBlockingDepTypes = map[string]struct{}{
	"blocks":     {},
	"blocked-by": {},
	"depends-on": {},
}

// dispatchGateBlockerTypes is the set of blocker bead TYPES that mean the task
// is parked on a human — i.e. no crew agent can advance it this cycle even in
// principle. These are exactly the operator merge-gate handles: a `gb decision`
// (the merge/hold fork) and a `gb escalation` (the raw approve+merge ask). A
// task blocked by an open bead of any OTHER type (e.g. a sibling task) is NOT
// operator-parked — a crew could still unblock it — so it stays dispatchable.
var dispatchGateBlockerTypes = map[string]struct{}{
	"decision":   {},
	"escalation": {},
}

// dispatchGateSelfExecMergeOpTypes is the set of escalation `operation_type`
// values that name a MERGE the group bot can perform ITSELF — not a human hold.
//
// # Why this exists (bug ka-0000002517): the self-suppression deadlock
//
// gardener-delivery's job is literally "merge green MRs yourself" (delivery.md).
// But when a crew agent despawns on an unmerged MR, the boot prompt tells it to
// file a `gitlab_mr_merge` escalation with `--close-on-execute` as its re-entry
// handle (init_prompts.go). That escalation is NOT an operator gate — it is a
// self-executable ask: the group bot CAN merge protected main (proven live on
// gardenio, user.can_merge=true), and the escalation auto-closes the moment the
// MR merges. So the intended resolution IS the gardener merging directly.
//
// The dispatch-gate, however, treated EVERY open escalation referencing an MR as
// an operator merge-gate and suppressed dispatch. That deadlocks: the escalation
// only clears when the gardener merges, but the gate forbids the gardener from
// acting on the one MR that would clear it. Measured fallout (bug ka-0000002517):
// 51 of 72 gardener-delivery cycles in 24h (70%) reported MRs as "gated on
// operator" / "legitimate holds" and dispatched nothing, while 32 mergeable MRs
// sat — a self-inflicted fleet-wide throughput ceiling, plus escalation spam
// (advice ka-0000000752: attempt the privileged action FIRST; ka-0000000014: a
// policy ban is not an IAM capability gap).
//
// A `gitlab_mr_merge`-family escalation is therefore explicitly NOT a gate: the
// gardener is cleared to walk the MR, check CI, and merge it (or file a genuine
// `decision` if it turns out risky — see delivery.md MERGE-GATE). Only a real
// operator FORK (a `decision`) or a NON-self-executable escalation (aws_iam,
// terraform_apply, credential_*, gitlab_admin, …) still parks the MR.
//
// operation_type is FREE-TEXT (never a closed enum — docs/research/
// escalation-beads-review-2026-05-05.md), so this normalizes the spelling drift
// the live board actually carries, mirroring the viewer's humanizeEscalationOp
// "merge MR" family (plans_escalations.go). Normalization: lowercase, hyphens →
// underscores.
var dispatchGateSelfExecMergeOpTypes = map[string]struct{}{
	"gitlab_mr_merge":         {},
	"gitlab_merge":            {},
	"merge_request":           {},
	"merge_approval":          {},
	"gitlab_mr_approval":      {},
	"gitlab_mr_approve_merge": {},
}

// dispatchGateSelfMergeableRepos is the set of repo SHORT NAMES (last path
// segment) whose PROJECT POLICY clears the group bot to self-merge its own MR on
// protected `main` WITHOUT a human operator.
//
// # Why the self-exec carve-out needs a repo condition (bug ka-0000003768)
//
// The carve-out above (dispatchGateSelfExecMergeOpTypes / isSelfExecMergeEscalation)
// treats EVERY gitlab_mr_merge-family escalation as a merge the bot can perform
// itself, so it never suppresses dispatch. That premise is TRUE for gardenio
// (standing rule ka-0000002230: gardenio owns its own repo, default-merge, no
// human carve-out) and FALSE for protected `main` in every other first-party repo
// (monorepo, site-deployment, acme-helm-chart, fluentbit, …). There the bot is
// Developer/30 and an approve+merge is a REAL operator gate
// (detailed_merge_status=not_approved, approvals_left>=1) — the bot cannot
// self-approve.
//
// Without the repo condition, a non-gardenio approve+merge escalation never
// suppressed dispatch, so gardener-delivery re-dispatched a fresh Opus review-merge
// crew every cycle forever — measured 80 crew generations / 88 HELD
// re-verifications across 10 non-gardenio MRs, each crew re-reaching the identical
// "HELD — cannot self-approve" verdict on an unchanged head SHA. Zero merges, zero
// new information, unbounded Opus spend.
//
// The distinction is PROJECT POLICY, not raw capability: user.can_merge can read
// true on both (init_prompts.go capability probe measured monorepo !2641
// can_merge=true), but only gardenio policy clears an agent to act on it
// unattended, and the monorepo "humans merge their own things" rule (bug
// ka-0000007570) still holds everywhere else. Keyed on the SHORT NAME so a full
// path ("example-org/gardenio"), a bare "gardenio", and a re-namespaced
// group all match. Overridable-in-spirit: add a repo here only when its policy
// genuinely lets an agent self-merge unattended.
var dispatchGateSelfMergeableRepos = map[string]struct{}{
	"gardenio": {},
}

// dispatchGateOpenBlockerStatuses mirrors isOpenBlockerStatus / the daemon
// NoOpenDeps status set: a blocker is still "live" (keeps its dependent parked)
// unless it reached a terminal status. `blocked`/`deferred` are non-terminal, so
// a blocker in either still holds its dependent back.
var dispatchGateOpenBlockerStatuses = map[string]struct{}{
	"open":        {},
	"in_progress": {},
	"blocked":     {},
	"deferred":    {},
}

// DispatchGateNudgeCycles is the age threshold, in gardener-delivery cycles, past
// which a still-parked MR earns ONE aging nudge to the operator (R3, bug
// ka-0000007496). The gardener fires every ~20m, so 6 cycles ≈ 2h — long enough
// that a genuinely-in-flight operator merge has already happened, short enough
// that a forgotten one is surfaced the same working session.
const DispatchGateNudgeCycles = 6

// dispatchGateCycleInterval is the gardener-delivery cadence one cycle represents
// (trigger ka-0000001510 fires every 20m). DispatchGateNudgeCycles × this is the
// wall-clock age a park must exceed before the aging nudge fires.
const dispatchGateCycleInterval = 20 * time.Minute

// DispatchGateNudgeAge is the wall-clock gate age past which the one-shot aging
// nudge fires: DispatchGateNudgeCycles cycles at the 20m cadence (== 2h).
const DispatchGateNudgeAge = DispatchGateNudgeCycles * dispatchGateCycleInterval

// DispatchGateNudgedAtField is the one-shot marker field the caller stamps on the
// parking decision/escalation bead once it has posted the aging nudge. Its
// presence (any non-empty value) tells a later cycle's pure evaluator the nudge
// already went out, so it is never repeated. Stored as an RFC3339 timestamp for
// human legibility, but the evaluator only checks emptiness — the VALUE is not
// parsed, so a hand-set marker of any shape still suppresses re-nudging.
const DispatchGateNudgedAtField = "gardener_delivery_gate_nudged_at"

// DispatchGateInput is the input to the review-merge suppress-dispatch gate. It
// is the projection of the MR's linked TASK bead plus its forward dependencies
// and the resolved blocker beads, so the decision is pure.
type DispatchGateInput struct {
	// TaskID is the MR's linked task bead id (for diagnostics; may be "" when
	// the MR has no linked bead — the caller then reports MayDispatch=true since
	// there is nothing to prove parked).
	TaskID string
	// Deps are the task bead's FORWARD dependencies (rows where BeadID==TaskID,
	// DependsOnID==the blocker). The caller obtains these from
	// GetDependencies(taskID).
	Deps []beadsapi.Dependency
	// Blockers maps a bead id to its detail, for every DependsOnID referenced by
	// a blocking-typed dep in Deps. A missing entry is treated as unprovable
	// (fail-open: not counted as an operator park). The caller batch-fetches
	// these.
	Blockers map[string]*beadsapi.BeadDetail

	// MRURL is the MR's full GitLab URL, MRProjectPath its "group/…/repo" path,
	// and MRIID its project-scoped IID. They feed the REFERENCE signal: a candidate
	// decision/escalation whose text names the MR by URL or `!iid` bang-reference
	// parks it even without a blocks edge. Any may be zero-valued when the caller
	// could not resolve it (an MR bead with no mr_url, say) — the reference scan
	// then simply matches nothing on the missing dimension, staying fail-open.
	//
	// MRProjectPath matters because an MR IID is PROJECT-scoped: a
	// `monorepo!1445` decision must NOT suppress a gardenio MR !1445. So a
	// PROJECT-QUALIFIED bang-reference only matches when its project agrees with
	// MRProjectPath; a bare `!iid` (no project) still matches, since that is the
	// common repo-local shape a gardenio decision writes.
	MRURL         string
	MRProjectPath string
	MRIID         int
	// RefCandidates are OPEN decision/escalation beads the caller gathered as
	// possible references to this MR — via the server-side mentions index on the
	// linked bead (typed or untyped) AND a bounded, explicitly-limited
	// decision/escalation listing for the URL/`!iid` literal. EvaluateDispatchGate
	// re-checks each candidate's TYPE, STATUS, and actual text match itself (the
	// caller's query is a coarse pre-filter, not the authority), so a candidate
	// that does not truly reference the MR is ignored. Empty is fine — the gate
	// then rests on the blocking-edge signal alone.
	RefCandidates []*beadsapi.BeadDetail

	// Now is the reference time the aging-nudge age is measured against (R3, bug
	// ka-0000007496). Injected so the age computation is pure and table-testable.
	// A zero value disables the aging nudge entirely: the gate still suppresses
	// dispatch, it just never flags a Nudge (fail-closed on the nudge, never on
	// the suppression). The CLI passes time.Now().
	Now time.Time
	// NudgeAge overrides the wall-clock park age past which the one-shot aging
	// nudge fires. A non-positive value (the common case) falls back to the
	// compiled DispatchGateNudgeAge. It is an INJECTION SEAM only — the CLI
	// resolves it from the GB_DISPATCH_GATE_NUDGE_AGE test-observability env var
	// (mirroring gb news' GB_NEWS_ALIVE_FRESHNESS_WINDOW) so an e2e can drive the
	// real one-shot nudge lifecycle against a freshly-created handle bead whose
	// created_at cannot be backdated on the shared daemon. Production never sets
	// it, so the 6-cycle (~2h) threshold is unchanged.
	NudgeAge time.Duration
}

// DispatchGateDecision is the outcome of the suppress-dispatch gate.
type DispatchGateDecision struct {
	// MayDispatch is true when the gardener MAY spawn a review-merge crew for the
	// MR. When false, the task is parked on an open operator merge-gate and the
	// gardener must comment-only ("no new crew (dedup)") instead of spawning a
	// pod — the existing decision/escalation governs.
	MayDispatch bool
	// Reason is a one-line, human-readable explanation — suitable for the
	// gardener's dedup comment or a squawk.
	Reason string
	// BlockerIDs are the open operator merge-gate handles (decision/escalation
	// bead ids) that park the task, sorted for determinism. Empty when
	// MayDispatch is true.
	BlockerIDs []string
	// Nudge, when non-nil, tells the caller to post ONE aging nudge to the
	// operator on the named handle bead and stamp its one-shot marker (R3, bug
	// ka-0000007496). It is only ever set on a suppression (MayDispatch=false):
	// the MR has been parked past DispatchGateNudgeCycles and no nudge has gone
	// out yet. nil means "no nudge this cycle" — either the park is younger than
	// the threshold, a nudge was already sent, or the gate is not suppressing.
	Nudge *DispatchGateNudge
}

// DispatchGateNudge is the one-shot aging-nudge directive the evaluator returns
// when a persistently-gated MR has aged past the threshold without a nudge yet
// (R3, bug ka-0000007496). The caller posts a single comment on HandleID naming
// the MR + the handle + GateAge, then stamps DispatchGateNudgedAtField on
// HandleID so the nudge never repeats.
type DispatchGateNudge struct {
	// HandleID is the decision/escalation bead the operator must act on — the
	// SAME existing handle that parks the MR, never a freshly-filed one. This is
	// the lex-first parking handle whose filing time is oldest-known, so the age
	// reported is the age of the park as a whole.
	HandleID string
	// HandleType is the parking handle's bead type ("decision" | "escalation"),
	// for a precise nudge message.
	HandleType string
	// GateAge is how long the MR has been parked — Now minus the handle's
	// creation time — rounded to the minute for a clean human-facing message.
	GateAge time.Duration
}

// EvaluateDispatchGate decides whether gardener-delivery may dispatch a fresh
// review-merge crew onto an MR, given its linked task's dependency graph AND any
// open decision/escalation that references the MR by URL/`!iid`.
//
// Rule (bugs ka-0000004390 + ka-0000001050): DO NOT dispatch when EITHER signal
// says the MR is parked on a human merge/hold call —
//
//   - BLOCKING-EDGE signal: the task carries a forward blocking-typed dep
//     (blocks / blocked-by / depends-on) to a bead of type decision|escalation in
//     a live status (open|in_progress|blocked|deferred); or
//   - REFERENCE signal: an OPEN decision/escalation candidate's text references
//     this MR by full URL or `!iid` bang-reference (even with no blocks edge, and
//     even for an orphan MR with no linked task).
//
// Either way the sole remaining action is a HUMAN merge/hold call no crew can
// take, so the gardener comments on the existing handle instead of spawning a
// pod. Otherwise dispatch.
//
// Fail-OPEN on every uncertainty (no task AND no reference, no deps, a blocker we
// could not resolve): return MayDispatch=true. Over-dispatching one extra crew is
// the prior (tolerable) behavior; suppressing a genuinely-actionable MR would
// strand real work, which is strictly worse. This mirrors the fail-safe contract
// of the scheduler's exclude_blocked_by_open_types filter, inverted for the
// caller's benefit (that filter fails toward "keep looping"; this gate fails
// toward "keep dispatching").
func EvaluateDispatchGate(in DispatchGateInput) DispatchGateDecision {
	seen := make(map[string]struct{})
	var blockers []string
	// handles keeps the resolved bead detail for each blocker id so the aging
	// nudge can read the parking handle's creation time (age) and one-shot marker
	// without a second lookup — the same beads already inspected for type/status.
	handles := make(map[string]*beadsapi.BeadDetail)
	add := func(b *beadsapi.BeadDetail) {
		if b == nil || b.ID == "" {
			return
		}
		if _, dup := seen[b.ID]; dup {
			return
		}
		seen[b.ID] = struct{}{}
		blockers = append(blockers, b.ID)
		handles[b.ID] = b
	}

	// Resolve the crew's actual merge TARGET repo — the authoritative anchor for
	// the self-exec carve-out's repo condition (bug ka-0000003768). The gate is
	// evaluating one MR, and a dispatched crew would merge THAT MR, so its repo
	// governs whether the bot can self-merge. Prefer the resolved project path;
	// fall back to parsing the MR URL. Empty when neither resolved — the carve-out
	// then leans on the escalation's own repo signal, and finally fails toward
	// SUPPRESS (see isSelfExecMergeEscalation).
	mrRepo := in.MRProjectPath
	if mrRepo == "" && in.MRURL != "" {
		if m := ParseMRURL(in.MRURL); m != nil {
			mrRepo = m.ProjectPath
		}
	}

	// BLOCKING-EDGE signal — the task is wait-edged to an open operator handle.
	if in.TaskID != "" {
		for _, d := range in.Deps {
			if !isDispatchGateBlockingDepType(d.Type) || d.DependsOnID == "" {
				continue
			}
			b, ok := in.Blockers[d.DependsOnID]
			if !ok || b == nil {
				// Unresolved blocker — cannot prove it is an operator park.
				// Fail-open: do not count it.
				continue
			}
			if !isOpenDispatchGateHandle(b, mrRepo) {
				continue
			}
			add(b)
		}
	}

	// REFERENCE signal — an open decision/escalation names this MR by URL/`!iid`
	// without necessarily wiring a blocks edge (R1, bug ka-0000001050). Runs even
	// for an orphan MR (in.TaskID == "") so a decision on a task-less MR still
	// dedups. The caller's RefCandidates query is a coarse pre-filter; re-verify
	// type/status/text here so a false candidate never suppresses a live MR.
	for _, c := range in.RefCandidates {
		if c == nil || !isOpenDispatchGateHandle(c, mrRepo) {
			continue
		}
		if !beadReferencesMR(c, in.MRURL, in.MRProjectPath, in.MRIID, in.TaskID) {
			continue
		}
		add(c)
	}

	if len(blockers) == 0 {
		reason := "MR has no linked task bead and no open decision/escalation references it; nothing to prove parked — dispatch"
		if in.TaskID != "" {
			reason = fmt.Sprintf(
				"task %s is not blocked by nor referenced by an open decision/escalation — dispatch a review-merge crew",
				in.TaskID)
		}
		return DispatchGateDecision{MayDispatch: true, Reason: reason}
	}

	sort.Strings(blockers)
	subject := "this MR"
	if in.TaskID != "" {
		subject = "task " + in.TaskID
	}
	return DispatchGateDecision{
		MayDispatch: false,
		Reason: fmt.Sprintf(
			"%s is parked on open operator merge-gate %s — awaiting a HUMAN merge/hold call, so NO new review-merge crew (comment-only dedup); the existing handle governs",
			subject, strings.Join(blockers, ", ")),
		BlockerIDs: blockers,
		Nudge:      evaluateAgingNudge(blockers, handles, in.Now, in.NudgeAge),
	}
}

// evaluateAgingNudge decides — purely — whether a persistently-parked MR earns
// its ONE aging nudge this cycle (R3, bug ka-0000007496). It runs only on the
// suppression path (blockers is non-empty). The nudge fires on the OLDEST-known
// parking handle so the age it reports is the age of the park as a whole, and
// only when ALL of:
//
//   - in.Now is set (a zero Now disables the nudge — fail-closed on the nudge,
//     never on the suppression);
//   - the handle's creation time is known and the park has aged past the
//     threshold (nudgeAge, defaulting to DispatchGateNudgeAge ==
//     DispatchGateNudgeCycles cycles); and
//   - that handle has not already been nudged — its DispatchGateNudgedAtField
//     one-shot marker is empty.
//
// Returns nil (no nudge) whenever any condition is unmet, so a younger park, an
// already-nudged park, or an unknown filing time all silently skip the nudge
// while still suppressing dispatch.
//
// nudgeAge is the injectable threshold (DispatchGateInput.NudgeAge); a
// non-positive value falls back to the compiled DispatchGateNudgeAge, so
// production keeps the 6-cycle (~2h) age and only a test seam moves it.
//
// "Oldest-known" is chosen over lex-first so the reported age is truthful when
// several handles park the same MR: the park began when the FIRST one was filed.
// Ties (and the all-unknown-time case) fall back to lex order for determinism.
func evaluateAgingNudge(blockers []string, handles map[string]*beadsapi.BeadDetail, now time.Time, nudgeAge time.Duration) *DispatchGateNudge {
	if now.IsZero() || len(blockers) == 0 {
		return nil
	}
	if nudgeAge <= 0 {
		nudgeAge = DispatchGateNudgeAge
	}
	// Pick the oldest-filed parking handle. blockers is already sorted, so a tie
	// on CreatedAt (or handles with an unknown/zero CreatedAt) resolves to the
	// lex-first id deterministically.
	var chosen *beadsapi.BeadDetail
	for _, id := range blockers {
		h := handles[id]
		if h == nil {
			continue
		}
		if chosen == nil {
			chosen = h
			continue
		}
		// A known, earlier creation time wins. A zero CreatedAt never displaces a
		// known one (an unknown filing time cannot claim to be older).
		if !h.CreatedAt.IsZero() && (chosen.CreatedAt.IsZero() || h.CreatedAt.Before(chosen.CreatedAt)) {
			chosen = h
		}
	}
	if chosen == nil || chosen.CreatedAt.IsZero() {
		// No handle with a known filing time — cannot prove the park is old
		// enough. Suppress the nudge (but the caller still suppresses dispatch).
		return nil
	}
	if strings.TrimSpace(chosen.Fields[DispatchGateNudgedAtField]) != "" {
		// One-shot already spent on this handle — never nudge twice.
		return nil
	}
	age := now.Sub(chosen.CreatedAt)
	if age < nudgeAge {
		return nil
	}
	return &DispatchGateNudge{
		HandleID:   chosen.ID,
		HandleType: chosen.Type,
		GateAge:    age.Round(time.Minute),
	}
}

// isOpenDispatchGateHandle reports whether b is a live operator merge-gate handle
// — a decision/escalation bead in a non-terminal status. Shared by the
// blocking-edge and reference scans so both key on the exact same "still parked"
// notion.
//
// A self-executable `gitlab_mr_merge`-family escalation ON A SELF-MERGEABLE REPO
// is explicitly NOT a gate (bugs ka-0000002517 + ka-0000003768): it is a re-entry
// handle for a merge the group bot can perform itself, not a human hold, so
// treating it as a gate deadlocks gardener-delivery against its own KPI. On a
// non-self-mergeable repo the same escalation IS a real operator gate. mrRepo is
// the gate MR's target repo (see EvaluateDispatchGate). See
// isSelfExecMergeEscalation.
func isOpenDispatchGateHandle(b *beadsapi.BeadDetail, mrRepo string) bool {
	if b == nil {
		return false
	}
	if _, isGate := dispatchGateBlockerTypes[b.Type]; !isGate {
		return false
	}
	if isSelfExecMergeEscalation(b, mrRepo) {
		return false
	}
	_, isOpen := dispatchGateOpenBlockerStatuses[b.Status]
	return isOpen
}

// isSelfExecMergeEscalation reports whether b is an escalation whose
// operation_type names a merge the group bot can perform itself
// (dispatchGateSelfExecMergeOpTypes) AND whose merge target is a repo whose policy
// clears an agent to self-merge (dispatchGateSelfMergeableRepos — gardenio today).
// Such a handle is a self-executable re-entry ask, NOT an operator gate — so it
// must not suppress gardener-delivery's own merge (bug ka-0000002517).
//
// operation_type is free-text, so it is normalized (lowercase, hyphens →
// underscores) exactly as the viewer's humanizeEscalationOp does. Only escalations
// carry operation_type; a decision never matches here.
//
// # The repo condition (bug ka-0000003768)
//
// The carve-out only holds where the bot can actually self-merge — gardenio.
// mrRepo is the gate MR's own target repo (the crew's actual merge target, the
// authoritative anchor); when it is empty (unresolved) we fall back to the repo
// the escalation itself names. If NEITHER confirms a self-mergeable repo, the
// carve-out is DECLINED (returns false) and the escalation is treated as a real
// gate. That is a deliberate fail-toward-SUPPRESS: the carve-out is an exception
// to "escalation = operator gate" and must earn itself with a positively-confirmed
// self-mergeable repo. Over-suppressing costs one operator aging-nudge (R3);
// over-dispatching is the unbounded-Opus-crew leak this bug fixes. A genuine
// gardenio self-exec escalation reliably carries the "gardenio" signal (the gate
// MR path/URL, its own mr_url, or a "gardenio !iid" title), so the original
// deadlock (ka-0000002517) stays fixed.
func isSelfExecMergeEscalation(b *beadsapi.BeadDetail, mrRepo string) bool {
	if !isMergeFamilyEscalation(b) {
		return false
	}
	repo := mrRepo
	if repo == "" {
		repo = escalationMergeRepo(b)
	}
	return isDispatchGateSelfMergeableRepo(repo)
}

// isMergeFamilyEscalation reports whether b is an escalation whose free-text
// operation_type names a merge-family ask (dispatchGateSelfExecMergeOpTypes),
// REGARDLESS of repo. It is the repo-agnostic half of isSelfExecMergeEscalation:
// the dispatch-gate carve-out layers a repo condition on top (bug ka-0000003768),
// but the escalation reaper keys on the merge-family shape alone — a non-gardenio
// approve+merge escalation is still a merge-shaped ask whose completion is MR
// state=merged, so once the operator merges it the reaper must recognize and close
// it (bug ka-0000006698). operation_type is normalized (lowercase, hyphens →
// underscores) exactly as the viewer's humanizeEscalationOp does. Only escalations
// carry operation_type; a decision never matches.
func isMergeFamilyEscalation(b *beadsapi.BeadDetail) bool {
	if b == nil || b.Type != "escalation" {
		return false
	}
	norm := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(b.Fields["operation_type"])), "-", "_")
	_, ok := dispatchGateSelfExecMergeOpTypes[norm]
	return ok
}

// escalationMergeRepo best-effort derives the GitLab project reference (full path
// or short repo name) an escalation's merge ask targets, from its OWN fields/text
// — the FALLBACK repo signal used only when the gate's own MR reference did not
// resolve. It mirrors the escalation reaper's resolveMRRef precedence: structured
// operation_spec project → an mr_url field → a merge_requests URL in title/desc →
// the "<repo> !<iid>" short-name in the title. Returns "" when no repo is
// discernible.
func escalationMergeRepo(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	spec := parseOperationSpec(b.Fields["operation_spec"])
	if p := firstStringField(spec, "project", "mr_project", "project_path"); p != "" {
		return p
	}
	for _, s := range []string{
		b.Fields["mr_url"],
		stringField(spec, "mr_url"),
		stringField(spec, "merge_request_url"),
		b.Title,
		b.Description,
	} {
		if u := mrURLInText.FindString(s); u != "" {
			if m := ParseMRURL(u); m != nil && m.ProjectPath != "" {
				return m.ProjectPath
			}
		}
	}
	// Short-name bang-ref in the TITLE ("gardenio !7656", "site-deployment!1234").
	if m := mrShortNameBangRef.FindStringSubmatch(b.Title); m != nil {
		repo := strings.TrimRight(m[1], ".-_")
		if repo != "" && !mrShortNameStopwords[strings.ToLower(repo)] {
			return repo
		}
	}
	return ""
}

// isDispatchGateSelfMergeableRepo reports whether projectRef (a full GitLab
// project path or a bare short repo name) names a repo whose policy lets the group
// bot self-merge unattended (dispatchGateSelfMergeableRepos — gardenio today). The
// match keys on the lowercased final path segment, so "example-org/gardenio"
// and a bare "gardenio" both match. Empty input is never self-mergeable.
func isDispatchGateSelfMergeableRepo(projectRef string) bool {
	p := strings.Trim(strings.TrimSpace(projectRef), "/")
	if p == "" {
		return false
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	_, ok := dispatchGateSelfMergeableRepos[strings.ToLower(p)]
	return ok
}

// beadReferencesMR reports whether the candidate handle's text (title +
// description) names the MR — by full merge_requests/<iid> URL, by a
// `project!iid` / bare `!iid` bang-reference, or by a literal mention of the MR's
// linked task bead-id (the advice ka-0000005023 mentions-index signal). It reuses
// the exact matchers the escalation reaper trusts (mrURLInText + the bang-ref
// regex), so the gate's notion of "this decision is about that MR" matches how
// the merge-then-apply beads are actually written. Each dimension is independent:
// a zero MRIID disables the bang match, an empty MRURL the URL match, and an
// empty taskID the mention match, so a partially-resolved MR still matches on the
// dimensions it does have.
//
// IIDs are PROJECT-scoped, so the bang-match is project-aware: a project-
// qualified bang-ref (`monorepo!1445`) matches only when its project agrees with
// mrProjectPath; a bare `!iid` still matches (the common repo-local shape). This
// prevents a `monorepo!1445` decision from falsely suppressing a gardenio
// MR !1445 — a persistent false-suppress would strand a real MR, the one outcome
// worse than an extra dispatch.
func beadReferencesMR(b *beadsapi.BeadDetail, mrURL, mrProjectPath string, mrIID int, taskID string) bool {
	if b == nil {
		return false
	}
	// URL match: the exact full URL appears verbatim in title or description.
	if mrURL != "" {
		for _, s := range []string{b.Title, b.Description} {
			for _, u := range mrURLInText.FindAllString(s, -1) {
				if u == mrURL {
					return true
				}
			}
		}
	}
	// Bang-reference match: any `!iid` (optionally project-qualified) that equals
	// THIS iid, anywhere in title/description. A decision may name several MRs
	// ("supersedes !6400, merge !6428"), so scan every match, not just the first.
	// A project-qualified ref must ALSO agree on the project (IIDs are project-
	// scoped); a bare ref matches on the iid alone.
	if mrIID > 0 {
		for _, s := range []string{b.Title, b.Description} {
			for _, m := range mrBangRefPattern.FindAllStringSubmatch(s, -1) {
				n, err := strconv.Atoi(m[2])
				if err != nil || n != mrIID {
					continue
				}
				if bangRefProjectMatches(m[1], mrProjectPath) {
					return true
				}
			}
		}
	}
	// Mention match: the linked task bead-id appears as a whole-token reference in
	// the candidate's text. This is the signal the server-side mentions index
	// keys on; re-checking it here keeps the gate authoritative even if the caller
	// widened the candidate set. Guarded to a word boundary so ka-ABC does not
	// spuriously match ka-ABCDEF.
	if taskID != "" && (mentionsBeadID(b.Title, taskID) || mentionsBeadID(b.Description, taskID)) {
		return true
	}
	return false
}

// mentionsBeadID reports whether s references beadID as a whole token (bounded by
// a non-alphanumeric or string edge on both sides), so a short id can never
// substring-match a longer one.
func mentionsBeadID(s, beadID string) bool {
	if beadID == "" {
		return false
	}
	for idx := strings.Index(s, beadID); idx >= 0; {
		before := idx == 0 || !isBeadIDChar(rune(s[idx-1]))
		endPos := idx + len(beadID)
		after := endPos >= len(s) || !isBeadIDChar(rune(s[endPos]))
		if before && after {
			return true
		}
		next := strings.Index(s[idx+1:], beadID)
		if next < 0 {
			return false
		}
		idx += 1 + next
	}
	return false
}

// isBeadIDChar reports whether r can appear inside a bead id token (gd-<base62>).
func isBeadIDChar(r rune) bool {
	return r == '-' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// bangRefProjectMatches decides whether a bang-reference's project capture agrees
// with the MR's project path, so a project-scoped IID does not collide across
// repos.
//
//   - A BARE ref (empty project capture, e.g. "Merge MR !6428") matches on the
//     iid alone — that is the repo-local shape a gardenio decision writes.
//   - A QUALIFIED ref must agree with mrProjectPath: full-path equality, a
//     "/<proj>" path-suffix, or the MR repo's last path segment (how humans write
//     "monorepo!1445"). An unresolved mrProjectPath rejects a qualified ref, so an
//     unconfirmable cross-repo `!iid` never suppresses (fail toward dispatch).
//
// Case-insensitive; surrounding slashes on the capture are trimmed.
func bangRefProjectMatches(bangProj, mrProjectPath string) bool {
	if bangProj == "" {
		return true
	}
	if mrProjectPath == "" {
		return false
	}
	p := strings.Trim(strings.ToLower(bangProj), "/")
	mrp := strings.ToLower(mrProjectPath)
	if p == mrp || strings.HasSuffix(mrp, "/"+p) {
		return true
	}
	// Last path segment of the MR (its repo name) — "monorepo" vs
	// ".../monorepo".
	if seg := mrp[strings.LastIndex(mrp, "/")+1:]; seg == p {
		return true
	}
	return false
}

// isDispatchGateBlockingDepType reports whether t is a blocking-typed dep.
func isDispatchGateBlockingDepType(t string) bool {
	_, ok := dispatchGateBlockingDepTypes[t]
	return ok
}
