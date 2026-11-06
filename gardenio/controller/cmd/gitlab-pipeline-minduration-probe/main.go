// Command gitlab-pipeline-minduration-probe is a TEST-ONLY binary that drives the
// REAL gitlab-pipeline trigger source (internal/bridge/trigger_source_gitlab.go)
// end-to-end through the REAL bridge.TriggerEngine against a STUB GitLab
// httptest server + an in-process mock beads daemon, then asserts that the
// min_duration ("slow pipeline") field actually GATES firing.
//
// Bead ka-0000001474 (parent ka-0000000958, E2E coverage-gap sprint). Backfills
// the e2e gap MR left open on min_duration — the optional Go-duration threshold a
// gitlab-pipeline trigger applies to each candidate pipeline's RUN DURATION AFTER
// the status/ref filters and BEFORE a match is returned, so the fleet wakes only
// on a pipeline that got SLOW (not merely on every pipeline). It shipped with
// UNIT coverage (parseMinDuration + the evaluatePipelines duration gate in
// trigger_source_gitlab_test.go, the --min-duration authoring path in
// cmd/gb/trigger_test.go, the "≥<dur>" render in beads-viewer/triggers_test.go)
// but NO behavioral e2e that drives the whole evaluation→resolvePipelineDuration→
// fireAction→recordTriggerAudit loop and proves BOTH failure directions:
//   - a pipeline BELOW the threshold fires NOTHING (a regression that inverts the
//     comparison, or drops the gate, turns a slow-pipeline trigger into one that
//     fires on EVERY pipeline — a runaway agent-spawn firehose);
//   - a pipeline with UNKNOWN/null duration (still running, skipped, or a
//     detail-fetch failure) fires NOTHING (fail closed — a transient API error
//     must never manufacture a spurious match).
// The unit tests assert the returned match LIST is filtered; this probe asserts
// the SIDE EFFECT the field exists to control — a fired trigger (and thus a
// spawned agent / a filed bead in production) — is actually suppressed for a
// too-fast or unknown-duration pipeline, and DOES happen for a slow one.
//
// ── Why a from-source probe, not a live-daemon trigger-probe or a live bridge ──
//
// The SAME three constraints that landed cmd/gitlab-pipeline-trigger-probe (the
// sibling pipeline_ref_pattern backfill under this epic) on a hermetic from-
// source probe apply verbatim here — they are re-stated so this file stands
// alone:
//
//  1. The deployed trigger-engine does NOT register the gitlab source (see
//     cmd/trigger-engine/main.go — only webhook/schedule/bead/multi/mirror/
//     account). ONLY gitlab-bridge registers NewGitLabTriggerSource, and
//     gitlab-bridge is `enabled: false` in the e2e namespace.
//     So there is no live process evaluating a gitlab-pipeline trigger to
//     observe — the same reason test-gitlab-dark-provenance.sh and
//     test-jira-conversational.sh are probes.
//  2. The gate lives inside GitLabTriggerSource.evaluatePipelines +
//     resolvePipelineDuration, which need a live GitLabClient HTTP API (the
//     min_duration gate in particular MUST hit the single-pipeline DETAIL
//     endpoint — GitLab's LIST endpoint omits `duration`). Driving it means
//     standing up a stub GitLab server; the existing cmd/trigger-probe --source
//     gitlab-pipeline runs against the shared --http-url daemon, which
//     LoadTriggers scans for EVERY enabled trigger:gitlab bead and fans out to
//     unrelated production gitlab triggers, leaving audit beads on the shared
//     store.
//  3. The whole point of the field is to suppress a firing (a spawned agent in
//     production). Asserting "nothing fired" is only trustworthy when the ONLY
//     triggers in scope are the ones the probe authored — a hermetic mock daemon
//     gives exactly that: LoadTriggers sees ONLY the seeded trigger, and every
//     audit CreateBead is a recorded in-memory call.
//
// ── Fixture fidelity: LIST omits duration, DETAIL carries it ─────────────────
//
// The stub GitLab server mirrors the shape prod actually EMITS (ka-0000001521):
// the LIST pipelines response NEVER carries `duration` (GitLab's list endpoint
// omits it), so every leg exercises the REAL per-candidate DETAIL fetch
// (resolvePipelineDuration → GetPipelineByPath). Emitting duration on the list
// would silently take the resolvePipelineDuration fast path and prove nothing
// about the code path prod runs. The DETAIL endpoint carries the authoritative
// duration — or null, or an HTTP 500 — per fixture, so the null/unknown and
// detail-fetch-failure fail-closed branches are both exercised for real.
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// canonThreshold is the real monorepo shape: a merge train that normally runs in
// well under an hour but has gone SLOW. The gate fires only on candidates whose
// run duration is at/over this. (The --min-duration flag help uses the same
// register — "e.g. 45m".)
const canonThreshold = "30m"

// detailPathRe matches the single-pipeline DETAIL endpoint
// (GET /api/v4/projects/:id/pipelines/:pipeline_id) so the stub can tell a
// DETAIL fetch from a LIST fetch (…/pipelines with no trailing id). The project
// path segment decodes to org/repo in r.URL.Path, so anchor on the trailing id.
var detailPathRe = regexp.MustCompile(`/pipelines/(\d+)$`)

// stubPipeline is a minimal pipeline the stub GitLab server returns. The LIST
// response carries id/status/ref/sha/web_url but NEVER duration (prod-accurate).
// The DETAIL response carries the run duration this fixture models:
//   - DetailDurationSecs non-nil  → DETAIL returns that duration (seconds).
//   - DetailDurationSecs nil       → DETAIL returns "duration": null (unknown /
//     still-running / skipped) — the fail-closed branch.
//   - DetailError true             → DETAIL returns HTTP 500 (a transient fetch
//     failure) — the other fail-closed branch. Overrides DetailDurationSecs.
type stubPipeline struct {
	ID                 int
	Status             string
	Ref                string
	DetailDurationSecs *float64
	DetailError        bool
}

func secs(v float64) *float64 { return &v }

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"over-and-under", runOverAndUnderLeg},
		{"all-under-suppressed", runAllUnderLeg},
		{"unknown-duration-fails-closed", runUnknownDurationLeg},
		{"invalid-min-duration-fails-closed", runInvalidMinDurationLeg},
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

// stubGitLabServer returns an httptest server serving the given pipelines. A
// LIST request (…/pipelines) returns the status-filtered set WITHOUT duration
// (GitLab's list endpoint omits it — the reason the gate MUST detail-fetch). A
// DETAIL request (…/pipelines/:id) returns that pipeline's duration, a null
// duration, or an HTTP 500 per its stubPipeline fixture.
func stubGitLabServer(pips []stubPipeline) *httptest.Server {
	byID := make(map[int]stubPipeline, len(pips))
	for _, p := range pips {
		byID[p.ID] = p
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DETAIL first: …/pipelines/<id>.
		if m := detailPathRe.FindStringSubmatch(r.URL.Path); m != nil {
			id, _ := strconv.Atoi(m[1])
			p, ok := byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if p.DetailError {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"stub detail error"}`))
				return
			}
			detail := map[string]any{
				"id":      p.ID,
				"status":  p.Status,
				"ref":     p.Ref,
				"sha":     fmt.Sprintf("sha-%d", p.ID),
				"web_url": fmt.Sprintf("https://gitlab.example/pipeline/%d", p.ID),
				// duration: the authoritative run seconds, or null when unknown.
				"duration": p.DetailDurationSecs,
			}
			writeJSON(w, detail)
			return
		}
		// LIST: …/pipelines (no trailing id). Honor ?status= like GitLab, and
		// NEVER include duration (prod-accurate — the list omits it).
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
// gitlab-pipeline --min-duration` would persist it (fields shaped by
// buildGitLabPipelineFields: event_type=pipeline, project_path, pipeline_status,
// optional min_duration, action=notify), seeds it into the mock daemon, and
// returns via the daemon. last_checked is stamped in the past so the poll-based
// gitlab source bypasses the runtime-bootstrap seed-without-firing guard and
// fires on the very first EvaluateSource pass. CreatedBy is a non-OIDC service
// identity so recheckSpawnAuthzAtFire's OIDC path is bypassed (a notify action
// is not a spawn action anyway) AND so recordTriggerAudit runs (it only records
// for triggers with a non-empty CreatedBy).
func seedPipelineTrigger(daemon *mockDaemon, id, project, minDuration string) {
	fields := map[string]string{
		"event_type":      "pipeline",
		"project_path":    project,
		"pipeline_status": "failed",
		"action":          "notify",
		"prompt":          "e2e pipeline min_duration probe {{gitlab_pipeline_ref}}",
		"enabled":         "true",
		"max_concurrent":  "10",
		"last_checked":    time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339),
	}
	if minDuration != "" {
		fields["min_duration"] = minDuration
	}
	daemon.seedTrigger(&beadsapi.BeadDetail{
		ID:        id,
		Title:     "e2e-gitlab-pipeline-minduration-" + id,
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:gitlab", "trigger-source:gitlab-pipeline"},
		Fields:    fields,
		CreatedBy: "fleet:gitlab-pipeline-minduration-probe",
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

// firedPipelineIDs maps the audit match_keys back to the pipeline IDs that fired.
// A match key is "<project>/pipeline/<id>" (gitlabPipelineToMatch).
func firedPipelineIDs(audits []auditRecord) map[string]bool {
	out := map[string]bool{}
	for _, a := range audits {
		if a.Outcome != "allow" {
			continue
		}
		if i := strings.LastIndex(a.MatchKey, "/pipeline/"); i >= 0 {
			out[a.MatchKey[i+len("/pipeline/"):]] = true
		}
	}
	return out
}

func allowCount(audits []auditRecord) int {
	n := 0
	for _, a := range audits {
		if a.Outcome == "allow" {
			n++
		}
	}
	return n
}

// ── LEG 1: over + under ──────────────────────────────────────────────────────
//
// A mixed set with min_duration=30m. Pipelines whose DETAIL duration is at/over
// 30m MUST fire (each producing one allow audit); pipelines UNDER 30m MUST NOT
// fire. This is the whole contract in one pass: the positive leg proves the gate
// does not over-suppress a genuinely slow pipeline, the negative (below-
// threshold) leg proves it does not under-suppress a fast one — the direction
// whose regression is a runaway spawn firehose. The at-exactly-30m pipeline pins
// the boundary as >= (at/over fires), matching `dur < minDuration` in the gate.
func runOverAndUnderLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 1, Status: "failed", Ref: "main", DetailDurationSecs: secs(2400)}, // 40m — OVER  → fire
		{ID: 2, Status: "failed", Ref: "main", DetailDurationSecs: secs(1800)}, // 30m — AT    → fire (boundary)
		{ID: 3, Status: "failed", Ref: "main", DetailDurationSecs: secs(600)},  // 10m — UNDER → suppress
		{ID: 4, Status: "failed", Ref: "main", DetailDurationSecs: secs(60)},   // 1m  — UNDER → suppress
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-mindur-mixed"
	seedPipelineTrigger(daemon, trigID, "org/repo", canonThreshold)

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)

	wantFire := []string{"1", "2"}
	wantSuppressed := []string{"3", "4"}

	for _, id := range wantFire {
		if !fired[id] {
			return fail("expected pipeline %s (duration ≥ %s) to FIRE, but no allow audit carried its key; fired=%v audits=%d", id, canonThreshold, sortedKeys(fired), len(audits))
		}
	}
	for _, id := range wantSuppressed {
		if fired[id] {
			return fail("RUNAWAY: pipeline %s (duration < %s) FIRED though min_duration should have suppressed it; fired=%v", id, canonThreshold, sortedKeys(fired))
		}
	}
	if n := allowCount(audits); n != len(wantFire) {
		return fail("expected exactly %d allow audits (slow pipelines), got %d (fired=%v)", len(wantFire), n, sortedKeys(fired))
	}
	// Every audit is the real ledger shape: type=event, action=notify, source=gitlab.
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
	return pass("slow pipelines {40m,30m} fired (%d allow audits); fast {10m,1m} suppressed at ≥%s", len(wantFire), canonThreshold)
}

// ── LEG 2: all-under suppression + no-threshold control ──────────────────────
//
// A set where EVERY pipeline is under 30m. NOTHING may fire — this is the
// runaway-guard leg in its purest form: a gate that under-suppressed by even one
// pipeline would spawn a no-op agent in production. A CONTROL trigger WITHOUT
// min_duration on the SAME pipelines fires ALL of them, proving the suppression
// is caused by the threshold and not some unrelated reason nothing fired (a
// born-vacuous test would pass even if the gate never ran).
func runAllUnderLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 10, Status: "failed", Ref: "main", DetailDurationSecs: secs(600)}, // 10m
		{ID: 11, Status: "failed", Ref: "main", DetailDurationSecs: secs(300)}, // 5m
		{ID: 12, Status: "failed", Ref: "main", DetailDurationSecs: secs(60)},  // 1m
	}

	// (a) WITH the threshold: nothing may fire.
	serverA := stubGitLabServer(pips)
	defer serverA.Close()
	daemonA := newMockDaemon()
	const trigWith = "ka-mindur-allunder"
	seedPipelineTrigger(daemonA, trigWith, "org/repo", canonThreshold)
	withAudits := driveOneEval(logger, daemonA, serverA.URL, trigWith)
	withFired := firedPipelineIDs(withAudits)
	if len(withFired) != 0 {
		return fail("RUNAWAY: %d pipeline(s) fired though EVERY duration is under %s: %v", len(withFired), canonThreshold, sortedKeys(withFired))
	}

	// (b) CONTROL without the threshold: the SAME pipelines now all fire,
	// proving the suppression above is caused by min_duration, not an unrelated
	// cause. Without a threshold the gate never detail-fetches; all status
	// matches fire.
	serverB := stubGitLabServer(pips)
	defer serverB.Close()
	daemonB := newMockDaemon()
	const trigWithout = "ka-nodur-control"
	seedPipelineTrigger(daemonB, trigWithout, "org/repo", "") // no threshold
	ctrlAudits := driveOneEval(logger, daemonB, serverB.URL, trigWithout)
	ctrlFired := firedPipelineIDs(ctrlAudits)
	if len(ctrlFired) != len(pips) {
		return fail("control (no min_duration) fired %d/%d pipelines — expected ALL to fire, so the threshold-leg's zero is not a false negative; fired=%v", len(ctrlFired), len(pips), sortedKeys(ctrlFired))
	}

	return pass("all-under set fired 0 WITH ≥%s; same pipelines fired %d/%d WITHOUT a threshold (min_duration is the cause)", canonThreshold, len(ctrlFired), len(pips))
}

// ── LEG 3: unknown/null duration + detail-fetch failure fail closed ──────────
//
// The load-bearing fail-closed leg. A pipeline whose DETAIL duration is null
// (still running / skipped) and one whose DETAIL fetch ERRORS (HTTP 500 — a
// transient API failure) must BOTH fire NOTHING: an unknown duration can never
// become a match, so a slow-pipeline trigger fires ONLY on a pipeline CONFIRMED
// at/over the threshold. A KNOWN-slow control pipeline (well over the threshold)
// in the SAME set DOES fire — proving the zero above is specifically the
// unknown-duration fail-closed path, not that nothing ever fires here.
func runUnknownDurationLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 20, Status: "failed", Ref: "main", DetailDurationSecs: nil},        // null duration → fail closed
		{ID: 21, Status: "failed", Ref: "main", DetailError: true},              // detail 500    → fail closed
		{ID: 22, Status: "failed", Ref: "main", DetailDurationSecs: secs(3600)}, // 60m known slow → fires (control)
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-mindur-unknown"
	seedPipelineTrigger(daemon, trigID, "org/repo", canonThreshold)

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)

	if fired["20"] {
		return fail("RUNAWAY: pipeline 20 (null/unknown duration) FIRED — an unknown duration must fail closed, never match; fired=%v", sortedKeys(fired))
	}
	if fired["21"] {
		return fail("RUNAWAY: pipeline 21 (DETAIL fetch failed) FIRED — a transient fetch error must fail closed, never match; fired=%v", sortedKeys(fired))
	}
	if !fired["22"] {
		return fail("expected the known-slow control pipeline 22 (60m ≥ %s) to fire, so the unknown-duration zero is not a false negative; fired=%v", canonThreshold, sortedKeys(fired))
	}
	if len(fired) != 1 {
		return fail("expected exactly 1 fire (the known-slow control), got %d: %v", len(fired), sortedKeys(fired))
	}
	return pass("null-duration + detail-500 pipelines fired 0 (fail closed); the known-slow 60m control fired (unknown ≠ match)")
}

// ── LEG 4: invalid min_duration fails closed ─────────────────────────────────
//
// An unparseable min_duration must FAIL CLOSED: evaluatePipelines returns an
// error, zero matches, and thus zero fires — a typo can never silently reopen
// the spawn firehose (fire on every pipeline) NOR silently disable the trigger.
// The CLI rejects a bad value at authoring time (asserted in the shell module's
// live-daemon leg), but this proves the RUNTIME defense-in-depth: even a bad
// value written directly onto a bead (bypassing the CLI) fails closed. Even the
// 60m pipeline (which WOULD fire under a well-formed threshold) must not fire.
func runInvalidMinDurationLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 30, Status: "failed", Ref: "main", DetailDurationSecs: secs(3600)}, // 60m — would fire under a valid threshold
		{ID: 31, Status: "failed", Ref: "main", DetailDurationSecs: secs(4800)}, // 80m
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-mindur-invalid"
	seedPipelineTrigger(daemon, trigID, "org/repo", "not-a-duration") // invalid

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)
	if len(fired) != 0 {
		return fail("RUNAWAY: an invalid min_duration fired %d pipeline(s) instead of failing closed: %v", len(fired), sortedKeys(fired))
	}
	return pass("invalid min_duration fired 0 pipelines (fails closed — neither the firehose reopens nor the trigger silently fires on all)")
}

// ── LEG 5: field round-trip through the production parse path ─────────────────
//
// The min_duration field must round-trip bead → gitlabTriggerData through the
// production ParseTriggerDef / parseGitLabTriggerData path that LoadTriggers
// runs, so the threshold the engine filters on is EXACTLY the value authored on
// the bead. We prove it behaviorally: a trigger whose min_duration is `1h` fires
// a pipeline at exactly 60m and one at 61m, but NOT one at 59m — so the specific
// authored threshold (not a default, not the wrong field) is what gated the
// fire. A misread field (default 30m, or the max-age field) would either fire
// the 59m pipeline or drop the 60m one.
func runFieldRoundTripLeg(logger *slog.Logger) legResult {
	pips := []stubPipeline{
		{ID: 40, Status: "failed", Ref: "main", DetailDurationSecs: secs(3540)}, // 59m — under 1h  → drop
		{ID: 41, Status: "failed", Ref: "main", DetailDurationSecs: secs(3600)}, // 60m — at 1h     → fire (boundary)
		{ID: 42, Status: "failed", Ref: "main", DetailDurationSecs: secs(3660)}, // 61m — over 1h   → fire
	}
	server := stubGitLabServer(pips)
	defer server.Close()

	daemon := newMockDaemon()
	const trigID = "ka-mindur-1h"
	seedPipelineTrigger(daemon, trigID, "org/repo", "1h")

	audits := driveOneEval(logger, daemon, server.URL, trigID)
	fired := firedPipelineIDs(audits)

	if fired["40"] {
		return fail("min_duration=1h fired a 59m pipeline (40) — the authored threshold did not round-trip; a smaller/default value was used; fired=%v", sortedKeys(fired))
	}
	if !fired["41"] {
		return fail("expected the 60m pipeline (41) to fire at exactly the 1h boundary, got fired=%v", sortedKeys(fired))
	}
	if !fired["42"] {
		return fail("expected the 61m pipeline (42) to fire over 1h, got fired=%v", sortedKeys(fired))
	}
	if len(fired) != 2 {
		return fail("expected exactly 2 fires under 1h (60m + 61m), got %d: %v", len(fired), sortedKeys(fired))
	}
	return pass("authored threshold 1h round-tripped verbatim: 60m+61m fired, 59m dropped (the exact boundary, not a default)")
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
