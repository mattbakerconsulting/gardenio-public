package landmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

func modelType(s string) model.BeadType { return model.BeadType(s) }

func ptr[T any](v T) *T { return &v }

// fakeDaemon serves the map's route table from memory, the way the daemon's
// routes answer, and records every call. Beads are JSON objects so the tests
// can carry keys the map must never pass on.
type fakeDaemon struct {
	mu     sync.Mutex
	mux    *http.ServeMux
	beads  map[string]map[string]any
	calls  []fakeCall
	forced map[routeID]int
	nextID int
	// comments are each bead's comments, as GET /v1/beads/{id}/comments
	// answers them.
	comments map[string][]map[string]any
}

type fakeCall struct {
	route Route
	path  string
	query string
	body  map[string]any
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 500, time.UTC)

func newFakeDaemon() *fakeDaemon {
	d := &fakeDaemon{mux: http.NewServeMux(), beads: map[string]map[string]any{}, comments: map[string][]map[string]any{}, forced: map[routeID]int{}}
	for id, rt := range routeTable {
		d.mux.HandleFunc(rt.String(), d.handle(routeID(id)))
	}
	return d
}

func (d *fakeDaemon) add(b map[string]any) {
	for k, v := range map[string]any{
		"kind": "issue", "status": "open", "priority": 2, "description": "",
		"created_at": t0, "updated_at": t0, "fleet": "default",
		"operated_by": "oidc:someone@example.com", "notes": "operator-only notes",
	} {
		if _, ok := b[k]; !ok {
			b[k] = v
		}
	}
	d.beads[b["id"].(string)] = b
}

func (d *fakeDaemon) handle(id routeID) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
		}
		d.calls = append(d.calls, fakeCall{route: routeTable[id], path: r.URL.Path, query: r.URL.RawQuery, body: body})
		if st, ok := d.forced[id]; ok {
			w.Header().Set("X-Kbeads-Version", "v-test")
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(st)
			fmt.Fprint(w, `{"error":"kbeads says: bead gd-secret type=task fields.notes denied by scoped_principal.go"}`)
			return
		}
		switch id {
		case rListBeads:
			d.list(w, r)
		case rGetBead:
			b, ok := d.beads[r.PathValue("id")]
			if !ok {
				http.Error(w, `{"error":"bead not found"}`, http.StatusNotFound)
				return
			}
			writeTestJSON(w, http.StatusOK, b)
		case rCreateBead:
			d.nextID++
			b := map[string]any{"id": "ac-new" + strconv.Itoa(d.nextID), "created_by": "oidc:al@example.com"}
			for k, v := range body {
				if k != "idempotency_key" {
					b[k] = v
				}
			}
			d.add(b)
			writeTestJSON(w, http.StatusCreated, b)
		case rUpdateBead:
			b := d.beads[r.PathValue("id")]
			for k, v := range body {
				b[k] = v
			}
			b["updated_at"] = t0.Add(time.Hour)
			writeTestJSON(w, http.StatusOK, b)
		case rCloseBead:
			d.beads[r.PathValue("id")]["status"] = "closed"
			writeTestJSON(w, http.StatusOK, d.beads[r.PathValue("id")])
		case rAddComment:
			writeTestJSON(w, http.StatusCreated, map[string]any{
				"id": 41, "bead_id": r.PathValue("id"), "author": "oidc:al@example.com",
				"text": body["text"], "created_at": t0, "operated_by": "oidc:al@example.com",
			})
		case rListComments:
			if _, ok := d.beads[r.PathValue("id")]; !ok {
				http.Error(w, `{"error":"bead not found"}`, http.StatusNotFound)
				return
			}
			cs := d.comments[r.PathValue("id")]
			if cs == nil {
				cs = []map[string]any{}
			}
			writeTestJSON(w, http.StatusOK, map[string]any{"comments": cs})
		case rResolveDecision:
			b := d.beads[r.PathValue("id")]
			f := b["fields"].(map[string]any)
			f["chosen"] = body["selected_option"]
			b["status"] = "closed"
			writeTestJSON(w, http.StatusOK, map[string]any{"decision": f, "issue": b})
		case rCancelDecision:
			b := d.beads[r.PathValue("id")]
			f := b["fields"].(map[string]any)
			f["chosen"], f["canceled"] = "dismissed", true
			b["status"] = "closed"
			writeTestJSON(w, http.StatusOK, map[string]any{"decision": f, "issue": b})
		case rMe:
			writeTestJSON(w, http.StatusOK, map[string]any{
				"operator_id": "oidc:al@example.com", "source": "oidc", "unrestricted": false,
				"lands": []any{map[string]any{"name": "acme", "tier": "owner"}},
			})
		case rListReleases, rGetRelease, rDeployRelease:
			d.release(id, w, r, body)
		case rLandMembers:
			if r.PathValue("land") != "acme" {
				http.Error(w, `{"error":"no such land"}`, http.StatusNotFound)
				return
			}
			writeTestJSON(w, http.StatusOK, map[string]any{"land": "acme", "members": []any{
				map[string]any{"principal": "oidc:Al@example.com", "name": "Al", "role": "owner"},
				map[string]any{"principal": "oidc:bo@example.com", "name": "bo", "role": "guest"},
				map[string]any{"principal": "oidc:cy@example.com", "name": "", "email": "cy@example.com"},
			}})
		}
	}
}

func (d *fakeDaemon) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var out []map[string]any
	for _, b := range d.beads {
		if v := q.Get("type"); v != "" && !slices.Contains(strings.Split(v, ","), b["type"].(string)) {
			continue
		}
		if v := q.Get("status"); v != "" && !slices.Contains(strings.Split(v, ","), b["status"].(string)) {
			continue
		}
		if v := q.Get("labels"); v != "" {
			labels, _ := b["labels"].([]any)
			ok := true
			for _, want := range strings.Split(v, ",") {
				ok = ok && slices.Contains(labels, any(want))
			}
			if !ok {
				continue
			}
		}
		if v := q.Get("parent"); v != "" && b["parent"] != v {
			continue
		}
		if v := q.Get("search"); v != "" && !strings.Contains(b["title"].(string), v) {
			continue
		}
		if q.Get("with_open_blockers") == "true" {
			b = d.withOpenBlockers(b, q.Get("labels"))
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	total := len(out)
	off, _ := strconv.Atoi(q.Get("offset"))
	lim, _ := strconv.Atoi(q.Get("limit"))
	out = out[min(off, len(out)):]
	if lim > 0 && len(out) > lim {
		out = out[:lim]
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeTestJSON(w, http.StatusOK, map[string]any{"beads": out, "total": total})
}

// withOpenBlockers is b with open_blockers, as the daemon's list adds it: each
// open bead b depends on through a blocking edge, out of scope when it lacks
// the land label the list asked for.
func (d *fakeDaemon) withOpenBlockers(b map[string]any, labels string) map[string]any {
	land, _, _ := strings.Cut(labels, ",")
	blockers := []any{}
	deps, _ := b["dependencies"].([]any)
	for _, dep := range deps {
		e := dep.(map[string]any)
		x, ok := d.beads[e["depends_on_id"].(string)]
		if !ok || !slices.Contains(blockingDeps, model.DependencyType(e["type"].(string))) || x["status"] == "closed" {
			continue
		}
		xl, _ := x["labels"].([]any)
		blockers = append(blockers, map[string]any{
			"id": x["id"], "status": x["status"], "type": x["type"], "edge_type": e["type"],
			"out_of_scope": !slices.Contains(xl, any(land)),
		})
	}
	c := maps.Clone(b)
	c["open_blockers"] = blockers
	return c
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type fakeLands struct {
	paused, applicant bool
	// plot is acme's repository; zero for none.
	plot PlotRecord
	// budget is acme's purse snapshot; noPurse makes acme an operator land.
	budget  BudgetRecord
	noPurse bool
	// livePurse, when set, is read for acme's snapshot instead of budget, so
	// a test can change the purse while a stream is open.
	livePurse func() BudgetRecord
	// history is acme's daily credits; historyErr fails the read.
	history    BudgetHistoryRecord
	historyErr error
	// caller is the caller's class; zero is a member.
	caller Caller
	// agents are acme's own-machine agents; agentErr is what every agent
	// call answers (a refusal or a failure) when set.
	agents   []AgentRecord
	agentErr error
	// minted records the last MintAgent request.
	minted *AgentMintRequest
	// pictures is the map's picture bucket; nil gives it none.
	pictures *fakeBucket
}

func (l fakeLands) Agents(_ context.Context, land string) ([]AgentRecord, error) {
	if l.agentErr != nil {
		return nil, l.agentErr
	}
	if land != "acme" {
		return nil, nil
	}
	return l.agents, nil
}

func (l fakeLands) Agent(_ context.Context, land, id string) (AgentRecord, bool, error) {
	if l.agentErr != nil {
		return AgentRecord{}, false, l.agentErr
	}
	for _, a := range l.agents {
		if land == "acme" && a.ID == id {
			return a, true, nil
		}
	}
	return AgentRecord{}, false, nil
}

// fakeGrant is what the fake mints: hex of the minted shape, spelling "bead".
var fakeGrant = AgentGrantRecord{
	AgentID:   "ac-a1",
	Bearer:    "4bead31d6905e1493668ced44bead31d6905e1493668ced4000000000000000a",
	Refresh:   "0bead31d6905e1493668ced44bead31d6905e1493668ced4000000000000000b",
	ExpiresAt: t0.Add(24 * time.Hour),
}

func (l fakeLands) MintAgent(_ context.Context, _ string, req AgentMintRequest) (AgentGrantRecord, error) {
	if l.minted != nil {
		*l.minted = req
	}
	if l.agentErr != nil {
		return AgentGrantRecord{}, l.agentErr
	}
	return fakeGrant, nil
}

func (l fakeLands) RefreshAgent(_ context.Context, _, id, refresh string) (AgentGrantRecord, error) {
	if l.agentErr != nil {
		return AgentGrantRecord{}, l.agentErr
	}
	if id != fakeGrant.AgentID || refresh != fakeGrant.Refresh {
		return AgentGrantRecord{}, ErrAgentNotFound
	}
	return fakeGrant, nil
}

func (l fakeLands) RevokeAgent(_ context.Context, land, id string) (AgentRecord, error) {
	a, ok, err := l.Agent(context.Background(), land, id)
	if err != nil {
		return AgentRecord{}, err
	}
	if !ok {
		return AgentRecord{}, ErrAgentNotFound
	}
	a.Stopped = true
	return a, nil
}

func (l fakeLands) Applicant(context.Context) bool { return l.applicant }

func (l fakeLands) Caller(context.Context) Caller {
	if l.caller == "" {
		return CallerMember
	}
	return l.caller
}

func (fakeLands) Admits(_ context.Context, land string) bool { return land == "acme" }

func (l fakeLands) Land(_ context.Context, land string) (LandRecord, bool, error) {
	if land != "acme" {
		return LandRecord{}, false, nil
	}
	return LandRecord{CreatedAt: t0, Paused: l.paused}, true, nil
}

func (l fakeLands) Budget(_ context.Context, land string) (BudgetRecord, bool, error) {
	if land != "acme" || l.noPurse {
		return BudgetRecord{}, false, nil
	}
	if l.livePurse != nil {
		return l.livePurse(), true, nil
	}
	return l.budget, true, nil
}

func (l fakeLands) BudgetHistory(_ context.Context, land string, days int) (BudgetHistoryRecord, bool, error) {
	if land != "acme" || l.noPurse {
		return BudgetHistoryRecord{}, false, nil
	}
	if days < 1 || days > maxHistoryDays {
		return BudgetHistoryRecord{}, false, fmt.Errorf("days %d reached the source: the map must refuse it", days)
	}
	return l.history, true, l.historyErr
}

func (l fakeLands) Plot(_ context.Context, land string) (PlotRecord, bool, error) {
	if land != "acme" || l.plot.GitURL == "" {
		return PlotRecord{}, false, nil
	}
	return l.plot, true, nil
}

// fixture: land acme (prefix ac-) and a bead in another land (zz-).
func newMapFixture(t *testing.T, lands fakeLands) (*fakeDaemon, http.Handler) {
	t.Helper()
	d := newFakeDaemon()
	acme := []any{"project:acme"}
	d.add(map[string]any{"id": "ac-t1", "type": "task", "title": "Fix the gate", "description": "it creaks", "labels": acme, "assignee": "oidc:bo@example.com", "priority": 0, "status": "blocked",
		"dependencies": []any{map[string]any{"bead_id": "ac-t1", "depends_on_id": "ac-d1", "type": "blocks"}}})
	d.add(map[string]any{"id": "ac-b1", "type": "bug", "title": "Leaky tap", "labels": acme, "status": "closed", "priority": 3})
	d.add(map[string]any{"id": "ac-c1", "type": "chore", "title": "hidden chore", "labels": acme})
	d.add(map[string]any{"id": "zz-t1", "type": "task", "title": "Other land's task", "labels": []any{"project:zeta"}})
	d.add(map[string]any{"id": "ac-d1", "type": "decision", "title": "Pick a colour", "labels": acme, "kind": "data",
		"fields": map[string]any{"prompt": "Which colour?", "context": "for the shed", "requesting_plan": "ac-p1",
			"options":         []any{map[string]any{"id": "red", "label": "Paint it red", "short": "Red", "description": "Two coats.", "artifact_type": "plan"}, map[string]any{"id": "blue", "short": "Blue", "artifact_type": "slides"}},
			"requesting_task": "gd-internal", "operated_by": "oidc:op@example.com"}})
	d.add(map[string]any{"id": "ac-e1", "type": "escalation", "title": "Need a domain", "labels": acme, "kind": "data", "created_by": "oidc:al@example.com",
		"fields": map[string]any{"operation_type": "manual_operator_action", "reason": "for the site", "operation_spec": map[string]any{"cmd": "secret"}}})
	d.add(map[string]any{"id": "ac-p1", "type": "epic", "title": "Shed plan", "labels": []any{"project:acme", "plan"}, "heraldry": "🔥🧭📦",
		"fields": map[string]any{"plan_state": "ready", "phase_order": 3}})
	d.add(map[string]any{"id": "ac-s1", "type": "task", "title": "Lay the base", "labels": acme, "parent": "ac-p1", "status": "in_progress"})

	mux := http.NewServeMux()
	m := New(d.mux, lands, nil)
	if lands.pictures != nil {
		m.SetPictures(newPictures(lands.pictures, "sessions", ""))
	}
	Register(mux, m)
	// The daemon stamps its version on every response, outside the map.
	return d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", "v-test")
		w.Header().Set("X-Kbeads-Commit", "abc")
		mux.ServeHTTP(w, r)
	})
}

type resp struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr ...string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "/v1/lands/v1/lands"+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := resp{status: w.Code, header: w.Header(), body: w.Body.String()}
	assertHygiene(t, method+" "+path, out)
	return out
}

func decodeAs[T any](t *testing.T, r resp, want int) T {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
	var v T
	dec := json.NewDecoder(strings.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, r.body)
	}
	return v
}

func problemOf(t *testing.T, r resp, code landsv1.ProblemCode) landsv1.Problem {
	t.Helper()
	p := decodeAs[landsv1.Problem](t, r, problemShape[code].status)
	if p.Code != code || p.Type != "urn:lands:problem:"+string(code) || p.CorrelationId == "" {
		t.Errorf("problem = %+v, want code %s", p, code)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("problem Content-Type = %q", ct)
	}
	return p
}

func TestTickets_ListProjectsOnlyThisLandsTickets(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	page := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets", ""), http.StatusOK)
	var ids []string
	for _, it := range page.Items {
		ids = append(ids, it.Id)
	}
	// ac-s1 is a plan step, and a ticket too.
	if want := []string{"ac-b1", "ac-s1", "ac-t1"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	tk := page.Items[2]
	if tk.Status != landsv1.TicketStatusWaiting || tk.Priority != 1 || tk.Kind != landsv1.Task ||
		tk.Assignee == nil || *tk.Assignee != "bo" || tk.Body == nil || *tk.Body != "it creaks" {
		t.Errorf("ticket = %+v", tk)
	}
	if !tk.UpdatedAt.Equal(t0.Truncate(time.Second)) || tk.UpdatedAt.Nanosecond() != 0 {
		t.Errorf("updated_at = %v, want whole seconds", tk.UpdatedAt)
	}
	if page.Items[0].Status != landsv1.TicketStatusDone || page.Items[0].Priority != 3 {
		t.Errorf("closed bug = %+v", page.Items[0])
	}
	c := lastCall(d, rListBeads)
	if c.route != routeTable[rListBeads] || !strings.Contains(c.query, "labels=project%3Aacme") || !strings.Contains(c.query, "type=task%2Cbug%2Cfeature") {
		t.Errorf("list call = %+v", c)
	}
}

// lastCall is the last call d took on route id.
func lastCall(d *fakeDaemon, id routeID) fakeCall {
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].route == routeTable[id] {
			return d.calls[i]
		}
	}
	return fakeCall{}
}

// TestTickets_WaitingOnlyOnWhatTheLandSees is the contract's waiting rule
// (TicketStatus in openapi.yaml; D.12's blocked-ticket scenario): a ticket
// waits on an open blocker the land can see, and on nothing else, whether it
// is listed, read, filtered on or a plan's step.
func TestTickets_WaitingOnlyOnWhatTheLandSees(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	acme := []any{"project:acme"}
	edge := func(typ, id string) []any { return []any{map[string]any{"depends_on_id": id, "type": typ}} }
	d.add(map[string]any{"id": "ac-x1", "type": "epic", "title": "Not a plan", "labels": acme})
	for _, b := range []map[string]any{
		{"id": "ac-w1", "dependencies": edge("blocks", "ac-t1")},                              // a ticket of the land
		{"id": "ac-w2", "dependencies": edge("depends-on", "ac-e1"), "status": "in_progress"}, // an escalation, while held
		{"id": "ac-w3", "dependencies": edge("blocks", "zz-t1")},                              // another land's ticket
		{"id": "ac-w4", "status": "blocked"},                                                  // blocked by hand, on nothing
		{"id": "ac-w5", "dependencies": edge("blocks", "ac-c1")},                              // a chore the land never sees
		{"id": "ac-w6", "dependencies": edge("blocks", "ac-b1")},                              // a closed bug
		{"id": "ac-w7", "dependencies": edge("related", "ac-t1")},                             // not a blocking edge
		{"id": "ac-w8", "dependencies": edge("blocks", "ac-x1")},                              // an epic that is not a plan
	} {
		b["type"], b["title"], b["labels"] = "task", "Waits? "+b["id"].(string), acme
		d.add(b)
	}
	want := map[string]landsv1.TicketStatus{
		"ac-t1": landsv1.TicketStatusWaiting, // on decision ac-d1
		"ac-w1": landsv1.TicketStatusWaiting, "ac-w2": landsv1.TicketStatusWaiting,
		"ac-w3": landsv1.TicketStatusOpen, "ac-w4": landsv1.TicketStatusOpen, "ac-w5": landsv1.TicketStatusOpen,
		"ac-w6": landsv1.TicketStatusOpen, "ac-w7": landsv1.TicketStatusOpen, "ac-w8": landsv1.TicketStatusOpen,
		"ac-s1": landsv1.TicketStatusInProgress, "ac-b1": landsv1.TicketStatusDone,
	}
	for id, st := range want {
		if tk := decodeAs[landsv1.Ticket](t, do(t, h, "GET", "/acme/tickets/"+id, ""), http.StatusOK); tk.Status != st {
			t.Errorf("getTicket %s: %s, want %s", id, tk.Status, st)
		}
	}
	page := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets", ""), http.StatusOK)
	for _, tk := range page.Items {
		if tk.Status != want[tk.Id] {
			t.Errorf("listTickets %s: %s, want %s", tk.Id, tk.Status, want[tk.Id])
		}
	}
	for _, st := range []landsv1.TicketStatus{landsv1.TicketStatusOpen, landsv1.TicketStatusInProgress, landsv1.TicketStatusWaiting, landsv1.TicketStatusDone} {
		page := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets?status="+string(st), ""), http.StatusOK)
		var got, exp []string
		for _, tk := range page.Items {
			got = append(got, tk.Id)
		}
		for id, s := range want {
			if s == st {
				exp = append(exp, id)
			}
		}
		slices.Sort(got)
		slices.Sort(exp)
		if !slices.Equal(got, exp) {
			t.Errorf("status=%s lists %v, want %v", st, got, exp)
		}
	}
	// A plan's step waits the same way.
	d.beads["ac-s1"]["dependencies"] = edge("blocks", "ac-d1")
	p := decodeAs[landsv1.Plan](t, do(t, h, "GET", "/acme/plans/ac-p1", ""), http.StatusOK)
	if len(p.Steps) != 1 || p.Steps[0].Status != landsv1.TicketStatusWaiting {
		t.Errorf("plan steps = %+v, want ac-s1 waiting", p.Steps)
	}
	// A blocker the daemon will not show the caller does not hold a ticket.
	d.forced[rGetBead] = http.StatusForbidden
	if page := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets?status=waiting", ""), http.StatusOK); len(page.Items) != 0 {
		t.Errorf("status=waiting with every blocker refused = %+v", page.Items)
	}
}

func TestTickets_Pagination(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	p1 := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets?limit=2", ""), http.StatusOK)
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2 := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets?limit=2&cursor="+*p1.NextCursor, ""), http.StatusOK)
	if len(p2.Items) != 1 || p2.NextCursor != nil || p2.Items[0].Id != "ac-t1" {
		t.Fatalf("page 2 = %+v", p2)
	}
	problemOf(t, do(t, h, "GET", "/acme/tickets?cursor=bm9wZQ", ""), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "GET", "/acme/tickets?limit=201", ""), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "GET", "/acme/tickets?limit=abc", ""), landsv1.InvalidRequest)
}

func TestTickets_StatusFilterAndSearch(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets?status=open", ""), http.StatusOK)
	if q := lastCall(d, rListBeads).query; !strings.Contains(q, "status=open%2Cblocked%2Cdeferred") {
		t.Errorf("status=open query = %s", q)
	}
	page := decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/search/tickets?q=gate", ""), http.StatusOK)
	if len(page.Items) != 1 || page.Items[0].Id != "ac-t1" {
		t.Errorf("search = %+v", page)
	}
}

func TestTickets_GetOutsideTheLandOrNounIsNotFound(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	decodeAs[landsv1.Ticket](t, do(t, h, "GET", "/acme/tickets/ac-t1", ""), http.StatusOK)
	for _, id := range []string{"zz-t1", "ac-c1", "ac-d1", "ac-missing"} {
		problemOf(t, do(t, h, "GET", "/acme/tickets/"+id, ""), landsv1.NotFound)
	}
	// A land the caller is not in reads like one that does not exist.
	problemOf(t, do(t, h, "GET", "/zeta/tickets/zz-t1", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/zeta/tickets", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/Bad_Land/tickets", ""), landsv1.NotFound)
}

func TestTickets_CreateUpdateCloseReopenComment(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	tk := decodeAs[landsv1.Ticket](t, do(t, h, "POST", "/acme/tickets", `{"kind":"bug","title":"Door sticks","body":"after rain","priority":4}`, "Idempotency-Key", "k-1"), http.StatusCreated)
	if tk.Kind != landsv1.Bug || tk.Priority != 4 || tk.Status != landsv1.TicketStatusOpen {
		t.Errorf("created = %+v", tk)
	}
	create := d.calls[len(d.calls)-1]
	if create.body["idempotency_key"] != "k-1" || create.body["type"] != "bug" ||
		!slices.Equal(create.body["labels"].([]any), []any{"project:acme"}) {
		t.Errorf("create body = %v", create.body)
	}

	tk = decodeAs[landsv1.Ticket](t, do(t, h, "PATCH", "/acme/tickets/"+tk.Id, `{"title":"Door sticks badly"}`), http.StatusOK)
	if tk.Title != "Door sticks badly" {
		t.Errorf("updated = %+v", tk)
	}
	if _, sent := d.calls[len(d.calls)-2].body["heraldry"]; sent || tk.Heraldry != nil {
		t.Errorf("an edit naming no arms sent or showed some: %v, %v", d.calls[len(d.calls)-2].body, tk.Heraldry)
	}

	// Arms pass through to the daemon, which owns the form and the register;
	// "" on an edit removes them.
	armed := decodeAs[landsv1.Ticket](t, do(t, h, "POST", "/acme/tickets", `{"kind":"task","title":"Price the apples","heraldry":"🍎🏷️🪙"}`), http.StatusCreated)
	if got := d.calls[len(d.calls)-1].body["heraldry"]; got != "🍎🏷️🪙" || armed.Heraldry == nil || *armed.Heraldry != "🍎🏷️🪙" {
		t.Errorf("create with arms sent %v, answered %v", got, armed.Heraldry)
	}
	armed = decodeAs[landsv1.Ticket](t, do(t, h, "PATCH", "/acme/tickets/"+armed.Id, `{"heraldry":""}`), http.StatusOK)
	if got, sent := d.calls[len(d.calls)-2].body["heraldry"]; !sent || got != "" || armed.Heraldry != nil {
		t.Errorf("removing arms sent %v (%v), answered %v", got, sent, armed.Heraldry)
	}
	tk = decodeAs[landsv1.Ticket](t, do(t, h, "POST", "/acme/tickets/"+tk.Id+"/close", `{"reason":"fixed"}`), http.StatusOK)
	if tk.Status != landsv1.TicketStatusDone {
		t.Errorf("closed = %+v", tk)
	}
	tk = decodeAs[landsv1.Ticket](t, do(t, h, "POST", "/acme/tickets/"+tk.Id+"/reopen", ""), http.StatusOK)
	if tk.Status != landsv1.TicketStatusOpen {
		t.Errorf("reopened = %+v", tk)
	}
	c := decodeAs[landsv1.Comment](t, do(t, h, "POST", "/acme/tickets/"+tk.Id+"/comments", `{"text":"thanks"}`), http.StatusCreated)
	if c.Author != "al" || c.Text != "thanks" || c.Id == "" || c.Id == "41" {
		t.Errorf("comment = %+v", c)
	}
	// Writes on another land's ticket never reach a write route.
	before := len(d.calls)
	problemOf(t, do(t, h, "PATCH", "/acme/tickets/zz-t1", `{"title":"x"}`), landsv1.NotFound)
	for _, c := range d.calls[before:] {
		if c.route.Method != "GET" {
			t.Errorf("write route called for another land's ticket: %v", c.route)
		}
	}
}

func TestTickets_RequestValidation(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/acme/tickets", `{"kind":"epic","title":"x"}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":""}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x","priority":0}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x","labels":["project:zeta"]}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x","type":"decision"}`},
		{"PATCH", "/acme/tickets/ac-t1", `{}`},
		{"PATCH", "/acme/tickets/ac-t1", `{"status":"closed"}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x","heraldry":""}`},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x","heraldry":"` + strings.Repeat("🍎", 201) + `"}`},
		{"PATCH", "/acme/tickets/ac-t1", `{"heraldry":"` + strings.Repeat("🍎", 201) + `"}`},
		{"POST", "/acme/tickets/ac-t1/comments", `{"text":""}`},
	} {
		problemOf(t, do(t, h, tc.method, tc.path, tc.body), landsv1.InvalidRequest)
	}
}

func TestWrites_InTypeRows(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	do(t, h, "POST", "/acme/tickets", `{"kind":"task","title":"a","body":"b","priority":2,"heraldry":"red 🍎 . . . ."}`)
	do(t, h, "PATCH", "/acme/tickets/ac-t1", `{"title":"a","body":"b","priority":3,"heraldry":""}`)
	do(t, h, "POST", "/acme/tickets/ac-b1/reopen", "")
	do(t, h, "POST", "/acme/escalations", `{"title":"a","reason":"b"}`)
	for _, c := range d.calls {
		if c.route != routeTable[rCreateBead] && c.route != routeTable[rUpdateBead] {
			continue
		}
		typ, _ := c.body["type"].(string)
		if typ == "" {
			typ = "task"
		}
		row, _ := RowFor(modelType(typ))
		for k, v := range c.body {
			if k == "idempotency_key" {
				continue
			}
			if k == "fields" {
				for fk := range v.(map[string]any) {
					if !slices.Contains(row.Writes, "fields."+fk) {
						t.Errorf("%s writes fields.%s, not in its TypeRow.Writes", typ, fk)
					}
				}
				continue
			}
			if !slices.Contains(row.Writes, k) {
				t.Errorf("%s writes %s, not in its TypeRow.Writes", typ, k)
			}
		}
	}
}

func TestDecisions(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	dec := decodeAs[landsv1.Decision](t, do(t, h, "GET", "/acme/decisions/ac-d1", ""), http.StatusOK)
	if dec.Question != "Which colour?" || dec.Context == nil || *dec.Context != "for the shed" ||
		dec.Status != landsv1.DecisionStatusOpen || dec.Chosen != nil ||
		!reflect.DeepEqual(dec.Options, []landsv1.DecisionOption{
			{Id: "red", Label: "Paint it red", Short: ptr("Red"), Description: ptr("Two coats."), ArtifactType: ptr(landsv1.ArtifactPlan)},
			// Only a short label: it is the label. An artifact type the
			// contract does not list is left out.
			{Id: "blue", Label: "Blue"},
		}) {
		t.Errorf("decision = %+v", dec)
	}
	page := decodeAs[landsv1.DecisionPage](t, do(t, h, "GET", "/acme/decisions?status=open", ""), http.StatusOK)
	if len(page.Items) != 1 {
		t.Errorf("open decisions = %+v", page)
	}
	problemOf(t, do(t, h, "POST", "/acme/decisions/ac-d1/resolve", `{"option_id":"green"}`), landsv1.InvalidRequest)
	dec = decodeAs[landsv1.Decision](t, do(t, h, "POST", "/acme/decisions/ac-d1/resolve", `{"option_id":"red","note":"bright"}`), http.StatusOK)
	if dec.Status != landsv1.DecisionStatusResolved || dec.Chosen == nil || *dec.Chosen != "red" {
		t.Errorf("resolved = %+v", dec)
	}
	// The same answer again is a no-op; a different one is a conflict.
	decodeAs[landsv1.Decision](t, do(t, h, "POST", "/acme/decisions/ac-d1/resolve", `{"option_id":"red"}`), http.StatusOK)
	problemOf(t, do(t, h, "POST", "/acme/decisions/ac-d1/resolve", `{"option_id":"blue"}`), landsv1.Conflict)
	problemOf(t, do(t, h, "POST", "/acme/decisions/ac-d1/cancel", `{"reason":"nah"}`), landsv1.Conflict)
	page = decodeAs[landsv1.DecisionPage](t, do(t, h, "GET", "/acme/decisions?status=resolved", ""), http.StatusOK)
	if len(page.Items) != 1 {
		t.Errorf("resolved decisions = %+v", page)
	}
	problemOf(t, do(t, h, "GET", "/acme/decisions/ac-t1", ""), landsv1.NotFound)
}

// Evidence stays absent until a land-door image route exists: a tenant
// cannot GET the viewer's admin-only /image/<key>, so no linked record's
// screenshot projects, land-local or not.
func TestDecisions_NoEvidenceWithoutALandImageRoute(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	d.add(map[string]any{"id": "ac-t2", "type": "task", "title": "Shed photos", "labels": []any{"project:acme"},
		"description": "![front](https://viewer.example/image/images/ac-t2/front.png) ![back](https://viewer.example/image/images/ac-t2/back.png)"})
	d.add(map[string]any{"id": "ac-d2", "type": "decision", "title": "Which side?", "labels": []any{"project:acme"}, "kind": "data",
		"fields": map[string]any{"prompt": "Which side?", "evidence": []any{"ac-t2"}, "requesting_task": "gd-internal",
			"options": []any{map[string]any{"id": "front", "label": "Front"}}}})
	r := do(t, h, "GET", "/acme/decisions/ac-d2", "")
	if dec := decodeAs[landsv1.Decision](t, r, http.StatusOK); dec.Evidence != nil {
		t.Errorf("evidence = %+v, want absent", *dec.Evidence)
	}
	if strings.Contains(r.body, "/image/") || strings.Contains(r.body, "viewer.example") {
		t.Errorf("decision carries an image link:\n%s", r.body)
	}
	assertHygiene(t, "decision ac-d2", r)
}

func TestDecisions_CancelReadsCancelled(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	problemOf(t, do(t, h, "POST", "/acme/decisions/ac-d1/cancel", `{}`), landsv1.InvalidRequest)
	dec := decodeAs[landsv1.Decision](t, do(t, h, "POST", "/acme/decisions/ac-d1/cancel", `{"reason":"moot"}`), http.StatusOK)
	if dec.Status != landsv1.DecisionStatusCancelled || dec.Chosen != nil {
		t.Errorf("cancelled = %+v", dec)
	}
}

func TestEscalations(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	e := decodeAs[landsv1.Escalation](t, do(t, h, "GET", "/acme/escalations/ac-e1", ""), http.StatusOK)
	if e.Reason != "for the site" || e.RequestedBy != "al" || e.Status != landsv1.EscalationStatusOpen {
		t.Errorf("escalation = %+v", e)
	}
	e = decodeAs[landsv1.Escalation](t, do(t, h, "POST", "/acme/escalations", `{"title":"Need DNS","reason":"for mail"}`), http.StatusCreated)
	if e.Title != "Need DNS" || e.Reason != "for mail" || e.Status != landsv1.EscalationStatusOpen {
		t.Errorf("created = %+v", e)
	}
	var create fakeCall
	for _, c := range d.calls {
		if c.route == routeTable[rCreateBead] {
			create = c
		}
	}
	if f := create.body["fields"].(map[string]any); f["operation_type"] != tenantEscalationOperation {
		t.Errorf("escalation fields = %v", f)
	}
	page := decodeAs[landsv1.EscalationPage](t, do(t, h, "GET", "/acme/escalations?status=approved", ""), http.StatusOK)
	if len(page.Items) != 0 {
		t.Errorf("approved = %+v", page)
	}
}

func TestPlans(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	p := decodeAs[landsv1.Plan](t, do(t, h, "GET", "/acme/plans/ac-p1", ""), http.StatusOK)
	if p.State != landsv1.Ready || len(p.Steps) != 1 || p.Steps[0].Status != landsv1.TicketStatusInProgress {
		t.Errorf("plan = %+v", p)
	}
	// Arms pass through as they are stored; a step bearing none carries none.
	if p.Heraldry == nil || *p.Heraldry != "🔥🧭📦" || p.Steps[0].Heraldry != nil {
		t.Errorf("plan arms = %v, step arms = %v", p.Heraldry, p.Steps[0].Heraldry)
	}
	page := decodeAs[landsv1.PlanPage](t, do(t, h, "GET", "/acme/plans", ""), http.StatusOK)
	if len(page.Items) != 1 || page.Items[0].Heraldry == nil {
		t.Errorf("plans = %+v", page)
	}
	problemOf(t, do(t, h, "GET", "/acme/plans/ac-t1", ""), landsv1.NotFound)
	dec := decodeAs[landsv1.Decision](t, do(t, h, "POST", "/acme/plans/ac-p1/answer", `{"decision_id":"ac-d1","option_id":"blue"}`), http.StatusOK)
	if dec.Chosen == nil || *dec.Chosen != "blue" {
		t.Errorf("answered = %+v", dec)
	}
}

func TestLandAndWhoami(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	l := decodeAs[landsv1.Land](t, do(t, h, "GET", "/acme", ""), http.StatusOK)
	if l.Id != "acme" || l.Name != "acme" || l.Status != landsv1.LandStatusActive {
		t.Errorf("land = %+v", l)
	}
	m := decodeAs[landsv1.Member](t, do(t, h, "GET", "/acme/whoami", ""), http.StatusOK)
	if m.Name != "al" || m.Role != landsv1.MemberRoleOwner || !strings.HasPrefix(m.Id, "m") {
		t.Errorf("whoami = %+v", m)
	}
	problemOf(t, do(t, h, "GET", "/zeta", ""), landsv1.NotFound)
}

// The crew is the daemon's members of the land, each with their land role, and
// a member's id is the id whoami gives the same person.
func TestCrew(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	pg := decodeAs[landsv1.MemberPage](t, do(t, h, "GET", "/acme/crew", ""), http.StatusOK)
	if len(pg.Items) != 3 || pg.NextCursor != nil {
		t.Fatalf("crew = %+v", pg)
	}
	want := []landsv1.Member{
		{Name: "Al", Role: landsv1.MemberRoleOwner, Status: landsv1.MemberStatusActive},
		{Name: "bo", Role: landsv1.MemberRoleGuest, Status: landsv1.MemberStatusActive},
		// No name falls back to the principal's; no role recorded reads other.
		{Name: "cy", Role: landsv1.MemberRoleOther, Status: landsv1.MemberStatusActive},
	}
	for i, m := range pg.Items {
		w := want[i]
		w.Id = m.Id
		if m != w || !strings.HasPrefix(m.Id, "m") {
			t.Errorf("crew[%d] = %+v, want %+v", i, m, w)
		}
	}
	if c := d.calls[len(d.calls)-1]; c.route != routeTable[rLandMembers] || c.path != "/v1/lands/acme/members" {
		t.Errorf("crew called %+v", c)
	}

	me := decodeAs[landsv1.Member](t, do(t, h, "GET", "/acme/whoami", ""), http.StatusOK)
	if me.Id != pg.Items[0].Id {
		t.Errorf("whoami id %s, crew id %s: one person, two ids", me.Id, pg.Items[0].Id)
	}
	got := decodeAs[landsv1.Member](t, do(t, h, "GET", "/acme/crew/"+me.Id, ""), http.StatusOK)
	if got != pg.Items[0] {
		t.Errorf("getMember = %+v, want %+v", got, pg.Items[0])
	}
	problemOf(t, do(t, h, "GET", "/acme/crew/mnobody", ""), landsv1.NotFound)

	// Paged: a cursor moves past the first page.
	p1 := decodeAs[landsv1.MemberPage](t, do(t, h, "GET", "/acme/crew?limit=2", ""), http.StatusOK)
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2 := decodeAs[landsv1.MemberPage](t, do(t, h, "GET", "/acme/crew?limit=2&cursor="+*p1.NextCursor, ""), http.StatusOK)
	if len(p2.Items) != 1 || p2.NextCursor != nil || p2.Items[0] != pg.Items[2] {
		t.Errorf("page 2 = %+v", p2)
	}
	problemOf(t, do(t, h, "GET", "/acme/crew?cursor=bogus", ""), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "GET", "/acme/crew?limit=0", ""), landsv1.InvalidRequest)

	// Another land reads as absent, and never reaches the daemon.
	n := len(d.calls)
	problemOf(t, do(t, h, "GET", "/zeta/crew", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/zeta/crew/"+me.Id, ""), landsv1.NotFound)
	if len(d.calls) != n {
		t.Errorf("another land's crew reached the daemon: %v", d.calls[n:])
	}
}

// A land has one plot, its repository, or none; the plot shows the
// repository's name and branch, never its address.
func TestPlots(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{plot: PlotRecord{GitURL: "https://github.com/acme-co/shed-web.git", DefaultBranch: "trunk"}})
	pg := decodeAs[landsv1.PlotPage](t, do(t, h, "GET", "/acme/plots", ""), http.StatusOK)
	if len(pg.Items) != 1 || pg.NextCursor != nil {
		t.Fatalf("plots = %+v", pg)
	}
	p := pg.Items[0]
	if p.Name != "shed-web" || p.DefaultBranch != "trunk" || !strings.HasPrefix(p.Id, "p") {
		t.Errorf("plot = %+v", p)
	}
	r := do(t, h, "GET", "/acme/plots/"+p.Id, "")
	if got := decodeAs[landsv1.Plot](t, r, http.StatusOK); got != p {
		t.Errorf("getPlot = %+v, want %+v", got, p)
	}
	for _, leak := range []string{"github", "acme-co", ".git"} {
		if strings.Contains(r.body, leak) {
			t.Errorf("plot carries %q: %s", leak, r.body)
		}
	}
	problemOf(t, do(t, h, "GET", "/acme/plots/pnothing", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/zeta/plots", ""), landsv1.NotFound)

	// No repository: no plots, and no plot by any id.
	_, h = newMapFixture(t, fakeLands{})
	if pg := decodeAs[landsv1.PlotPage](t, do(t, h, "GET", "/acme/plots", ""), http.StatusOK); len(pg.Items) != 0 {
		t.Errorf("plots with no repository = %+v", pg)
	}
	problemOf(t, do(t, h, "GET", "/acme/plots/"+p.Id, ""), landsv1.NotFound)
}

func TestPlotFrom(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/o/shed.git":  "shed",
		"git@github.com:o/shed.git":      "shed",
		"https://gitlab.com/g/sub/shed/": "shed",
		"shed":                           "shed",
		"https://github.com/o/":          "o",
	} {
		if got := plotFrom("acme", PlotRecord{GitURL: url}); got.Name != want || got.DefaultBranch != "main" {
			t.Errorf("plotFrom(%q) = %+v, want name %q, branch main", url, got, want)
		}
	}
	if a, b := plotFrom("acme", PlotRecord{GitURL: "x/a"}), plotFrom("acme", PlotRecord{GitURL: "y/a"}); a.Id == b.Id {
		t.Error("two repositories share a plot id")
	}
}

func TestPausedLandTakesNoWrites(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{paused: true})
	l := decodeAs[landsv1.Land](t, do(t, h, "GET", "/acme", ""), http.StatusOK)
	if l.Status != landsv1.LandStatusPaused {
		t.Errorf("land = %+v", l)
	}
	problemOf(t, do(t, h, "POST", "/acme/tickets", `{"kind":"task","title":"x"}`), landsv1.LandPaused)
	for _, c := range d.calls {
		if c.route.Method != "GET" {
			t.Errorf("paused land: %v called", c.route)
		}
	}
	decodeAs[landsv1.TicketPage](t, do(t, h, "GET", "/acme/tickets", ""), http.StatusOK)
}

// A daemon refusal becomes a code; its text, headers and status line never
// reach the tenant.
func TestUpstreamErrorsAreCodesOnly(t *testing.T) {
	for st, code := range map[int]landsv1.ProblemCode{
		http.StatusBadRequest:          landsv1.InvalidRequest,
		http.StatusUnauthorized:        landsv1.Unauthenticated,
		http.StatusForbidden:           landsv1.Forbidden,
		http.StatusConflict:            landsv1.Conflict,
		http.StatusTooManyRequests:     landsv1.RateLimited,
		http.StatusInternalServerError: landsv1.UpstreamUnavailable,
		http.StatusBadGateway:          landsv1.UpstreamUnavailable,
	} {
		d, h := newMapFixture(t, fakeLands{})
		d.forced[rCreateBead] = st
		p := problemOf(t, do(t, h, "POST", "/acme/tickets", `{"kind":"task","title":"x"}`), code)
		if p.Detail != nil {
			t.Errorf("%d: detail %q", st, *p.Detail)
		}
	}
	// A refused read of a bead looks like an absent one.
	d, h := newMapFixture(t, fakeLands{})
	d.forced[rGetBead] = http.StatusForbidden
	problemOf(t, do(t, h, "GET", "/acme/tickets/ac-t1", ""), landsv1.NotFound)
}

func TestDeferredVerbsAreNotFound(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{})
	for _, p := range []string{"/acme/threads", "/acme/threads/x", "/acme/threads/x/messages"} {
		problemOf(t, do(t, h, "GET", p, ""), landsv1.NotFound)
	}
	problemOf(t, do(t, h, "POST", "/acme/threads/x/messages", `{"text":"hi"}`), landsv1.NotFound)
}

// The budget is the purse snapshot: exhausted at the wall, when the purse is
// unknown and when the snapshot is stale; no snapshot is unavailable, never
// zeros; an operator land and another land have no budget.
func TestBudget(t *testing.T) {
	fig := func(v float64) *float64 { return &v }
	fresh := time.Now().Add(-time.Minute)
	for _, c := range []struct {
		name string
		rec  BudgetRecord
		want landsv1.BudgetStatus
	}{
		{"ok", BudgetRecord{Granted: fig(100), Used: fig(30.25), State: "ok", ReadAt: fresh}, landsv1.Ok},
		// At the wall, spent is still under the limit: the margin.
		{"wall", BudgetRecord{Granted: fig(100), Used: fig(96), State: "wall", ReadAt: fresh}, landsv1.Exhausted},
		{"unknown", BudgetRecord{Granted: fig(100), Used: fig(30), State: "unknown", ReadAt: fresh}, landsv1.Exhausted},
		{"stale", BudgetRecord{Granted: fig(100), Used: fig(30), State: "ok", ReadAt: time.Now().Add(-purseUnknownAfter - time.Minute)}, landsv1.Exhausted},
		{"no state", BudgetRecord{Granted: fig(100), Used: fig(30), ReadAt: fresh}, landsv1.Exhausted},
	} {
		_, h := newMapFixture(t, fakeLands{budget: c.rec})
		b := decodeAs[landsv1.Budget](t, do(t, h, "GET", "/acme/budget", ""), http.StatusOK)
		want := landsv1.Budget{Limit: float32(*c.rec.Granted), Spent: float32(*c.rec.Used), Unit: landsv1.Credits, Status: c.want}
		if b != want {
			t.Errorf("%s: budget = %+v, want %+v", c.name, b, want)
		}
	}
	for name, rec := range map[string]BudgetRecord{
		"no snapshot": {},
		"no used":     {Granted: fig(100), State: "unknown", ReadAt: fresh},
		"no read_at":  {Granted: fig(100), Used: fig(1), State: "ok"},
	} {
		_, h := newMapFixture(t, fakeLands{budget: rec})
		if p := problemOf(t, do(t, h, "GET", "/acme/budget", ""), landsv1.UpstreamUnavailable); p.Detail != nil {
			t.Errorf("%s: detail %q", name, *p.Detail)
		}
	}
	_, h := newMapFixture(t, fakeLands{noPurse: true})
	problemOf(t, do(t, h, "GET", "/acme/budget", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/zeta/budget", ""), landsv1.NotFound)
}

func TestFillPattern(t *testing.T) {
	got, err := fillPattern("/v1/beads/{id}/comments", []string{"ac-1/../x"})
	if err != nil || got != "/v1/beads/ac-1%2F..%2Fx/comments" {
		t.Errorf("fillPattern = %q, %v", got, err)
	}
	if _, err := fillPattern("/v1/beads/{id}", nil); err == nil {
		t.Error("missing argument accepted")
	}
	if _, err := fillPattern("/v1/beads", []string{"x"}); err == nil {
		t.Error("extra argument accepted")
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"oidc:al@example.com": "al",
		"al@example.com":      "al",
		"gardenio-crew-x":     "gardenio-crew-x",
		"slack:U123":          "U123",
		"":                    "",
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// An applicant is refused every route, ahead of parameter checks, deferred
// verbs and the land check alike.
func TestApplicantRefusedEverywhere(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{applicant: true})
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/acme/whoami", ""},
		{"GET", "/acme/tickets?limit=abc", ""},
		{"GET", "/acme/search/tickets", ""},
		{"GET", "/acme/budget", ""},
		{"GET", "/acme/budget/history?days=0", ""},
		{"GET", "/zeta/tickets", ""},
		{"POST", "/acme/tickets", `{"kind":"task","title":"x"}`},
	} {
		problemOf(t, do(t, h, tc.method, tc.path, tc.body), landsv1.Forbidden)
	}
	if len(d.calls) != 0 {
		t.Errorf("an applicant reached the daemon: %v", d.calls)
	}
}
