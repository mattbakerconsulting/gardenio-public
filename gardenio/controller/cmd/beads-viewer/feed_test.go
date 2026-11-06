package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fixedFeedNow is a stable wall-clock for card-render assertions.
func fixedFeedNow(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-06-14T21:30:05Z")
	if err != nil {
		t.Fatalf("parse fixed ts: %v", err)
	}
	return ts
}

func TestFeedSchedulerBookkeeping(t *testing.T) {
	fireFields := json.RawMessage(`{"multi_fire_key":"gd-trig:cron:2026-06-16T22:00"}`)
	cases := []struct {
		name string
		bead *feedEventBead
		want bool
	}{
		{
			name: "cron-fire wrapper (source:multi + multi_fire_key)",
			bead: &feedEventBead{ID: "gd-a", Type: "task", Labels: []string{"project:gardenio", "source:multi", "trigger:gd-x"}, Fields: fireFields},
			want: true,
		},
		{
			name: "source:multi label without multi_fire_key field is NOT swept",
			bead: &feedEventBead{ID: "gd-b", Type: "task", Labels: []string{"source:multi"}},
			want: false,
		},
		{
			name: "genuine task that merely mentions multi in title is kept",
			bead: &feedEventBead{ID: "gd-c", Type: "task", Title: "Multi-region failover plan", Labels: []string{"project:gardenio"}},
			want: false,
		},
		{
			name: "nil bead",
			bead: nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedSchedulerBookkeeping(c.bead); got != c.want {
				t.Errorf("feedSchedulerBookkeeping = %v, want %v", got, c.want)
			}
		})
	}
}

// TestClassifyFeedEvent_DropsSchedulerBookkeeping confirms the classifier drops
// a scheduler-fire wrapper bead end-to-end (ok=false), so cron/event tick
// claim/close churn never reaches a card, while a genuine task survives.
func TestClassifyFeedEvent_DropsSchedulerBookkeeping(t *testing.T) {
	fire, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id": "gd-fire", "type": "task", "title": "[1:1:cron] Multi[cron]: gardener-delivery", "status": "closed",
			"labels": []string{"project:gardenio", "source:multi"},
			"fields": map[string]any{"multi_fire_key": "gd-trig:cron:2026-06-16T22:00"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, _, ok := classifyFeedEvent("beads.bead.closed", fire); ok {
		t.Errorf("scheduler-fire wrapper should be dropped (ok=false)")
	}

	real, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id": "gd-real", "type": "task", "title": "Fix feed dedup", "status": "closed",
			"labels": []string{"project:gardenio"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, _, ok := classifyFeedEvent("beads.bead.closed", real); !ok {
		t.Errorf("genuine task should be surfaced (ok=true)")
	}
}

func TestFeedBaseKind(t *testing.T) {
	cases := []struct {
		topic string
		want  string
	}{
		{"beads.bead.created", "created"},
		{"beads.bead.closed", "closed"},
		{"beads.bead.updated", "updated"},
		{"beads.bead.weird", ""},
	}
	for _, c := range cases {
		if got := feedBaseKind(c.topic).Name; got != c.want {
			t.Errorf("feedBaseKind(%q) = %q, want %q", c.topic, got, c.want)
		}
	}
}

func TestFeedKindForBead(t *testing.T) {
	cases := []struct {
		name   string
		base   feedKind
		bead   *feedEventBead
		status string
		want   feedKind
	}{
		{"decision overrides everything", feedKindUpdated, &feedEventBead{Type: "decision"}, "open", feedKindDecision},
		{"merged MR", feedKindUpdated, &feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"merged"}`)}, "open", feedKindMRMerged},
		// Phase 3 (ka-0000006994): an updated open MR with no merge and no
		// terminal pipeline is routine churn — dropped (zero kind), not surfaced
		// as a generic "updated". MR open/merge/pipeline get their own families.
		{"open MR update with no terminal pipeline is churn", feedKindUpdated, &feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened"}`)}, "open", feedKind{}},
		{"open MR on create is mr-opened", feedKindCreated, &feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened"}`)}, "open", feedKindMROpened},
		{"closed stays closed", feedKindClosed, &feedEventBead{Type: "task"}, "closed", feedKindClosed},
		{"created stays created", feedKindCreated, &feedEventBead{Type: "task"}, "open", feedKindCreated},
		{"update to in_progress is a claim", feedKindUpdated, &feedEventBead{Type: "task"}, "in_progress", feedKindClaimed},
		{"update to blocked is blocked", feedKindUpdated, &feedEventBead{Type: "task"}, "blocked", feedKindBlocked},
		{"plain update", feedKindUpdated, &feedEventBead{Type: "task"}, "open", feedKindUpdated},
		// Squawks: a substantive squawk surfaces; an agent-online heartbeat is
		// dropped (ka-0000004805).
		{"substantive squawk surfaces", feedKindCreated, &feedEventBead{Type: "message", Title: "Merged MR 3286 — feed dedup fix shipped", Labels: []string{"squawk"}}, "open", feedKindSquawk},
		{"online-starting-cycle heartbeat dropped", feedKindCreated, &feedEventBead{Type: "message", Title: "gardener-release-owner online — starting release cycle", Labels: []string{"squawk"}}, "open", feedKind{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedKindForBead(c.base, c.bead, c.status, nil); got != c.want {
				t.Errorf("feedKindForBead = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestFeedSquawkHeartbeat covers the agent-greeting classifier: agent
// wake-up / start-of-cycle / "claiming X" announcements are dropped as routine
// noise, while a squawk that reports a concrete completed workflow outcome is
// retained (ka-0000004805, ka-0000004501).
func TestFeedSquawkHeartbeat(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// Dropped: "online" + start-of-cycle verb, no outcome (original shape).
		{"release gardener starting cycle", "gardener-release-owner online — starting release cycle", true},
		{"currency gardener checking drift", "AWS-POC currency gardener online — checking drift", true},
		{"crew picking up work", "crew/gardenio online — picking up feed polish", true},
		{"assessing on startup", "triage gardener online, assessing the ready queue", true},
		// Dropped: Class-B non-"online" greetings the 169.1 filter missed.
		{"reporting in", "feed-relevance gardener reporting in", true},
		{"role reporting with project", "MR Relevance Gardener reporting (project:gardenio)", true},
		{"reporting for duty", "crew reporting for duty", true},
		{"crew on proj claiming", "crew on gardenio: claiming ka-0000004501", true},
		{"crew on proj starting", "crew on gardenio: starting molecule gd-abc", true},
		{"crew slash proj claiming", "crew/gardenio: claiming feed polish molecule", true},
		// Dropped: Class-C non-"online" wake-up greetings (ka-0000001954).
		{"currency gardener waking up", "AWS-POC currency gardener (gardenio) waking up. Checking chart pin drift and lane health.", true},
		{"online pouring critic formula", "Feed-relevance critic gardener (rubric-s-xxo5) online — project:gardenio. Pouring S-L-R critic formula ka-0000001036 against fresh /feed.", true},
		{"online beginning assess", "Gardenio Bug-Response Gardener (aggressive) online — driving project:gardenio bugs to resolved. Beginning ASSESS cycle.", true},
		// Dropped: Class-B outcome-guard false-retains — a KPI-label "merged" or a
		// bare gd-<id> self-reference must NOT trip the retain-guard.
		{"online with KPI-label merged", "gardener-delivery online — starting MR walk cycle — KPI: MRs merged", true},
		{"greeting self-referencing a bead id", "crew on gardenio: claiming ka-0000004501 (feed signal fix)", true},
		// Dropped: Escape-1 prior-cycle merge token in a parenthetical aside must
		// NOT re-retain a wake-up greeting (ka-0000002345 / ka-0000002577, critic
		// iter 2026-06-19T0129). The "gardener online" + standing-state recap shape
		// has no start-verb adjacent to "online".
		{"NAT gardener online prior-cycle merge in parens", "NAT-elimination research gardener online (epic ka-0000007498, project gardenio). Both research children CLOSED (edge-map ka-0000005663 + options/cost gd-rAJkcyXEww, merged !4625). Sole blocker: operator decision ka-0000003998 [P1] — pick NAT-minimized topology.", true},
		{"NAT gardener online children closed colon", "NAT-elimination research gardener online (epic ka-0000007498, project gardenio). Both research children CLOSED: edge-map ka-0000005663 + options/cost gd-rAJkcyXEww (merged !4625). Finding: fully NAT-free is INFEASIBLE.", true},
		// Dropped: Escape-2 non-"online" wake-up variants.
		{"waking for sweep", "triage-intake gardener (project=gardenio) waking for orphaned/abandoned bead sweep into needs-intake lane", true},
		{"on it investigating", "thread-1781830866-842059 (gardener, gardenio) on it — investigating beads-viewer 503 at /beads/agents", true},
		// Dropped: critic iter 2026-06-19T0751 greeting escapes — an intervening
		// "(project:…)" clause breaks the literal "gardener online" adjacency, and
		// "sweeping"/"driving" weren't in the start-verb list (ka-0000001954 refresh).
		{"gardener online project-clause sweeping", "triage gardener (project:gardenio) online — sweeping orphaned beads into needs-intake", true},
		{"gardener online project-clause driving", "Bug-Response gardener (project:gardenio) online — driving open bugs to resolved", true},
		// Dropped: "spinning up" without "online" — was gated behind "online" before
		// (ka-0000001954 refresh, critic iter 2026-06-19T0751).
		{"first-mate spinning up no online", "First-mate gardener spinning up — sweeping gardener trajectories", true},
		{"spinning-up hyphen variant", "release-owner gardener spinning-up for the cut cycle", true},
		// Retained: substantive squawks naming a concrete completed action.
		{"online but reports a merge", "gardener online — merged MR 3286, cycle complete", false},
		{"online but names a bead outcome", "crew online — fixed gd-abc, tests pass", false},
		{"plain outcome squawk, no heartbeat shape", "Merged MR 3286 — feed dedup fix shipped", false},
		{"finding with no greeting keyword", "found 9/60 heartbeat cards in the feed", false},
		{"online with no start verb", "crew/gardenio online and waiting for review", false},
		// CRITICAL retain set (must NOT be dropped) — ka-0000004501 mandate.
		{"release-owner CUT", "release-owner: CUT 2026.169.2 (pipeline 12345 GREEN)", false},
		{"BUMP done merged", "BUMP done: MR !4575 merged", false},
		{"decision follow-through shipped", ":newspaper: Decision gd-x follow-through shipped — MR 26 MERGED", false},
		{"phase3 prove checkpoint", "Phase 3 PROVE checkpoint — independently re-verified soak liveness", false},
		{"mr relevance scored", "MR Relevance Gardener — scored 2 oldest never-scored gardenio MRs", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkHeartbeat(b); got != c.want {
				t.Errorf("feedSquawkHeartbeat(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkHeartbeat(nil) {
		t.Errorf("feedSquawkHeartbeat(nil) = true, want false")
	}
}

// TestFeedSquawkNoopTick covers the Class-A no-op zero-delta gardener cycle
// reports — a scheduled gardener announcing it ran and did nothing this tick.
// Dropped only when the title names an explicit zero/no-action delta AND carries
// no concrete outcome; a cycle line that took an action is retained
// (ka-0000004501).
func TestFeedSquawkNoopTick(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// Dropped: the 9 Class-A no-op fixtures from the 169.1 capture.
		{"release-owner no cut", "release-owner: cuts_today=0 — NO CUT this cycle", true},
		{"delivery merged 0 spawned 0", "delivery: merged 0, spawned 0", true},
		{"delivery no eligible MRs", "delivery: no eligible MRs to walk this cycle", true},
		{"triage swept 0", "triage-intake: swept=0, abandoned-reopened=0", true},
		{"aws-poc no drift", "AWS-POC currency cycle: NO actionable drift", true},
		{"resume-latency all green", "resume-latency watchdog: all green", true},
		{"nothing to do", "fleet-liveness guard: nothing actionable this cycle", true},
		{"lane empty", "bug-response gardener: lane empty", true},
		// Dropped: Escape-2 count-BEFORE-verb zero-delta ("0 closed", "0 spawned")
		// — the prior-cycle "P0/P1 merged" token is in a later recap sentence so it
		// does not re-retain (ka-0000007218, critic iter 2026-06-19T0129).
		{"count before verb zero deltas", "Bug gardener (agg): 8 open, 0 in-flight (0 live/1 stale non-bug), 0 closed, 0 spawned, 1 decision filed (gd-EU7ggFuqU0: drain 3 stale P2 tail -> queue to 5). 4 P0/P1 merged+blocked on operator/gVisor redrive; 1 P1 infra flake. No actionable code-crew work this cycle.", true},
		// Retained: a cycle line that names a concrete action taken.
		{"cut made despite zero stale", "release-owner: cuts_today=0 but CUT 2026.169.2", false},
		{"merged a real MR", "delivery: merged MR !4575, spawned 0 followups", false},
		{"drift landed", "AWS-POC currency cycle: drift found — deployed 2026.169.2", false},
		{"closed a bead", "triage-intake: swept 3 — closed gd-abc, gd-def", false},
		// Not a no-op tick at all (no zero-delta phrase): left for other filters.
		{"plain greeting no counts", "gardener-release-owner online — starting release cycle", false},
		{"substantive report no zero", "MR Relevance Gardener — scored 2 oldest never-scored gardenio MRs", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkNoopTick(b); got != c.want {
				t.Errorf("feedSquawkNoopTick(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkNoopTick(nil) {
		t.Errorf("feedSquawkNoopTick(nil) = true, want false")
	}
}

// TestFeedSquawkGardenerCycleTick covers the STRUCTURAL gardener-cycle-tick
// detector (ka-0000001954) that replaces the literal zero-delta allowlist. All
// fixtures are VERBATIM live squawk titles captured from prod (2026-06-18T1326,
// image 169.10). A cycle/tick-shaped recap with no this-cycle action drops; a
// cycle line naming a concrete action this tick (or a genuine non-cycle outcome)
// retains.
func TestFeedSquawkGardenerCycleTick(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// --- DROP: Class-A zero-delta cycle ticks (verbatim live) ---
		{"triage cycle cdho 0s", "triage cycle (cdho): 0 self-labelled (0 Lane-0 candidates), 0 dups closed (queue fully deduped in prior cycles), 0 done-closed (all 6 survivor MRs still open/in-review — !1199/!1438/!991/+606/603/601, none merged), 0 crew spawned (every survivor has a live conflict-free MR awaiting human approval), 0 escalations.", true},
		{"triage cycle qy7i 0s", "triage cycle (qy7i): 0 self-labelled (Lane 0 clean, 0 untagged DEVOPS in window), 0 dups closed (fully deduped prior cycles), 0 done-closed (no survivor MR merged yet), 0 crew spawned (survivors 625/714 have live green reviewer-assigned MRs !1199+!1200 [matthewbaker], !991 [terryvance]), 0 escalations (perf-obs resolved via gd-sbGQZQ3Anu).", true},
		{"triage cycle susj 0s", "triage cycle (susj): 0 self-labelled (Lane 0 clean — 0 candidates updated<=2h), 0 dups closed (queue fully deduped prior cycles), 0 done-closed (no survivor MR merged — !991/!1199/!1200 all open, conflict-free, blocked on human approval only), 0 crew spawned, 0 escalations (perf-obs resolved gd-sbGQZQ3Anu, 610 resolved gd-zJAYVdsiCZ).", true},
		{"aws-poc currency cycle mc0p no drift", "AWS-POC currency cycle (mc0p): NO drift — pin=2026.169.10 IS latest golden (top GitLab Release + bare tag). No molecule in flight.", true},
		{"aws-poc currency cycle wg3z no new pass", "AWS-POC currency cycle (gardener wg3z): NO new pass — 169.8->169.10 pass ALREADY IN FLIGHT & HEALTHY (molecule gd-pouM6Fd6ba, landed by sibling ejyz 13:07, wired under epic). detect+bump CLOSED (pin on main=2026.169.10), apply gd-z1IWpGi1MZ in_progress with LIVE crew confirm-lb0.", true},
		{"aws-poc currency cycle 9vf8 no actionable", "AWS-POC currency cycle 2026-06-18T13:3x (gardener 9vf8): NO actionable drift. Pin=2026.169.10 IS the latest GOLDEN release (top GitLab Release, released 12:55Z). Tag 2026.169.11 NOT golden — pipeline #2611703836 still RUNNING, no Release (404).", true},
		// --- DROP: Class-B gardener cycle status recaps (verbatim live) ---
		{"gardener cycle 59zz phase healthy", "Gardener cycle [59zz] 2026-06-18T13:15Z: Phase 1+2+gate CLOSED. Phase 3 active — 3.1/3.3/3.4 CLOSED; 3.2 soak HEALTHY in-flight on canonical-prod (autonomous k8s deploy), 803 SLI samples / 0 Tier-1 breaches, window ends 2026-06-22T10:02Z. 3.5 proof bundle correctly blocked on soak completion; Phase 4 gated.", true},
		{"gardener r3cw fresh-prod cycle phases closed", "Gardener ka-0000000398 (fresh-prod EXECUTION PROGRAM) cycle: Phases 1+2+gate CLOSED. Phase 3 PROVE in-flight & HEALTHY — soak on canonical-prod (acct 123456789012) 855 SLI samples / 0 Tier-1 breaches, load-gen+slo-collect 1/1 Running 2d23h, RDS 17.7 Multi-AZ available; window ends 2026-06-22T10:02Z. 3.3+3.4 CLOSED; 3.5 proof-bundle correctly blocked on soak completion; Phase 4 gated.", true},
		{"ci-minutes cycle 14 levers live", "CI-MINUTES gardener cycle 14: Levers A+B+C+D live (2.9x off baseline; rwx-main verified 73-100s vs ~1800s). Final lever MR !4570 (rwx-release decouple, last ~711 min/48h to hit true 10x) Draft+GREEN, blocked solely on open operator decision gd-Mvmt9R5to2. Holding — no crew/dup needed. Stopping.", true},
		{"ci-minutes cycle 12 levers live", "CI-minutes gardener cycle 12: Levers A+B+C+D live (rwx-main 93s, was 1800s; 2.9x off baseline). Final lever MR !4570 (rwx-release fire-and-forget, ~711 min/48h, GREEN+Draft) ready — filed operator decision gd-Mvmt9R5to2 since it touches release+prod path. Awaiting sign-off to merge + re-measure.", true},
		{"ci-minutes-reduction cycle 13 landed", "CI-MINUTES-REDUCTION gardener (gardenio) cycle 13 — assessing: Levers A+B+C+D landed; final lever ka-0000001459 (rwx-release decouple, MR !4570) pending operator decision gd-Mvmt9R5to2.", true},
		{"relevance tick scored 2", "Relevance tick 06-18 ~13:0x UTC/09:0x ET (-6frb, LIVE): progress=1. Main adbc6c87. Scored 2 stalest (both reviewed 06-11, 7d due): !1337 PE-10134 frontend → 9.2 KEEP+digest (conflicts resolved+CI green, was 7.2); !1339 PE-10134 backend → 8.4 KEEP digest-HELD (CI red 06-06). SUP=10 both. No hard gates, no human takeover → 0 closes.", true},
		{"aws-poc currency cycle ejyz drift", "AWS-POC currency cycle (gardener ejyz): DRIFT 2026.169.8 -> 2026.169.10 (2 patches). 169.10 confirmed golden (GitLab Release + promote/create-release/deploy-prod success, bare chart published 12:53Z). Applied currency formula -> molecule gd-pouM6Fd6ba (wired under epic ka-0000005495); spawned crew gardenio-awspoc-currency-confirm-lb0. No stale children. gb stop; reassess next cycle.", true},
		{"fresh-prod execution cycle 6Wf", "Gardener ka-0000000398 (fresh-prod EXECUTION PROGRAM) cycle: Phases 1+2+gate CLOSED. Phase 3 PROVE in-flight & HEALTHY — soak on canonical-prod 855 SLI samples / 0 Tier-1 breaches, load-gen+slo-collect 1/1 Running 2d23h. 3.3+3.4 CLOSED; 3.5 proof-bundle correctly blocked on soak completion; Phase 4 gated. Daily-checkpoint executor self-manages — no crew to spawn, no AWS writes. Stopping; scheduler re-fires.", true},
		// --- DROP: Class-C greetings carrying the cycle/tick shape ---
		{"mr relevance starting tick", "MR Relevance Gardener (-6frb) for monorepo: starting tick. Digest already posted today ~08:08 ET; this tick scores the 2 least-recently-scored open gardenio MRs.", true},
		// --- DROP: Escape-1 "run N/M" cadence + prior-cycle merge in a parenthetical ---
		// The "(MR !4625 merged)" aside is last-cycle context, stripped before the
		// action-guard runs, so this recap drops (ka-0000007262, iter 2026-06-19T0129).
		{"nat gardener run 1/12 paren merge", "NAT-elimination gardener (epic ka-0000007498), project gardenio — run 1/12. Edge-matrix child ka-0000005663 DONE (MR !4625 merged): NAT-free infeasible. Spawned crew on options-eval+cost-delta child gd-rAJkcyXEww. Yielding — crew in-flight + decision open.", true},
		// "triage cycle" with a prior-cycle "(MR !4463 MERGED …)" inside a
		// parenthetical — the EXACT over-fire the original acceptance warned about
		// (ka-0000005955, iter 2026-06-19T0129).
		{"triage cycle a2uz paren prior merge", "triage cycle (a2uz): 0 self-labelled (all new Lane-0 candidates already gardenio-tagged), 1 done-closed (DEVOPS-781 AI-MR review policy: fix MR !4463 MERGED to gardenio main 6/16 but ticket still in Refine -> closed Done w/ analysis), 0 crew spawned (3 survivor MRs all green+conflict-free), 0 escalations.", true},
		// --- RETAIN: genuine cycle lines that took a real action this tick ---
		{"bug gardener cycle closed 5 spawned 1", "Bug gardener (agg) cycle: 15 open bugs -> closed 5 (3 superseded E2E .168.x RC flakes; 2 gVisor layers fixed-in-merged-MR !4162/!3938). Spawned 1 crew on P3 flaky git-clone hardening (ka-0000004811). 9 remaining.", false},
		{"cut made despite zero stale", "release-owner: cuts_today=0 but CUT 2026.169.2 this cycle", false},
		{"cycle merged a real MR", "delivery cycle: merged MR !4575, spawned 0 followups", false},
		// --- RETAIN: genuine non-cycle squawks (no cadence word at all) ---
		{"release-owner cut 169.11", "release-owner: cut 2026.169.11 (pipeline 2611703836 running) golden_age=26m cuts_today=1 escalations=0", false},
		{"molecule complete merged", "AWS-POC chart currency molecule gd-pouM6Fd6ba COMPLETE: pin 2026.169.8 -> 2026.169.10. MR !4598 (bump, merged) + !4599 (apply+verify artifacts, merged). Agent-driven terraform apply clean. All 4 steps + molecule closed. Operator: Matt Baker.", false},
		{"bump done merged", "BUMP done: MR !4575 merged", false},
		{"plain merge no cadence", "Merged MR 3286 — feed dedup fix shipped", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkGardenerCycleTick(b); got != c.want {
				t.Errorf("feedSquawkGardenerCycleTick(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkGardenerCycleTick(nil) {
		t.Errorf("feedSquawkGardenerCycleTick(nil) = true, want false")
	}
}

// TestFeedSquawkStandingState covers the no-cycle-word fleet-liveness health
// recap (ka-0000001954, critic iter 2026-06-19T0129 Escape 2) — a periodic
// watchdog squawk that reports the watched machinery is up with no this-cycle
// action. Dropped unless it names a concrete completed action.
func TestFeedSquawkStandingState(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// Dropped: verbatim live fleet-liveness health recap.
		{"fleet-liveness all healthy", "fleet-liveness: all watched gardeners healthy (ka-0000007481, ka-0000003783) — status=open, enabled=true, last_checked fresh (~2m)", true},
		{"liveness guard all green", "fleet-liveness guard: all green — every watched gardener enabled=true, last_checked fresh", true},
		// Retained: a liveness card that DID take an action this cycle.
		{"liveness respawned a gardener", "fleet-liveness: gardener triage-intake went dark — respawned; CUT 2026.170.3", false},
		// Not a health recap at all: left for other filters.
		{"plain greeting", "gardener-release-owner online — starting release cycle", false},
		{"genuine cut", "release-owner: CUT 2026.170.1 (preflight GREEN)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkStandingState(b); got != c.want {
				t.Errorf("feedSquawkStandingState(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkStandingState(nil) {
		t.Errorf("feedSquawkStandingState(nil) = true, want false")
	}
}

// TestFeedScheduleFireBead covers the scheduled-gardener SCHEDULE-BEAD lifecycle
// churn filter (ka-0000000435): a cron fire bead — type=schedule, or a
// "[<ts>] Schedule: …" titled wrapper / one stamped with schedule_fire_key — is
// dropped, while a genuine work bead still renders. The 5 fixtures are verbatim
// live captures from the 2026-06-19T0129 prod capture.
func TestFeedScheduleFireBead(t *testing.T) {
	cases := []struct {
		name string
		bead *feedEventBead
		want bool
	}{
		// Dropped: the 5 verbatim live "[<ts>] Schedule: …" fire wrappers
		// (type=task, source:schedule, schedule_fire_key set).
		{"bug-response gardener fire", &feedEventBead{ID: "ka-0000004447", Type: "task", Title: "[2026-06-19T01:29] Schedule: Bug-Response Gardener (gardenio, aggressive): drain open bugs to closed", Fields: json.RawMessage(`{"schedule_fire_key":"2026-06-19T01:29"}`)}, true},
		{"keycloak migration fire", &feedEventBead{ID: "ka-0000001373", Type: "task", Title: "[2026-06-19T01:23] Schedule: Gardener: monorepo Keycloak ECS→EKS migration", Fields: json.RawMessage(`{"schedule_fire_key":"2026-06-19T01:23"}`)}, true},
		{"fleet-liveness guard fire", &feedEventBead{ID: "ka-0000006107", Type: "task", Title: "[2026-06-19T01:23] Schedule: Fleet-liveness guard: alert when janitor/triage gardeners go dark (every 2h)", Fields: json.RawMessage(`{"schedule_fire_key":"2026-06-19T01:23"}`)}, true},
		{"resume-latency watchdog fire", &feedEventBead{ID: "ka-0000004338", Type: "task", Title: "[2026-06-19T00:23] Schedule: Gardener: resume-latency regression watchdog (succeeds ka-0000000069)", Fields: json.RawMessage(`{"schedule_fire_key":"2026-06-19T00:23"}`)}, true},
		{"bug-response gardener fire 2", &feedEventBead{ID: "ka-0000001969", Type: "task", Title: "[2026-06-18T23:29] Schedule: Bug-Response Gardener (gardenio, aggressive): drain open bugs to closed", Fields: json.RawMessage(`{"schedule_fire_key":"2026-06-18T23:29"}`)}, true},
		// Dropped: a native type=schedule bead, regardless of title.
		{"native schedule type", &feedEventBead{ID: "gd-sched", Type: "schedule", Title: "Bug-Response Gardener (gardenio, aggressive)"}, true},
		// Dropped: title shape alone (no field) still catches a fire wrapper.
		{"title shape only", &feedEventBead{ID: "gd-t", Type: "task", Title: "[2026-06-19T01:29] Schedule: Some gardener"}, true},
		// Retained: genuine work beads.
		{"genuine created card", &feedEventBead{ID: "gd-d", Type: "task", Title: "DEVOPS-786: retry docker push on transient registry 5xx"}, false},
		{"task mentioning schedule in prose", &feedEventBead{ID: "gd-p", Type: "task", Title: "Fix the schedule-fire reaper race"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedScheduleFireBead(c.bead); got != c.want {
				t.Errorf("feedScheduleFireBead(%v) = %v, want %v", c.bead, got, c.want)
			}
		})
	}
}

// TestFeedFormulaStepBead covers the scheduled-audit formula/molecule STEP-bead
// filter (ka-0000004173): the CDQR/GRADE audit machinery the formula engine fans
// out every pour. All 14 DROP fixtures are VERBATIM live captures from the
// 2026-06-19T0751 critic capture (Gardener-Page CDQR molecule gd-3QqJPSgkAw + its
// 6 step tasks, People-dir CDQR molecule gd-GWCidrLLq0 + its 6 step tasks). The
// RETAIN fixtures are the durable artifacts an audit PRODUCES — the filed bug
// beads (type=bug, which reuse the kind:*-cdqr label for grouping) and a genuine
// work bead — which must keep rendering.
func TestFeedFormulaStepBead(t *testing.T) {
	cases := []struct {
		name string
		bead *feedEventBead
		want bool
	}{
		// --- DROP: Gardener-Page CDQR audit — molecule head + 6 step tasks (verbatim). ---
		{"gardener-page molecule head (static-authored, no token/label)", &feedEventBead{ID: "gd-3QqJPSgkAw", Type: "molecule", Title: "Gardener-Page CDQR Audit", CreatedBy: "static", Labels: []string{"project:gardenio"}}, true},
		{"gardener-page warm-up step", &feedEventBead{ID: "gd-iZ4wxXiKey", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): warm up http://localhost:8095 and pick/verify the audit gardener", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "needs-intake", "project:gardenio"}}, true},
		{"gardener-page capture step", &feedEventBead{ID: "gd-2s4hfMQcQn", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): capture screenshots + DOM for /gardener/", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"}}, true},
		{"gardener-page metrics step", &feedEventBead{ID: "gd-HopULQZabl", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): compute objective metrics for the five dimensions", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"}}, true},
		{"gardener-page score step", &feedEventBead{ID: "gd-SUmEfExS0X", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): score Correctness / Density / Quality / Relevance / Operability", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"}}, true},
		{"gardener-page file-bugs step", &feedEventBead{ID: "gd-zuTtGBS7Cs", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): file bug beads per low-scoring dimension", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"}}, true},
		{"gardener-page publish step", &feedEventBead{ID: "gd-CMpQhP2AEL", Type: "task", Title: "Gardener-page CDQR ({{fire_time_iso}}): publish roll-up report and land MR", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"}}, true},
		// --- DROP: People-dir CDQR audit — molecule head + 6 step tasks (verbatim). ---
		{"people-dir molecule head (static-authored, no token/label)", &feedEventBead{ID: "gd-GWCidrLLq0", Type: "molecule", Title: "People-Directory CDQR Audit", CreatedBy: "static", Labels: []string{"project:gardenio"}}, true},
		{"people-dir warm-up step", &feedEventBead{ID: "gd-sVInPirSbB", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): warm up http://localhost:8095/people", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "needs-intake", "project:gardenio"}}, true},
		{"people-dir capture step", &feedEventBead{ID: "gd-2sCuY00RCA", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): capture screenshots + DOM for /people", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "project:gardenio"}}, true},
		{"people-dir metrics step", &feedEventBead{ID: "gd-XfuTk50GMQ", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): compute objective metrics for the five dimensions", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "project:gardenio"}}, true},
		{"people-dir score step", &feedEventBead{ID: "gd-2N3U5HPGP2", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): score Correctness / Density / Quality / Relevance / Dedup", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "project:gardenio"}}, true},
		{"people-dir file-bugs step", &feedEventBead{ID: "gd-6fvgb54ZGE", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): file bug beads per low-scoring dimension", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "project:gardenio"}}, true},
		{"people-dir publish step", &feedEventBead{ID: "gd-zrFAKML9I1", Type: "task", Title: "People-dir CDQR ({{fire_time_iso}}): publish roll-up report and land MR", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:people-cdqr", "project:gardenio"}}, true},
		// --- DROP: a step whose token already expanded still carries the audit label. ---
		{"expanded-token step still labelled kind:*-cdqr", &feedEventBead{ID: "gd-PfVnRHmdYD", Type: "task", Title: "Users-page CDQR (20260617-cycle1): capture screenshots + DOM for /u/", CreatedBy: "thread-1781703439-011109", Labels: []string{"area:beads-viewer", "kind:users-cdqr", "project:gardenio"}}, true},
		// --- DROP: a Grafana GRADE step carries the literal token but NO kind:*-cdqr. ---
		{"grafana GRADE step via {{ token", &feedEventBead{ID: "gd-rfKeWvVVcv", Type: "task", Title: "Grafana GRADE ({{fire_time_iso}}): cross-check every displayed value against VictoriaMetrics", CreatedBy: "static", Labels: []string{"area:grafana", "project:gardenio"}}, true},
		// --- DROP: non-CDQR rubric lanes. The suffix check was once "-cdqr" only, so
		// these leaked their whole 6-step molecule into the feed once their titles had
		// interpolated (the {{ token in (b) only masks the bug pre-expansion). Each is
		// a real lane observed in the fleet: kind:users-flat, kind:grafana-grade,
		// kind:users-camp (the /people + /u/{slug} CAMP lane, epic gd-g35z8CQNt5). ---
		{"users-FLAT step (expanded title, kind:users-flat)", &feedEventBead{ID: "gd-flat1", Type: "task", Title: "Users-FLAT (2026-07-02T09:23:00Z): compute objective metrics for the four dimensions", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:users-flat", "project:gardenio"}}, true},
		{"grafana-grade step (expanded title, kind:grafana-grade)", &feedEventBead{ID: "gd-grade1", Type: "task", Title: "Grafana GRADE (2026-07-20T0600): score the dashboard rubric", CreatedBy: "static", Labels: []string{"area:grafana", "kind:grafana-grade", "project:gardenio"}}, true},
		{"users-CAMP step (expanded title, kind:users-camp)", &feedEventBead{ID: "gd-camp1", Type: "task", Title: "Users-CAMP (2026-07-26T2000): compute objective metrics incl. link/route integrity", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:users-camp", "project:gardenio"}}, true},
		{"maps CLUB step (expanded title, kind:maps-club)", &feedEventBead{ID: "gd-club1", Type: "task", Title: "Maps CLUB (2026-06-01T0300): score Clarity / Legibility / Utility / Beauty", CreatedBy: "static", Labels: []string{"area:beads-viewer", "kind:maps-club", "project:gardenio"}}, true},
		// --- RETAIN: artifacts of the NEW lanes too — the vessel-type guard must still
		// win over the widened label match, else broadening the filter would start
		// eating the bugs these audits file (the whole point of the audit). ---
		{"users-CAMP filed bug (type=bug, kept)", &feedEventBead{ID: "gd-camp2", Type: "bug", Title: "beads-viewer /u/{slug}: href='#recent-commits' points at an id that does not exist", CreatedBy: "gardenio-users-camp-audit", Labels: []string{"area:beads-viewer", "camp:integrity", "kind:users-camp", "project:gardenio"}}, false},
		{"users-CAMP tracking epic (type=epic, kept)", &feedEventBead{ID: "gd-camp3", Type: "epic", Title: "Users-CAMP lane: CAMP-rubric audit gardener over /people + /u/{slug}", CreatedBy: "gardenio-trigger-reviver", Labels: []string{"area:beads-viewer", "kind:users-camp", "project:gardenio"}}, false},
		// --- RETAIN: a kind: label that merely ENDS in a rubric-ish word is not a lane.
		// Guards against the suffix set over-matching real work labels. ---
		{"kind:upgrade is not a -grade audit lane", &feedEventBead{ID: "gd-up1", Type: "task", Title: "Bump the chart dependency", CreatedBy: "gardenio-crew-abc", Labels: []string{"kind:upgrade", "project:gardenio"}}, false},
		// --- RETAIN: the durable artifacts an audit produces (type=bug, kind:*-cdqr). ---
		{"filed bug — quality dimension (type=bug, kept)", &feedEventBead{ID: "gd-hdLxB9X8Uk", Type: "bug", Title: "Users page CDQR: Quality scored 6/10 — visible Loading placeholders, repetitive empty banners", CreatedBy: "gardenio-userspage-cdqra-20260617cycle1-m1c", Labels: []string{"area:beads-viewer", "cdqr:quality", "kind:users-cdqr", "needs-intake", "project:gardenio"}}, false},
		{"filed bug — correctness dimension (type=bug, kept)", &feedEventBead{ID: "ka-0000004155", Type: "bug", Title: "Users page CDQR: Correctness scored 6/10 — stale (0) count chips, perpetual Loading", CreatedBy: "gardenio-userspage-cdqra-20260617cycle1-m1c", Labels: []string{"area:beads-viewer", "cdqr:correctness", "kind:users-cdqr", "needs-intake", "project:gardenio"}}, false},
		// --- RETAIN: the CDQR/legibility EPIC (type=epic) the audit lives under. ---
		{"audit epic (type=epic, kept)", &feedEventBead{ID: "ka-0000000581", Type: "epic", Title: "Per-user homepage (/u/{slug}) accomplishment-legibility gardener: rubric + formula + schedule", CreatedBy: "matthew.baker", Labels: []string{"kind:users-cdqr", "project:gardenio"}}, false},
		// --- RETAIN: genuine work beads (no audit label, no token, not a static vessel). ---
		{"genuine task", &feedEventBead{ID: "gd-real1", Type: "task", Title: "Fix feed dedup", CreatedBy: "gardenio-feed-crew-abc", Labels: []string{"project:gardenio"}}, false},
		{"genuine task mentioning a page in prose", &feedEventBead{ID: "gd-real2", Type: "task", Title: "Gardener page: widen the lane filter", CreatedBy: "gardenio-crew-xyz", Labels: []string{"area:beads-viewer", "project:gardenio"}}, false},
		{"non-static molecule (a real workflow molecule) is kept", &feedEventBead{ID: "gd-mol", Type: "molecule", Title: "AWS-POC chart currency molecule", CreatedBy: "gardenio-aws-poc-gardener", Labels: []string{"project:gardenio"}}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedFormulaStepBead(c.bead); got != c.want {
				t.Errorf("feedFormulaStepBead(%q) = %v, want %v", titleOf(c.bead), got, c.want)
			}
		})
	}
}

// TestClassifyFeedEvent_DropsCDQRFormulaStep confirms the classifier drops a CDQR
// audit step bead end-to-end (ok=false) across the created/claimed/updated/closed
// lifecycle, so a single audit pour's ~7 step beads never reach a card — while a
// bug bead the audit FILES still surfaces (ka-0000004173).
func TestClassifyFeedEvent_DropsCDQRFormulaStep(t *testing.T) {
	step := func() []byte {
		b, err := json.Marshal(map[string]any{
			"bead": map[string]any{
				"id": "gd-zuTtGBS7Cs", "type": "task",
				"title":      "Gardener-page CDQR ({{fire_time_iso}}): file bug beads per low-scoring dimension",
				"status":     "open",
				"created_by": "static",
				"labels":     []string{"area:beads-viewer", "kind:gardener-cdqr", "project:gardenio"},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}()
	for _, topic := range []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"} {
		if _, _, _, ok := classifyFeedEvent(topic, step); ok {
			t.Errorf("CDQR audit step should be dropped for %s (ok=false)", topic)
		}
	}

	filedBug, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id": "gd-hdLxB9X8Uk", "type": "bug",
			"title":      "Users page CDQR: Quality scored 6/10 — visible Loading placeholders",
			"status":     "open",
			"created_by": "gardenio-userspage-cdqra-20260617cycle1-m1c",
			"labels":     []string{"area:beads-viewer", "cdqr:quality", "kind:users-cdqr", "needs-intake", "project:gardenio"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, _, ok := classifyFeedEvent("beads.bead.created", filedBug); !ok {
		t.Errorf("a bug bead the audit filed should be surfaced (ok=true)")
	}
}

// titleOf is a nil-safe accessor for test error messages.
func titleOf(b *feedEventBead) string {
	if b == nil {
		return "<nil>"
	}
	return b.Title
}

// TestFeedSquawkClassifier_LiveNoiseDistribution is the end-to-end acceptance
// for ka-0000001954: every one of the noise squawks from the prod captures
// (2026-06-18T1326 + the 2026-06-19T0129 Escape-1/2 additions) must be dropped
// by the combined classifier (heartbeat || noop || cycle-tick || standing-state),
// and the genuine + canonical retains must survive. Titles are verbatim live
// captures.
func TestFeedSquawkClassifier_LiveNoiseDistribution(t *testing.T) {
	drop := func(b *feedEventBead) bool {
		return feedSquawkHeartbeat(b) || feedSquawkNoopTick(b) || feedSquawkGardenerCycleTick(b) || feedSquawkStandingState(b)
	}
	noise := []string{
		// Class A — zero-delta cycle ticks (4 triage + 2 aws-poc no-drift/no-pass)
		"triage cycle (cdho): 0 self-labelled (0 Lane-0 candidates), 0 dups closed, 0 done-closed (all 6 survivor MRs still open/in-review), 0 crew spawned, 0 escalations.",
		"triage cycle (qy7i): 0 self-labelled (Lane 0 clean), 0 dups closed, 0 done-closed (no survivor MR merged yet), 0 crew spawned, 0 escalations (perf-obs resolved via gd-sbGQZQ3Anu).",
		"triage cycle (susj): 0 self-labelled (Lane 0 clean), 0 dups closed, 0 done-closed, 0 crew spawned, 0 escalations (perf-obs resolved gd-sbGQZQ3Anu).",
		"triage-intake: orphaned-open swept=0, abandoned-reopened=0, lane=needs-intake (no-op cycle — all open work beads parented or already labeled)",
		"AWS-POC currency cycle (mc0p): NO drift — pin=2026.169.10 IS latest golden (top GitLab Release + bare tag). No molecule in flight.",
		"AWS-POC currency cycle (gardener wg3z): NO new pass — 169.8->169.10 pass ALREADY IN FLIGHT & HEALTHY (molecule gd-pouM6Fd6ba).",
		// Class B — gardener cycle status recaps
		"Gardener cycle [59zz] 2026-06-18T13:15Z: Phase 1+2+gate CLOSED. Phase 3 active — 3.2 soak HEALTHY in-flight, 803 SLI samples / 0 Tier-1 breaches.",
		"Gardener ka-0000000398 (fresh-prod EXECUTION PROGRAM) cycle: Phases 1+2+gate CLOSED. Phase 3 PROVE in-flight & HEALTHY.",
		"Gardener ka-0000000398 (fresh-prod EXECUTION PROGRAM) cycle: Phases 1+2+gate CLOSED. Phase 3 PROVE in-flight & HEALTHY — soak on canonical-prod 855 SLI samples / 0 Tier-1 breaches.",
		"CI-MINUTES gardener cycle 14: Levers A+B+C+D live (2.9x off baseline; rwx-main verified 73-100s vs ~1800s). Final lever MR !4570 Draft+GREEN.",
		"CI-minutes gardener cycle 12: Levers A+B+C+D live (rwx-main 93s, was 1800s; 2.9x off baseline). Final lever MR !4570 GREEN+Draft.",
		"CI-MINUTES-REDUCTION gardener (gardenio) cycle 13 — assessing: Levers A+B+C+D landed; final lever ka-0000001459 (MR !4570) pending operator decision.",
		"Relevance tick 06-18 ~13:0x UTC (-6frb, LIVE): progress=1. Scored 2 stalest: !1337 → 9.2 KEEP+digest; !1339 → 8.4 KEEP digest-HELD. No hard gates → 0 closes.",
		"MR Relevance Gardener (-6frb) for monorepo: starting tick. Digest already posted today ~08:08 ET; this tick scores the 2 least-recently-scored open gardenio MRs.",
		"AWS-POC currency cycle (gardener ejyz): DRIFT 2026.169.8 -> 2026.169.10 (2 patches). Applied currency formula -> molecule gd-pouM6Fd6ba; spawned crew. gb stop; reassess next cycle.",
		// Class C — greetings the heartbeat filter missed
		"AWS-POC currency gardener (gardenio) waking up. Checking chart pin drift and lane health for customer-poc POC (acct 123456789012).",
		"AWS-POC currency gardener online (project: gardenio). Checking chart-pin drift on POC account 123456789012.",
		"Feed-relevance critic gardener (rubric-s-xxo5) online — project:gardenio. P1 squawk-noise fix ka-0000004501 (MR !4593, feedSquawkNoopTick) confirmed LIVE in prod beads-viewer 2026.169.10. Pouring S-L-R critic formula ka-0000001036 against fresh /feed.",
		"gardener-delivery online (project gardenio): starting MR-walk cycle",
		"triage-intake gardener online (project:gardenio). Starting orphaned/abandoned bead sweep into needs-intake lane.",
		"delivery: merged 0, spawned 0, green-waiting 0 — all non-draft MRs human-authored, no buildable beads in lane",
		// 2026-06-19T0129 Escape-1: prior-cycle merge/close token buried in a
		// parenthetical / later recap clause must NOT re-retain (verbatim live).
		"NAT-elimination research gardener online (epic ka-0000007498, project gardenio). Both research children CLOSED (edge-map ka-0000005663 + options/cost gd-rAJkcyXEww, merged !4625). Sole blocker: operator decision ka-0000003998 [P1] — pick NAT-minimized topology.",
		"NAT-elimination gardener (epic ka-0000007498), project gardenio — run 1/12. Edge-matrix child ka-0000005663 DONE (MR !4625 merged): NAT-free infeasible. Spawned crew on options-eval+cost-delta child gd-rAJkcyXEww. Yielding — crew in-flight + decision open.",
		"triage cycle (a2uz): 0 self-labelled (all new Lane-0 candidates already gardenio-tagged), 1 done-closed (DEVOPS-781 AI-MR review policy: fix MR !4463 MERGED to gardenio main 6/16 but ticket still in Refine -> closed Done w/ analysis), 0 crew spawned (3 survivor MRs all green+conflict-free), 0 escalations.",
		// 2026-06-19T0129 Escape-2: no-cycle-word standing-state recaps.
		"fleet-liveness: all watched gardeners healthy (ka-0000007481, ka-0000003783) — status=open, enabled=true, last_checked fresh (~2m)",
		"Bug gardener (agg): 8 open, 0 in-flight (0 live/1 stale non-bug), 0 closed, 0 spawned, 1 decision filed (gd-EU7ggFuqU0: drain 3 stale P2 tail -> queue to 5). 4 P0/P1 merged+blocked on operator/gVisor redrive; 1 P1 infra flake. No actionable code-crew work this cycle.",
		"triage-intake gardener (project=gardenio) waking for orphaned/abandoned bead sweep into needs-intake lane",
		"thread-1781830866-842059 (gardener, gardenio) on it — investigating beads-viewer 503 at /beads/agents",
		// 2026-06-19T0751 greeting escapes: "(project:…)" clause breaks the literal
		// "gardener online" adjacency; "sweeping"/"driving" verbs + "spinning up"
		// without "online" leaked (ka-0000001954 refresh).
		"triage gardener (project:gardenio) online — sweeping orphaned beads into needs-intake",
		"Bug-Response gardener (project:gardenio) online — driving open bugs to resolved",
		"First-mate gardener spinning up — sweeping gardener trajectories for stalls",
	}
	for _, title := range noise {
		b := &feedEventBead{Type: "message", Title: title, Labels: []string{"squawk"}}
		if !drop(b) {
			t.Errorf("NOISE squawk should drop but RENDERS: %q", title)
		}
	}

	retain := []string{
		// 3 genuine squawks from the 1326 capture
		"release-owner: cut 2026.169.11 (pipeline 2611703836 running) golden_age=26m cuts_today=1 escalations=0",
		"release-owner: CUT 2026.169.10 (preflight GREEN, golden .169.9 age 30m) cuts_today=1 escalations=0 prod-drift=0",
		"AWS-POC chart currency molecule gd-pouM6Fd6ba COMPLETE: pin 2026.169.8 -> 2026.169.10. MR !4598 (bump, merged) + !4599 (apply+verify artifacts, merged).",
		// canonical retains already covered by prior tests
		"BUMP done: MR !4575 merged",
		":newspaper: Decision gd-x follow-through shipped — MR 26 MERGED",
		"release-owner: CUT 2026.169.2 (pipeline 12345 GREEN)",
		// a real-work cycle line must survive the cycle-tick detector
		"Bug gardener (agg) cycle: 15 open bugs -> closed 5; Spawned 1 crew on P3 flaky git-clone hardening (ka-0000004811). 9 remaining.",
		// 2026-06-19T0129 mandatory retains — genuine outcomes from the live capture.
		"release-owner: CUT 2026.169.20 (preflight GREEN, golden age 30m) cuts_today=1 escalations=0",
		"release-owner: CUT 2026.170.1 (preflight GREEN) cuts_today=1 escalations=0 prod-drift=0",
		"delivery: merged 1 (!4630 docs/NAT research), spawned 0, green-waiting 1 (!4619 prod-terraform gated on decision gd-P0NXQyEzol). Ready queue: no unblocked buildable beads in lane.",
	}
	for _, title := range retain {
		b := &feedEventBead{Type: "message", Title: title, Labels: []string{"squawk"}}
		if drop(b) {
			t.Errorf("GENUINE squawk should RENDER but dropped: %q", title)
		}
	}
}

func TestRenderFeedCard_Claimed(t *testing.T) {
	b := &feedEventBead{
		ID:       "gd-xyz",
		Type:     "task",
		Title:    "Wire up <feed> & rail",
		Status:   "in_progress",
		Assignee: "gardenio-crew-1",
		Labels:   []string{"project:gardenio", "scope:test"},
	}
	row := renderFeedCard("/base", feedKindClaimed, b, "", fixedFeedNow(t))

	for _, want := range []string{
		`class="feed-card feed-card-claimed"`,
		`data-bead-id="gd-xyz"`,
		`data-kind="claimed"`,
		`data-lane="in_progress"`,
		`data-project="gardenio"`,
		`href="/base/gd-xyz"`,
		`@gardenio-crew-1`,
		"21:30:05",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("card missing %q\n%s", want, row)
		}
	}
	// Title must be HTML-escaped — no raw angle brackets in the markup.
	if strings.Contains(row, "<feed>") {
		t.Errorf("title not escaped:\n%s", row)
	}
	if !strings.Contains(row, "&lt;feed&gt;") {
		t.Errorf("expected escaped title, got:\n%s", row)
	}
}

func TestRenderFeedCard_DecisionAndMerge(t *testing.T) {
	dec := renderFeedCard("/b", feedKindDecision, &feedEventBead{ID: "gd-d", Type: "decision", Title: "Pick session strategy"}, "", fixedFeedNow(t))
	if !strings.Contains(dec, "needs you") {
		t.Errorf("decision card should label 'needs you':\n%s", dec)
	}
	mr := renderFeedCard("/b", feedKindMRMerged, &feedEventBead{ID: "gd-m", Type: "merge-request", Title: "Merge it"}, "matt", fixedFeedNow(t))
	if !strings.Contains(mr, "by matt") {
		t.Errorf("merged card should show closer:\n%s", mr)
	}
	if !strings.Contains(mr, "feed-card-mr-merged") {
		t.Errorf("merged card css class missing:\n%s", mr)
	}
}

// TestHandleFeed_RendersShell drives the /feed route end-to-end against the
// fake daemon and asserts the page chrome, view toggle, and rail render.
func TestHandleFeed_RendersShell(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="feed-page"`,
		`data-view="stream"`,             // Concept A default
		`data-events-url="/feed/events"`, // SSE wiring
		`class="feed-viewtoggle"`,
		`class="feed-stream"`,
		`feed.js`,
		"Open decisions", // rail "needs you" panel
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/feed page missing %q", want)
		}
	}
}

// TestFeedPage_LayoutGuardsAgainstMobileOverflow is the static guardrail for
// ka-0000002820 (dedup ka-0000001664): the feed page — also served at "/" — must
// not overflow a 375px viewport. Two CSS guards, both asserted on the rendered
// <style>, so a future edit that reverts either one fails the build:
//
//   - .feed-layout collapses to minmax(0, 1fr) at ≤60rem, NOT a bare 1fr. A bare
//     1fr grid track floors at min-content, so an unbroken token in a rail item
//     forces the collapsed column — and the page — past 375px (measured
//     scrollWidth 708 > 375 before the fix). Same class as .charts-grid
//     (ka-0000004897) and the /plans cockpit (ka-0000000256).
//   - .feed-rail-list li carries min-width:0 + overflow-wrap:anywhere so a long
//     bead id / path / URL in a rail title breaks instead of widening the track.
func TestFeedPage_LayoutGuardsAgainstMobileOverflow(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf(`GET "/" = %d, want 200; body=%s`, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The ≤60rem collapse must use minmax(0, 1fr), never a bare 1fr. Assert the
	// exact media rule declaration is present (a bare `1fr;` would not match).
	if !strings.Contains(body, "@media (max-width: 60rem) { .feed-layout { grid-template-columns: minmax(0, 1fr); } }") {
		t.Error(".feed-layout ≤60rem collapse is not minmax(0, 1fr) — a bare 1fr track " +
			"floors at min-content and overflows narrow viewports (ka-0000002820)")
	}

	// The rail list items must carry the overflow-wrap break guard. Scope the
	// window to the `.feed-rail-list li {` declaration so a sibling rule's
	// properties can't satisfy the check.
	li := sliceAfter(body, ".feed-rail-list li {")
	if li == "" {
		t.Fatal(".feed-rail-list li rule not found in rendered page")
	}
	if end := strings.Index(li, "}"); end >= 0 {
		li = li[:end]
	}
	if !strings.Contains(li, "min-width: 0") || !strings.Contains(li, "overflow-wrap: anywhere") {
		t.Error(".feed-rail-list li lacks min-width:0 + overflow-wrap:anywhere — a long " +
			"unbroken token in a rail title widens the collapsed column past 375px (ka-0000002820)")
	}
}

// TestHandleFeed_DefaultMutedChips asserts the routine-churn families start
// muted by default (aria-pressed="false") while the actionable families start
// shown (aria-pressed="true"). Per Matt (ka-0000005238): the feed should not
// show normal events (created/updated), trigger updates (gardener run-tally edits
// classify as feedKindUpdated), or session creation (spawned/stopped) by default
// — each is one click to re-admit via the mute chip. feed.js seeds mutedTypes
// off these server-rendered aria-pressed states, so the default IS the markup.
func TestHandleFeed_DefaultMutedChips(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Muted by default — normal events + squawk firehose. The whole agent family
	// (spawn/stop/gate) is no longer surfaced at all (feedKindForBead drops it,
	// ka-0000003549), so it has no chip.
	mutedByDefault := []string{"created", "updated", "squawk"}
	for _, typ := range mutedByDefault {
		want := `data-type="` + typ + `" aria-pressed="false"`
		if !strings.Contains(body, want) {
			t.Errorf("chip %q should be muted by default (missing %q)", typ, want)
		}
	}

	// Agent-family chips were removed — their events never load (ka-0000003549).
	for _, typ := range []string{"spawned", "stopped", "gate"} {
		if strings.Contains(body, `data-type="`+typ+`"`) {
			t.Errorf("chip %q should not exist — the agent family is no longer surfaced", typ)
		}
	}

	// Shown by default — the actionable signal the feed exists to surface.
	shownByDefault := []string{"decision", "blocked", "claimed", "closed", "mr-merged", "pipeline", "mail"}
	for _, typ := range shownByDefault {
		want := `data-type="` + typ + `" aria-pressed="true"`
		if !strings.Contains(body, want) {
			t.Errorf("chip %q should be shown by default (missing %q)", typ, want)
		}
	}
}

// TestHandleFeed_LanesView confirms ?view=lanes flips the default arrangement.
func TestHandleFeed_LanesView(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed?view=lanes", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed?view=lanes = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-view="lanes"`) {
		t.Errorf("?view=lanes should set data-view=lanes")
	}
}

// TestHandleFeed_LanesSSRPopulated is the ka-0000001008 regression: the lanes
// view must render the SAME cards the stream shows, SSR'd into their status
// lane, so ?view=lanes is not an empty page. Before the fix only a client-side
// mirror filled the lanes, so a server-rendered load left all lanes empty.
func TestHandleFeed_LanesSSRPopulated(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-open", "task", "new", "open")},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(time.Minute), Payload: feedEventData(t, "gd-claim", "task", "claimed", "in_progress")},
			{ID: 3, Topic: "beads.bead.closed", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventData(t, "gd-close", "task", "done", "closed")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed?view=lanes", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed?view=lanes = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Each populated lane body must contain at least one feed card SSR'd inside
	// it — not an empty <div data-lane="...">. Assert the open/in_progress/done
	// lane bodies each carry a card for the matching bead.
	for _, c := range []struct{ lane, beadID string }{
		{"open", "gd-open"},
		{"in_progress", "gd-claim"},
		{"done", "gd-close"},
	} {
		marker := `data-lane="` + c.lane + `" role="log"`
		idx := strings.Index(body, marker)
		if idx == -1 {
			t.Fatalf("lane body %q not found in rendered page", c.lane)
		}
		// Slice from the lane-body open tag to the end of that section and confirm
		// the matching bead card landed inside (the SSR range placed it there).
		rest := body[idx:]
		end := strings.Index(rest, "</section>")
		if end == -1 {
			end = len(rest)
		}
		if !strings.Contains(rest[:end], `data-bead-id="`+c.beadID+`"`) {
			t.Errorf("lane %q should be SSR-populated with %s, but its body was empty:\n%s", c.lane, c.beadID, rest[:end])
		}
	}
}

// TestRenderFeedCard_OperatorAttrs asserts the card carries lower-cased
// operator identity attributes so the client can scope the feed to "mine"
// (ka-0000003403). Both the resolved email and the raw operator id are emitted.
func TestRenderFeedCard_OperatorAttrs(t *testing.T) {
	b := &feedEventBead{
		ID:              "gd-op",
		Type:            "task",
		Title:           "Owned work",
		Status:          "in_progress",
		Labels:          []string{"project:gardenio"},
		OperatedBy:      "oidc:Matt@acme.example",
		OperatedByEmail: "Matt@acme.example",
	}
	row := renderFeedCard("/b", feedKindClaimed, b, "", fixedFeedNow(t))
	for _, want := range []string{
		`data-operator="oidc:matt@acme.example"`,
		`data-operator-email="matt@acme.example"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("card missing %q\n%s", want, row)
		}
	}
}

// TestHandleFeed_DefaultScopeAndProject covers the per-viewer default scoping
// (ka-0000003403, revised ka-0000003901): an identified viewer with a stored
// default project lands on scope=everyone (the pre-populated firehose — "mine"
// hid ~95% of the agent-driven seed and left the feed showing ~2 cards) + that
// stored project, and the project dropdown + scope toggle render.
func TestHandleFeed_DefaultScopeAndProject(t *testing.T) {
	fake := newFake()
	fake.userResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{
		ID:   "gd-user",
		Type: "user",
		Fields: map[string]string{
			"email":       "matt@acme.example",
			"preferences": `{"default_spawn_project":"gardenio"}`,
		},
	}}}
	fake.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {},
		"monorepo": {},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-scope="everyone"`,                 // identified viewer now defaults to everyone (ka-0000003901)
		`data-project="gardenio"`,               // stored default project applied
		`data-viewer-email="matt@acme.example"`, // viewer identity exposed to JS
		`class="feed-scopetoggle"`,              // Mine/Everyone toggle present
		`class="feed-project"`,                  // project dropdown present
		`<option value="monorepo"`,              // dropdown lists known projects
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/feed page missing %q", want)
		}
	}
	// The default project's option must be pre-selected.
	if !strings.Contains(body, `<option value="gardenio" selected>`) {
		t.Errorf("default project not selected in dropdown:\n%s", body)
	}
}

// TestHandleFeed_AnonymousDefaultsEveryone confirms an unidentified viewer
// gets scope=everyone (a "mine" default would render a blank page) and no
// scope toggle.
func TestHandleFeed_AnonymousDefaultsEveryone(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-scope="everyone"`) {
		t.Errorf("anonymous viewer should default to scope=everyone")
	}
	if !strings.Contains(body, `data-has-viewer="0"`) {
		t.Errorf("anonymous viewer should set data-has-viewer=0")
	}
	if strings.Contains(body, `class="feed-scopetoggle"`) {
		t.Errorf("scope toggle should be hidden for anonymous viewers")
	}
}

// TestHandleFeed_ExplicitScopeAndProjectOverride confirms URL params override
// the stored defaults — ?scope=everyone&project= widens to the whole fleet,
// all projects.
func TestHandleFeed_ExplicitScopeAndProjectOverride(t *testing.T) {
	fake := newFake()
	fake.userResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{
		ID:     "gd-user",
		Type:   "user",
		Fields: map[string]string{"email": "matt@acme.example", "preferences": `{"default_spawn_project":"gardenio"}`},
	}}}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/feed?scope=everyone&project=", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-scope="everyone"`) {
		t.Errorf("?scope=everyone should override the mine default")
	}
	if !strings.Contains(body, `data-project=""`) {
		t.Errorf("explicit ?project= should override the stored default to all-projects")
	}
}

// TestRenderFeedCard_ClaimedLiveAgentDecoration covers the live-agent dot +
// agent link on a claimed card whose assignee resolves to an alive agent, and
// the plain-text fallback when the agent is not alive / there is no resolver
// (ka-0000003918). renderFeedCardFull is the shared renderer for both the
// first-paint/backfill and the live SSE paths, so exercising it directly
// covers both — they only differ in how the resolver is constructed.
func TestRenderFeedCard_ClaimedLiveAgentDecoration(t *testing.T) {
	now := time.Now()
	claimed := func(assignee string) *feedEventBead {
		return &feedEventBead{
			ID:       "gd-claim",
			Type:     "task",
			Title:    "Claimed work",
			Status:   "in_progress",
			Assignee: assignee,
			Labels:   []string{"project:gardenio"},
		}
	}

	t.Run("alive agent shows dot + link", func(t *testing.T) {
		r := liveResolverFor(liveAgentFake(now), now)
		row := renderFeedCardFull("/b", feedKindClaimed, claimed("live-bot"),
			"gardenio", "", "1m", now, r)
		for _, want := range []string{
			`class="agent-live-icon"`,
			`href="/b/gd-agent-live"`,
			`title="agent working: live-bot"`,
			`class="agent-link"`,
			`>live-bot<`,
		} {
			if !strings.Contains(row, want) {
				t.Errorf("alive claimed card missing %q\n%s", want, row)
			}
		}
		// The "@" prefix is preserved ahead of the agent link.
		if !strings.Contains(row, `@<a class="agent-link"`) {
			t.Errorf("alive claimed card should keep @ before the agent link\n%s", row)
		}
		// No plain "@live-bot" text — the decorated link replaces it.
		if strings.Contains(row, `>@live-bot<`) {
			t.Errorf("alive claimed card should not render plain @assignee text\n%s", row)
		}
	})

	t.Run("idle agent falls back to plain @assignee", func(t *testing.T) {
		r := liveResolverFor(liveAgentFake(now), now)
		row := renderFeedCardFull("/b", feedKindClaimed, claimed("idle-bot"),
			"gardenio", "", "1m", now, r)
		if strings.Contains(row, `agent-live-icon`) {
			t.Errorf("idle assignee should not surface a live dot\n%s", row)
		}
		if !strings.Contains(row, `@idle-bot`) {
			t.Errorf("idle assignee should render plain @assignee text\n%s", row)
		}
	})

	t.Run("nil resolver falls back to plain @assignee", func(t *testing.T) {
		row := renderFeedCardFull("/b", feedKindClaimed, claimed("live-bot"),
			"gardenio", "", "1m", now, nil)
		if strings.Contains(row, `agent-live-icon`) || strings.Contains(row, `agent-link`) {
			t.Errorf("nil resolver should not decorate the actor\n%s", row)
		}
		if !strings.Contains(row, `@live-bot`) {
			t.Errorf("nil resolver should render plain @assignee text\n%s", row)
		}
	})

	t.Run("spawned card decorates like claimed", func(t *testing.T) {
		r := liveResolverFor(liveAgentFake(now), now)
		spawned := &feedEventBead{ID: "gd-a", Type: "agent", Title: "live-bot", Assignee: "live-bot"}
		row := renderFeedCardFull("/b", feedKindSpawned, spawned, "", "", "1m", now, r)
		if !strings.Contains(row, `class="agent-live-icon"`) {
			t.Errorf("spawned card with alive assignee should surface the live dot\n%s", row)
		}
	})
}

// TestFeedActorAgentDecoration_OnlyClaimedSpawned ensures the live-agent
// decoration is gated to the claimed/spawned kinds — other kinds keep their
// plain actor even when their bead carries an alive-agent assignee.
func TestFeedActorAgentDecoration_OnlyClaimedSpawned(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	b := &feedEventBead{ID: "gd-x", Type: "task", Assignee: "live-bot"}
	for _, kind := range []feedKind{feedKindClosed, feedKindBlocked, feedKindCreated, feedKindUpdated} {
		icon, link := feedActorAgentDecoration("/b", kind, b, r)
		if icon != "" || link != "" {
			t.Errorf("kind %q should not be decorated, got icon=%q link=%q", kind.Name, icon, link)
		}
	}
	// Sanity: claimed IS decorated under the same fixture.
	icon, _ := feedActorAgentDecoration("/b", feedKindClaimed, b, r)
	if icon == "" {
		t.Errorf("claimed kind with alive assignee should be decorated")
	}
}
