package landmap

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/lands/contract/landsv1"
)

// goldenCall is one request the golden test replays. fixture names a contract
// request schema (fixtures/<Schema>/): the call is made once with each of its
// fixtures as the body. body is used when fixture is "".
type goldenCall struct {
	op, method, path, fixture, body string
}

const goldenDecision = "or-Dcs4nB8vCx"

// goldenPicture is a picture on ac-t1, the golden decision's requesting task.
const goldenPicture = "coop/sessions/images/ac-t1/1a2b3c4d-gate.png"

// goldenCalls covers every verb (TestResponseHygiene_Golden fails for one
// left out) on records whose prose carries what the scrub removes.
var goldenCalls = []goldenCall{
	{op: "getLand", method: "GET", path: "/acme"},
	{op: "whoami", method: "GET", path: "/acme/whoami"},
	{op: "listPlots", method: "GET", path: "/acme/plots"},
	{op: "getPlot", method: "GET", path: "/acme/plots/x"},
	{op: "listCrew", method: "GET", path: "/acme/crew"},
	{op: "getMember", method: "GET", path: "/acme/crew/x"},
	{op: "getBudget", method: "GET", path: "/acme/budget"},
	{op: "getBudgetHistory", method: "GET", path: "/acme/budget/history?days=7"},

	{op: "listTickets", method: "GET", path: "/acme/tickets"},
	{op: "listTickets", method: "GET", path: "/acme/tickets?status=waiting&limit=1"},
	{op: "searchTickets", method: "GET", path: "/acme/search/tickets?q=gate"},
	{op: "getTicket", method: "GET", path: "/acme/tickets/ac-t1"},
	{op: "createTicket", method: "POST", path: "/acme/tickets", fixture: "TicketCreate"},
	{op: "updateTicket", method: "PATCH", path: "/acme/tickets/ac-t1", fixture: "TicketUpdate"},
	{op: "commentTicket", method: "POST", path: "/acme/tickets/ac-t1/comments", fixture: "CommentCreate"},
	{op: "commentTicket", method: "POST", path: "/acme/tickets/ac-t1/comments", body: `{"text":"dup of gd-Ab3dEf9hIj, see /v1/beads/gd-Ab3dEf9hIj"}`},
	{op: "closeTicket", method: "POST", path: "/acme/tickets/ac-t1/close", fixture: "TicketClose"},
	{op: "reopenTicket", method: "POST", path: "/acme/tickets/ac-b1/reopen"},

	{op: "listDecisions", method: "GET", path: "/acme/decisions"},
	{op: "getDecision", method: "GET", path: "/acme/decisions/" + goldenDecision},
	{op: "resolveDecision", method: "POST", path: "/acme/decisions/" + goldenDecision + "/resolve", fixture: "DecisionResolve"},
	{op: "cancelDecision", method: "POST", path: "/acme/decisions/" + goldenDecision + "/cancel", fixture: "DecisionCancel"},
	{op: "commentDecision", method: "POST", path: "/acme/decisions/" + goldenDecision + "/comments", fixture: "CommentCreate"},
	{op: "getDecisionEvidence", method: "GET", path: "/acme/decisions/" + goldenDecision + "/evidence/" + pictureIDOf(goldenPicture)},

	{op: "listEscalations", method: "GET", path: "/acme/escalations"},
	{op: "getEscalation", method: "GET", path: "/acme/escalations/ac-e1"},
	{op: "createEscalation", method: "POST", path: "/acme/escalations", fixture: "EscalationCreate"},

	{op: "listPlans", method: "GET", path: "/acme/plans"},
	{op: "getPlan", method: "GET", path: "/acme/plans/ac-p1"},
	{op: "answerPlan", method: "POST", path: "/acme/plans/ac-p1/answer", fixture: "PlanAnswer"},

	{op: "listAgents", method: "GET", path: "/acme/agents"},
	{op: "getAgent", method: "GET", path: "/acme/agents/ac-a1"},
	{op: "createAgent", method: "POST", path: "/acme/agents", fixture: "AgentCreate"},
	{op: "refreshAgent", method: "POST", path: "/acme/agents/ac-a1/refresh", fixture: "AgentRefresh"},
	{op: "revokeAgent", method: "POST", path: "/acme/agents/ac-a1/revoke"},
	{op: "listReleases", method: "GET", path: "/acme/releases"},
	{op: "listReleases", method: "GET", path: "/acme/releases?limit=1"},
	{op: "getRelease", method: "GET", path: "/acme/releases/v1.1.0"},
	{op: "getRelease", method: "GET", path: "/acme/releases/ac-r1"},
	{op: "reportDeployment", method: "POST", path: "/acme/releases/v1.2.0/deployments", fixture: "DeploymentReport"},
	{op: "reportDeployment", method: "POST", path: "/acme/releases/v1.2.0/deployments", body: `{"target":"staging"}`},

	{op: "listThreads", method: "GET", path: "/acme/threads"},
	{op: "getThread", method: "GET", path: "/acme/threads/x"},
	{op: "listMessages", method: "GET", path: "/acme/threads/x/messages"},
	{op: "postMessage", method: "POST", path: "/acme/threads/x/messages", fixture: "MessageCreate"},
	{op: "streamEvents", method: "GET", path: "/acme/events"},
}

// newGoldenFixture is newMapFixture with leaks in every prose field the map
// projects: other lands' ids, daemon paths, service hosts. The decision is the
// one the contract's request fixtures answer. The plot's repository address
// names a daemon host and another land's id; only its name may reach a tenant.
// The purse snapshot is fresh, so getBudget answers rather than 503s, and
// getBudgetHistory has a day.
func newGoldenFixture(t *testing.T) (*fakeDaemon, http.Handler) {
	t.Helper()
	granted, used := 100.0, 30.25
	d, h := newMapFixture(t, fakeLands{pictures: &fakeBucket{objects: map[string]fakeObject{
		goldenPicture: {contentType: "image/png", body: []byte("\x89PNG gate")},
	}}, plot: PlotRecord{
		GitURL: "https://kbeads.gardenio.svc.cluster.local/gd-Ab3dEf9hIj/claim-web.git", DefaultBranch: "trunk",
	}, budget: BudgetRecord{Granted: &granted, Used: &used, State: purseOK, ReadAt: time.Now()},
		history: BudgetHistoryRecord{Days: []BudgetDayRecord{{Day: time.Now().UTC(), Spent: 2.5, Remaining: 69.75}}, Stale: true},
		agents:  []AgentRecord{{ID: "ac-a1", Name: "laptop-x7rq", StartedBy: "oidc:robin@example.com", StartedAt: t0}}})
	d.beads["ac-t1"]["description"] = "blocked on gd-Ab3dEf9hIj (/v1/beads/gd-Ab3dEf9hIj) per kbeads.gardenio.svc.cluster.local"
	d.beads["ac-t1"]["title"] = "Fix the gate, like zz-Qw3rTy9UiO"
	d.beads["ac-p1"]["title"] = "Shed plan (gd-Pl4nPl4nPl)"
	d.beads["ac-s1"]["title"] = "Lay the base after ka-0000004089"
	d.beads["ac-e1"]["fields"].(map[string]any)["reason"] = "ask beads.gardenio.svc about gd-Ab3dEf9hIj"
	seedReleases(d)
	d.beads["ac-r3"]["fields"].(map[string]any)["contents"].([]any)[0].(map[string]any)["title"] = "Banner, per gd-Ab3dEf9hIj on kbeads.gardenio.svc.cluster.local"
	d.add(map[string]any{"id": goldenDecision, "type": "decision", "title": "Which logo?", "labels": []any{"project:acme"}, "kind": "data",
		"fields": map[string]any{
			"prompt": "Which logo, given gd-Ab3dEf9hIj?", "context": "see GET /v1/decisions/gd-Ab3dEf9hIj", "requesting_plan": "ac-p1",
			"requesting_task": "ac-t1", "evidence": "gd-Ab3dEf9hIj",
			"options": []any{map[string]any{"id": "b", "label": "Keep, as in zz-Qw3rTy9UiO"}, map[string]any{"id": "or-Opt0000001", "short": "New"}},
		}})
	d.comments["ac-t1"] = []map[string]any{{"id": 7, "author": "oidc:robin@example.com", "created_at": t0,
		"text": "![gate, see gd-Ab3dEf9hIj](https://viewer.gardenio.svc.cluster.local/beads/image/" + goldenPicture + ") and https://cdn.example/x.png"}}
	return d, h
}

// requestFixtures returns each fixture of a contract request schema.
func requestFixtures(t *testing.T, schema string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(contractFixturesDir, schema, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no contract fixtures for %s: %v", schema, err)
	}
	out := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(raw)
	}
	return out
}

// outerRefusal stands in for the daemon's middleware: a request carrying
// X-Test-Refuse is answered in the daemon's words and headers before the map
// sees it, the way auth (401) and the rate limiter (429) answer.
func outerRefusal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", "v-test")
		if v := r.Header.Get("X-Test-Refuse"); v != "" {
			var st int
			fmt.Sscan(v, &st)
			w.Header().Set("Retry-After", "7")
			w.Header().Set("WWW-Authenticate", `Bearer realm="kbeads"`)
			http.Error(w, `{"error":"kbeads: bead route /v1/beads refused for scoped_principal"}`, st)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestResponseHygiene_Golden replays every verb, with each contract request
// fixture as its body, through Guard, the daemon's middleware and the map, in
// four ways: as is; with each daemon route it reaches refusing in the daemon's
// leakiest words; and refused before the map by a 401 and by a 429. Every
// response goes through the denylist (assertHygiene, via do). A new verb or
// request fixture that is not replayed fails the test. The map's live scan
// runs over the same answers, and DenylistHits must not move.
func TestResponseHygiene_Golden(t *testing.T) {
	hitsBefore := counterSum(t, DenylistHits)
	defer func() {
		if n := counterSum(t, DenylistHits) - hitsBefore; n != 0 {
			t.Errorf("the live scan counted %v denylist hits over the golden replay", n)
		}
	}()
	served := map[string]bool{}
	replayed := map[string]bool{}
	for _, c := range goldenCalls {
		served[c.op] = true
		bodies := map[string]string{"": c.body}
		if c.fixture != "" {
			bodies = requestFixtures(t, c.fixture)
		}
		for src, body := range bodies {
			replayed[src] = true
			what := fmt.Sprintf("%s %s %s", c.op, c.method, c.path)
			if src != "" {
				what += " < " + filepath.Base(filepath.Dir(src)) + "/" + filepath.Base(src)
			}

			d, h := newGoldenFixture(t)
			g := Guard(outerRefusal(h))
			r := do(t, g, c.method, c.path, body)
			if r.status >= 500 {
				t.Errorf("%s: status %d: %s", what, r.status, r.body)
			}
			routes := map[routeID]bool{}
			for _, call := range d.calls {
				for id, rt := range routeTable {
					if rt == call.route {
						routes[routeID(id)] = true
					}
				}
			}
			for id := range routes {
				for _, st := range []int{http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError} {
					d, h := newGoldenFixture(t)
					d.forced[id] = st
					do(t, Guard(outerRefusal(h)), c.method, c.path, body)
				}
			}
			for st, code := range map[int]landsv1.ProblemCode{
				http.StatusUnauthorized:    landsv1.Unauthenticated,
				http.StatusTooManyRequests: landsv1.RateLimited,
			} {
				problemOf(t, do(t, g, c.method, c.path, body, "X-Test-Refuse", fmt.Sprint(st)), code)
			}
		}
	}
	for _, v := range Verbs {
		if !served[v.Op] {
			t.Errorf("verb %s has no goldenCalls row", v.Op)
		}
	}
	// Every contract request fixture is replayed: a schema a verb takes as its
	// body is one of the ones named above.
	requestSchemas := []string{}
	for _, c := range goldenCalls {
		if c.fixture != "" && !slices.Contains(requestSchemas, c.fixture) {
			requestSchemas = append(requestSchemas, c.fixture)
		}
	}
	for _, s := range requestSchemas {
		for p := range requestFixtures(t, s) {
			if !replayed[p] {
				t.Errorf("request fixture %s was not replayed", p)
			}
		}
	}
	entries, err := os.ReadDir(contractFixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		isRequest := false
		for _, suffix := range []string{"Create", "Update", "Close", "Resolve", "Cancel", "Answer", "Report"} {
			isRequest = isRequest || strings.HasSuffix(n, suffix)
		}
		if isRequest && !slices.Contains(requestSchemas, n) {
			t.Errorf("request schema %s has no goldenCalls row", n)
		}
	}
}

// The prose the golden fixture leaks reaches the tenant scrubbed, and the
// land's own words and ids survive.
func TestProseScrub_OnProjectedFields(t *testing.T) {
	_, h := newGoldenFixture(t)
	tk := decodeAs[landsv1.Ticket](t, do(t, h, "GET", "/acme/tickets/ac-t1", ""), http.StatusOK)
	if tk.Title != "Fix the gate, like [ref]" || tk.Body == nil || *tk.Body != "blocked on [ref] ([path]) per [host]" {
		t.Errorf("ticket = %+v", tk)
	}
	dec := decodeAs[landsv1.Decision](t, do(t, h, "GET", "/acme/decisions/"+goldenDecision, ""), http.StatusOK)
	if dec.Question != "Which logo, given [ref]?" || *dec.Context != "see GET [path]" ||
		dec.Options[0].Label != "Keep, as in [ref]" || dec.Options[1].Id != "or-Opt0000001" {
		t.Errorf("decision = %+v", dec)
	}
	e := decodeAs[landsv1.Escalation](t, do(t, h, "GET", "/acme/escalations/ac-e1", ""), http.StatusOK)
	if e.Reason != "ask [host] about [ref]" {
		t.Errorf("escalation reason = %q", e.Reason)
	}
	p := decodeAs[landsv1.Plan](t, do(t, h, "GET", "/acme/plans/ac-p1", ""), http.StatusOK)
	if p.Title != "Shed plan ([ref])" || p.Steps[0].Title != "Lay the base after [ref]" {
		t.Errorf("plan = %+v", p)
	}
	c := decodeAs[landsv1.Comment](t, do(t, h, "POST", "/acme/tickets/ac-t1/comments", `{"text":"see ac-AbCdEf1234 and gd-Ab3dEf9hIj"}`), http.StatusCreated)
	if c.Text != "see ac-AbCdEf1234 and [ref]" {
		t.Errorf("comment = %q", c.Text)
	}
}

func TestScrubProse(t *testing.T) {
	for _, tc := range []struct{ own, in, want string }{
		{"or-Dcs4nB8vCx", "after or-Abc0000001 and gd-Abc0000001", "after or-Abc0000001 and [ref]"},
		{"or-Dcs4nB8vCx", "we re-initialize it, co-ordinated", "we re-initialize it, co-ordinated"},
		{"or-Dcs4nB8vCx", "curl http://beads.gardenio.svc.cluster.local:8080/v1/beads/x?y=1 now", "curl http://[host]:8080[path] now"},
		{"or-Dcs4nB8vCx", "the page /lands/v1/lands/or/tickets stays", "the page /lands/v1/lands/or/tickets stays"},
		{"or-Dcs4nB8vCx", "GET /v1/me, then /v1/members", "GET [path], then /v1/members"},
		{"or-Dcs4nB8vCx", "see /v1/beads/gd-Abc0000001.", "see [path]."},
		{"or-Dcs4nB8vCx", "mail svc.example.com", "mail svc.example.com"},
		{"gd-AAAAAAAAA1", "same land gd-Abc0000001", "same land gd-Abc0000001"},
		{"x", "", ""},
	} {
		if got := scrubProse(tc.own, tc.in); got != tc.want {
			t.Errorf("scrubProse(%q, %q) = %q, want %q", tc.own, tc.in, got, tc.want)
		}
	}
}

// Guard on its own: a response from outside the map is a problem whatever its
// status, the map's own pass through with only ResponseHeaders, and paths
// outside the Lands API are untouched (gd-enMSApFne1).
func TestGuard(t *testing.T) {
	daemon := func(status int, body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Kbeads-Version", "v-test")
			w.Header().Set("Location", "/v1/beads")
			w.WriteHeader(status)
			io.WriteString(w, body)
		})
	}
	for status, code := range map[int]landsv1.ProblemCode{
		http.StatusNotFound:            landsv1.NotFound,
		http.StatusMethodNotAllowed:    landsv1.NotFound,
		http.StatusMovedPermanently:    landsv1.NotFound,
		http.StatusOK:                  landsv1.NotFound,
		http.StatusForbidden:           landsv1.Forbidden,
		http.StatusServiceUnavailable:  landsv1.UpstreamUnavailable,
		http.StatusRequestTimeout:      landsv1.UpstreamUnavailable,
		http.StatusInternalServerError: landsv1.UpstreamUnavailable,
	} {
		r := do(t, Guard(daemon(status, "kbeads: no bead route")), "GET", "/acme/tickets", "")
		problemOf(t, r, code)
	}
	// Nothing written at all.
	problemOf(t, do(t, Guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), "GET", "/acme", ""), landsv1.NotFound)

	// The map's own response passes, its status and body intact.
	_, h := newMapFixture(t, fakeLands{})
	decodeAs[landsv1.Land](t, do(t, Guard(h), "GET", "/acme", ""), http.StatusOK)

	// Another path is not the Guard's.
	w := httptest.NewRecorder()
	Guard(daemon(http.StatusTeapot, "kd speaks here")).ServeHTTP(w, httptest.NewRequest("GET", "/v1/beads", nil))
	if w.Code != http.StatusTeapot || w.Header().Get("X-Kbeads-Version") != "v-test" || w.Body.String() != "kd speaks here" {
		t.Errorf("non-Lands path rewritten: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
}
