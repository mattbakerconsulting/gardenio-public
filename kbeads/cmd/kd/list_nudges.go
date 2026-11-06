package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gardenio/kbeads/internal/client"
)

// Self-revealing stderr nudges for what the DEFAULT issue-kind filter hid.
//
// `kd list` / `kd count` inject kind=issue when the caller passes no explicit
// --type/--kind/--all-types (buildListRequest, gated by listRequest.
// implicitIssueKind). That default is right for the common case but silently
// hides whole classes of bead, and a 0-row answer is indistinguishable from
// "none exist". Each nudge here detects one such silence and prints a one-line
// note naming the flag that lifts it. Both are best-effort (transport errors
// swallowed) and stderr-only, so stdout contracts — the bare-array `--json`,
// the bare integer from `kd count` — are untouched.

// operationalNudgeTypes are the operator-artifact types the default kind=issue
// filter hides. Kept as a named var so the count query and the tests agree on
// exactly which types the nudge sizes.
var operationalNudgeTypes = []string{"escalation", "decision"}

// maybePrintOperationalNudge issues a cheap count-only query for open
// escalation/decision beads and, when any exist, prints a one-line stderr hint
// pointing at --include-operational. It is FLEET-WIDE (no project label): a
// decision bead carries no project: label (gb decision create stamps none,
// cmd/gb/decision.go), so a project-scoped count would silently miss every
// decision — worse than omitting the note (ka-0000007254). Counting fleet-wide
// also makes the number match exactly what the suggested `kd list
// --include-operational` (itself fleet-wide) would surface. The note states
// "fleet-wide" so a reader whose main list WAS project-scoped is not misled
// about the count's scope.
//
// The fleet scope of the MAIN list (req.Fleet — "" for the token default, "*"
// under --all-fleets, or a named fleet) is threaded through so the count spans
// the same fleets the reader's list did; otherwise an admin `kd list
// --all-fleets` would show a cross-fleet list but a single-fleet count under a
// "fleet-wide" note.
//
// Best-effort: any transport error is swallowed so the nudge never fails or
// delays the primary listing.
func maybePrintOperationalNudge(ctx context.Context, mainScopeApplied bool, fleet string) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:      operationalNudgeTypes,
		Status:    []string{"open"},
		Fleet:     fleet,
		CountOnly: true,
	})
	if err != nil || resp == nil || resp.Total == 0 {
		return
	}
	noun := "beads"
	if resp.Total == 1 {
		noun = "bead"
	}
	// Only flag the fleet-vs-project scope mismatch when the main list was
	// itself narrowed to a project — otherwise both are fleet-wide and the
	// "fleet-wide" qualifier would be noise.
	scopeNote := " fleet-wide"
	if !mainScopeApplied {
		scopeNote = ""
	}
	fmt.Fprintf(os.Stderr,
		"note: %d open escalation/decision %s hidden%s — kd list --include-operational to show (or -t escalation -t decision)\n",
		resp.Total, noun, scopeNote)
}

// scopeKindNudgeProbeTimeout bounds the scope-kind probe. The probe is
// deliberately BROADER than the primary query (it drops the kind=issue clause
// and the implicit project label — the two most selective predicates). A
// `-f key=*` presence predicate on a HOT indexed key now reaches the
// migration-025/031/034 partial expression indexes (ka-0000001686 added the
// `fields ? key` guard the planner needs), but any non-indexed key still
// degrades to a sequential scan on a large table. A hint is worth a moment,
// never a hang.
const scopeKindNudgeProbeTimeout = 3 * time.Second

// maybePrintScopeKindNudge closes two variants of the same false-answer footgun:
// a predicate whose matching beads the default scoping (implicit kind=issue,
// implicit project label) silently excluded, so the answer reads as complete
// when it is not.
//
//   - `-f/--field` (ka-0000004030): a custom field carried only by beads the
//     scoping excluded — e.g. `spawned_by`, which lives on kind=config agent
//     beads — matches nothing under kind=issue, so `kd count --all-projects -f
//     spawned_by=*` printed a bare, confident `0` while `--all-types` reported
//     40k+. The FALSE-EMPTY variant.
//   - `--created-by` / `--assignee` (ka-0000008078): the most common actor
//     traces. An agent's non-issue-kind artifacts (agent/session/message beads)
//     are hidden by kind=issue, so `kd list --created-by X` showed 1 of the 6
//     beads X created and the footer's `(1 total)` read as the whole count. The
//     FALSE-UNDERCOUNT variant — a NON-empty answer that still lies.
//
// Both are the same class of false all-clear that --label-any (ka-0000003307)
// and the unknown-`--type` validation (ka-0000006244) already fail loud on.
//
// It fires only on the misleading case, and the ENTRY gate differs by predicate:
//   - a `-f` predicate is probed only when the answer was 0 (its footgun is the
//     confident bare zero; probing every non-empty `-f` list would cost a
//     round-trip for no signal);
//   - an actor column (`--created-by`/`--assignee`) is probed whenever set,
//     because its footgun is the undercount — a non-empty answer that omits the
//     hidden beads.
//
// In BOTH cases the kind=issue clause must be IMPLICIT (an explicit
// --type/--kind/--all-types is the reader's own scoping choice and stays
// silent), and the note prints only when the recount exceeds what the primary
// query returned — so a predicate the scoping did not truncate prints nothing.
// The firing test is `probe > total`, which for the `-f` variant (total always 0
// here) reduces to `probe > 0`, i.e. the original "found any rows" behavior.
//
// Cost, stated honestly: the probe is not "cheap by construction". It is
// deliberately BROADER than the primary query — dropping kind=issue and the
// implicit project label removes the two most selective predicates. A `key=*`
// presence predicate on a HOT indexed key now reaches the partial expression
// indexes on fields->>'key' (ka-0000001686 gave the presence branch the `fields
// ? key` guard Postgres needs — it cannot derive that index predicate from IS
// NOT NULL alone), but a non-indexed key still worst-cases to a full-table
// COUNT(*), and the not-truncated case pays it to print nothing at all. That is
// why the probe carries a deadline (scopeKindNudgeProbeTimeout) instead of
// relying on the query being small: the fix is worth a bounded moment on a path
// that just gave a misleading answer, and never worth a hang.
//
// The probe must also drop the IMPLICIT project label: `--all-types` suppresses
// project scoping (issue beads always carry project:, agent/config beads may
// not), so keeping the label would make the probe itself return 0 and swallow
// the very hint the reader needs. Explicit -l labels are preserved so the note
// can never over-promise rows the suggested command would not surface — and only
// ONE occurrence of the injected label is removed, because a caller may repeat it
// explicitly (`--project X -l project:X`) and stripping both would widen the
// probe past the command being suggested.
//
// Because that means the probe can lift TWO clauses at once, the note never
// asserts which one mattered without proving it: when the project label was also
// dropped, ONE follow-up probe (kind restored, label still off) distinguishes
// "not issue-kind" from "issue-kind, just in other projects" and the message
// names whichever the data supports.
//
// stderr-only, and NOT TTY-gated — unlike the operational nudge, whose probe
// would otherwise cost EVERY plain `kd list` a round-trip. Scripted callers are
// the PRIMARY victims of this footgun (`N=$(kd count -f spawned_by=*)` silently
// yields 0), so TTY-gating would gut the fix.
//
// It fires in EVERY output mode, --json included (ka-0000005220). The note is
// stderr-only, so it never pollutes the bare-array or enveloped JSON on stdout,
// and --json is exactly the mode agents script against — the one place a silent
// false-empty or undercount had no other safety net (`kd list --created-by X
// --json` returned a bare `[]`/short page with nothing on stderr). A caller who
// merges the streams with `2>&1 | jq` still sees a parse error rather than a
// clean-but-wrong answer, which is strictly the safer failure; the correct
// idiom `2>/dev/null | jq` keeps stdout pristine and the note readable
// separately. This supersedes the earlier `--json` suppression, which prized
// the `2>&1 | jq` convenience over the false-answer the note exists to catch.
//
// Best-effort: any transport error is swallowed so the nudge never fails or
// delays the primary answer.
func maybePrintScopeKindNudge(ctx context.Context, req *listRequest, total int) {
	if req == nil || !req.implicitIssueKind {
		return
	}
	hasField := len(req.FieldFilters) > 0
	// Actor-column predicates evaluated server-side (not JSONB fields): the two
	// highest-frequency actor traces a gardener or watchdog runs. --unassigned is
	// deliberately excluded — its matches are overwhelmingly issue-kind, so it
	// does not exhibit the hidden-kind undercount these two do.
	hasActor := req.CreatedBy != "" || req.Assignee != ""
	// Field filters probe only on an empty answer (their footgun is the confident
	// bare 0); actor columns probe whenever set (their footgun is the undercount).
	if !hasActor && !(hasField && total == 0) {
		return
	}
	// A nonzero --offset makes total the COUNT(*) OVER() of the whole match set,
	// but pairing it with "matched N" against a probe that ignores --offset would
	// misread a mid-page position as a truncation. More sharply for the `-f`
	// variant: an empty page past the end leaves total at 0, so firing would print
	// a flatly false note — `kd list -f trigger_id=* --offset 99999999` claims
	// "matched 0" for a predicate that matches 15430 under the reader's own filter.
	// `kd count` never registers --offset, so this only guards `kd list`.
	if req.Offset > 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Bound the probe. Neither the HTTP client (internal/client/http.go builds
	// &http.Client{} with no Timeout) nor the root context (cobra's default is
	// context.Background(), no deadline) caps it, so without this a diagnostic
	// whose result is DISCARDED on any error could hang the CLI indefinitely —
	// and it would hang AFTER stdout already printed the answer, which looks
	// exactly like a wedged command. The deadline makes the docstring's
	// "never fails or delays the primary answer" promise true for latency, not
	// just for errors: on timeout the probe errors, the note is skipped, and the
	// already-correct stdout stands.
	ctx, cancel := context.WithTimeout(ctx, scopeKindNudgeProbeTimeout)
	defer cancel()

	// Re-ask the same predicate with the implicit kind=issue clause (and the
	// implicit project label) dropped — i.e. exactly what --all-types would do.
	probe := *req.ListBeadsRequest
	probe.Kind = nil
	probe.CountOnly = true
	// Limit=1 (not 0) is load-bearing, and only on the gRPC path. Over HTTP the
	// CountOnly fast path ignores Limit/Offset/Sort outright (queries.go: "Limit
	// / Offset / Sort are ignored") so any value is equivalent. Over gRPC,
	// CountOnly is silently dropped — the proto has no count_only field — so the
	// probe becomes a real data query, and the store emits a LIMIT clause only
	// `if filter.Limit > 0`. Limit=0 would therefore mean UNBOUNDED: a probe that
	// materializes and sorts every bead in the database (~577k rows in prod) to
	// answer a one-line diagnostic hint. LIMIT 1 costs one row, and Total is
	// unaffected either way because it comes from COUNT(*) OVER(), which is
	// independent of LIMIT. Offset/Sort are zeroed for the same reason: a
	// caller's --offset would skew a count that must span the whole set.
	probe.Limit, probe.Offset, probe.Sort = 1, 0, ""
	// Drop the label buildListRequest INJECTED (never re-derived here — see
	// listRequest.implicitProjectLabel), keeping the caller's explicit -l labels.
	// slices.Clone is required, not cosmetic: `probe` is a shallow copy, so
	// probe.Labels still aliases req.Labels' backing array and an in-place
	// DeleteFunc would corrupt the request the caller already sent.
	//
	// Exactly ONE occurrence is removed, not every match. buildListRequest
	// appends the injected label and THEN the caller's -l flags, so
	// `--project gardenio -l project:gardenio` yields the label twice; a
	// DeleteFunc would strip the reader's explicit label too and the probe would
	// then count rows the suggested command could never return.
	droppedProjectScope := false
	if req.implicitProjectLabel != "" {
		if i := slices.Index(req.Labels, req.implicitProjectLabel); i >= 0 {
			probe.Labels = slices.Delete(slices.Clone(req.Labels), i, i+1)
			droppedProjectScope = true
		}
	}

	resp, err := beadsClient.ListBeads(ctx, &probe)
	// Fire only when lifting the scope surfaces MORE than the primary query
	// returned — the recount must strictly exceed `total`, not merely be nonzero.
	// For the `-f` variant total is always 0 here, so this reduces to the original
	// "found any rows"; for actor columns it is the undercount test (probe 6 >
	// shown 1) and stays silent when nothing was hidden (probe == total).
	if err != nil || resp == nil || resp.Total <= total {
		return
	}

	// Name the predicate exactly as the caller typed it, so the note is
	// actionable verbatim. Field predicates and actor columns are both included
	// (a query may carry both), sorted for a deterministic line.
	preds := make([]string, 0, len(req.FieldFilters)+2)
	for k, v := range req.FieldFilters {
		preds = append(preds, "-f "+k+"="+v)
	}
	if req.CreatedBy != "" {
		preds = append(preds, "--created-by "+req.CreatedBy)
	}
	if req.Assignee != "" {
		preds = append(preds, "--assignee "+req.Assignee)
	}
	slices.Sort(preds)

	// Name what was DROPPED, never a cause the probe did not isolate. The probe
	// can clear two clauses at once (kind=issue, and the injected project label),
	// so "the matching beads are not issue-kind" would be an outright lie
	// whenever the project label was the real cause: `kd count --project gardenio
	// -f jira_status=*` returns 0 while 2221 beads carry that field, and every
	// one of them is type=task, kind=issue — in other projects. Reporting only
	// the clauses actually lifted keeps the note true in both cases and still
	// points at the same fix, and it also stays true for an ABSENCE predicate
	// (`-f key=`), where the field may not exist anywhere so any claim about
	// where it lives would be wrong.
	hidBy := "the default issue-kind filter"
	remedy := "--all-types"

	// The reported total must be the count the SUGGESTED command returns, not
	// whatever the widest probe happened to find. They differ: `resp.Total` comes
	// from the probe that lifted BOTH clauses, so pairing it with a remedy that
	// lifts only one over-promises — observed on prod, `-f jira_project=*` under
	// --project gardenio promised 1733 while `--all-projects -f jira_project=*`
	// returns 1730 (three of the rows are non-issue-kind). Each branch below
	// therefore reports the total from the probe matching its own remedy.
	reportTotal := resp.Total

	// When project scoping was ALSO lifted, one more count-only probe (kind
	// restored, label still dropped) settles which clause actually mattered, so
	// the note never blames the wrong one.
	if droppedProjectScope {
		hidBy += " + project scoping (" + req.implicitProjectLabel + ")"
		remedy = "--all-types (lifts both)"
		issueOnly := probe
		issueOnly.Kind = req.Kind
		// Blame project scoping only when restoring kind=issue STILL surfaces more
		// than the primary query returned — `r.Total > total`, not merely nonzero.
		// For the `-f` variant total is 0, so this is the original `> 0`; for actor
		// columns it prevents a mis-attribution the false-empty case never hit:
		// `--created-by X` shows 1 issue-kind bead in-project, and --all-projects
		// (kind=issue) also returns 1, so project scoping hid nothing — the note
		// must keep pointing at --all-types (which lifts the kind clause that hid
		// the other 5), not switch to --all-projects on a bare `1 > 0`.
		if r, err := beadsClient.ListBeads(ctx, &issueOnly); err == nil && r != nil && r.Total > total {
			hidBy = "project scoping (" + req.implicitProjectLabel + ")"
			remedy = "--all-projects (the matching beads ARE issue-kind — they are in other projects)"
			// This probe kept kind=issue, exactly as `--all-projects` would.
			reportTotal = r.Total
		}
	}

	// Deliberately NO "or -k <kind>" alternative. An unresolved placeholder is
	// worse than no suggestion: `agent` and `role` — the field names that lead
	// readers here — are bead TYPES, not kinds (only issue/data/config are valid),
	// --kind is not validated, and an explicit -k suppresses this very note. So
	// `-k agent` returns a bare 0 with no second warning: the hint would have
	// walked the reader out of one silent zero into a quieter one. Resolving the
	// kind for them would cost a probe per candidate on a path that may already
	// have paid for a full scan, so the note names the ONE flag it can promise.
	// "matched %d" reports what the reader actually saw (0 for the false-empty
	// `-f` variant, the shown count for the actor undercount), so "matched 1 …
	// but 6 without" reads truthfully in both cases.
	fmt.Fprintf(os.Stderr,
		"note: %s matched %d under %s, but %d without — add %s\n",
		strings.Join(preds, " "), total, hidBy, reportTotal, remedy)
}
