package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// The project panel (Lands workbench-glass spec WB-10; step 5.11, gd-fT15vJniWQ).

type fakeProjects struct {
	asked []string
	docs  map[string]*ProjectDoc
	err   error
}

func (f *fakeProjects) Project(_ context.Context, name string) (*ProjectDoc, error) {
	f.asked = append(f.asked, name)
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[name]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such project"}
}

// GET /garden/project/{name} sends the reader's project with every list present (empty, never null), a long list capped
// and the rest counted, the open asks in attention order with their prompts clipped, the secrets only to a viewer who may
// edit the config, and whether the viewer may approve an escalation; not there or not visible is a 404, a daemon failure
// a 502, and no reader a 503.
func TestProjectViewSendsTheProject(t *testing.T) {
	var agents []ProjectAgent
	for i := 0; i < projectRowsMax+3; i++ {
		agents = append(agents, ProjectAgent{ID: "gd-a" + strings.Repeat("x", i), Name: "a"})
	}
	secrets := []ProjectSecret{{Env: "TOKEN", Secret: "s", Key: "k"}}
	f := &fakeProjects{docs: map[string]*ProjectDoc{
		"gardenio": {Name: "gardenio", Agents: agents, Config: &ProjectConfig{BeadID: "gd-proj", Secrets: secrets}, CanEdit: true,
			Decisions: []QueueRow{
				{ID: "gd-old", Kind: "decision", Since: 100},
				{ID: "gd-mine", Kind: "decision", Since: 200, NeedsYou: true, Text: strings.Repeat("long prompt ", 100)},
			}},
		"label": {Name: "label", Config: &ProjectConfig{BeadID: "gd-p2", Secrets: secrets}},
	}}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Projects: f, CanApproveEscalation: func(*http.Request) bool { return true }}

	code, doc := getQueue(t, h, "/garden/project/gardenio")
	if code != http.StatusOK || doc["name"] != "gardenio" || doc["approve"] != true || doc["can_edit"] != true {
		t.Fatalf("%d %v", code, doc)
	}
	if got := len(doc["agents"].([]any)); got != projectRowsMax || doc["more"].(map[string]any)["agents"] != float64(3) {
		t.Errorf("%d agents, more %v; want the cap and 3 counted", got, doc["more"])
	}
	for _, k := range []string{"gardeners", "accounts", "growing", "recent", "escalations", "blocked"} {
		if l, ok := doc[k].([]any); !ok || len(l) != 0 {
			t.Errorf("%s: %v, want an empty list", k, doc[k])
		}
	}
	ds := doc["decisions"].([]any)
	first := ds[0].(map[string]any)
	if first["id"] != "gd-mine" || len([]rune(first["text"].(string))) > projectTextMax+1 {
		t.Errorf("decisions %v: the one asked of the viewer first, its prompt clipped", ds)
	}
	if s := doc["config"].(map[string]any)["secrets"]; s == nil {
		t.Error("an editor is sent the secrets' names")
	}

	_, doc = getQueue(t, h, "/garden/project/label")
	if doc["config"].(map[string]any)["secrets"] != nil || doc["can_edit"] != nil {
		t.Errorf("a viewer who may not edit is sent no secrets: %v", doc["config"])
	}

	// New gardener (step 5.12): offered with the presets only to a viewer who may operate gardeners
	f.docs["gardenio"].GardenerPresets = []GardenerPreset{{Key: "epic-driver", Label: "Epic driver"}}
	if _, doc = getQueue(t, h, "/garden/project/gardenio"); doc["can_create_gardener"] != nil || doc["gardener_presets"] != nil {
		t.Errorf("no gardener gate: offered %v %v", doc["can_create_gardener"], doc["gardener_presets"])
	}
	h.CanOperateGardeners = func(*http.Request) bool { return true }
	if _, doc = getQueue(t, h, "/garden/project/gardenio"); doc["can_create_gardener"] != true || len(doc["gardener_presets"].([]any)) != 1 {
		t.Errorf("an operator: offered %v %v", doc["can_create_gardener"], doc["gardener_presets"])
	}
	if _, doc = getQueue(t, h, "/garden/project/label"); doc["can_create_gardener"] != nil {
		t.Error("no presets, no form")
	}

	if code, _ := getQueue(t, h, "/garden/project/nothere"); code != http.StatusNotFound {
		t.Errorf("not a project: %d, want 404", code)
	}
	forbidden := &Handler{Hub: New(newFake(), "/v", nil), Projects: &fakeProjects{err: &beadsapi.APIError{StatusCode: http.StatusForbidden}}}
	if code, _ := getQueue(t, forbidden, "/garden/project/gardenio"); code != http.StatusNotFound {
		t.Errorf("not visible: %d, want 404", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), Projects: &fakeProjects{err: errors.New("down")}}, "/garden/project/gardenio"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil)}, "/garden/project/gardenio"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
	if strings.Join(f.asked, " ") != "gardenio label gardenio gardenio label nothere" {
		t.Errorf("reader asked %q", f.asked)
	}
}

// The page inlines project.js after the Workbench and the Queues panel whose rows it draws, and the project's sign opens it.
func TestPageCarriesProjects(t *testing.T) {
	q, i, j := strings.Index(pageHTML, `{{asset "queue.js"}}`), strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "project.js"}}`)
	if j < 0 || j < i || j < q {
		t.Fatalf("project.js must be inlined after workbench.js and queue.js: %d, %d, %d", i, q, j)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var PROJECT = ", "W.register('project',", "api.row = function", "{project:n.key}"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// project.js's pure half runs under node: the tabs and the one shown, the address's tab=, each tab's count, the money
// words (a figure not known is a dash), a purse in words for each state, each row's words, the config's rows and the
// editor's form as the route reads it, the Cost tab's charts: their route, range and failure words, the Share tab's, and
// which farm patches move the panel.
func TestProjectLogicUnderNode(t *testing.T) {
	script := `
var P = require(process.argv[1]), assert = require("assert");
assert.strictEqual(P.url("my land"), "/garden/project/my%20land");
assert.deepStrictEqual(P.TABS.map(function (t) { return t[0]; }), ["agents", "gardeners", "accounts", "cost", "health", "activity", "decisions", "config"]);
assert.ok(P.isTab("cost") && !P.isTab("nope") && !P.isTab(""));
assert.strictEqual(P.withTab("?project=g&at=x", "cost"), "?project=g&at=x&tab=cost");
assert.strictEqual(P.withTab("?tab=agents&project=g", "activity"), "?project=g&tab=activity");
assert.strictEqual(P.withTab("?tab=agents&project=g", ""), "?project=g");
assert.strictEqual(P.withTab("?tab=agents", ""), "");

var d = { agents: [{}, {}], gardeners: [{}], accounts: [], growing: [{}], decisions: [{}], escalations: [{}, {}], more: { agents: 3, decisions: 1 } };
assert.strictEqual(P.pickTab("cost", d), "cost", "the address wins");
assert.strictEqual(P.pickTab(null, d), "decisions", "an ask waits");
assert.strictEqual(P.pickTab("bogus", { decisions: [], escalations: [] }), "agents");
assert.strictEqual(P.tabCount("agents", d), "5", "the agents past the cap count");
assert.strictEqual(P.tabCount("decisions", d), "4");
assert.strictEqual(P.tabCount("accounts", d), "");
assert.strictEqual(P.tabCount("activity", d), "1", "what is growing");
assert.strictEqual(P.tabCount("config", d), "");
assert.strictEqual(P.tabCount("agents", null), "");
assert.deepStrictEqual(P.agentsByNeed([{ id: "a", state: "working" }, { id: "b", state: "failed" }, { id: "c", state: "idle" }, { id: "d", state: "failed" }]).map(function (a) { return a.id; }),
  ["b", "d", "a", "c"], "the failed agents first, the rest as listed (gd-icriMpe3wR)");
assert.strictEqual(P.agentFailed({ state: "done" }), false);
assert.strictEqual(P.agentsByNeed(undefined).length, 0);

assert.strictEqual(P.usd(4.2), "$4.20");
assert.strictEqual(P.usd(0), "$0.00");
assert.strictEqual(P.usd(null), "—", "not known is a dash, never $0");
assert.strictEqual(P.credits(12.345), "12.35 credits");
assert.strictEqual(P.credits(undefined), "—");

assert.strictEqual(P.summary({ agents: [{}], gardeners: [], decisions: [{}], escalations: [], plot: { has: true, today_usd: 1.5 } }), "1 agent · 1 ask waits · $1.50 today");
assert.strictEqual(P.summary({ agents: [], spend: { has: true, today_usd: 2 } }), "0 agents · $2.00 today", "the accounts' spend without a plot");
assert.strictEqual(P.summary({ agents: [], plot: { has: false, today_usd: null } }), "0 agents", "a stale plot names no figure");
assert.strictEqual(P.summary({ agents: [{}, {}], more: { agents: 3 } }), "5 agents", "the Agents tab's count, the ones not sent included");

var ok = P.purseWords({ state: "ok", remaining_credits: 40, granted_credits: 50, used_pct: 20 });
assert.strictEqual(ok.state, "40 credits left");
assert.strictEqual(ok.line, "40 credits left of 50 credits (20% used).");
var wall = P.purseWords({ state: "wall", remaining_credits: 1, margin_credits: 2 });
assert.ok(/^At the wall/.test(wall.line) && /New spawns are stopped/.test(wall.line), wall.line);
var unk = P.purseWords({ state: "unknown", reason: "spend not synced" });
assert.ok(/unknown \(spend not synced\)/.test(unk.line) && /counts as the wall/.test(unk.line), unk.line);
assert.deepStrictEqual(P.purseWords(null), { state: "", line: "" });

assert.deepStrictEqual(P.agentLine({ state: "working", role: "crew", ago: "3m" }), ["working", "crew", "3m ago"]);
assert.deepStrictEqual(P.gardenerLine({ kind: "trigger", enabled: false, status: "disabled", runs: 1 }), ["trigger", "off", "1 run"]);
assert.deepStrictEqual(P.gardenerLine({ kind: "schedule", enabled: true, status: "closed", runs: 0 }), ["schedule", "on", "closed", "0 runs"]);
assert.deepStrictEqual(P.accountLine({ provider: "anthropic", today_usd: 1, week_usd: 2, total_usd: 3, shared: true }),
  ["anthropic", "$1.00 today", "$2.00 this week", "$3.00 in all", "shares a key: counted once, on another account"]);
assert.deepStrictEqual(P.accountLine({ model: "m" }), ["m", "no spend read"]);
assert.deepStrictEqual(P.beadLine({ type: "bug", status: "in_progress", ago: "1h" }), ["bug", "growing", "1h ago"]);
assert.deepStrictEqual(P.beadLine({ type: "task", status: "closed" }), ["closed"]);

var rows = P.configRows({ git_url: "https://g/x", budget_usd: "", docker: true, schedules_enabled: true });
var at = function (k) { return (rows.filter(function (r) { return r[0] === k; })[0] || [])[1]; };
assert.strictEqual(at("Repository"), "https://g/x");
assert.strictEqual(at("Agent image"), "inherited");
assert.strictEqual(at("Budget (US$)"), "no budget");
assert.strictEqual(at("Budget period"), undefined, "no period without a budget");
assert.strictEqual(at("Slack card theme"), "garden");
assert.strictEqual(at("Turned on"), "Docker, Schedules");

var f = P.formParams({ git_url: " https://g/x ", budget_usd: "50", docker: true, postgres: false, add_secret_env: "", add_secret_secret: "", add_secret_key: "" });
var keys = f.map(function (p) { return p[0]; });
assert.ok(keys.indexOf("git_url") >= 0 && keys.indexOf("image") >= 0, "every field is sent: " + keys);
assert.deepStrictEqual(f.filter(function (p) { return p[0] === "git_url"; })[0], ["git_url", "https://g/x"]);
assert.deepStrictEqual(f.filter(function (p) { return p[0] === "docker"; })[0], ["docker", "on"]);
assert.ok(keys.indexOf("postgres") < 0, "an unticked box is not sent, as a form does not send it");
assert.ok(keys.indexOf("add_secret_env") < 0, "no secret typed, none sent");
var f2 = P.formParams({ add_secret_env: "TOKEN" });
assert.deepStrictEqual(f2.slice(-3), [["add_secret_env", "TOKEN"], ["add_secret_secret", ""], ["add_secret_key", ""]], "one typed sends the trio, for the route to say what is missing");
assert.ok(P.REFRESH_S >= 10);
var g = { id: "c1", project: "g", state: "working" }, o = { id: "o1", project: "o" }, cost = { projects: [{ project: "g", today_usd: 1 }] };
var doc0 = { gardeners: [g, o], seeds: [], cost: cost }, same = function (d) { return Object.assign({}, doc0, d); };
assert.ok(!P.moved(doc0, same({}), "g") && P.moved(null, doc0, "g"), "an untouched patch is not a move; the first document is");
assert.ok(!P.moved(doc0, same({ gardeners: [g, { id: "o1", project: "o", state: "idle" }] }), "g"), "another project's row");
assert.ok(P.moved(doc0, same({ gardeners: [Object.assign({}, g, { reason: { kind: "parked" } }), o] }), "g"), "a gardener parked has filed an ask (gd-HprlA40a7z)");
assert.ok(P.moved(doc0, same({ cost: { projects: [{ project: "g", today_usd: 2 }] } }), "g"), "the spend and purse move with the cost row");

assert.deepStrictEqual(P.createParams("gardenio", { preset: "epic-driver", epic_id: " gd-e1 ", prompt: " drive it ", terminal_goal: "", cron: " */20 * * * * " }),
  [["project", "gardenio"], ["preset", "epic-driver"], ["epic_id", "gd-e1"], ["prompt", "drive it"], ["cron", "*/20 * * * *"]], "a blank goal is left to the preset");
assert.ok(/epic/.test(P.createProblem({ prompt: "x" })), "the epic first");
assert.ok(/prompt/.test(P.createProblem({ epic_id: "gd-e1", prompt: " " })));
assert.strictEqual(P.createProblem({ epic_id: "gd-e1", prompt: "go" }), "");

assert.deepStrictEqual(P.RANGES.map(function (r) { return r[0]; }), ["today", "7d", "30d"]);
assert.strictEqual(P.pickRange("30d"), "30d");
assert.strictEqual(P.pickRange(null), "7d", "the default is a week");
assert.strictEqual(P.pickRange("1y"), "7d", "a stored range outside the chips is the default");
assert.strictEqual(P.chartsUrl("my-land", "today"), "/garden/charts/costs-project?project=my-land&range=today&theme=dark", "dark in both skins");
assert.strictEqual(P.chartsUrl("a&b", "bogus"), "/garden/charts/costs-project?project=a%26b&range=7d&theme=dark");
assert.ok(/for operators/.test(P.chartsFail(403)));
assert.ok(/no charts for this project/.test(P.chartsFail(400)));
assert.strictEqual(P.chartsUrl("my-land", "30d", "health"), "/garden/charts/health-land?project=my-land&range=30d&theme=dark", "the Health tab's set");
assert.strictEqual(P.chartsUrl("my-land", "30d", "cost"), "/garden/charts/costs-project?project=my-land&range=30d&theme=dark");
assert.strictEqual(P.chartsFail(403, "health"), "Health charts are for operators for now.");
assert.strictEqual(P.chartsFail(403, "cost"), "Costs charts are for operators for now.");
assert.strictEqual(P.chartsNone("health"), "The health dashboard has no charts for this project.");
assert.deepStrictEqual(Object.keys(P.CHARTS).filter(P.isTab), ["cost", "health"], "every charts tab is a tab");
assert.ok(/session ended/.test(P.chartsFail("redirect")) && /session ended/.test(P.chartsFail(401)));
assert.strictEqual(P.chartsFail(0), "Could not load the charts: the garden did not answer.");
assert.strictEqual(P.chartsFail(502), "Could not load the charts (502).");
assert.ok(P.chartsRetry(0) && P.chartsRetry(502), "no answer, or a server error: Try again");
assert.ok(!P.chartsRetry(400) && !P.chartsRetry(403) && !P.chartsRetry(401) && !P.chartsRetry("redirect"), "a refusal or an ended session: no Try again");
assert.strictEqual(P.grafanaHome("https://grafana.example/d/gardenio-garden-costs?var-project=x"), "https://grafana.example/");
assert.strictEqual(P.grafanaHome("https://h.example/grafana/d/u?x=1"), "https://h.example/grafana/", "a sub-path base is kept");
assert.strictEqual(P.grafanaHome(""), "");
assert.strictEqual(P.grafanaHome("javascript:x/d/u"), "", "only an http(s) link");

// Share (LW.5): the tab only for a viewer who may share, the address's tab=share only then, and a land window's words
assert.deepStrictEqual(P.tabsOf({}).map(function (t) { return t[0]; }), P.TABS.map(function (t) { return t[0]; }), "no Share tab by default");
assert.deepStrictEqual(P.tabsOf({ can_share: true }).slice(-1)[0], ["share", "Share"]);
assert.ok(P.isTab("share"));
assert.strictEqual(P.tabWord("share"), "Share");
assert.strictEqual(P.pickTab("share", { can_share: true, decisions: [] }), "share");
assert.strictEqual(P.pickTab("share", { decisions: [{}] }), "decisions", "tab=share for a viewer who may not share is the usual pick");
assert.deepStrictEqual(P.windowWords({ state: "live", view_count: 1, last_viewed_at: "2026-10-29T10:00:00Z", expires_at: "2026-11-28T00:00:00Z", created_by: "o@x" }),
  ["live", "1 view", "last viewed 2026-10-29", "expires 2026-11-28", "o@x"]);
assert.deepStrictEqual(P.windowWords({ state: "revoked", expires_at: "2026-11-28T00:00:00Z" }), ["revoked", "0 views", "never viewed", "expires 2026-11-28"]);
assert.strictEqual(P.withheldWords({ withheld: [] }), "Nothing is withheld: every field passes the window’s checks.");
assert.strictEqual(P.withheldWords({ withheld: [{ path: "plans[3].title" }, { path: "land" }], withheld_text: "withheld: x" }),
  "2 fields are withheld for an internal reference, and the page reads “withheld: x” in its place: plans[3].title, land.");
`
	runNodeLogic(t, "project.js", script)
}

// fixtureProjects is the card world's project reader (card_browser_test.go): gardenio, an operator land with two agents,
// two gardeners (one off), an account, its decisions and escalation (the Queues fixture's), what grows and what moved, and
// its config, editable; and acme, a tenant land at the wall of its purse, whose daemon did not answer its gardeners.
func fixtureProjects(now time.Time) ProjectReader {
	q := fixtureQueues(now).(*fakeQueues)
	dec, esc := q.doc(QueueDecisions, true).Rows, q.doc(QueueEscalations, true).Rows
	f2 := func(v float64) *float64 { return &v }
	return &fakeProjects{docs: map[string]*ProjectDoc{
		"gardenio": {Name: "gardenio", CanEdit: true, GardenerPresets: []GardenerPreset{
			{Key: "epic-driver", Label: "Epic driver (drive children to closure)", Description: "Wakes every 30m while the epic still has open children."},
			{Key: "mr-green-watcher", Label: "MR-green watcher", Description: "Wakes every 15m to shepherd open MRs to green."}},
			Config: &ProjectConfig{BeadID: "gd-ProjGarden1", Status: "open", GitURL: "https://github.com/acme/gardenio", DefaultBranch: "main", Account: "anthropic-main",
				Model: "claude-opus", CPURequest: "500m", MemoryLimit: "2Gi", BudgetUSD: "300", BudgetPeriod: "month", Docker: true, SchedulesEnabled: true, TriggersEnabled: true,
				Secrets:  []ProjectSecret{{Env: "GITHUB_TOKEN", Secret: "gardenio-gh", Key: "token"}},
				Findings: []ProjectFinding{{Field: "memory_request", Severity: "warning", Message: "memory_limit is set without memory_request."}}},
			Agents: []ProjectAgent{
				{ID: "gd-agentBeadW", Name: "gardenio-73-github-idp", Role: "crew", State: "working", Status: "in_progress", TaskID: "gd-EdgeIdP002", TaskTitle: "Roll the edge IdP out", Ago: "4m"},
				{ID: "gd-agentBeadI", Name: "gardenio-plan-gardener", Role: "plan-gardener", State: "idle", Status: "open", Ago: "1h"},
			},
			Gardeners: []ProjectGardener{
				{ID: "gd-GardTrig01", Name: "guard the plan", Kind: "trigger", Status: "active", Enabled: true, Goal: "Walk every crew PR to green.", Runs: 12},
				{ID: "gd-GardSched1", Name: "nightly sweep", Kind: "schedule", Status: "disabled", Runs: 3},
			},
			Accounts: []ProjectAccount{{Name: "anthropic-main", Provider: "anthropic", Model: "claude-opus", Status: "active", TodayUSD: f2(4.2), WeekUSD: f2(31.5), TotalUSD: f2(412)}},
			Spend:    &ProjectSpend{TodayUSD: 4.2, WeekUSD: 31.5, TotalUSD: 412, Has: true},
			Plot:     &FarmCostProject{Project: "gardenio", Has: true, HasBudget: true, BudgetUSD: f2(300), Period: BudgetMonth, TodayUSD: f2(4.2), WeekUSD: f2(31.5), SpentUSD: f2(120), RemainingUSD: f2(180), Level: BudgetLevelOK},
			Growing:  []ProjectBead{{ID: "gd-EdgeIdP002", Title: "Roll the edge IdP out", Type: "task", Status: "in_progress", Ago: "4m"}},
			Recent: []ProjectBead{
				{ID: "gd-EdgeIdP002", Title: "Roll the edge IdP out", Type: "task", Status: "in_progress", Ago: "4m"},
				{ID: "gd-QueueInb002", Title: "Bird poses for tool calls", Type: "epic", Status: "closed", Ago: "2h"},
			},
			Decisions: dec, Escalations: esc,
			Blocked: []ProjectBead{{ID: "gd-EdgeIdP003", Title: "Turn the old IdP off", Status: "blocked", Ago: "1d"}},
		},
		// orchard: a land its owner may share (LW.5), the Share tab's world (shareStub, card_browser_test.go)
		"orchard": {Name: "orchard", CanShare: true, Agents: []ProjectAgent{}},
		// one agent at work, the one the purse holds credits in flight for, so the summary and the Cost tab agree
		"acme": {Name: "acme", Partial: []string{"gardeners"}, Owner: &ProjectPerson{Principal: "oidc:phono@example.com", Name: "phonobaloon"},
			Agents: []ProjectAgent{{ID: "gd-agentAcme1", Name: "acme-crew-tidy-docs", Role: "crew", State: "working", Status: "in_progress", Ago: "3m"}},
			Purse: &purse.Purse{Land: "acme", State: purse.StateWall, Blocked: true, GrantedCredits: 50, SpentCredits: f2(46.5), InFlightCredits: f2(2), RemainingCredits: f2(1.5),
				MarginCredits: 2.5, RunningAgents: 1, RateVersion: 2, LLMMultiplier: 1.4}},
	}}
}

// The project panel in a real Chromium: the project's sign opens it, its tabs move the address and walk with the arrows,
// each tab shows its rows, an answer from the Decisions tab goes through the Mailbox's box, the config editor saves and a
// refused field says why beside it, a tenant land's purse at the wall says so in words, and a daemon failure or an unknown
// project says so. What is asserted lives in testdata/project_browser.js.
func TestProjectUnderChromium(t *testing.T) {
	underChromium(t, "project_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
