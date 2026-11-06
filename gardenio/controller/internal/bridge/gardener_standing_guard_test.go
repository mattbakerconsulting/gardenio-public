package bridge

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnStandingProseWithoutMarker_WarnsOnce covers the firing-time
// warn-with-grace half: a live gardener declaring standing in prose without a
// marker warns exactly once; adding a marker silences it entirely.
func TestWarnStandingProseWithoutMarker_WarnsOnce(t *testing.T) {
	const warnMsg = "declares STANDING in terminal_goal but carries NO standing marker"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := NewTriggerEngine(TriggerEngineConfig{Logger: logger})

	trig := TriggerDef{
		ID:       "ka-0000003783",
		Title:    "Triage-intake gardener (marker-less)",
		Gardener: &GardenerSpec{TerminalGoal: "STANDING sweep gardener: goes dormant between sweeps; does NOT self-complete on an empty queue."},
		Labels:   []string{"project:gardenio", "role:gardener"},
	}

	e.warnStandingProseWithoutMarker(trig)
	if got := strings.Count(buf.String(), warnMsg); got != 1 {
		t.Fatalf("expected exactly 1 warning, got %d:\n%s", got, buf.String())
	}
	// Re-evaluating the same trigger must NOT re-warn.
	e.warnStandingProseWithoutMarker(trig)
	if got := strings.Count(buf.String(), warnMsg); got != 1 {
		t.Fatalf("expected warn-once, got %d warnings", got)
	}

	// A gardener carrying the perpetual:true label never warns.
	buf.Reset()
	e.warnStandingProseWithoutMarker(TriggerDef{
		ID:       "ka-0000001510",
		Gardener: &GardenerSpec{TerminalGoal: "STANDING delivery gardener: drive ready beads to merge."},
		Labels:   []string{standingGardenerLabel},
	})
	if buf.Len() != 0 {
		t.Fatalf("marked standing gardener should not warn, got:\n%s", buf.String())
	}

	// An epic-bounded gardener (adjectival 'standing') never warns.
	e.warnStandingProseWithoutMarker(TriggerDef{
		ID:       "ka-0000003543",
		Gardener: &GardenerSpec{TerminalGoal: "All children of the epic are closed AND a standing warm runsc node is provisioned."},
	})
	if buf.Len() != 0 {
		t.Fatalf("epic-bounded gardener should not warn, got:\n%s", buf.String())
	}
}

// TestTerminalGoalDeclaresStanding_RealCohort drives the detector against the
// EXACT terminal_goal prose of the marker-less gardener cohort in bug
// ka-0000004311 (captured verbatim from the beads). The load-bearing assertion
// is the FALSE-POSITIVE line: ka-0000003543 is genuinely epic-bounded ("All
// children of the epic are closed AND …") and only tripped the reporter's broad
// /standing/i sweep because of the incidental adjective in "standing warm runsc
// node". The precise detector MUST NOT flag it — auto-normalizing it to
// perpetual would pin a gardener that MUST terminate into an eternal loop.
func TestTerminalGoalDeclaresStanding_RealCohort(t *testing.T) {
	cases := []struct {
		id   string
		goal string
		want bool
	}{
		{
			"ka-0000006210 groomer",
			"STANDING groomer gardener: de-lanes gardenio needs-intake WORK beads into the ready queue (triage/scope/ready-gate/close-junk); complement of the sweep-IN janitor ka-0000003783. Goes dormant between sweeps (bead-query pre-flight fires only while the gardenio needs-intake lane is non-empty). Does NOT self-complete on a momentarily-empty lane. Self-blocks on no_progress_for/3 (no real grooming over 3 runs) or anchor epic ka-0000003517 closed (lane retired); bounded by run_budget=50. NO wall_clock self-completion.",
			true,
		},
		{
			"ka-0000003783 sweep",
			"STANDING sweep gardener: goes dormant between sweeps and wakes on new orphaned beads; does NOT self-complete on a momentarily empty queue. Self-blocks on no_progress_for/3 (no real intake work over 3 runs) or anchor epic ka-0000007143 closed (lane retired); bounded by run_budget=50. NO wall_clock.",
			true,
		},
		{
			"ka-0000007197 dailbert (no standing/perpetual WORD, only negation prose)",
			"dailbert is continuously published: each cycle a new dated strip is generated via the two-agent engine, built with build.mjs, pushed to groblegark/dailbert main, and dailbert.com serves it (200). Loop persists while the strip continues; does NOT self-close on an empty queue — the standing task gd-4pA2r0BMwu re-arms each cycle.",
			true,
		},
		{
			"ka-0000007256 ced companion sweep",
			"Drain the open kind:ced-audit backlog (bugs + pre-scoped fix-tasks) the real-data session inspector (epic ka-0000003297) files, driving each to a committed/merged fix. STANDING label-driven companion sweep for epic ka-0000004371 (bugs are unparented, so work is keyed on the kind:ced-audit label, not epic-children). Goes idle (cron preflight bead_query>0) when the backlog is empty and re-wakes on the next open kind:ced-audit bug; self-bounds on run_budget, 3 consecutive zero-progress runs, and the 720h loop wall-clock.",
			true,
		},
		{
			"ka-0000005204 auto-block hygiene",
			"STANDING auto-block hygiene sweep (cron 11 */12). No terminal epic; each cycle audits the gardener fleet and files/executes remediation. Self-stops on run-budget ceiling or 3 consecutive zero-progress runs.",
			true,
		},
		{
			// FALSE POSITIVE GUARD — must stay false.
			"ka-0000003543 resume-latency (epic-bounded; adjectival 'standing warm node')",
			"All children of the resume-latency-hardening epic are closed AND resume_latency_seconds p95 for path=thaw-warm is sustained <1s over a verification window AND a new (never-frozen) runsc agent first-spawn no longer cold-starts (R2 or better). Workstream order: WS1 (standing warm runsc node + larger pool — operator-approved in Slack thread 1781538871-454879, supersedes the HARD HOLD plan ka-0000006865) -> WS3 (blank-placeholder pre-provision so first-spawn runsc agents claim a warm pod instead of cold-provisioning) -> WS2 (resume_latency_seconds Grafana dashboard + slow-resume alert). Iterate with bench-resume.sh and regression-check each run. Before completing, create a regression-watchdog successor (per gardenio/docs/gardener-resume-observability-and-succession-plan.md §2) that keeps the dashboard/alert honest after the loop exits.",
			false,
		},
		{
			"ka-0000007624 resumer watchdog",
			"STANDING watchdog: revives stuck gardeners; does NOT close on epic drain. Self-blocks on no_progress_for/3 (nothing to revive over 3 runs -> dormant) or anchor epic ka-0000007687 closed; bounded by run_budget=50. NO wall_clock.",
			true,
		},
		{
			"ka-0000002056 oss-sanitization sentinel",
			"STANDING weekly audit sentinel (cron 13 9 * * 1). No terminal epic: it reports internal-string + secret drift weekly. It self-stops on the run-budget ceiling or when 3 consecutive runs make no forward progress (no audit report posted, progress_score 0). Re-arm by re-running the remediation to refresh the run budget if the weekly OSS-sanitization audit is still wanted.",
			true,
		},
		{
			"ka-0000006629 fleet-liveness sentinel",
			"STANDING liveness sentinel (cron 23 */2 * * *). No terminal epic: it alerts when janitor/triage gardeners go dark. It self-stops on the run-budget ceiling or when 3 consecutive runs make no forward progress (no alert raised, progress_score 0). Re-arm by re-running the remediation to refresh the run budget if fleet-liveness watching is still wanted.",
			true,
		},
		{
			"ka-0000007481 investigative janitor",
			"STANDING janitor gardener (cron 11,41 * * * *). No terminal epic: it continuously verifies done-ness and files stall bugs. It self-stops on the run-budget ceiling or when 3 consecutive runs make no forward progress (nothing verified, no stall bug filed, progress_score 0). Re-arm by re-running the remediation to refresh the run budget if the janitor lane is still wanted.",
			true,
		},
		{
			"ka-0000005569 aws-poc currency",
			"Anchor epic ka-0000005495 (AWS-POC currency) is closed — the POC lane is frozen/torn down. This is a STANDING gardener: it goes dormant (cron preflight: no open children under ka-0000005495) between golden releases and wakes when new drift work appears; it does NOT self-complete on a single chart bump.",
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, matched := TerminalGoalDeclaresStanding(tc.goal)
			if got != tc.want {
				t.Errorf("TerminalGoalDeclaresStanding = %v (matched %q), want %v", got, matched, tc.want)
			}
			if got && matched == "" {
				t.Errorf("declared=true but matched text is empty")
			}
			if !got && matched != "" {
				t.Errorf("declared=false but matched text %q is non-empty", matched)
			}
		})
	}
}

// TestTerminalGoalDeclaresStanding_Precision covers the declaration/adjective
// boundary directly — the property that makes option-1 (guard) safe where
// option-2 (blind /standing/i auto-normalize) is not.
func TestTerminalGoalDeclaresStanding_Precision(t *testing.T) {
	cases := []struct {
		name string
		goal string
		want bool
	}{
		{"empty", "", false},
		{"whitespace", "   \n  ", false},
		{"default epic-drain goal", "All children of gd-abc123 are closed.", false},
		{"standing bound to gardener", "STANDING groomer gardener: does the thing.", true},
		{"perpetual bound to loop", "This is a perpetual loop with no terminal epic.", true},
		{"standing bound to sentinel", "STANDING liveness sentinel that alerts.", true},
		// Adjectival 'standing' with NO nature noun within the window ⇒ not a declaration.
		{"standing warm node (adjective)", "Provision a standing warm runsc node and larger pool.", false},
		{"standing army (adjective)", "Keep a standing army of pre-provisioned pods ready.", false},
		{"standing meeting (adjective)", "Attend the standing weekly meeting and report status.", false},
		// Explicit non-termination declarations (independent of the standing word).
		{"does not self-complete", "Drain the queue; does NOT self-complete on an empty lane.", true},
		{"does not self-close", "Publish each cycle; does not self-close on an empty queue.", true},
		{"never self-declares", "The loop never self-declares done; operator retires it.", true},
		{"no wall_clock self-completion", "bounded by run_budget=50. NO wall_clock self-completion.", true},
		{"loop persists", "Loop persists while the strip continues.", true},
		// Ordinary epic-bounded goal with none of the above ⇒ not a declaration.
		{"plain bounded goal", "Drive every ready bead to a merged MR, then stop when the epic drains.", false},
		{"goal mentions dark gardeners but not standing", "Alert when janitor gardeners go dark; stop when the epic closes.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := TerminalGoalDeclaresStanding(tc.goal); got != tc.want {
				t.Errorf("TerminalGoalDeclaresStanding(%q) = %v, want %v", tc.goal, got, tc.want)
			}
		})
	}
}

// standingBlob marshals a GardenerSpec with the given terminal_goal and standing
// flag into a raw gardener blob, for the guard tests.
func standingBlob(t *testing.T, goal string, standing bool) string {
	t.Helper()
	raw, err := json.Marshal(GardenerSpec{TerminalGoal: goal, Standing: standing})
	if err != nil {
		t.Fatalf("marshalling gardener blob: %v", err)
	}
	return string(raw)
}

func TestValidateGardenerStandingMarker(t *testing.T) {
	const standingGoal = "STANDING groomer gardener: does NOT self-complete on a momentarily-empty lane."
	const boundedGoal = "All children of gd-abc are closed."

	cases := []struct {
		name        string
		spec        *GardenerSpec
		labels      []string
		wantViolate bool
	}{
		{"nil spec", nil, nil, false},
		{
			"declares standing, no marker ⇒ violation",
			&GardenerSpec{TerminalGoal: standingGoal},
			[]string{"project:gardenio", "role:gardener"},
			true,
		},
		{
			"declares standing + standing:true blob marker ⇒ ok",
			&GardenerSpec{TerminalGoal: standingGoal, Standing: true},
			[]string{"project:gardenio"},
			false,
		},
		{
			"declares standing + perpetual:true label ⇒ ok",
			&GardenerSpec{TerminalGoal: standingGoal},
			[]string{"project:gardenio", standingGardenerLabel},
			false,
		},
		{
			"epic-bounded goal, no marker ⇒ ok (nothing declared)",
			&GardenerSpec{TerminalGoal: boundedGoal},
			[]string{"project:gardenio"},
			false,
		},
		{
			"epic-bounded goal WITH marker ⇒ ok (marked standing is always fine)",
			&GardenerSpec{TerminalGoal: boundedGoal, Standing: true},
			nil,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateGardenerStandingMarker(tc.spec, tc.labels)
			if (v != nil) != tc.wantViolate {
				t.Fatalf("ValidateGardenerStandingMarker violation=%v, want %v (err=%q)", v != nil, tc.wantViolate, v.Error())
			}
			if v != nil {
				msg := v.Error()
				if !strings.Contains(msg, "ka-0000004311") {
					t.Errorf("violation message missing bug id: %q", msg)
				}
				if v.MatchedProse == "" || !strings.Contains(msg, v.MatchedProse) {
					t.Errorf("violation message %q should quote matched prose %q", msg, v.MatchedProse)
				}
			}
		})
	}
}

// TestValidateGardenerStandingBlob covers the raw-blob authoring entry point,
// including the empty / unparseable short-circuits.
func TestValidateGardenerStandingBlob(t *testing.T) {
	const standingGoal = "STANDING sweep gardener: goes dormant between sweeps."

	// Empty blob ⇒ not a gardener ⇒ no violation.
	if v := ValidateGardenerStandingBlob("", nil); v != nil {
		t.Errorf("empty blob should be a no-op, got: %s", v.Error())
	}
	// Unparseable blob ⇒ this guard is silent (bounds guard fails closed on it).
	if v := ValidateGardenerStandingBlob("{not json", nil); v != nil {
		t.Errorf("unparseable blob should be a no-op here, got: %s", v.Error())
	}
	// Standing prose, no marker ⇒ violation.
	if v := ValidateGardenerStandingBlob(standingBlob(t, standingGoal, false), []string{"project:gardenio"}); v == nil {
		t.Error("standing prose without marker should violate")
	}
	// Standing prose + blob marker ⇒ ok.
	if v := ValidateGardenerStandingBlob(standingBlob(t, standingGoal, true), nil); v != nil {
		t.Errorf("standing prose with blob marker should be ok, got: %s", v.Error())
	}
	// Standing prose + label marker ⇒ ok.
	if v := ValidateGardenerStandingBlob(standingBlob(t, standingGoal, false), []string{standingGardenerLabel}); v != nil {
		t.Errorf("standing prose with label marker should be ok, got: %s", v.Error())
	}
}
