package gardenhub

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/oidcauth"
)

// The Glass panel, watch (Lands workbench-glass spec GL-1, GL-2, GL-12; step 6.4, gd-9adIfFKLls): web/glass.js.

// glassRecording is testdata/glass_session.jsonl, a recorded coop session: the frames coop answers a new subscriber with
// (agent, usage:update, screen, replay), one per line, as coop's ServerMessage JSON. A replay line carries its bytes as
// "text" (readable in review) where coop sends base64 "data"; the fake encodes it.
type glassRecording struct {
	frames map[string]string // event -> the frame coop sends
	screen *coopmuxapi.Screen
}

func loadGlassRecording(t testing.TB) *glassRecording {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "glass_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rec := &glassRecording{frames: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("glass_session.jsonl: %v", err)
		}
		ev, _ := m["event"].(string)
		if ev == "replay" {
			text, _ := m["text"].(string)
			delete(m, "text")
			m["data"] = base64.StdEncoding.EncodeToString([]byte(text))
			m["next_offset"] = len(text)
			m["total_written"] = len(text)
		}
		b, _ := json.Marshal(m)
		rec.frames[ev] = string(b)
		if ev == "screen" {
			var s struct {
				Lines     []string `json:"lines"`
				Cols      uint16   `json:"cols"`
				Rows      uint16   `json:"rows"`
				AltScreen bool     `json:"alt_screen"`
				Seq       uint64   `json:"seq"`
			}
			_ = json.Unmarshal(b, &s)
			rec.screen = &coopmuxapi.Screen{Lines: s.Lines, Cols: s.Cols, Rows: s.Rows, AltScreen: s.AltScreen, Seq: s.Seq}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"agent", "usage:update", "screen", "replay"} {
		if rec.frames[ev] == "" {
			t.Fatalf("glass_session.jsonl has no %s frame", ev)
		}
	}
	return rec
}

// glassReplayUp is one upstream that answers the relay's first asks (agent:get, usage:get, screen:get, replay:get) from
// the recording, then stays open with nothing more to say, so a shot is the recorded screen, frozen.
type glassReplayUp struct {
	rec  *glassRecording
	ctx  context.Context
	in   chan coopmuxapi.Frame
	once sync.Once
	done chan struct{}
}

func (u *glassReplayUp) Recv() (coopmuxapi.Frame, error) {
	select {
	case f := <-u.in:
		return f, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.done:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *glassReplayUp) Send(frame any) error {
	b, _ := json.Marshal(frame)
	var m struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(b, &m)
	answer := map[string]string{"agent:get": "agent", "usage:get": "usage:update", "screen:get": "screen", "replay:get": "replay"}[m.Event]
	if raw := u.rec.frames[answer]; raw != "" {
		select {
		case u.in <- coopmuxapi.Frame{Event: answer, Data: json.RawMessage(raw)}:
		default:
		}
	}
	return nil
}

func (u *glassReplayUp) Close() error { u.once.Do(func() { close(u.done) }); return nil }

type glassRecordedScreens struct{ rec *glassRecording }

func (s glassRecordedScreens) GetScreen(_ context.Context, id string) (*coopmuxapi.Screen, error) {
	if id == "gardenio-64-glass-panel-7f9c4" || id == "gardenio-63-glass-relay-7f9c4" || id == "gardenio-65-glass-nudge-7f9c4" || id == "gardenio-66-glass-attach-7f9c4" {
		return s.rec.screen, nil
	}
	return nil, coopmuxapi.ErrSessionNotFound
}

// glassFixturePrompts is the card world's coop for the Glass's prompt (step 6.5): gd-GlassAsk001's session is parked at a
// Bash permission prompt, with the options coop reads off its bash_permission fixture; every other session is working.
type glassFixturePrompts struct{}

func (glassFixturePrompts) GetAgentState(_ context.Context, session string) (*coopmuxapi.AgentState, error) {
	if session != "gardenio-65-glass-nudge-7f9c4" {
		return &coopmuxapi.AgentState{State: "working"}, nil
	}
	return &coopmuxapi.AgentState{State: "prompt", Prompt: &coopmuxapi.Prompt{Type: "permission", Subtype: "tool", Tool: "Bash",
		Input: "go test ./internal/edgeauth/ -run TestExchange -count=1", Ready: true,
		Options: []string{"Yes", "Yes, and don't ask again for go test commands in this project", "No, and tell Claude what to do differently"}}}, nil
}

func (glassFixturePrompts) RespondOutcome(context.Context, string, coopmuxapi.RespondRequest) (*coopmuxapi.RespondOutcome, error) {
	return &coopmuxapi.RespondOutcome{Delivered: true}, nil
}

func (glassFixturePrompts) SendKeys(context.Context, string, []string) error { return nil }

// fixtureGlass is the card world's Glass (card_browser_test.go): three agent beads put in beads, none on the farm.
// gd-GlassLive01 is live, its stream the recording; gd-GlassOff001's stream ends "unavailable", as every stream does
// while the viewer has no relay key (COOPMUX_RELAY_HMAC_KEY, 6.3b gd-GLYhNn9ORF), so the panel shows the cached screen and says why;
// gd-GlassAsk001's too, and it is parked at a permission prompt (glassFixturePrompts). The viewer is an operator, so the page
// offers the Glass, and its nudge box and answers (step 6.5).
func fixtureGlass(t testing.TB, beads *fakeBeads) *Glass {
	rec := loadGlassRecording(t)
	// coopmux knows a pod by its name (pod_name), not by coop's own session id in last_session_id (gd-oJOJvsOdw7).
	agent := func(id, name, pod string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", Title: name, Labels: []string{"project:gardenio", "role:crew"},
			Notes: "pod_name: " + pod + "\n",
			Fields: map[string]string{"agent": name, "project": "gardenio", "role": "crew", "task_id": "gd-A1a", "agent_state": "working",
				"last_session_id": "coop-session-of-" + name, "account": "claude-pool-2", "live_cost_usd": "2.41", "context_tokens": "61000"}}
	}
	beads.beads["gd-GlassLive01"] = agent("gd-GlassLive01", "gardenio-64-glass-panel", "gardenio-64-glass-panel-7f9c4")
	beads.beads["gd-GlassOff001"] = agent("gd-GlassOff001", "gardenio-63-glass-relay", "gardenio-63-glass-relay-7f9c4")
	beads.beads["gd-GlassAsk001"] = agent("gd-GlassAsk001", "gardenio-65-glass-nudge", "gardenio-65-glass-nudge-7f9c4")
	beads.beads["gd-GlassKey001"] = agent("gd-GlassKey001", "gardenio-66-glass-attach", "gardenio-66-glass-attach-7f9c4")
	dial := func(ctx context.Context, session string, _ []string) (glass.Upstream, error) {
		if session != "gardenio-64-glass-panel-7f9c4" && session != "gardenio-66-glass-attach-7f9c4" {
			return nil, coopmuxapi.ErrNoRelayKey
		}
		return &glassReplayUp{rec: rec, ctx: ctx, in: make(chan coopmuxapi.Frame, 8), done: make(chan struct{})}, nil
	}
	return &Glass{
		Relay: glass.NewRelay(dial, nil), Screens: glassRecordedScreens{rec}, Wall: fixtureWall(rec, beads),
		InScope: func(*http.Request, string) bool { return true },
		// an admin, so the credential pool (glass_pool.go) is theirs to read and gardenio's header offers it; a browser
		// check that sends X-Test-Roles is that viewer instead (glass_transcript_browser.js: one below the operator tier)
		Who: func(r *http.Request) coopmuxapi.Identity {
			if roles := r.Header.Get("X-Test-Roles"); roles != "" {
				return coopmuxapi.Identity{Operator: "vi@example.com", AuthMode: "cookie", Roles: strings.Split(roles, ",")}
			}
			return coopmuxapi.Identity{Operator: "mo@example.com", AuthMode: "cookie", Roles: []string{oidcauth.RoleAdmin}}
		},
		Nudge:   func(context.Context, string, string) NudgeOutcome { return NudgeDelivered },
		Prompts: glassFixturePrompts{},
		// Attach (step 6.6) is offered on gd-GlassKey001 alone, so the watch goldens keep no Attach button; its keyboard
		// takes every frame and types nowhere.
		MayAttach: func(r *http.Request, _ string) bool { return r.PathValue("agent") == "gd-GlassKey001" },
		Keyboard:  glassFixtureKeyboard{},
	}
}

// glassFixtureAgents are the card world's Glass agents (fixtureGlass), on no plot of the farm.
var glassFixtureAgents = []string{"gd-GlassLive01", "gd-GlassOff001", "gd-GlassAsk001", "gd-GlassKey001"}

// glassActivity serves testdata/glass_activity.json as the transcript of a Glass fixture agent (GET
// /garden/farm/activity/{id}): they are on no plot of the farm, whose ring has none of their calls, so the Glass's
// transcript section (gd-GPu096omMw) reads this fixture activity document where the farm's would be.
func glassActivity(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(filepath.Join("testdata", "glass_activity.json"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// glassFixtureKeyboard is the card world's coop keyboard for attach: it takes every frame.
type glassFixtureKeyboard struct{}

func (glassFixtureKeyboard) SendInput(context.Context, string, string) error      { return nil }
func (glassFixtureKeyboard) SendKeys(context.Context, string, []string) error     { return nil }
func (glassFixtureKeyboard) Resize(context.Context, string, uint16, uint16) error { return nil }
func (glassFixtureKeyboard) Nudge(context.Context, string, string) error          { return nil }
func (glassFixtureKeyboard) SendMouse(context.Context, string, coopmuxapi.MouseEvent) (int, error) {
	return 0, nil
}

// withFixturePool gives the card world's Glass its credential pool (GL-15), read at now: four accounts in the broker's
// order, one for each health. claude-pool-1 healthy with room, claude-pool-2 warming near its 5 h window, claude-pool-3
// throttled until 40 minutes from now, and codex-spare with no key and not probed yet.
func withFixturePool(g *Glass, now func() time.Time) *Glass {
	g.now = now
	g.Pool = glassPoolFunc(func() *coopmuxapi.CredentialPool {
		return &coopmuxapi.CredentialPool{
			Accounts: []coopmuxapi.PoolAccount{
				{Name: "claude-pool-1", Provider: "claude", Status: "healthy", SessionCount: 4, CumulativeAssignments: 61, HasAPIKey: true,
					ThrottleStatus: "allowed", Utilization5h: "0.4200", Utilization7d: "0.1800", RepresentativeClaim: "five_hour"},
				{Name: "claude-pool-2", Provider: "claude", Status: "warming", SessionCount: 2, CumulativeAssignments: 38, HasAPIKey: true,
					ThrottleStatus: "allowed_warning", Utilization5h: "0.7800", Utilization7d: "0.5500", ThrottleResetAt: now().Add(2 * time.Hour).UTC().Format(time.RFC3339)},
				{Name: "claude-pool-3", Provider: "claude", Status: "throttled", SessionCount: 0, CumulativeAssignments: 52, HasAPIKey: true,
					ThrottleStatus: "rejected", Utilization5h: "1.0000", Utilization7d: "0.9100", ThrottleResetAt: now().Add(40 * time.Minute).UTC().Format(time.RFC3339)},
				{Name: "codex-spare", Provider: "openai", Status: "missing"},
			},
			TotalSessions: 6, HealthyAccounts: 1, TotalAccounts: 4, Strategy: "least_loaded",
		}
	})
	return g
}

// glassPoolFunc is a GlassPool answering what its function builds.
type glassPoolFunc func() *coopmuxapi.CredentialPool

func (f glassPoolFunc) CredentialPool(context.Context) (*coopmuxapi.CredentialPool, error) {
	return f(), nil
}

// The page offers the Glass (body data-glass, glass.js registering its kind) only to a viewer who may watch: an
// operator, with the Glass wired. Anyone else gets no glass kind and so no Watch link anywhere.
func TestPageOffersGlassToWatchersOnly(t *testing.T) {
	if w, g := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "glass.js"}}`); g < 0 || g < w {
		t.Fatalf("glass.js must be inlined after workbench.js: %d %d", w, g)
	}
	page := func(h *Handler, roles string) string {
		mux := http.NewServeMux()
		h.Register(mux)
		req := httptest.NewRequest("GET", "/garden", nil)
		req.Header.Set("X-Test-Roles", roles)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return inlined(t, rec.Body.String())
	}
	who := func(r *http.Request) coopmuxapi.Identity {
		return coopmuxapi.Identity{Roles: strings.Split(r.Header.Get("X-Test-Roles"), ",")}
	}
	on := page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Glass: &Glass{Who: who}}, oidcauth.RoleOperator)
	xterm := `data-xterm-js="/v` + assetsPath + pageAssets.byName["xterm.bundle.js"].file + `" data-xterm-css="/v` + assetsPath + pageAssets.byName["xterm.css"].file + `"`
	for _, want := range []string{`data-glass="1"`, xterm, "var GLASS = ", "W.register('glass',"} {
		if !strings.Contains(on, want) {
			t.Errorf("an operator's page lacks %q", want)
		}
	}
	for name, body := range map[string]string{
		"a viewer":  page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Glass: &Glass{Who: who}}, oidcauth.RoleViewer),
		"no Glass":  page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}, oidcauth.RoleOperator),
		"no who fn": page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Glass: &Glass{}}, oidcauth.RoleOperator),
	} {
		if strings.Contains(body, `data-glass="1"`) { // the body's flag; garden.css's [data-glass=prompt] marks are not an offer
			t.Errorf("%s: the page offers the Glass", name)
		}
	}
}

// The recorded session plays through the real relay and the real routes: the panel's document carries the recorded screen
// and its stream, and the stream opens with the recorded state, usage and replay (the bytes the terminal is reset to).
func TestGlassRecordingPlaysThroughTheRelay(t *testing.T) {
	beads := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{"gd-A1a": {ID: "gd-A1a", Type: "task", Title: "Token exchange for agent pods"}}}
	h := &Handler{BasePath: "/v", Beads: beads}
	h.Glass = fixtureGlass(t, beads)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(http.StripPrefix("/v", mux))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v/garden/glass/gd-GlassLive01")
	if err != nil {
		t.Fatal(err)
	}
	var doc GlassDoc
	_ = json.NewDecoder(res.Body).Decode(&doc)
	res.Body.Close()
	if !doc.Live || doc.Stream != "/v/garden/glass/gd-GlassLive01/stream" || doc.Screen == nil || doc.Screen.Cols != 100 || doc.TaskTitle != "Token exchange for agent pods" {
		t.Fatalf("document: %+v", doc)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+doc.Stream, nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	seen := map[string]string{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ev := ""
	for sc.Scan() && seen["replay"] == "" {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			ev = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			seen[ev] = strings.TrimPrefix(line, "data: ")
		}
	}
	if !strings.Contains(seen["state"], `"state":"working"`) || !strings.Contains(seen["usage"], `"total_cost_usd":3.18`) {
		t.Errorf("state and usage first: %q %q", seen["state"], seen["usage"])
	}
	var rp struct {
		Data string `json:"data"`
	}
	_ = json.Unmarshal([]byte(seen["replay"]), &rp)
	if b, _ := base64.StdEncoding.DecodeString(rp.Data); !strings.Contains(string(b), "token exchange for agent pods") {
		t.Errorf("the replay is the recording's bytes: %q", b)
	}
}

// glass.js's pure half runs under node: the state chip (GL-4: rate limited wins, Prompt and Error pulse, a Prompt says who it
// waits on), the header's rows (GL-3), the usage a stream says, the PTY geometry and the scale that fits it (GL-2: down
// only), a screen as bytes, base64, why a stream ended, and the gardener a Commons thread is for.
func TestGlassLogicUnderNode(t *testing.T) {
	script := `
var G = require(process.argv[1]), assert = require("assert");
assert.strictEqual(G.url("gd-a b"), "/garden/glass/gd-a%20b");

var w = G.stateView({ state: "working" });
assert.deepStrictEqual([w.key, w.word, w.tone, w.pulse], ["working", "Working", "ok", false]);
var p = G.stateView({ state: "prompt", attention: true });
assert.deepStrictEqual([p.word, p.tone, p.pulse], ["Prompt", "ask", true]);
assert.strictEqual(G.stateView({ state: "prompt", prompt: { type: "permission", tool: "Bash" } }, true).detail, "Waiting on you: it asks to use Bash. Answer it below.",
  "a prompt says who it waits on: the viewer who may answer it");
assert.strictEqual(G.stateView({ state: "prompt", prompt: { type: "plan" } }, false).detail, "Waiting on an operator: it is stopped at a prompt.",
  "and an operator when the viewer may not");
assert.strictEqual(G.stateView("prompt", true).detail, "Waiting on you: it is stopped at a prompt. Answer it below.");
assert.strictEqual(G.waitLine(null, false), "Waiting on an operator: it is stopped at a prompt.");
assert.strictEqual(G.stateView({ state: "error", error_detail: "boom" }).pulse, true, "Error pulses");
assert.strictEqual(G.stateView({ state: "error", error_detail: "boom" }).detail, "boom");
var rl = G.stateView({ state: "error", rate_limited: true, error_category: "rate_limited" });
assert.deepStrictEqual([rl.key, rl.word, rl.tone], ["rate_limited", "Rate limited", "warn"], "rate limited wins over the state it rides on");
assert.strictEqual(G.stateView({ state: "parked", parked_reason: "budget_cap" }).detail, "Parked: budget cap.");
assert.strictEqual(G.stateView("spawning").word, "Starting", "the bead's agent_state reads the same");
assert.strictEqual(G.stateView("done").word, "Exited");
assert.strictEqual(G.stateView("mystery_state").word, "mystery state", "any other state is said as it is");
assert.strictEqual(G.stateView(null).word, "State unknown");
["starting", "working", "idle", "prompt", "error", "parked", "restarting", "exited"].forEach(function (s) {
  assert.notStrictEqual(G.stateView(s).key, "other", "coop's state " + s + " has a chip");
});

assert.deepStrictEqual(G.headRows({ task: "gd-t", task_title: "Fix it", project: "gardenio", role: "crew", pod: "p-1", account: "a", cost: "$1.23", context_tokens: 48000 }),
  [["Tending", "Fix it", "gd-t"], ["Works", "on gardenio as crew"], ["Pod", "p-1"], ["Account", "a"], ["Spent so far", "$1.23"], ["Context", "48k tokens"]]);
assert.deepStrictEqual(G.headRows({ task: "gd-t" }), [["Tending", "gd-t", "gd-t"]], "a task with no title shows its id");
assert.deepStrictEqual(G.headRows({}), []);
assert.strictEqual(G.tokens(999), "999 tokens");
assert.strictEqual(G.tokens(1), "1 token");

assert.deepStrictEqual(G.usageOf({ event: "usage:update", cumulative: { total_cost_usd: 3.181, context_tokens: 74120 } }), { cost: "$3.18", context_tokens: 74120 });
assert.deepStrictEqual(G.usageOf({ event: "usage", input_tokens: 5 }), { cost: "", context_tokens: 0 }, "a snapshot says no cost and no level");

assert.strictEqual(G.pinchFont(13, 1, 2), 26, "a spread doubles the glyphs");
assert.strictEqual(G.pinchFont(13, 0.4, 2), 10, "from a terminal fitted at 0.4: what the fingers left on the screen");
assert.strictEqual(G.pinchFont(13, 1, 10), 32, "no larger than FONT_MAX");
assert.strictEqual(G.pinchFont(13, 1, 0.01), 6, "no smaller than FONT_MIN");
assert.strictEqual(G.pinchFont(13, 1, NaN), G.FONT);

assert.deepStrictEqual(G.geometry({ cols: 132, rows: 40 }), { cols: 132, rows: 40 });
assert.deepStrictEqual(G.geometry(null), { cols: 80, rows: 24 });
assert.deepStrictEqual(G.geometry({ cols: 0, rows: 40 }), { cols: 80, rows: 24 });
assert.strictEqual(G.fitScale(400, 800), 0.5);
assert.strictEqual(G.fitScale(1200, 800), 1, "never scaled up");
assert.strictEqual(G.fitScale(0, 800), 1, "a box not laid out yet is not a scale");
assert.strictEqual(G.actualSize(330, 800, null), true, "an upright phone: fitting would shrink the glyphs under 10 px, so it pans at its own size");
assert.strictEqual(G.actualSize(940, 800, null), false, "a wide panel fits it whole");
assert.strictEqual(G.actualSize(640, 800, null), false, "scaled to 0.8 is still legible");
assert.strictEqual(G.actualSize(330, 800, false), false, "the viewer's Fit to the panel stands");
assert.strictEqual(G.actualSize(940, 800, true), true, "the viewer's Actual size stands");
assert.strictEqual(G.actualSize(0, 800, null), false, "a box not laid out yet starts fitted");
var L = require(process.argv[1].replace(/glass\.js$/, "wall.js"));
[{ cols: 132, rows: 40 }, null, { cols: 1, rows: 9 }, { cols: "90", rows: "30" }, { cols: 80, rows: 501 }].forEach(function (s) { assert.deepStrictEqual(G.geometry(s), L.geometry(s), "the Wall's geometry"); });
[[400, 800], [1200, 800], [0, 800], [800, 800]].forEach(function (a) { assert.strictEqual(G.fitScale(a[0], a[1]), L.fitScale(a[0], a[1]), "the Wall's fitScale"); });
assert.deepStrictEqual(G.COLORS, L.COLORS, "the Wall's 16 colours");

assert.strictEqual(G.screenText({ lines: ["a", "b"] }), "a\r\nb");
assert.strictEqual(G.screenText({ lines: ["a"], cursor: { row: 3, col: 7 } }), "a\x1b[4;8H", "the cursor where coop says it is");
assert.deepStrictEqual(Array.from(G.bytes(Buffer.from("hé").toString("base64"))), Array.from(Buffer.from("hé")));
assert.strictEqual(G.bytes("!!not base64!!").length, 0);

["gone", "refused", "unavailable", "", undefined].forEach(function (r) { assert.ok(/last screen/.test(G.endedLine(r)), "every ending keeps the last screen: " + r); });
assert.ok(/not available here yet/.test(G.endedLine("unavailable")));
assert.ok(/no screen/.test(G.notLive({})));
assert.ok(/last screen/.test(G.notLive({ screen: { lines: [] } })));
assert.strictEqual(G.exitLine({ code: 0 }), "The agent process exited with code 0.");
assert.strictEqual(G.exitLine({ signal: 9 }), "The agent process exited on signal 9.");
assert.strictEqual(G.exitLine(null), "The agent process exited.");

// the transcript section under the terminal (gd-GPu096omMw): its heading says how many calls and when the last was, from
// testdata/glass_activity.json, the fixture activity document; it starts folded on a phone, and as the viewer left it after
var doc = JSON.parse(require("fs").readFileSync(process.argv[1].replace(/web\/glass\.js$/, "testdata/glass_activity.json"), "utf8"));
assert.strictEqual(G.txHead(doc.rows), "Its last 8 calls, the last at 19:34:12");
assert.strictEqual(G.txHead(doc.rows.slice(7)), "Its last call, the last at 19:28:22");
assert.strictEqual(G.txHead([]), "No calls yet");
assert.strictEqual(G.folded(390, null), true, "a phone's terminal keeps the screen");
assert.strictEqual(G.folded(599, null), true);
assert.strictEqual(G.folded(G.FOLD_W, null), false, "a panel 600 px wide shows it");
assert.strictEqual(G.folded(0, null), false, "a panel not laid out yet is not a phone");
assert.strictEqual(G.folded(390, "1"), false, "the viewer opened it");
assert.strictEqual(G.folded(1280, "0"), true, "the viewer folded it");

`
	runNodeLogic(t, "glass.js", script)
}

// Watch never writes to the PTY (G-D3, GL-2): glass.js posts nothing, opens no WebSocket and wires none of xterm's input
// or resize callbacks to anything, and its terminal has stdin off.
func TestGlassNeverWrites(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("web", "glass.js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"method:", "'POST'", `"POST"`, "WebSocket", ".onData", ".onBinary", ".onKey", ".onResize", "sendBeacon", "/send"} {
		if strings.Contains(s, banned) {
			t.Errorf("glass.js contains %q: watch is look only", banned)
		}
	}
	if !strings.Contains(s, "disableStdin: true") {
		t.Error("the terminal must have stdin off")
	}
	// xterm queues writes but runs reset() at once, so a reset that way can land before queued bytes and the replay then
	// paints over the cached screen (seen in CI): the panel resets in-band (RIS) instead.
	if strings.Contains(s, ".reset()") {
		t.Error("glass.js must reset the terminal in-band (RIS), not with term.reset()")
	}
}

// glassact.js's pure half under node: the routes it posts to and reads, when a state changes the prompt shown, and the
// words a send's answer is said in.
func TestGlassActLogicUnderNode(t *testing.T) {
	script := `
var A = require(process.argv[1]), assert = require("assert");
assert.strictEqual(A.sendURL("gd-a b"), "/garden/glass/gd-a%20b/send");
assert.strictEqual(A.promptURL("gd-a"), "/garden/glass/gd-a/prompt");

assert.strictEqual(A.promptKey({ state: "working" }), "", "no prompt");
assert.strictEqual(A.promptKey(null), "");
var k1 = A.promptKey({ state: "prompt", prompt: { type: "permission", options: [] } });
var k2 = A.promptKey({ state: "prompt", prompt: { type: "permission", options: ["Yes", "No"] } });
assert.ok(k1 && k2 && k1 !== k2, "coop's options arriving is a new prompt to read");
assert.strictEqual(A.promptKey({ state: "Prompt", prompt: { type: "plan" } }), A.promptKey({ state: "prompt", prompt: { type: "plan" } }));

var ok = A.sentLine(200, { ok: true, result: "delivered", note: "Sent. It reaches the agent in its next turn, and it is in the agent's thread." });
assert.deepStrictEqual([ok.ok, ok.again], [true, false]);
assert.ok(/next turn/.test(ok.text));
var busy = A.sentLine(409, { ok: false, result: "busy", note: "Not sent: the agent is busy" });
assert.deepStrictEqual([busy.ok, busy.again, busy.text], [false, false, "Not sent: the agent is busy"], "the server's words, said as they are");
assert.strictEqual(A.sentLine(409, { ok: false, result: "changed", note: "x" }).again, true, "a changed prompt is read again");
assert.strictEqual(A.sentLine(409, { ok: false, result: "no_prompt", note: "x" }).again, true);
assert.strictEqual(A.sentLine(403, "watching an agent needs operator\n").text, "watching an agent needs operator", "a plain refusal is said");
assert.ok(/not answer \(502\)/.test(A.sentLine(502, null).text));
assert.strictEqual(A.sentLine(204, "").text, "Sent.");
assert.strictEqual(A.utf8("hé"), 3);
assert.strictEqual(A.utf8("😀"), 4);
assert.strictEqual(A.NUDGE_MAX, 4096);
`
	runNodeLogic(t, "glassact.js", script)
}

// glassact.js writes only what is not the keyboard (GL-7, GL-8): a nudge and an answer to the prompt, to the Glass's own
// /send. It types nothing (input, keys and resize are attach's, GL-9) and touches no terminal.
func TestGlassActWritesOnlyNudgeAndRespond(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("web", "glassact.js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"'input'", `"input"`, "'keys'", `"keys"`, "'resize'", `"resize"`, "'compact'", "WebSocket", ".onData", ".onKey", "term.", "sendBeacon", "/agents/"} {
		if strings.Contains(s, banned) {
			t.Errorf("glassact.js contains %q: it nudges and answers prompts, nothing else", banned)
		}
	}
	if strings.Count(s, "method: 'POST'") != 1 || !strings.Contains(s, "base + sendURL(id)") {
		t.Error("glassact.js posts in one place, to the Glass's /send")
	}
	if !strings.Contains(s, "['type', 'nudge']") || !strings.Contains(s, "f.dataset.glass !== 'respond'") {
		t.Error("the two writes are a nudge and the respond component's forms")
	}
}
