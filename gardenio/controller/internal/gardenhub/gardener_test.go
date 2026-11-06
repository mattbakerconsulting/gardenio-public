package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The gardener panel (WB-11) and the settings panel (WB-13) (Lands workbench-glass spec; step 5.12, gd-au7ywvWAG8).

type fakeGardeners struct {
	asked []string
	docs  map[string]*GardenerDoc
	err   error
}

func (f *fakeGardeners) Gardener(_ context.Context, id string) (*GardenerDoc, error) {
	f.asked = append(f.asked, id)
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[id]; ok {
		cp := *d
		if d.Config != nil {
			c := *d.Config
			cp.Config = &c
		}
		return &cp, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such gardener"}
}

type fakeSettings struct {
	doc *SettingsDoc
	err error
}

func (f *fakeSettings) Settings(context.Context) (*SettingsDoc, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := *f.doc
	return &cp, nil
}

// GET /garden/gardener/{id} sends the reader's gardener with every list present (empty, never null), the runs and a
// lane's beads capped and the rest counted, a long prompt clipped with its line breaks kept, and the config only to a
// viewer who may operate it; not there or not visible is a 404, a daemon failure a 502, and no reader a 503.
func TestGardenerPanelSendsTheGardener(t *testing.T) {
	var runs []GardenerRun
	for i := 0; i < gardenerRunsMax+4; i++ {
		runs = append(runs, GardenerRun{ID: "gd-run" + strings.Repeat("x", i), Headline: strings.Repeat("did it ", 100)})
	}
	var lane []ProjectBead
	for i := 0; i < gardenerLaneMax+2; i++ {
		lane = append(lane, ProjectBead{ID: "gd-b" + strings.Repeat("x", i)})
	}
	f := &fakeGardeners{docs: map[string]*GardenerDoc{
		"gd-g1": {ID: "gd-g1", Title: "guard", Prompt: "line one\nline two" + strings.Repeat(" more", 600), Runs: runs,
			Beads: []GardenerLane{{Lane: "done", Label: "Done", Count: len(lane), Beads: lane}, {Lane: "open", Label: "Open"}}, Config: &GardenerConfig{Prompt: "p"}},
		"gd-g2": {ID: "gd-g2", Title: "bare"},
	}}
	op := true
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Gardeners: f, CanOperateGardeners: func(*http.Request) bool { return op }}

	code, doc := getQueue(t, h, "/garden/gardener/gd-g1")
	if code != http.StatusOK || doc["id"] != "gd-g1" || doc["can_operate"] != true || doc["config"] == nil {
		t.Fatalf("%d %v", code, doc)
	}
	if got := len(doc["runs"].([]any)); got != gardenerRunsMax || doc["more"].(map[string]any)["runs"] != float64(4) {
		t.Errorf("%d runs, more %v; want the cap and 4 counted", got, doc["more"])
	}
	if h := doc["runs"].([]any)[0].(map[string]any)["headline"].(string); len([]rune(h)) > gardenerTitleMax+1 {
		t.Errorf("a run's headline is %d runes, want it clipped", len([]rune(h)))
	}
	l0 := doc["beads"].([]any)[0].(map[string]any)
	if len(l0["beads"].([]any)) != gardenerLaneMax || l0["count"] != float64(gardenerLaneMax+2) || doc["more"].(map[string]any)["done"] != float64(2) {
		t.Errorf("lane %v: capped, its count whole, the rest counted", l0["count"])
	}
	if l1 := doc["beads"].([]any)[1].(map[string]any); l1["beads"] == nil {
		t.Error("an empty lane sends an empty list, never null")
	}
	if p := doc["prompt"].(string); !strings.HasPrefix(p, "line one\nline two") || len([]rune(p)) > gardenerTextMax {
		t.Errorf("prompt %q…: clipped, its line breaks kept", p[:20])
	}

	_, doc = getQueue(t, h, "/garden/gardener/gd-g2")
	for _, k := range []string{"runs", "beads", "successors"} {
		if l, ok := doc[k].([]any); !ok || len(l) != 0 {
			t.Errorf("%s: %v, want an empty list", k, doc[k])
		}
	}

	op = false
	if _, doc = getQueue(t, h, "/garden/gardener/gd-g1"); doc["config"] != nil || doc["can_operate"] != nil {
		t.Errorf("a viewer who may not operate is sent no config: %v", doc["config"])
	}
	if code, _ := getQueue(t, h, "/garden/gardener/nothere"); code != http.StatusNotFound {
		t.Errorf("not a gardener: %d, want 404", code)
	}
	forbidden := &Handler{Hub: New(newFake(), "/v", nil), Gardeners: &fakeGardeners{err: &beadsapi.APIError{StatusCode: http.StatusForbidden}}}
	if code, _ := getQueue(t, forbidden, "/garden/gardener/gd-g1"); code != http.StatusNotFound {
		t.Errorf("not visible: %d, want 404", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), Gardeners: &fakeGardeners{err: errors.New("down")}}, "/garden/gardener/gd-g1"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil)}, "/garden/gardener/gd-g1"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
}

// GET /garden/settings sends the viewer's settings with its choices as lists; no session is a 401, a daemon failure a 502
// and no reader a 503.
func TestSettingsPanelSendsTheSettings(t *testing.T) {
	h := &Handler{Hub: New(newFake(), "/v", nil), Settings: &fakeSettings{doc: &SettingsDoc{Email: "op@example.com", HasBead: true, Spawn: SettingsSpawn{Role: "crew"}}}}
	code, doc := getQueue(t, h, "/garden/settings")
	if code != http.StatusOK || doc["email"] != "op@example.com" || doc["has_bead"] != true || doc["spawn"].(map[string]any)["default_spawn_role"] != "crew" {
		t.Fatalf("%d %v", code, doc)
	}
	for _, k := range []string{"projects", "roles"} {
		if l, ok := doc[k].([]any); !ok || len(l) != 0 {
			t.Errorf("%s: %v, want an empty list", k, doc[k])
		}
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), Settings: &fakeSettings{doc: &SettingsDoc{}}}, "/garden/settings"); code != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), Settings: &fakeSettings{err: errors.New("down")}}, "/garden/settings"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil)}, "/garden/settings"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
}

// The page inlines gardener.js and settings.js after the Workbench and the project panel, and the Menu carries Settings.
func TestPageCarriesGardenerAndSettings(t *testing.T) {
	w, p, g, s := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "project.js"}}`), strings.Index(pageHTML, `{{asset "gardener.js"}}`), strings.Index(pageHTML, `{{asset "settings.js"}}`)
	if g < 0 || s < 0 || g < w || g < p || s < w {
		t.Fatalf("gardener.js and settings.js must be inlined after workbench.js and project.js: %d %d %d %d", w, p, g, s)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var GARDENER = ", "W.register('gardener',", "var SETTINGS = ", "W.register('settings',", `id="cam-settings"`, "getElementById('cam-settings')"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// gardener.js's pure half runs under node: the tabs and the one shown, each tab's count, its state in words for each
// state, the line under the title, its counters, a run's words, the levers an operator is offered in each state, the
// config's rows and the editor's and the successor form's fields as the routes read them.
func TestGardenerLogicUnderNode(t *testing.T) {
	script := `
var G = require(process.argv[1]), assert = require("assert");
assert.strictEqual(G.url("gd-a b"), "/garden/gardener/gd-a%20b");
assert.deepStrictEqual(G.TABS.map(function (t) { return t[0]; }), ["about", "runs", "beads", "config", "successors", "charts"]);
assert.deepStrictEqual(G.tabsFor(false).map(function (t) { return t[0]; }), ["about", "runs", "beads", "config", "successors"], "Charts only for one who may read them");
assert.strictEqual(G.tabsFor(true).length, 6);
assert.strictEqual(G.pickTab("runs"), "runs");
assert.strictEqual(G.pickTab("nope"), "about");
assert.strictEqual(G.pickTab("charts"), "about", "a pasted tab=charts for one who may not read them");
assert.strictEqual(G.pickTab("charts", true), "charts");
assert.strictEqual(G.chartsUrl("gd-a b", "30d"), "/garden/charts/gardener?gardener=gd-a%20b&range=30d&theme=dark");
assert.strictEqual(G.chartsUrl("g", "1y"), "/garden/charts/gardener?gardener=g&range=7d&theme=dark", "a range outside the chips is 7 days");
assert.strictEqual(G.chartsFail(403), "Its charts are for operators for now.");
assert.strictEqual(G.chartsFail(0), "Could not load its charts: the garden did not answer.");
assert.strictEqual(G.chartsRetry(502), true);
assert.strictEqual(G.chartsRetry(404), false, "a gardener that is not there is not one to try again");
assert.strictEqual(G.withTab("?gardener=g&tab=runs", "beads"), "?gardener=g&tab=beads");
assert.strictEqual(G.withTab("?gardener=g&tab=runs", ""), "?gardener=g");

var d = { runs: [{}, {}], more: { runs: 3 }, beads: [{ lane: "in_progress", count: 2 }, { lane: "done", count: 9 }], successors: ["a"] };
assert.strictEqual(G.tabCount("runs", d), "5", "the runs past the cap count");
assert.strictEqual(G.tabCount("beads", d), "11", "every lane the pane lists, done ones too (gd-TCstotDcIL)");
assert.strictEqual(G.tabCount("successors", d), "1");
assert.strictEqual(G.tabCount("config", d), "");
assert.strictEqual(G.tabCount("runs", null), "");

assert.deepStrictEqual(G.state({ enabled: true, next_fire: "in 12m" }), { text: "Waiting to fire in 12m.", tone: "" });
assert.deepStrictEqual(G.state({ enabled: true, live_count: 1 }), { text: "Running: a run is out now.", tone: "" });
assert.ok(!G.running({ enabled: true, in_flight: true }), "a last run on record is not a run out");
assert.ok(G.running({ in_flight: true, partial: ["runs"] }), "unknown runs: its last run is not known to be done");
assert.deepStrictEqual(G.state({ enabled: false, live_count: 1 }), { text: "Paused: it does not fire until it is resumed.", tone: "dim" });
assert.deepStrictEqual(G.state({ enabled: true, failures: 2 }), { text: "Waiting for its next fire. 2 runs in a row failed.", tone: "ask" });
assert.strictEqual(G.state({ enabled: true, overdue: "It should have fired 2h ago." }).tone, "ask");
assert.deepStrictEqual(G.state({ closed: true, goal_state: "achieved", ended: "its goal was met" }), { text: "Ended, its goal reached: its goal was met.", tone: "ok" });
assert.deepStrictEqual(G.state({ closed: true, goal_state: "unachieved" }), { text: "Ended; its goal was not reached.", tone: "dim" });

assert.strictEqual(G.summary({ mode: "timed", project: "gardenio", run_count: 1, spend: "$3.10", standing: true }), "wakes on a schedule · in gardenio · 1 run · $3.10 spent · standing");
assert.strictEqual(G.summary({ run_count: 0 }), "0 runs");
var rows = G.counterRows({ run_count: 3, live_count: 1, did_not_run: 2, max_runs: 10, standing: true, cron: "*/30 * * * *", timezone: "UTC", next_fire: "in 5m", enabled: true });
var at = function (k) { return (rows.filter(function (r) { return r[0] === k; })[0] || [])[1]; };
assert.strictEqual(at("Runs"), "3 runs, 1 running (+2 never ran)");
assert.ok(/stops after 10 runs.*counted, not enforced/.test(at("Limits")), at("Limits"));
assert.strictEqual(at("Schedule"), "*/30 * * * * (UTC), fires in 5m");
assert.deepStrictEqual(G.runLine({ label: "run 3", live: true, state: "working", cost: "$0.40", ago: "2m" }), ["working", "run 3", "$0.40", "2m ago"]);
assert.deepStrictEqual(G.runLine({ outcome: "fail" }), ["failed"]);
assert.deepStrictEqual(G.runLine({ did_not_run: true }), ["never ran"]);

function acts(x) { return G.levers(x).map(function (l) { return l.act; }).join(" "); }
assert.strictEqual(acts({ can_operate: false, enabled: true }), "", "a viewer is offered no lever");
assert.strictEqual(acts({ can_operate: true, closed: true }), "", "an ended gardener has none");
assert.strictEqual(acts({ can_operate: true, enabled: true, mode: "timed", live_count: 1 }), "disable run-now mode cancel-run retire", "the two that stop it go last");
assert.strictEqual(acts({ can_operate: true, enabled: false, mode_ambiguous: true, mode: "timed" }), "enable run-now retire", "no mode flip when the mode is not known");
var flip = G.levers({ can_operate: true, enabled: true, mode: "event" }).filter(function (l) { return l.act === "mode"; })[0];
assert.deepStrictEqual(flip.params, [["mode", "timed"]]);
assert.strictEqual(flip.label, "Wake on a schedule");
var rt = G.levers({ can_operate: true, enabled: true, id: "gd-g", name: "guard the plan" }).filter(function (l) { return l.act === "retire"; })[0];
assert.ok(rt.danger && /^Retire the gardener “guard the plan”\?/.test(rt.ask), "Retire is danger and asks first, naming the gardener: " + rt.ask);
assert.ok(G.levers({ can_operate: true, enabled: true, id: "gd-g", live_count: 1 }).filter(function (l) { return l.act === "cancel-run"; })[0].ask.indexOf("“gd-g”") > 0, "no name: the id");

var cr = G.configRows({ cron: "0 * * * *", retry_on_failure: true });
assert.deepStrictEqual(cr.filter(function (r) { return r[0] === "Schedule (cron)"; })[0], ["Schedule (cron)", "0 * * * *"]);
assert.deepStrictEqual(cr.filter(function (r) { return r[0] === "Retry a failed run"; })[0], ["Retry a failed run", "yes"]);
assert.ok(!cr.some(function (r) { return r[0] === "Run prompt" || r[0] === "Stop spec"; }), "the long texts are their own blocks");
var cp = G.configParams({ prompt: "go", max_runs: 5, retry_on_failure: false, one_spawn_at_a_time: true, stop_json: "{}" });
var keys = cp.map(function (p) { return p[0]; });
["prompt", "terminal_goal", "cron", "cooldown_minutes", "max_concurrent", "stop_json", "progress_json", "conditions"].forEach(function (k) { assert.ok(keys.indexOf(k) >= 0, "every field is sent: " + k); });
assert.ok(keys.indexOf("retry_on_failure") < 0, "an unticked box is not sent");
assert.deepStrictEqual(cp.filter(function (p) { return p[0] === "one_spawn_at_a_time"; })[0], ["one_spawn_at_a_time", "on"]);
assert.deepStrictEqual(cp.filter(function (p) { return p[0] === "max_runs"; })[0], ["max_runs", "5"]);
assert.ok(!G.heirReady({ epic: "gd-e" }) && G.heirReady({ phase: " review " }), "a successor needs a phase, a goal or a prompt advice");
assert.deepStrictEqual(G.heirParams({ phase: " review " })[0], ["phase", "review"]);
assert.ok(G.REFRESH_S >= 10);
`
	runNodeLogic(t, "gardener.js", script)
}

// settings.js's pure half runs under node: the rows it shows (a blank says what it falls back to), a Slack channel id
// checked as the route checks it, a select's choices keeping a stored value outside them, and the save's form: scope=garden
// and only the fields the panel edits.
func TestSettingsLogicUnderNode(t *testing.T) {
	script := `
var S = require(process.argv[1]), assert = require("assert");
assert.strictEqual(S.url("me"), "/garden/settings");
assert.deepStrictEqual(S.spawnRows({}), [["Default project", "none (gardenio)"], ["Default role", "none (crew)"], ["Prompt template", "none"], ["For a selection", "one agent for all of them"]]);
assert.strictEqual(S.spawnRows({ spawn_per_selection: "per-bead", default_spawn_prompt_template: "x" })[3][1], "one agent for each bead");
assert.strictEqual(S.spawnRows({ default_spawn_prompt_template: "x" })[2][1], "set (below)");
assert.deepStrictEqual(S.identityRows({ team: "core" }), [["Display name", "—"], ["Pronouns", "—"], ["Team", "core"], ["Slack DM channel", "—"]]);
assert.strictEqual(S.channelProblem(""), "");
assert.strictEqual(S.channelProblem("D0123ABC"), "");
assert.ok(S.channelProblem("#general"));
assert.deepStrictEqual(S.choices(["a", "b"], "z"), ["", "a", "b", "z"]);
assert.deepStrictEqual(S.choices(["a"], "a"), ["", "a"]);
var f = S.formParams({ default_spawn_project: " gardenio ", default_spawn_prompt_template: "  keep {{id}}  ", pronouns: " they/them " });
assert.deepStrictEqual(f[0], ["scope", "garden"]);
assert.deepStrictEqual(f.map(function (p) { return p[0]; }).slice(1), ["default_spawn_project", "default_spawn_role", "default_spawn_prompt_template", "spawn_per_selection", "display_name", "pronouns", "team", "slack_channel_id"]);
assert.deepStrictEqual(f[1], ["default_spawn_project", "gardenio"]);
assert.deepStrictEqual(f[3], ["default_spawn_prompt_template", "  keep {{id}}  "], "the prompt template is sent as typed");
assert.ok(!f.some(function (p) { return p[0] === "vibe_mode" || /^section_/.test(p[0]); }), "nothing the panel does not edit");
`
	runNodeLogic(t, "settings.js", script)
}

// runNodeLogic runs script under node with web/<file> as its first argument; it skips when node is not installed.
func runNodeLogic(t *testing.T, file, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := filepath.Abs(filepath.Join("web", file))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "-e", script, src).CombinedOutput(); err != nil {
		t.Fatalf("node %s test failed: %v\n%s", file, err, out)
	}
}

// fixtureGardenerPanels is the card world's gardener reader (card_browser_test.go): gd-GardTrig01, "guard the plan", the
// gardenio project's trigger (fixtureProjects), timed, running, with three runs (one live, one failed), the beads its runs
// touched, a successor and its config; gd-GardSched1, "nightly sweep", paused; and gd-GardDone01, ended with its goal reached.
func fixtureGardenerPanels(now time.Time) GardenerReader {
	cfg := &GardenerConfig{Prompt: "Walk every crew PR to green; merge what is ready.", TerminalGoal: "Every crew PR is merged.", RunBudget: "40", MaxRuns: "40",
		RunTimeout: "30m", Cron: "*/20 * * * *", Cooldown: "5", MaxConcurrent: "1", OneSpawnAtATime: true, StopJSON: "{\n  \"max_runs\": 40\n}"}
	return &fakeGardeners{docs: map[string]*GardenerDoc{
		"gd-GardTrig01": {ID: "gd-GardTrig01", Title: "Gardener: guard the plan", Name: "guard the plan", Project: "gardenio", Status: "open", Enabled: true, Mode: "timed",
			Goal: "Every crew PR is merged.", Prompt: cfg.Prompt, Wakes: []string{"Every 20 minutes: */20 * * * * (UTC)"}, Stops: []string{"ends the loop: after 40 runs"},
			Cron: "*/20 * * * *", Timezone: "UTC", NextFire: "in 7m", RunsSpawned: 12, RunsCompleted: 11, MaxRuns: 40, RunBudget: 40, RunCount: 3, LiveCount: 1,
			EngineCount: "12 spawned, 11 finished (lifetime tally; older runs pruned)", Spend: "$14.20", Owner: "Matt", InFlight: true, LastRun: "gd-agentBeadW",
			Successors: []string{"phase=review · goal=Every merged PR is deployed."},
			Runs: []GardenerRun{
				{ID: "gd-agentBeadW", Name: "gardenio-guard-run-12", Label: "run 12", State: "working", Live: true, Headline: "Walking PR #1043 to green", Cost: "$0.82", Turns: "31 turns", Ago: "4m", TaskID: "gd-EdgeIdP002", TaskTitle: "Roll the edge IdP out"},
				{ID: "gd-GardRun011", Name: "gardenio-guard-run-11", Label: "run 11", Outcome: "fail", Headline: "CI red on TestTourUnderChromium; filed a bug", Cost: "$1.10", Ago: "1h"},
				{ID: "gd-GardRun010", Name: "gardenio-guard-run-10", Label: "run 10", Outcome: "ok", Headline: "Merged PR #1041", Cost: "$0.95", Ago: "3h"},
			},
			Beads: []GardenerLane{
				{Lane: "in_progress", Label: "In progress", Count: 1, Beads: []ProjectBead{{ID: "gd-EdgeIdP002", Title: "Roll the edge IdP out", Type: "task", Status: "in_progress"}}},
				{Lane: "blocked", Label: "Blocked", Count: 1, Beads: []ProjectBead{{ID: "gd-EdgeIdP003", Title: "Turn the old IdP off", Type: "task", Status: "blocked"}}},
				{Lane: "open", Label: "Open", Beads: []ProjectBead{}},
				{Lane: "done", Label: "Done", Count: 1, Beads: []ProjectBead{{ID: "gd-QueueInb002", Title: "Bird poses for tool calls", Type: "epic", Status: "closed"}}},
			},
			Config: cfg},
		"gd-GardSched1": {ID: "gd-GardSched1", Title: "nightly sweep", Name: "nightly sweep", Project: "gardenio", Status: "open", Mode: "timed", Cron: "0 3 * * *", RunsSpawned: 3, RunCount: 3,
			Config: &GardenerConfig{Prompt: "Sweep stale beads.", Cron: "0 3 * * *"}},
		"gd-GardDone01": {ID: "gd-GardDone01", Title: "drive the epic", Name: "drive the epic", Project: "gardenio", Status: "closed", Closed: true, GoalState: "achieved",
			Ended: "every child of its epic closed", RunsSpawned: 7, RunsCompleted: 7, RunCount: 7},
	}}
}

// fixtureSettings is the card world's settings reader: an operator with a user bead, a default role and a prompt template.
func fixtureSettings() SettingsReader {
	return &fakeSettings{doc: &SettingsDoc{Email: "operator@example.com", HasBead: true, BeadID: "gd-UserOp0001",
		Spawn:    SettingsSpawn{Project: "gardenio", Role: "crew", Prompt: "Work {{id}}: {{title}}."},
		Identity: SettingsIdentity{DisplayName: "Operator", Pronouns: "they/them", Team: "core"},
		Projects: []string{"acme", "gardenio", "music"}, Roles: []string{"crew", "gardener", "thread"}}}
}

// The gardener panel in a real Chromium: a gardener's row in the project panel opens it; its state, levers and tabs, each
// tab's rows; a lever posts and says so; the config editor saves and a refusal says why; a successor is added; a paused one
// and an ended one say so. What is asserted lives in testdata/gardener_browser.js.
func TestGardenerUnderChromium(t *testing.T) {
	underChromium(t, "gardener_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}

// The settings panel in a real Chromium: the Menu's Settings opens it, it shows the viewer's settings, and the editor saves
// with scope=garden and only its fields; a refused channel id says why. What is asserted lives in testdata/settings_browser.js.
func TestSettingsUnderChromium(t *testing.T) {
	underChromium(t, "settings_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
