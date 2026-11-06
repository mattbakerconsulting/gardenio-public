package gardenhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/coopmuxapi"
)

// glassCoop is a fake coopmux for the Glass's writes: the agent's state as coop has it, and what was sent.
type glassCoop struct {
	mu       sync.Mutex
	state    *coopmuxapi.AgentState
	notThere bool // a missing session
	answered []coopmuxapi.RespondRequest
	keys     [][]string
	outcome  *coopmuxapi.RespondOutcome
}

func (c *glassCoop) GetAgentState(_ context.Context, _ string) (*coopmuxapi.AgentState, error) {
	if c.notThere {
		return nil, coopmuxapi.ErrSessionNotFound
	}
	return c.state, nil
}

func (c *glassCoop) RespondOutcome(_ context.Context, _ string, req coopmuxapi.RespondRequest) (*coopmuxapi.RespondOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answered = append(c.answered, req)
	if c.outcome != nil {
		return c.outcome, nil
	}
	return &coopmuxapi.RespondOutcome{Delivered: true}, nil
}

func (c *glassCoop) SendKeys(_ context.Context, _ string, keys []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, keys)
	return nil
}

func bashPrompt() *coopmuxapi.AgentState {
	return &coopmuxapi.AgentState{State: "prompt", Prompt: &coopmuxapi.Prompt{Type: "permission", Subtype: "tool", Tool: "Bash", Input: "rm -rf <tmp>",
		Options: []string{"Yes", "Yes, and always allow access to tmp/ from this project", "No"}, Ready: true}}
}

type glassSendRig struct {
	h       *Handler
	coop    *glassCoop
	logs    *bytes.Buffer
	nudged  []string // agent: message
	outcome NudgeOutcome
	said    []string // agentID: text
}

func newGlassSendRig() *glassSendRig {
	g := &glassSendRig{coop: &glassCoop{state: bashPrompt()}, logs: &bytes.Buffer{}, outcome: NudgeDelivered}
	g.h = glassHandler(&glassMux{}, glassScreens{})
	g.h.Glass.Prompts = g.coop
	g.h.Glass.Logger = slog.New(slog.NewJSONHandler(g.logs, nil))
	g.h.Glass.Nudge = func(_ context.Context, agent, message string) NudgeOutcome {
		g.nudged = append(g.nudged, agent+": "+message)
		return g.outcome
	}
	g.h.Glass.Say = func(_ *http.Request, agentID, text string) (string, error) {
		g.said = append(g.said, agentID+": "+text)
		return "gd-msg1", nil
	}
	return g
}

func (g *glassSendRig) send(t *testing.T, agent, roles string, form url.Values, hdr ...string) (*httptest.ResponseRecorder, GlassSent) {
	t.Helper()
	m := http.NewServeMux()
	g.h.Register(m)
	req := httptest.NewRequest(http.MethodPost, "/garden/glass/"+agent+"/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Test-Email", "op@example.com")
	req.Header.Set("X-Test-Roles", roles)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	var s GlassSent
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
	}
	return rec, s
}

// auditLines are the agent_terminal_write lines logged.
func (g *glassSendRig) auditLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(g.logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == "agent_terminal_write" {
			out = append(out, m)
		}
	}
	return out
}

func TestGlassSend_Gates(t *testing.T) {
	g := newGlassSendRig()
	nudge := url.Values{"type": {"nudge"}, "message": {"look at the failing test"}}
	cases := []struct {
		name  string
		agent string
		roles string
		hdr   []string
		want  int
	}{
		{"an agent outside the viewer's lands is not there", "gd-other", "gardenio-operator", nil, http.StatusNotFound},
		{"a missing agent", "gd-nope", "gardenio-operator", nil, http.StatusNotFound},
		{"not an agent", "gd-plain", "gardenio-operator", nil, http.StatusNotFound},
		{"below the watch tier", "gd-agent1", "gardenio-viewer", nil, http.StatusForbidden},
		{"another site's form", "gd-agent1", "gardenio-operator", []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
	}
	for _, c := range cases {
		if rec, _ := g.send(t, c.agent, c.roles, nudge, c.hdr...); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if len(g.nudged) != 0 {
		t.Fatalf("a refused send nudged: %q", g.nudged)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"raw"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown frame: %d", rec.Code)
	}
	// The keyboard frames are attach's (GL-9, glass_attach.go): refused where attach is not offered, and audited.
	rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"input"}, "text": {"ls"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("input where attach is not offered: %d", rec.Code)
	}
	if l := g.auditLines(t); len(l) != 1 || l[0]["subverb"] != "input" || l[0]["result"] != "refused" {
		t.Errorf("audit: %v", l)
	}
}

func TestGlassSend_NudgeDeliveredIsWrittenIntoTheThread(t *testing.T) {
	g := newGlassSendRig()
	rec, s := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"nudge"}, "message": {"  look at the failing test "}})
	if rec.Code != http.StatusOK || !s.OK || s.Result != "delivered" || s.Message != "gd-msg1" || s.AuditID == "" {
		t.Fatalf("status %d: %+v", rec.Code, s)
	}
	if len(g.nudged) != 1 || g.nudged[0] != "crew-one: look at the failing test" {
		t.Fatalf("nudged %q: the agent by name, the words trimmed", g.nudged)
	}
	if len(g.said) != 1 || g.said[0] != "gd-agent1: look at the failing test" {
		t.Fatalf("said %q", g.said)
	}
	l := g.auditLines(t)
	if len(l) != 1 {
		t.Fatalf("audit lines %v", l)
	}
	want := map[string]any{"subverb": "nudge", "surface": "glass", "agent": "gd-agent1", "session": "gardenio-crew-one-abc", "operator": "op@example.com",
		"result": "delivered", "audit_id": s.AuditID, "message": "gd-msg1"}
	for k, v := range want {
		if l[0][k] != v {
			t.Errorf("audit %s = %v, want %v", k, l[0][k], v)
		}
	}
}

// A nudge that does not land says why, and writes nothing into the thread: the thread shows only what the agent was told.
func TestGlassSend_NudgeNotDeliveredSaysWhy(t *testing.T) {
	for _, out := range []NudgeOutcome{NudgeBusy, NudgeStopped, NudgeGone, NudgeFailed} {
		g := newGlassSendRig()
		g.outcome = out
		rec, s := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"nudge"}, "message": {"hello"}})
		if s.OK || s.Result != string(out) || s.Note == "" || rec.Code < 400 {
			t.Errorf("%s: status %d %+v", out, rec.Code, s)
		}
		if len(g.said) != 0 {
			t.Errorf("%s: an undelivered nudge was written into the thread", out)
		}
		if l := g.auditLines(t); len(l) != 1 || l[0]["result"] != string(out) {
			t.Errorf("%s: audit %v", out, l)
		}
	}
	g := newGlassSendRig()
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"nudge"}, "message": {"   "}}); rec.Code != http.StatusBadRequest || len(g.nudged) != 0 {
		t.Errorf("an empty nudge: %d", rec.Code)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"nudge"}, "message": {strings.Repeat("a", glassNudgeMax+1)}}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a long nudge: %d", rec.Code)
	}
}

func TestGlassSend_RespondChecksTheLabel(t *testing.T) {
	g := newGlassSendRig()
	ans := url.Values{"type": {"respond"}, "prompt": {"permission"}, "option": {"2"}, "label": {"Yes, and always allow access to tmp/ from this project"}}
	rec, s := g.send(t, "gd-agent1", "gardenio-operator", ans)
	if rec.Code != http.StatusOK || !s.OK {
		t.Fatalf("status %d %+v", rec.Code, s)
	}
	if len(g.coop.answered) != 1 || g.coop.answered[0].Option == nil || *g.coop.answered[0].Option != 2 {
		t.Fatalf("answered %+v", g.coop.answered)
	}
	// The prompt was re-read and its second option is something else now: nothing is sent.
	g.coop.state.Prompt.Options = []string{"Yes", "Yes, allow all edits during this session (shift+tab)", "No"}
	rec, s = g.send(t, "gd-agent1", "gardenio-operator", ans)
	if rec.Code != http.StatusConflict || s.Result != "changed" || len(g.coop.answered) != 1 {
		t.Fatalf("a stale answer: %d %+v, answered %d", rec.Code, s, len(g.coop.answered))
	}
	// The agent moved on from the prompt.
	g.coop.state = &coopmuxapi.AgentState{State: "working"}
	if rec, s = g.send(t, "gd-agent1", "gardenio-operator", ans); rec.Code != http.StatusConflict || s.Result != "no_prompt" {
		t.Fatalf("no prompt: %d %+v", rec.Code, s)
	}
	// Coop says the answer reached no prompt.
	g.coop.state, g.coop.outcome = bashPrompt(), &coopmuxapi.RespondOutcome{Delivered: false, Reason: "no prompt active"}
	if rec, s = g.send(t, "gd-agent1", "gardenio-operator", ans); rec.Code != http.StatusConflict || s.Result != "no_prompt" {
		t.Fatalf("not delivered: %d %+v", rec.Code, s)
	}
	g.coop.notThere = true
	if rec, s = g.send(t, "gd-agent1", "gardenio-operator", ans); rec.Code != http.StatusConflict || s.Result != "not_live" {
		t.Fatalf("no session: %d %+v", rec.Code, s)
	}
	results := []string{}
	for _, l := range g.auditLines(t) {
		if l["subverb"] != "respond" || l["operator"] != "op@example.com" || l["audit_id"] == "" {
			t.Errorf("audit line %v", l)
		}
		results = append(results, l["result"].(string))
	}
	if strings.Join(results, ",") != "ok,changed,no_prompt,no_prompt,not_live" {
		t.Errorf("audit results %v", results)
	}
}

// TestGlassSend_PermissionAnsweredClosesTheCard: a permission prompt answered on the Glass closes its Commons card too
// (spec 3.3), with the answer as allow, deny or neither and the option's label; the person is told so. A card that
// would not close leaves the answer given and says so; a question's answer closes no card.
func TestGlassSend_PermissionAnsweredClosesTheCard(t *testing.T) {
	g := newGlassSendRig()
	var closed []string
	var closeErr error
	g.h.Glass.Answered = func(_ *http.Request, agentID, tool, behavior, said string) (string, error) {
		closed = append(closed, strings.Join([]string{agentID, tool, behavior, said}, "|"))
		if closeErr != nil {
			return "", closeErr
		}
		return "gd-perm1", nil
	}
	ans := func(option, label string) url.Values {
		return url.Values{"type": {"respond"}, "prompt": {"permission"}, "option": {option}, "label": {label}}
	}
	rec, s := g.send(t, "gd-agent1", "gardenio-operator", ans("2", "Yes, and always allow access to tmp/ from this project"))
	if rec.Code != http.StatusOK || !s.OK || !strings.Contains(s.Note, "permission card in the Commons says so too") {
		t.Fatalf("status %d %+v", rec.Code, s)
	}
	if want := "gd-agent1|Bash|allow|Yes, and always allow access to tmp/ from this project"; len(closed) != 1 || closed[0] != want {
		t.Fatalf("closed %v; want %q", closed, want)
	}
	closeErr = errors.New("bridge down")
	if rec, s = g.send(t, "gd-agent1", "gardenio-operator", ans("3", "No")); rec.Code != http.StatusOK || !s.OK || !strings.Contains(s.Note, "still shows it open") {
		t.Fatalf("card not closed: %d %+v", rec.Code, s)
	}
	if len(closed) != 2 || closed[1] != "gd-agent1|Bash|deny|No" {
		t.Fatalf("closed %v", closed)
	}
	lines := g.auditLines(t)
	if len(lines) != 2 || lines[0]["permission"] != "gd-perm1" || lines[1]["permission"] != "" {
		t.Errorf("audit lines %v", lines)
	}
	// a workspace-trust prompt is no tool permission: no card to close
	g.coop.state = &coopmuxapi.AgentState{State: "prompt", Prompt: &coopmuxapi.Prompt{Type: "permission", Subtype: "trust", Options: []string{"Yes, proceed", "No, exit"}, Ready: true}}
	if rec, _ = g.send(t, "gd-agent1", "gardenio-operator", ans("1", "Yes, proceed")); rec.Code != http.StatusOK || len(closed) != 2 {
		t.Fatalf("trust: %d, closed %v", rec.Code, closed)
	}
}

func TestPermissionBehavior(t *testing.T) {
	for label, want := range map[string]string{
		"Yes": "allow", "yes": "allow", "Yes, allow all edits during this session (shift+tab)": "allow",
		"Yes, and don't ask again for make commands in /repo": "allow",
		"No": "deny", "No, and tell Claude what to do differently (esc)": "deny",
		"Continue": "", "Nope": "", "Yesterday": "", "": "",
	} {
		if got := permissionBehavior(label); got != want {
			t.Errorf("permissionBehavior(%q) = %q; want %q", label, got, want)
		}
	}
}

func TestGlassSend_RespondConfirmStepSendsKeys(t *testing.T) {
	g := newGlassSendRig()
	g.coop.state = &coopmuxapi.AgentState{State: "prompt", Prompt: &coopmuxapi.Prompt{Type: "question",
		Questions: []coopmuxapi.PromptQuestion{{Question: "Which?", Options: []string{"Rust"}}}, QuestionCurrent: 1, Ready: true}}
	rec, s := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"respond"}, "prompt": {"question"}, "confirm": {"submit"}})
	if rec.Code != http.StatusOK || !s.OK || len(g.coop.keys) != 1 || g.coop.keys[0][0] != "Enter" {
		t.Fatalf("status %d %+v keys %v", rec.Code, s, g.coop.keys)
	}
}

func TestGlassPromptFragment(t *testing.T) {
	g := newGlassSendRig()
	rec := glassGet(t, g.h, "/garden/glass/gd-agent1/prompt", "gardenio-operator")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-prompt="permission"`, "Permission needed", "<code>Bash</code>", "rm -rf &lt;tmp&gt;",
		`action="/v/garden/glass/gd-agent1/send"`, `data-glass="respond"`, `name="type" value="respond"`,
		`>Yes, and always allow access to tmp/ from this project</button>`, `name="label" value="No"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment lacks %s:\n%s", want, body)
		}
	}
	if rec := glassGet(t, g.h, "/garden/glass/gd-other/prompt", "gardenio-operator"); rec.Code != http.StatusNotFound {
		t.Errorf("out of scope: %d", rec.Code)
	}
	g.coop.state = &coopmuxapi.AgentState{State: "idle"}
	if rec := glassGet(t, g.h, "/garden/glass/gd-agent1/prompt", "gardenio-operator"); rec.Code != http.StatusNoContent {
		t.Errorf("no prompt: %d", rec.Code)
	}
}

// beads-viewer's /agents/{id}/nudge is re-pointed at the Glass's nudge: a form message, 204 when it lands.
func TestGlassNudge_LegacyForm(t *testing.T) {
	g := newGlassSendRig()
	m := http.NewServeMux()
	m.HandleFunc("POST /agents/{id}/nudge", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("agent", r.PathValue("id"))
		g.h.GlassNudge(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agent1/nudge", strings.NewReader(url.Values{"message": {"go on"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Test-Email", "op@example.com")
	req.Header.Set("X-Test-Roles", "gardenio-operator")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(g.nudged) != 1 || len(g.said) != 1 {
		t.Fatalf("status %d nudged %q said %q", rec.Code, g.nudged, g.said)
	}
}
