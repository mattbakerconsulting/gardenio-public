// Command gitlab-pipeline-trigger-probe is a TEST-ONLY binary that drives the
// REAL gitlab-pipeline trigger source (internal/bridge/trigger_source_gitlab.go)
// end-to-end through the REAL bridge.TriggerEngine against a STUB GitLab
// httptest server + an in-process mock beads daemon, then asserts that the
// pipeline_ref_pattern field actually GATES firing.
//
// Bead ka-0000002595 (parent ka-0000000958, E2E coverage-gap sprint). Backfills
// the e2e gap left by MR !6601: pipeline_ref_pattern — the optional Go regexp a
// gitlab-pipeline trigger applies to each pipeline ref AFTER the GitLab fetch
// and BEFORE a match is returned — shipped with strong UNIT coverage
// (trigger_source_gitlab_test.go's EvaluatePipelines_RefPattern /
// _InvalidRefPattern, init_gitlab_pipeline_test.go, cmd/gb/trigger_test.go, and
// beads-viewer/triggers_test.go's per-source row render) but NO behavioral e2e
// that drives the whole evaluation→fireAction→recordTriggerAudit loop and proves
// a non-matching ref never fires. The unit tests assert the returned match LIST
// is filtered; this probe asserts the SIDE EFFECT the field exists to prevent —
// a fired trigger (and thus a spawned agent / a filed bead in production) — is
// actually suppressed for a non-matching ref. That negative leg is the one that
// protects against a runaway trigger.
//
// ── Why a from-source probe, not a live-daemon trigger-probe or a live bridge ──
//
// Three independent constraints — each sufficient on its own — rule out the
// obvious alternatives and land on the exact hermetic precedent of
// cmd/gitlab-prov-probe / cmd/escalation-reap-probe (both sibling backfills
// under the same epic):
//
//  1. The deployed trigger-engine does NOT register the gitlab source (see
//     cmd/trigger-engine/main.go — only webhook/schedule/bead/multi/mirror/
//     account). ONLY gitlab-bridge registers NewGitLabTriggerSource, and
//     gitlab-bridge is `enabled: false` in the e2e namespace. So there is no live process evaluating a gitlab-pipeline
//     trigger to observe — the same reason test-gitlab-dark-provenance.sh and
//     test-jira-conversational.sh are probes.
//  2. The gate lives inside GitLabTriggerSource.evaluatePipelines, which needs a
//     live GitLabClient HTTP API. Driving it means standing up a stub GitLab
//     server — but the existing cmd/trigger-probe --source gitlab-pipeline runs
//     EvaluateSource against the shared --http-url daemon, which LoadTriggers
//     scans for EVERY enabled trigger:gitlab bead and evaluates each against the
//     one stub server. Against the shared shard daemon that fans out to
//     unrelated production gitlab triggers and leaves audit beads on the shared
//     store — cross-talk this hermetic probe exists to avoid.
//  3. The whole point of the field is to suppress a firing (a spawned agent in
//     production). Asserting "nothing fired" is only trustworthy when the ONLY
//     triggers in scope are the ones the probe authored — a hermetic mock daemon
//     gives exactly that: LoadTriggers sees ONLY the seeded trigger, and every
//     audit CreateBead is a recorded in-memory call.
//
// So this wires the REAL bridge.GitLabTriggerSource + REAL bridge.TriggerEngine
// (via NewGitLabTriggerSourceWithLogger + NewTriggerEngine + EvaluateSource) —
// the production code path, not a reimplementation — against stub GitLab + mock
// daemon. A refactor that drops the ref-pattern filter, mis-orders it relative
// to the match construction, or stops failing closed on a bad pattern reds this
// probe. The trigger bead itself is built from a real fields map and parsed by
// the production ParseTriggerDef / parseGitLabTriggerData path (LoadTriggers),
// so the pipeline_ref_pattern field round-trip through bead → gitlabTriggerData
// is exercised too.
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
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// canonPattern is the real monorepo case: keep main + merge-train refs, drop
// the .../merge, .../head, and push-branch pipelines that make up the bulk of
// failed-pipeline firings (see the comment on TestGitLabTriggerSource_
// EvaluatePipelines_RefPattern and the --ref-pattern flag help).
const canonPattern = `^(main|refs/merge-requests/[0-9]+/train)$`

// stubPipeline is a minimal pipeline the stub GitLab server returns.
type stubPipeline struct {
	ID     int
	Status string
	Ref    string
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"match-and-negative", runMatchAndNegativeLeg},
		{"all-filtered-suppressed", runAllFilteredLeg},
		{"invalid-pattern-fails-closed", runInvalidPatternLeg},
		{"field-roundtrip", runFieldRoundTripLeg},
	}

	failed := false
	for _, leg := range legs {
		res := leg.run(logger)
		status := "PASS"
		if !res.ok {
			status = "FAIL"
			failed = true
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}
	if failed {
		os.Exit(1)
	}
}

type legResult struct {
	ok     bool
	detail string
}

func pass(format string, a ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, a...)}
}
func fail(format string, a ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, a...)}
}

// stubGitLabServer returns an httptest server that serves the given pipelines
// on the ListPipelines endpoint (GET /api/v4/projects/:id/pipelines). It also
// honors the ?status= filter the source sets, mirroring GitLab, so the probe's
// stub never returns a pipeline whose status the trigger did not ask for.
func stubGitLabServer(pips []stubPipeline) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pipelines") {
			writeJSON(w, []any{})
			return
		}
		wantStatus := r.URL.Query().Get("status")
		out := make([]map[string]any, 0, len(pips))
		for _, p := range pips {
			if wantStatus != "" && p.Status != wantStatus {
				continue
			}
			out = append(out, map[string]any{
				"id":      p.ID,
				"status":  p.Status,
				"ref":     p.Ref,
				"sha":     fmt.Sprintf("sha-%d", p.ID),
				"web_url": fmt.Sprintf("https://gitlab.example/pipeline/%d", p.ID),
			})
		}
		writeJSON(w, out)
	}))
}

// seedPipelineTrigger builds a trigger bead exactly as `gb trigger create
// gitlab-pipeline` would persist it (fields shaped by buildGitLabPipelineFields:
// event_type=pipeline, project_path, pipeline_status, optional
// pipeline_ref_pattern, action=notify), seeds it into the mock daemon, and
// returns its id. last_checked is stamped in the past so the poll-based
// gitlab source bypasses the runtime-bootstrap seed-without-firing guard and
// fires on the very first EvaluateSource pass. CreatedBy is a non-OIDC service
// identity so recheckSpawnAuthzAtFire's OIDC path is bypassed (a notify action
// is not a spawn action anyway) AND so recordTriggerAudit runs (it only records
// for triggers with a non-empty CreatedBy).
func seedPipelineTrigger(daemon *mockDaemon, id, project, refPattern string) {
	fields := map[string]string{
		"event_type":      "pipeline",
		"project_path":    project,
		"pipeline_status": "failed",
		"action":          "notify",
		"prompt":          "e2e pipeline ref-pattern probe {{gitlab_pipeline_ref}}",
		"enabled":         "true",
		"max_concurrent":  "10",
		"last_checked":    time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339),
	}
	if refPattern != "" {
		fields["pipeline_ref_pattern"] = refPattern
	}
	daemon.seedTrigger(&beadsapi.BeadDetail{
		ID:        id,
		Title:     "e2e-gitlab-pipeline-refpattern-" + id,
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:gitlab", "trigger-source:gitlab-pipeline"},
		Fields:    fields,
		CreatedBy: "fleet:gitlab-pipeline-trigger-probe",
	})
}

// driveOneEval wires the REAL source + engine against the stub server and mock
// daemon and runs exactly ONE EvaluateSource("gitlab") pass. Returns the audit
// beads the fire loop produced for trigID (one per FIRED match).
func driveOneEval(logger *slog.Logger, daemon *mockDaemon, serverURL, trigID string) []auditRecord {
	gl := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: serverURL, Token: "probe", Logger: logger})
	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{Daemon: daemon, Logger: logger})
	engine.RegisterSource(bridge.NewGitLabTriggerSourceWithLogger(gl, logger))

	// Belt-and-suspenders bootstrap bypass: seed the trigger's seen-set with a
	// sentinel key so evalOne's runtime-bootstrap branch (LastChecked.IsZero()
	// && !hasSeen) is skipped even if the seeded last_checked were somehow lost.
	// The sentinel is not a real match key, so every real pipeline match is
	// still unseen and fires on this pass. Mirrors cmd/trigger-probe.
	engine.SeedSeen(trigID, "__probe_bootstrap_sentinel__")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	engine.EvaluateSource(ctx, "gitlab")
	return daemon.auditsForTrigger(trigID)
}

// firedRefIDs maps the audit match_keys back to the pipeline IDs that fired.
// A match key is "<project>/pipeline/<id>" (gitlabPipelineToMatch).
func firedPipelineIDs(audits []auditRecord) map[string]bool {
	out := map[string]bool{}
	for _, a := range audits {
		if a.Outcome != "allow" {
			continue
		}
		// take the trailing "/pipeline/<id>" segment as the id
		if i := strings.LastIndex(a.MatchKey, "/pipeline/"); i >= 0 {
			out[a.MatchKey[i+len("/pipeline/"):]] = true
		}
	}
	return out
}

// ── LEG 1: match + negative ─────────────────────────────────────────────────
//
// A mixed ref set with a pipeline_ref_pattern set. Pipelines whose ref matches
// the pattern (main + the two /train refs) MUST fire — each producing one
// allow audit. Pipelines whose ref does NOT match (/merge, /head, a push
// branch) MUST NOT fire — zero audits carry their keys. This is the whole
// contract in one pass: the positive leg proves the pattern does not over-
// suppress, the negative leg proves it does not under-suppress.
func runMatchAndNegativeLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 1, Status: "failed", Ref: "main"},                         // match
		{ID: 2, Status: "failed", Ref: "refs/merge-requests/42/train"}, // match
		{ID: 3, Status: "failed", Ref: "refs/merge-requests/42/merge"}, // NO match
		{ID: 4, Status: "failed", Ref: "refs/merge-requests/42/head"},  // NO match
		{ID: 5, Status: "failed", Ref: "feature/some-branch"},          // NO match
		{ID: 6, Status: "failed", Ref: "refs/merge-requests/99/train"}, // match
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-refpat-mixed"
	seedPipelineTrigger(daemon, trigID, "org/repo", canonPattern)

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)

	wantFire := []string{"1", "2", "6"}
	wantSuppressed := []string{"3", "4", "5"}

	for _, id := range wantFire {
		if !fired[id] {
			return fail("expected pipeline %s (matching ref) to FIRE, but no allow audit carried its key; fired=%v audits=%d", id, sortedKeys(fired), len(audits))
		}
	}
	for _, id := range wantSuppressed {
		if fired[id] {
			return fail("RUNAWAY: pipeline %s (NON-matching ref) FIRED though pipeline_ref_pattern should have suppressed it; fired=%v", id, sortedKeys(fired))
		}
	}
	// Exactly the 3 matching pipelines fired — no more, no fewer.
	allowCount := 0
	for _, a := range audits {
		if a.Outcome == "allow" {
			allowCount++
		}
	}
	if allowCount != len(wantFire) {
		return fail("expected exactly %d allow audits (matching refs), got %d (fired=%v)", len(wantFire), allowCount, sortedKeys(fired))
	}
	// Every audit is the real ledger shape: type=event, action=notify,
	// source=gitlab, and carries the trigger id.
	for _, a := range audits {
		if a.Type != "event" {
			return fail("audit bead type=%q, want event", a.Type)
		}
		if a.Action != "notify" {
			return fail("audit action=%q, want notify", a.Action)
		}
		if a.Fields["source"] != "gitlab" {
			return fail("audit source=%q, want gitlab", a.Fields["source"])
		}
	}
	return pass("matching refs {main,42/train,99/train} fired (%d allow audits); non-matching {42/merge,42/head,feature/*} suppressed", allowCount)
}

// ── LEG 2: all-filtered suppression ─────────────────────────────────────────
//
// A ref set ENTIRELY outside the pattern (only .../merge, .../head, and a push
// branch — every ref the monorepo pattern is designed to drop). NOTHING may
// fire: zero allow audits. This is the runaway-guard leg in its purest form — a
// pattern that under-suppressed by even one ref would spawn a no-op agent in
// production. A control WITHOUT the pattern set on the same ref set proves the
// suppression is due to the pattern and not some unrelated reason nothing fired.
func runAllFilteredLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 10, Status: "failed", Ref: "refs/merge-requests/42/merge"},
		{ID: 11, Status: "failed", Ref: "refs/merge-requests/42/head"},
		{ID: 12, Status: "failed", Ref: "feature/some-branch"},
	}

	// (a) WITH the pattern: nothing may fire.
	serverA := stubGitLabServer(pips)
	defer serverA.Close()
	daemonA := newMockDaemon()
	const trigWith = "ka-refpat-allfiltered"
	seedPipelineTrigger(daemonA, trigWith, "org/repo", canonPattern)
	withAudits := driveOneEval(logger, daemonA, serverA.URL, trigWith)
	withFired := firedPipelineIDs(withAudits)
	if len(withFired) != 0 {
		return fail("RUNAWAY: %d pipeline(s) fired though EVERY ref is outside the pattern: %v", len(withFired), sortedKeys(withFired))
	}

	// (b) CONTROL without the pattern: the SAME refs now all fire, proving the
	// suppression above is caused by the pattern, not an unrelated cause.
	serverB := stubGitLabServer(pips)
	defer serverB.Close()
	daemonB := newMockDaemon()
	const trigWithout = "ka-nopat-control"
	seedPipelineTrigger(daemonB, trigWithout, "org/repo", "") // no ref pattern
	ctrlAudits := driveOneEval(logger, daemonB, serverB.URL, trigWithout)
	ctrlFired := firedPipelineIDs(ctrlAudits)
	if len(ctrlFired) != len(pips) {
		return fail("control (no ref pattern) fired %d/%d pipelines — expected ALL to fire, so the pattern-leg's zero is not a false negative; fired=%v", len(ctrlFired), len(pips), sortedKeys(ctrlFired))
	}

	return pass("all-filtered ref set fired 0 WITH pattern; same refs fired %d/%d WITHOUT it (pattern is the cause)", len(ctrlFired), len(pips))
}

// ── LEG 3: invalid pattern fails closed ─────────────────────────────────────
//
// An unparseable pipeline_ref_pattern must FAIL CLOSED: evaluate returns an
// error, zero matches, and thus zero fires — a typo can never silently reopen
// the spawn firehose. (The GitLab fetch is never even reached — the compile
// happens first — but from the engine's side the observable is simply: no
// audit, no fire.) Asserted end-to-end through the engine, not just the source.
func runInvalidPatternLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 20, Status: "failed", Ref: "main"},
		{ID: 21, Status: "failed", Ref: "refs/merge-requests/1/train"},
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-refpat-invalid"
	seedPipelineTrigger(daemon, trigID, "org/repo", "[unterminated") // invalid regexp

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)
	if len(fired) != 0 {
		return fail("RUNAWAY: an invalid pipeline_ref_pattern fired %d pipeline(s) instead of failing closed: %v", len(fired), sortedKeys(fired))
	}
	// Even the main ref (which WOULD match a well-formed pattern) must not fire.
	if fired["20"] {
		return fail("invalid pattern did not fail closed — the main-ref pipeline fired")
	}
	return pass("invalid pipeline_ref_pattern fired 0 pipelines (fails closed — the spawn firehose stays shut)")
}

// ── LEG 4: field round-trip through the production parse path ────────────────
//
// The pipeline_ref_pattern field must round-trip bead → gitlabTriggerData
// through the production ParseTriggerDef / parseGitLabTriggerData path that
// LoadTriggers runs, so the value the engine filters on is exactly the value
// authored on the bead. We prove this behaviorally: a trigger whose pattern is
// `^main$` fires ONLY the main-ref pipeline out of a set that also contains a
// /train ref the broader canonPattern would have kept — so the specific
// authored pattern (not a default, not the wrong field) is what gated the fire.
func runFieldRoundTripLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 30, Status: "failed", Ref: "main"},                        // matches ^main$
		{ID: 31, Status: "failed", Ref: "refs/merge-requests/7/train"}, // canonPattern would keep; ^main$ drops
		{ID: 32, Status: "failed", Ref: "mainline"},                    // NOT ^main$ (anchored) — must drop
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-refpat-mainonly"
	seedPipelineTrigger(daemon, trigID, "org/repo", `^main$`)

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)

	if !fired["30"] {
		return fail("expected the main-ref pipeline (30) to fire under ^main$, got fired=%v", sortedKeys(fired))
	}
	if fired["31"] {
		return fail("^main$ leaked a /train ref (31) — the authored pattern did not round-trip; a broader/default pattern was used")
	}
	if fired["32"] {
		return fail("^main$ matched 'mainline' — the anchored authored pattern did not round-trip verbatim (leading/trailing anchors lost)")
	}
	if len(fired) != 1 {
		return fail("expected exactly 1 fire under ^main$, got %d: %v", len(fired), sortedKeys(fired))
	}
	return pass("authored pattern ^main$ round-tripped verbatim: only main fired; /train and 'mainline' both dropped")
}

// ── helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeFields unmarshals a CreateBead request's fields JSON into a string map.
// recordTriggerAudit marshals a map[string]string, so this round-trips it.
func decodeFields(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
