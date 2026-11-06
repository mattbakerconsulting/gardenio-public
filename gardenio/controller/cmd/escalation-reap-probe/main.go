// Command escalation-reap-probe is a TEST-ONLY binary that drives the REAL
// escalation auto-reaper (internal/bridge/escalation_reaper.go +
// escalation_review_flag.go) end-to-end against a STUB GitLab httptest server
// and an in-process mock beads daemon, then asserts the writes it made.
//
// Bead ka-0000006112 (parent ka-0000000958). Backfills the e2e gap left by MRs
// !6074 / !6076 / the review-flag follow-up (ka-0000004105, ka-0000007203,
// ka-0000002563): the reaper's three detectors shipped with strong UNIT coverage
// (escalation_reaper_test.go) but NO black-box assertion, and the five
// escalation e2e modules (crud/approval/execution/v2/slack-notify) exercise the
// create/approve/execute/resolve verbs while never once asserting that an
// out-of-band-satisfied escalation gets auto-closed.
//
// ── Why a probe, and not the "add a reaper leg to test-escalation-execution.sh"
//    the bead proposed ─────────────────────────────────────────────────────────
//
// That approach is structurally impossible, for three independent reasons —
// each sufficient on its own:
//
//  1. The reaper lives in gitlab-bridge, and gitlab-bridge is NOT DEPLOYED in
//     the e2e namespace (its values set `gitlabBridge.enabled: false`, and
//     every template is gated on it). There is no process running
//     the sweep to observe — the same constraint that makes
//     test-gitlab-dark-provenance.sh and test-jira-conversational.sh probes.
//  2. The reaper HARD-SKIPS e2e fixtures: evaluateBead's first guard is
//     isE2EEscalation(b.CreatedBy, b.Labels), which matches any
//     createdBy with an "e2e-" prefix. Every escalation the e2e suite creates is
//     authored by KD_ACTOR, which defaults to "e2e-test" — so even with the
//     bridge running, a suite-created escalation is skipped BY DESIGN and could
//     never be reaped. (That guard is itself asserted here, as a leg.)
//  3. gitlab-bridge exposes only /healthz, /readyz, /webhook, and a separate
//     :9092 /metrics — there is no admin or sweep-trigger endpoint, and the
//     sweep interval is 10 minutes, far past a shard's wall-clock budget.
//
// So this follows the EXACT precedent of cmd/gitlab-prov-probe: wire the REAL
// EscalationReaper (via the exported NewEscalationReaper + Run) against stub
// GitLab + mock daemon. Run() sweeps IMMEDIATELY and then blocks on its ticker,
// so a long Interval plus a cancel-after-observation yields exactly one sweep —
// the same trick runBackstopPollLeg uses. This drives the real production code
// path (not a reimplementation), so a refactor that drops the verb allowlist,
// the human-operator gate, or the review flag's never-close rule reds this test.
//
// Each leg prints "LEG <name>: PASS|FAIL <detail>"; a non-zero exit fails the
// shard. The probe is hermetic: no pods, no shared-daemon writes, no network
// beyond loopback httptest.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"merge-verb-reap", runMergeVerbReapLeg},
		{"apply-verb-never-reaped", runApplyVerbNeverReapedLeg},
		{"approve-only-approvals", runApproveOnlyLeg},
		{"orchestration-witness", runOrchestrationLeg},
		{"e2e-fixture-skip", runE2EFixtureSkipLeg},
		{"review-flag-never-closes", runReviewFlagLeg},
		{"idempotent-sweep", runIdempotentSweepLeg},
		{"host-scope-and-dryrun", runHostScopeDryRunLeg},
	}

	failures := 0
	for _, leg := range legs {
		res := leg.run(logger)
		status := "PASS"
		if !res.ok {
			status = "FAIL"
			failures++
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}

	if failures > 0 {
		fmt.Printf("escalation-reap-probe: %d/%d legs FAILED\n", failures, len(legs))
		os.Exit(1)
	}
	fmt.Printf("escalation-reap-probe: all %d legs passed\n", len(legs))
}

// legResult is the outcome of one reaper leg.
type legResult struct {
	ok     bool
	detail string
}

func pass(format string, args ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, args...)}
}

// runSweep drives exactly ONE effective sweep of the REAL reaper and returns
// only once that sweep has PROVABLY completed.
//
// The barrier has to be exact, not a "looks quiet now" heuristic: a heuristic
// that inspects accumulated writes is already satisfied by the PREVIOUS sweep's
// output, so a second runSweep would cancel before doing anything and any
// assertion about re-sweep behaviour (the review flag's one-shot latch, the
// idempotency leg) would pass vacuously. That bug is not hypothetical — it hid a
// seeded mutation that removed the flag's mr_landed latch entirely.
//
// So the barrier keys on the one call the reaper makes exactly once per sweep:
// sweepOnce opens with ListBeadsFiltered. armSweep makes the mock serve the real
// open-set to the FIRST list call and an EMPTY set to every later one, so:
//   - sweep 1 does the real work;
//   - sweeps 2+ are structural no-ops (nothing to evaluate ⇒ no writes);
//   - observing list call #2 PROVES sweep 1 returned, because sweepOnce is
//     synchronous and the ticker cannot re-enter it until it has.
//
// A short interval keeps that proof fast, and the empty-set trick means the
// extra tick can never contaminate what the leg is about to assert.
func runSweep(cfg bridge.EscalationReaperConfig) {
	daemon := cfg.Daemon.(*mockDaemon)
	daemon.armSweep()
	if cfg.Interval == 0 {
		cfg.Interval = 20 * time.Millisecond
	}
	reaper := bridge.NewEscalationReaper(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = reaper.Run(ctx) }()
	waitFor(5*time.Second, func() bool { return daemon.listCallCount() >= 2 })
	cancel()
	<-done
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// newReaperGitLab builds a real GitLabClient pointed at a stub server.
func newReaperGitLab(stubURL string, logger *slog.Logger) *bridge.GitLabClient {
	return bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: stubURL, Token: "stub-token", Logger: logger,
	})
}

// ── LEG 1: merge verb + merged MR ⇒ reaped, with a full audit trail ──────────
//
// The headline behaviour: an `approve_and_merge_mr` ask whose MR an operator
// merged in the GitLab UI is auto-closed, and the close carries the attribution
// that makes it auditable (reaped=true, reaped_signal=mr_state, the merger in
// reaped_satisfier; result_status=success renders the Slack card Complete/green).
// A merge ask whose MR is still `opened` must be left ALONE — the negative
// control that proves the leg discriminates on MR state, not on the verb alone.
func runMergeVerbReapLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	stub.setMR("acme/app", 4242, bridge.GitLabMR{
		IID: 4242, State: "merged", WebURL: "https://gitlab.example.com/acme/app/-/merge_requests/4242",
		MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
		MergedAt: strptr("2026-08-05T04:00:00Z"),
	})
	stub.setMR("acme/app", 77, bridge.GitLabMR{IID: 77, State: "opened"})
	// A CLOSED (declined) MR: the merge ask is moot, so it is reaped too — but
	// nobody merged it, so there is no merger to credit and the audit must fall
	// back to the "out-of-band" actor rather than inventing one.
	stub.setMR("acme/app", 88, bridge.GitLabMR{IID: 88, State: "closed"})

	daemon := newMockDaemon()
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-merged", CreatedBy: "gardenio-crew-a",
		Title: "Merge the release MR",
		Fields: map[string]string{
			"operation_spec": `{"action":"approve_and_merge_mr","project":"acme/app","iid":4242}`,
		},
	})
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-open-mr", CreatedBy: "gardenio-crew-a",
		Title: "Merge the not-yet-merged MR",
		Fields: map[string]string{
			"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":77}`,
		},
	})
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-closed-mr", CreatedBy: "gardenio-crew-a",
		Title: "Merge the MR that got declined",
		Fields: map[string]string{
			"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":88}`,
		},
	})

	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	})

	if !daemon.wasClosed("esc-merged") {
		return fail("merged-MR escalation was NOT reaped (expected auto-close)")
	}
	// The negative control — an unmerged MR must never be reaped.
	if daemon.wasClosed("esc-open-mr") {
		return fail("escalation whose MR is still `opened` WAS reaped — the reaper must only close on a merged/closed MR")
	}
	// Audit trail: every reap must be attributable.
	checks := map[string]string{
		"reaped":        "true",
		"reaped_signal": "mr_state",
		"mr_state":      "merged",
		"reaped_action": "approve_and_merge_mr",
		"result_status": "success",
		// reaped_satisfier carries the out-of-band merger; result_status=success
		// is what routes the closed Slack card to "Complete" (green). The daemon
		// stamps executor itself (P7 Task 10).
		"reaped_satisfier": "matthewbaker",
		"mr_merged_by":     "matthewbaker",
	}
	for k, want := range checks {
		if got := daemon.closeField("esc-merged", k); got != want {
			return fail("close field %s=%q, want %q", k, got, want)
		}
	}
	if daemon.closeField("esc-merged", "reaped_at") == "" {
		return fail("close is missing reaped_at timestamp")
	}
	// The audit COMMENT must name the reaper and the out-of-band satisfaction.
	cs := daemon.commentsFor("esc-merged")
	if len(cs) != 1 {
		return fail("expected exactly 1 audit comment, got %d", len(cs))
	}
	if !strings.Contains(cs[0], "satisfied out-of-band") || !strings.Contains(cs[0], "4242") {
		return fail("audit comment lacks the out-of-band reason / MR ref: %q", truncate(cs[0], 120))
	}
	if authors := daemon.commentAuthors("esc-merged"); len(authors) != 1 || authors[0] != "escalation-reaper" {
		return fail("audit comment author = %v, want [escalation-reaper]", authors)
	}
	// A closed/declined MR also moots the merge ask, so it IS reaped — but with
	// mr_state=closed and no merger to credit, so reaped_satisfier falls back to
	// "out-of-band" instead of naming someone who never merged it.
	if !daemon.wasClosed("esc-closed-mr") {
		return fail("escalation whose MR was CLOSED/declined was not reaped — the merge ask is moot")
	}
	if got := daemon.closeField("esc-closed-mr", "mr_state"); got != "closed" {
		return fail("declined-MR close recorded mr_state=%q, want \"closed\"", got)
	}
	if got := daemon.closeField("esc-closed-mr", "reaped_satisfier"); got != "out-of-band" {
		return fail("declined-MR close recorded reaped_satisfier=%q, want \"out-of-band\" (nobody merged it)", got)
	}
	if cs := daemon.commentsFor("esc-closed-mr"); len(cs) != 1 || !strings.Contains(cs[0], "moot") {
		return fail("declined-MR audit comment should say the ask is moot, got %q", truncate(strings.Join(cs, "|"), 120))
	}
	return pass("merged MR !4242 auto-closed with full audit (signal=mr_state, reaped_satisfier=matthewbaker); declined MR !88 reaped as moot with reaped_satisfier=out-of-band; still-`opened` MR !77 left open")
}

// ── LEG 2: THE load-bearing guard — an apply/deploy verb is NEVER reaped ─────
//
// The reaper's package doc calls this the load-bearing correctness rule, and it
// is the one a naive "MR is merged ⇒ close it" implementation gets wrong. The
// named counterexample is ka-0000001987: it references a MERGED MR, but the ask
// is to `terragrunt apply` the merged code in a prod account — something no MR
// state can attest to. False-reaping it silently drops a live P0, which is
// strictly worse than the phantom it was trying to clear (a phantom wastes
// attention; a false-reap loses work).
//
// Asserted here over the shapes that matter: an explicit apply action, and the
// real ka-0000001987 shape that carries NO `action` field at all — both with a
// genuinely merged MR in play, so only the verb can be doing the discriminating.
func runApplyVerbNeverReapedLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	// Every MR here IS merged — so if any of these get reaped, the verb guard
	// (not the MR state) is what failed.
	for _, iid := range []int{1168, 1169, 1170} {
		stub.setMR("example-org/site-deployment", iid, bridge.GitLabMR{
			IID: iid, State: "merged",
			MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
		})
	}

	daemon := newMockDaemon()
	// (a) An explicit apply verb.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-apply", CreatedBy: "gardenio-crew-b",
		Title: "Apply the merged terraform in prod",
		Fields: map[string]string{
			"operation_spec": `{"action":"terragrunt_apply","project":"example-org/site-deployment","iid":1168}`,
		},
	})
	// (b) The REAL ka-0000001987 shape: operation_type=infrastructure with
	// commands, and NO `action` field — the allowlist must exclude it.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-noaction", CreatedBy: "gardenio-crew-b",
		Title:       "site-deployment!1169 — run terragrunt plan/apply",
		Description: "https://gitlab.example.com/example-org/site-deployment/-/merge_requests/1169",
		Fields: map[string]string{
			"operation_type": "infrastructure",
			"operation_spec": `{"operation_type":"infrastructure","commands":["terragrunt plan","terragrunt apply"],"project":"example-org/site-deployment","iid":1169}`,
		},
	})
	// (c) A mint/provision verb — same class, also excluded.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-mint", CreatedBy: "gardenio-crew-b",
		Title: "Mint a new API credential",
		Fields: map[string]string{
			"operation_spec": `{"action":"mint_secret","project":"example-org/site-deployment","iid":1170}`,
		},
	})

	// ReviewFlag deliberately OFF here so this leg isolates the REAP decision.
	// (The flag's own never-close behaviour is leg 6.)
	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	})

	for _, id := range []string{"esc-apply", "esc-noaction", "esc-mint"} {
		if daemon.wasClosed(id) {
			return fail("%s was FALSE-REAPED: an apply/mint ask referencing a merged MR must NEVER be auto-closed (the ka-0000001987 rule — the apply is the completion, and no MR state attests to it)", id)
		}
		if n := daemon.commentCount(id); n != 0 {
			return fail("%s got %d comment(s); a non-reapable ask must be left untouched by the reap path", id, n)
		}
	}
	return pass("all 3 apply/mint asks with MERGED MRs left OPEN (explicit apply verb, the no-action ka-0000001987 shape, and a mint verb) — the verb allowlist, not MR state, is the discriminator")
}

// ── LEG 3: approve_only reads APPROVALS, never MR state ─────────────────────
//
// The completion criterion depends on the verb, and approve_only is the trap:
// ka-0000000844 explicitly requires its MR to REMAIN draft and unmerged, so a
// state=merged reaper would never close it. The reaper must read the approvals
// endpoint instead. Asserted three ways: an approved-but-unmerged MR IS reaped;
// an unapproved one is NOT; and a Community-Edition 404 (which the client maps
// to a zero-value struct) must NOT read as "approved" — under-reaping is the
// safe direction.
func runApproveOnlyLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	// All three MRs stay `opened` — exactly what the approve-only ask requires.
	for _, iid := range []int{1181, 1182, 1183} {
		stub.setMR("acme/app", iid, bridge.GitLabMR{IID: iid, State: "opened"})
	}
	stub.setApprovals("acme/app", 1181, mrApprovalsJSON{
		ApprovalsRequired: 2, ApprovalsLeft: 0, Approved: true,
		ApprovedBy: []approverJSON{{User: userJSON{Username: "matthewbaker"}}, {User: userJSON{Username: "patrick"}}},
	})
	stub.setApprovals("acme/app", 1182, mrApprovalsJSON{ApprovalsRequired: 2, ApprovalsLeft: 1})
	// 1183 → no approvals entry ⇒ the stub 404s ⇒ the client returns a
	// zero-value struct (the CE case).

	daemon := newMockDaemon()
	for id, iid := range map[string]int{"esc-appr-done": 1181, "esc-appr-pending": 1182, "esc-appr-ce": 1183} {
		daemon.seedEscalation(&beadsapi.BeadDetail{
			ID: id, CreatedBy: "gardenio-crew-c",
			Title: "Approve (do not merge) the draft MR",
			Fields: map[string]string{
				"operation_spec": fmt.Sprintf(`{"action":"approve_only_do_not_merge","project":"acme/app","iid":%d}`, iid),
			},
		})
	}

	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	})

	if !daemon.wasClosed("esc-appr-done") {
		return fail("approve_only ask with approvals_left=0 was NOT reaped (MR is deliberately still `opened`; the approvals endpoint is the signal)")
	}
	if got := daemon.closeField("esc-appr-done", "reaped_signal"); got != "approvals" {
		return fail("reaped_signal=%q, want \"approvals\" (approve_only must not close on mr_state)", got)
	}
	if got := daemon.closeField("esc-appr-done", "approvals_left"); got != "0" {
		return fail("approvals_left=%q, want \"0\"", got)
	}
	if got := daemon.closeField("esc-appr-done", "mr_approved_by"); !strings.Contains(got, "matthewbaker") {
		return fail("mr_approved_by=%q, want it to record the approvers", got)
	}
	if daemon.wasClosed("esc-appr-pending") {
		return fail("approve_only ask with approvals_left=1 WAS reaped — it is not yet approved")
	}
	if daemon.wasClosed("esc-appr-ce") {
		return fail("approve_only ask on a Community-Edition 404 (approvals_required=0) WAS reaped — a missing approvals endpoint must never read as approved")
	}
	// The verb must pick the RIGHT endpoint: approve_only never GETs the MR.
	if n := stub.mrGets("acme/app", 1181); n != 0 {
		return fail("approve_only ask hit the MR endpoint %d time(s); it must read approvals only", n)
	}
	if n := stub.approvalGets("acme/app", 1181); n == 0 {
		return fail("approve_only ask never hit the approvals endpoint")
	}
	return pass("approve_only reaped on approvals_left=0 while its MR stayed `opened` (ka-0000000844); unapproved + CE-404 left open; MR endpoint never touched")
}

// ── LEG 4: the orchestration-layer witness (ka-0000007203) ──────────────────
//
// The second detector reaps on a witness the operator left ON THE BEAD, with no
// GitLab call at all — so it covers the apply/deploy asks the verb guard
// refuses. Two witnesses, and two safety rules that matter more than either:
//   - result_status=failure must NEVER reap (a failed ask is unfinished work;
//     closing it buries it);
//   - an AGENT comment saying "resolved out of band" must NEVER reap. Agents
//     file and comment on escalations constantly; only a HUMAN operator's
//     directive is authoritative.
func runOrchestrationLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	daemon := newMockDaemon()

	// (a) Terminal result_status on a STILL-OPEN escalation — the close half
	// never landed. Note the apply verb: no MR state could ever attest to this,
	// which is exactly why the orchestration detector exists.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-rs-success", CreatedBy: "gardenio-crew-d",
		Title: "Apply prod terraform",
		Fields: map[string]string{
			"operation_spec": `{"action":"terragrunt_apply"}`,
			"result_status":  "success",
			"executor":       "matthewbaker",
		},
	})
	// (b) result_status=failure — the load-bearing NEGATIVE.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-rs-failure", CreatedBy: "gardenio-crew-d",
		Title:  "Apply prod terraform (failed)",
		Fields: map[string]string{"operation_spec": `{"action":"terragrunt_apply"}`, "result_status": "failure"},
	})
	// (c) A HUMAN operator resolve-comment. The witness gates on the comment
	// AUTHOR being a human shape (typed slack:/oidc: id or a bare email), not on
	// operated_by (ka-0000002488) — an agent carries a human operated_by too, so
	// keying on operated_by let an agent comment reap. A real human comment lands
	// with the operator's own id in Author; "slack-bridge" is the bot forwarder,
	// not a human, and must NOT satisfy the witness.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-op-comment", CreatedBy: "gardenio-crew-d",
		Title:  "Grant IAM access",
		Fields: map[string]string{"operation_spec": `{"action":"grant_iam"}`},
	})
	daemon.seedComments("esc-op-comment", beadsapi.Comment{
		Author: "matthew.baker@acme.example", OperatedBy: "slack:U0OPERATOR1",
		Text: "resolved out of band — granted it in the console",
	})
	// (d) An AGENT comment with the SAME directive — must NOT reap.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-agent-comment", CreatedBy: "gardenio-crew-d",
		Title:  "Grant IAM access (agent claims done)",
		Fields: map[string]string{"operation_spec": `{"action":"grant_iam"}`},
	})
	daemon.seedComments("esc-agent-comment", beadsapi.Comment{
		Author: "gardenio-crew-zz", Text: "resolved out of band, I handled it",
	})
	// (e) A human operator commenting WITHOUT a resolve directive — mid-discussion
	// chatter must not fire the detector.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-op-chatter", CreatedBy: "gardenio-crew-d",
		Title:  "Grant IAM access (under discussion)",
		Fields: map[string]string{"operation_spec": `{"action":"grant_iam"}`},
	})
	daemon.seedComments("esc-op-chatter", beadsapi.Comment{
		Author: "slack:U0OPERATOR1", Text: "should I reap this?",
	})

	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	})

	if !daemon.wasClosed("esc-rs-success") {
		return fail("escalation with result_status=success on a still-open bead was NOT reaped")
	}
	if got := daemon.closeField("esc-rs-success", "reaped_signal"); got != "result_status" {
		return fail("reaped_signal=%q, want \"result_status\"", got)
	}
	if got := daemon.closeField("esc-rs-success", "reaped_orchestration"); got != "true" {
		return fail("reaped_orchestration=%q, want \"true\"", got)
	}
	if daemon.wasClosed("esc-rs-failure") {
		return fail("result_status=FAILURE was reaped — a failed ask is unfinished work; auto-closing it buries it")
	}
	if !daemon.wasClosed("esc-op-comment") {
		return fail("human-operator resolve-comment did NOT reap the escalation")
	}
	if got := daemon.closeField("esc-op-comment", "reaped_signal"); got != "operator_comment" {
		return fail("reaped_signal=%q, want \"operator_comment\"", got)
	}
	if daemon.wasClosed("esc-agent-comment") {
		return fail("an AGENT comment saying \"resolved out of band\" REAPED the escalation — only a human operator's directive is authoritative")
	}
	if daemon.wasClosed("esc-op-chatter") {
		return fail("an operator comment with no resolve directive (\"should I reap this?\") reaped the escalation")
	}
	// The field witness must short-circuit before paying for a comment fetch.
	if n := daemon.commentFetches("esc-rs-success"); n != 0 {
		return fail("result_status witness still fetched comments %d time(s); it must short-circuit", n)
	}
	// The orchestration path runs with NO GitLab call at all.
	if n := stub.totalGets(); n != 0 {
		return fail("orchestration detector made %d GitLab call(s); it must need none", n)
	}
	return pass("result_status={success} reaped / {failure} spared; human-operator directive reaped; agent \"resolved out of band\" + operator chatter both spared; zero GitLab calls")
}

// ── LEG 5: e2e fixtures are skipped ─────────────────────────────────────────
//
// evaluateBead's FIRST guard. It is why this whole module is a probe rather
// than a live-bridge test, so it is worth pinning directly: an escalation
// authored by an "e2e-" actor, or labelled e2e:true, must never be reaped
// against the real GitLab API — even when every other signal says reap. The
// createdBy value here is the literal default KD_ACTOR the e2e suite uses.
func runE2EFixtureSkipLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	stub.setMR("acme/app", 900, bridge.GitLabMR{
		IID: 900, State: "merged", MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
	})

	daemon := newMockDaemon()
	// The exact shape the escalation e2e scripts create (KD_ACTOR=e2e-test).
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-e2e-actor", CreatedBy: "e2e-test",
		Title:  "e2e fixture — merged MR",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":900}`},
	})
	// The label-based fixture marker.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-e2e-label", CreatedBy: "gardenio-crew-e", Labels: []string{"e2e:true"},
		Title:  "e2e fixture by label — merged MR",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":900}`},
	})
	// A fixture carrying the strongest possible orchestration witness — still skipped.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-e2e-rs", CreatedBy: "e2e-agent-v2",
		Title:  "e2e fixture with terminal result_status",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr"}`, "result_status": "success"},
	})
	// A REAL escalation alongside them, to prove the sweep ran at all — without
	// this, an inert sweep would pass the leg vacuously.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-real", CreatedBy: "gardenio-crew-e",
		Title:  "real ask — merged MR",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":900}`},
	})

	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	})

	for _, id := range []string{"esc-e2e-actor", "esc-e2e-label", "esc-e2e-rs"} {
		if daemon.wasClosed(id) {
			return fail("%s was reaped — e2e fixtures must be skipped unconditionally (isE2EEscalation)", id)
		}
	}
	// The positive control: the sweep DID work, so the skips above are real.
	if !daemon.wasClosed("esc-real") {
		return fail("the non-fixture control escalation was not reaped — the sweep did not run, so the fixture skips prove nothing")
	}
	return pass("e2e fixtures skipped by actor prefix (e2e-test / e2e-agent-v2) and by e2e:true label, even with a merged MR + terminal result_status; the non-fixture control WAS reaped")
}

// ── LEG 6: the review flag flips to needs-review and NEVER closes ───────────
//
// The third detector (ka-0000002563) is the complement to the reaper: for a
// merge-THEN-apply ask whose MR has landed, it must surface ONE actionable
// signal without closing the bead — because the apply step may still stand.
// The never-close property is the whole point, and the one-shot latch is what
// stops the 28-nag stale re-notify the deep-dive measured.
func runReviewFlagLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	stub.setMR("example-org/site-deployment", 1168, bridge.GitLabMR{
		IID: 1168, State: "merged",
		WebURL:   "https://gitlab.example.com/example-org/site-deployment/-/merge_requests/1168",
		MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
	})
	stub.setMR("example-org/site-deployment", 1200, bridge.GitLabMR{IID: 1200, State: "opened"})

	daemon := newMockDaemon()
	// A merge-then-apply ask whose MR has now merged — bang-reference shape.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-flag", CreatedBy: "gardenio-crew-f",
		Title: "Merge example-org/site-deployment!1168 then terragrunt apply",
		Fields: map[string]string{
			"operation_type": "infrastructure",
			"operation_spec": `{"operation_type":"infrastructure","commands":["terragrunt apply"],"project":"example-org/site-deployment","iid":1168}`,
		},
	})
	// Same shape but the MR has NOT landed — must not be flagged.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-noflag", CreatedBy: "gardenio-crew-f",
		Title: "Merge example-org/site-deployment!1200 then terragrunt apply",
		Fields: map[string]string{
			"operation_spec": `{"commands":["terragrunt apply"],"project":"example-org/site-deployment","iid":1200}`,
		},
	})

	cfg := bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
		ReviewFlag: true,
	}
	runSweep(cfg)

	// THE rule: flagged, never closed.
	if daemon.wasClosed("esc-flag") {
		return fail("the review flag CLOSED a merge-then-apply escalation — it must only flag; the apply step may still be outstanding")
	}
	f := daemon.fieldsFor("esc-flag")
	if f["mr_landed"] != "true" {
		return fail("mr_landed=%q, want \"true\" (the needs-review latch)", f["mr_landed"])
	}
	if f["mr_landed_state"] != "merged" || f["mr_landed_iid"] != "1168" {
		return fail("mr_landed_state=%q iid=%q, want merged/1168", f["mr_landed_state"], f["mr_landed_iid"])
	}
	// Exactly ONE actionable pingback replaces the endless stale nag.
	if f["pingback_source"] != "review-flag" {
		return fail("pingback_source=%q, want \"review-flag\"", f["pingback_source"])
	}
	if f["pingback_count"] != "1" {
		return fail("pingback_count=%q, want \"1\"", f["pingback_count"])
	}
	if labels := daemon.labelsFor("esc-flag"); len(labels) != 1 || labels[0] != "needs-review" {
		return fail("labels=%v, want [needs-review]", labels)
	}
	// The unmerged twin must be untouched.
	if len(daemon.fieldsFor("esc-noflag")) != 0 || daemon.wasClosed("esc-noflag") {
		return fail("an escalation whose MR is still `opened` was flagged/closed")
	}

	// ONE-SHOT LATCH: a second sweep must not re-flag, re-comment, or re-nag —
	// that is what actually stops the stale re-notify loop.
	updatesAfterFirst := daemon.updateCount("esc-flag")
	commentsAfterFirst := daemon.commentCount("esc-flag")
	runSweep(cfg)
	if n := daemon.updateCount("esc-flag"); n != updatesAfterFirst {
		return fail("second sweep re-flagged the bead (%d→%d field patches); mr_landed must latch it", updatesAfterFirst, n)
	}
	if n := daemon.commentCount("esc-flag"); n != commentsAfterFirst {
		return fail("second sweep re-commented (%d→%d); the flag is one-shot", commentsAfterFirst, n)
	}
	if daemon.wasClosed("esc-flag") {
		return fail("second sweep CLOSED the flagged escalation")
	}
	return pass("merged MR flipped the merge-then-apply ask to needs-review (mr_landed + 1 pingback + label) WITHOUT closing it; unmerged twin untouched; second sweep re-flagged nothing (one-shot latch)")
}

// ── LEG 7: the sweep is idempotent ──────────────────────────────────────────
//
// A reaped bead is closed, so it drops out of the next sweep's open-set query
// and is never re-processed or double-commented. Belt-and-suspenders: a bead
// already stamped reaped=true is skipped even if it somehow reappears open —
// asserted directly, since the sweep-set property alone would hide a regression
// in that guard.
func runIdempotentSweepLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	stub.setMR("acme/app", 555, bridge.GitLabMR{
		IID: 555, State: "merged", MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
	})

	daemon := newMockDaemon()
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-idem", CreatedBy: "gardenio-crew-g",
		Title:  "Merge MR !555",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":555}`},
	})
	// An ALREADY-reaped bead left open (the guard's own scenario).
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-already-reaped", CreatedBy: "gardenio-crew-g",
		Title: "Merge MR !555 (already reaped)",
		Fields: map[string]string{
			"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":555}`,
			"reaped":         "true",
		},
	})

	cfg := bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
	}
	runSweep(cfg)
	if !daemon.wasClosed("esc-idem") {
		return fail("first sweep did not reap the merged-MR escalation")
	}
	if daemon.wasClosed("esc-already-reaped") {
		return fail("a bead already stamped reaped=true was re-processed and closed again")
	}
	if n := daemon.commentCount("esc-already-reaped"); n != 0 {
		return fail("a reaped=true bead got %d comment(s)", n)
	}

	commentsAfterFirst := daemon.commentCount("esc-idem")
	getsAfterFirst := stub.mrGets("acme/app", 555)
	runSweep(cfg)
	if n := daemon.commentCount("esc-idem"); n != commentsAfterFirst {
		return fail("second sweep double-commented the reaped bead (%d→%d)", commentsAfterFirst, n)
	}
	// A closed bead leaves the open-set, so the second sweep must not even
	// re-query GitLab for it — the property that keeps the 10-min sweep cheap.
	if n := stub.mrGets("acme/app", 555); n != getsAfterFirst {
		return fail("second sweep re-queried GitLab for an already-closed escalation (%d→%d GETs)", getsAfterFirst, n)
	}
	return pass("reaped bead drops out of the open-set: no double-close, no double-comment, no repeat GitLab GET; a reaped=true bead left open is skipped by the guard")
}

// ── LEG 8: the dual-GitLab host guard, and dry-run ──────────────────────────
//
// Two operator-facing safety valves that share a leg because both are about the
// reaper NOT acting.
//
// HOST SCOPE: one beads daemon can be shared by a dark-pointed and an
// upstream-pointed bridge. A reaper scoped to host A must never GET a host-B MR
// against its own API — it would be querying the wrong GitLab with the wrong
// token, and a coincidentally-merged IID over there could reap a live ask over
// here. The guard applies only when the ref carries a host (i.e. came from a
// full URL); a structured project+iid ref has none and is always in-scope, so
// both directions are asserted.
//
// DRY-RUN: the flag exists so an operator can watch what the sweep WOULD close
// before enabling closes. If dry-run ever closed anything it would be actively
// dangerous — the operator explicitly asked it not to.
func runHostScopeDryRunLeg(logger *slog.Logger) legResult {
	stub := newGitLabStub()
	defer stub.Close()
	stub.setMR("acme/app", 700, bridge.GitLabMR{
		IID: 700, State: "merged", MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
	})

	// ── host scope ──
	daemon := newMockDaemon()
	// A FOREIGN-host ref (full URL on gitlab-dark) — must be skipped entirely.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-foreign", CreatedBy: "gardenio-crew-h",
		Title: "Merge the dark-host MR",
		Fields: map[string]string{
			"operation_spec": `{"action":"merge_mr","mr_url":"https://gitlab-dark.example.com/acme/app/-/merge_requests/700"}`,
		},
	})
	// A structured project+iid ref carries NO host → always in scope.
	daemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-hostless", CreatedBy: "gardenio-crew-h",
		Title: "Merge the MR (structured ref, no host)",
		Fields: map[string]string{
			"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":700}`,
		},
	})
	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(stub.URL(), logger), Daemon: daemon, Logger: logger,
		Host: "gitlab.example.com",
	})
	if daemon.wasClosed("esc-foreign") {
		return fail("a FOREIGN-host escalation was reaped — a host-scoped reaper must never resolve another GitLab's MR against its own API")
	}
	if stub.totalGets() != 1 {
		return fail("expected exactly 1 GitLab GET (the in-scope ref only), got %d — the foreign-host ref must be skipped BEFORE any API call", stub.totalGets())
	}
	if !daemon.wasClosed("esc-hostless") {
		return fail("a hostless structured project+iid ref was skipped; it carries no host and is always in scope")
	}

	// ── dry-run ──
	dryStub := newGitLabStub()
	defer dryStub.Close()
	dryStub.setMR("acme/app", 701, bridge.GitLabMR{
		IID: 701, State: "merged", MergedBy: &bridge.GitLabUser{Username: "matthewbaker"},
	})
	dryDaemon := newMockDaemon()
	dryDaemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-dry", CreatedBy: "gardenio-crew-h",
		Title:  "Merge MR !701 (dry-run)",
		Fields: map[string]string{"operation_spec": `{"action":"merge_mr","project":"acme/app","iid":701}`},
	})
	// A merge-then-apply ask too: dry-run must suppress the review FLAG's writes
	// as well, not just the reap's close.
	dryDaemon.seedEscalation(&beadsapi.BeadDetail{
		ID: "esc-dry-flag", CreatedBy: "gardenio-crew-h",
		Title: "Merge acme/app!701 then apply (dry-run)",
		Fields: map[string]string{
			"operation_spec": `{"commands":["terragrunt apply"],"project":"acme/app","iid":701}`,
		},
	})
	runSweep(bridge.EscalationReaperConfig{
		GitLab: newReaperGitLab(dryStub.URL(), logger), Daemon: dryDaemon, Logger: logger,
		DryRun: true, ReviewFlag: true,
	})
	if dryDaemon.wasClosed("esc-dry") {
		return fail("DRY-RUN closed an escalation — it must only log what it would do")
	}
	if n := dryDaemon.commentCount("esc-dry"); n != 0 {
		return fail("DRY-RUN wrote %d audit comment(s); it must not mutate the bead", n)
	}
	if n := dryDaemon.updateCount("esc-dry-flag"); n != 0 {
		return fail("DRY-RUN stamped %d review-flag field patch(es); dry-run must suppress the flag's writes too", n)
	}
	if l := dryDaemon.labelsFor("esc-dry-flag"); len(l) != 0 {
		return fail("DRY-RUN added labels %v", l)
	}
	// The check itself still runs in dry-run — that is the point of the mode.
	if dryStub.totalGets() == 0 {
		return fail("DRY-RUN made no GitLab call; it must still CHECK, just not act")
	}
	return pass("foreign-host ref skipped with zero API calls while the hostless structured ref reaped; dry-run checked GitLab but closed/commented/flagged/labelled nothing")
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub GitLab: an httptest server serving the two endpoints the reaper reads —
// GET /api/v4/projects/:path/merge_requests/:iid and .../approvals — and
// recording every call so a leg can assert the verb picked the RIGHT endpoint.
// An unregistered MR 404s (the real client maps an approvals 404 to a
// zero-value struct: the Community-Edition case).
// ─────────────────────────────────────────────────────────────────────────────

type gitlabStub struct {
	srv *httptest.Server
	mu  sync.Mutex

	mrs       map[string]bridge.GitLabMR
	approvals map[string]mrApprovalsJSON
	mrCalls   map[string]int
	apprCalls map[string]int
}

// mrApprovalsJSON mirrors the subset of the approvals payload the real client
// decodes (gitlab_mr_stats.go mrApprovalsResponse).
type mrApprovalsJSON struct {
	Approved          bool           `json:"approved"`
	ApprovalsRequired int            `json:"approvals_required"`
	ApprovalsLeft     int            `json:"approvals_left"`
	ApprovedBy        []approverJSON `json:"approved_by"`
}

type approverJSON struct {
	User userJSON `json:"user"`
}

type userJSON struct {
	Username string `json:"username"`
}

func newGitLabStub() *gitlabStub {
	s := &gitlabStub{
		mrs:       map[string]bridge.GitLabMR{},
		approvals: map[string]mrApprovalsJSON{},
		mrCalls:   map[string]int{},
		apprCalls: map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *gitlabStub) URL() string { return s.srv.URL }
func (s *gitlabStub) Close()      { s.srv.Close() }

func stubKey(project string, iid int) string { return project + "!" + strconv.Itoa(iid) }

func (s *gitlabStub) setMR(project string, iid int, mr bridge.GitLabMR) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mrs[stubKey(project, iid)] = mr
}

func (s *gitlabStub) setApprovals(project string, iid int, a mrApprovalsJSON) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals[stubKey(project, iid)] = a
}

func (s *gitlabStub) mrGets(project string, iid int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mrCalls[stubKey(project, iid)]
}

func (s *gitlabStub) approvalGets(project string, iid int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apprCalls[stubKey(project, iid)]
}

// totalGets is every call the stub saw, across both endpoints.
func (s *gitlabStub) totalGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.mrCalls {
		n += c
	}
	for _, c := range s.apprCalls {
		n += c
	}
	return n
}

// handle routes /api/v4/projects/<url-escaped path>/merge_requests/<iid>[/approvals].
func (s *gitlabStub) handle(w http.ResponseWriter, r *http.Request) {
	project, iid, isApprovals, ok := parseStubPath(r.URL.EscapedPath())
	if !ok {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
		return
	}
	key := stubKey(project, iid)

	s.mu.Lock()
	if isApprovals {
		s.apprCalls[key]++
	} else {
		s.mrCalls[key]++
	}
	appr, hasAppr := s.approvals[key]
	mr, hasMR := s.mrs[key]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if isApprovals {
		if !hasAppr {
			// The Premium-only endpoint is absent on CE → 404, which the real
			// client maps to a zero-value struct.
			http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(appr)
		return
	}
	if !hasMR {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(mr)
}

// parseStubPath pulls (project, iid, isApprovals) out of an API path. The
// project segment is URL-escaped by the client (url.PathEscape), so "/" arrives
// as %2F; EscapedPath keeps it that way and we unescape just that segment.
func parseStubPath(p string) (project string, iid int, isApprovals bool, ok bool) {
	const prefix = "/api/v4/projects/"
	rest, found := strings.CutPrefix(p, prefix)
	if !found {
		return "", 0, false, false
	}
	proj, rest, found := strings.Cut(rest, "/merge_requests/")
	if !found {
		return "", 0, false, false
	}
	if a, found := strings.CutSuffix(rest, "/approvals"); found {
		rest, isApprovals = a, true
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return "", 0, false, false
	}
	decoded := strings.ReplaceAll(proj, "%2F", "/")
	return strings.ReplaceAll(decoded, "%2f", "/"), n, isApprovals, true
}

func strptr(s string) *string { return &s }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
