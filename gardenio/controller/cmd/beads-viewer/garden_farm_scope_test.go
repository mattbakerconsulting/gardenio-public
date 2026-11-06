package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// farmFake is a two-land daemon for the farm read model: it honours the list filters the farm sends (types,
// statuses, labels, field presence, updated_after, paging, count-only) and answers the batch reads.
type farmFake struct {
	mu    sync.Mutex
	beads []*beadsapi.BeadDetail
}

func (f *farmFake) add(bs ...*beadsapi.BeadDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beads = append(f.beads, bs...)
}

func (f *farmFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []*beadsapi.BeadDetail
	for _, b := range f.beads {
		ok := (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			(q.UpdatedAfter.IsZero() || b.UpdatedAt.After(q.UpdatedAfter))
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		for k, v := range q.Fields {
			switch v {
			case "*":
				ok = ok && b.Fields[k] != ""
			default:
				ok = ok && b.Fields[k] == v
			}
		}
		if ok {
			c := *b
			if !q.WithParent {
				c.ParentID = ""
			}
			m = append(m, &c)
		}
	}
	total := len(m)
	if q.CountOnly {
		return &beadsapi.ListBeadsResult{Total: total}, nil
	}
	m = m[min(q.Offset, len(m)):]
	if q.Limit > 0 && len(m) > q.Limit {
		m = m[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: m, Total: total}, nil
}

// EventStream makes farmFake a hub Source too (the /garden page's lists): a stream that sends nothing.
func (f *farmFake) EventStream(ctx context.Context, _ string) (<-chan beadsapi.SSEEvent, error) {
	ch := make(chan beadsapi.SSEEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func (f *farmFake) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*beadsapi.BeadDetail{}
	for _, b := range f.beads {
		if slices.Contains(ids, b.ID) {
			out[b.ID] = b
		}
	}
	return out, nil
}

func (f *farmFake) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]beadsapi.Dependency{}
	for _, b := range f.beads {
		if slices.Contains(ids, b.ID) && b.ParentID != "" {
			out[b.ID] = append(out[b.ID], beadsapi.Dependency{BeadID: b.ID, DependsOnID: b.ParentID, Type: "parent-child"})
		}
	}
	return out, nil
}

func (f *farmFake) BatchGetChildrenByStatus(_ context.Context, ids []string) (map[string]map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]int{}
	for _, id := range ids {
		out[id] = map[string]int{}
	}
	for _, b := range f.beads {
		if h, ok := out[b.ParentID]; ok {
			h[b.Status]++
		}
	}
	return out, nil
}

// farmBead is a bead of typ in project; plan and state make it a plan (state "ready" with status open is a seed).
func farmBead(id, project, typ, status, parent string, plan bool, state string) *beadsapi.BeadDetail {
	now := time.Now().Add(-time.Hour)
	b := &beadsapi.BeadDetail{
		ID: id, Title: "Title " + id, Type: typ, Status: status, ParentID: parent, Priority: 2,
		Labels: []string{"project:" + project}, Fields: map[string]string{},
		CreatedAt: now, UpdatedAt: now,
	}
	if plan {
		b.Labels = append(b.Labels, "plan")
		b.Fields["plan_state"] = state
	}
	return b
}

// twoLandFarm is a farm with a building plan, a seed and a meadow bead in each of gardenio and claimlayer.
func twoLandFarm() *farmFake {
	f := &farmFake{}
	for _, p := range []struct{ project, pre string }{{"gardenio", "gd"}, {"claimlayer", "cl"}} {
		f.add(
			farmBead(p.pre+"-plan", p.project, "epic", "in_progress", "", true, "building"),
			farmBead(p.pre+"-step", p.project, "task", "in_progress", p.pre+"-plan", false, ""),
			farmBead(p.pre+"-seed", p.project, "epic", "open", "", true, "ready"),
			farmBead(p.pre+"-meadow", p.project, "task", "open", "", false, ""),
		)
	}
	return f
}

// TestGardenFarm_ScopedPrincipalThroughTheChain drives /garden, /garden/farm and /garden/farm/events through the
// viewer's own chain (scopeMiddleware, the Server's mux, gardenhub with gardenFarmCut) for real OIDC principals
// (gd-16UJtQh3Ua), on the route classes as registered (routeScoped since gd-l7yYxZpoZo): a claimlayer member's page,
// first paint and stream carry only claimlayer's plan, seed and meadow, a member with no project groups gets an empty
// but valid farm, and an admin still gets both lands.
func TestGardenFarm_ScopedPrincipalThroughTheChain(t *testing.T) {
	src := twoLandFarm()
	farm := gardenhub.NewFarm(src, testLogger())
	farm.Start()
	defer farm.Close()
	hub := gardenhub.New(src, "", testLogger())
	hub.Start()
	defer hub.Close()

	srv := NewServer(newScopedDaemon(twoProjectFake()), testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetGardenHandler(&gardenhub.Handler{Hub: hub, Farm: farm, Cut: gardenFarmCut,
		ReadOnly: func(r *http.Request) bool { return viewerScopeFromContext(r.Context()).Scoped() }})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	chain := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	who := map[string]principal{
		"claimlayer": scopedPrincipal(),
		"nogroups":   {email: "new@example.com", roles: scopedPrincipal().roles},
		"admin":      adminPrincipal(),
	}
	// The principal rides a test header into the verified context, as the OIDC middleware would put it there.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chain.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), who[r.Header.Get("X-Test-Who")])))
	}))
	defer ts.Close()

	gardenioIDs := []string{"gd-plan", "gd-step", "gd-seed", "gd-meadow", "gd-new", `"gardenio"`}
	claimlayerIDs := []string{"cl-plan", "cl-step", "cl-seed", "cl-meadow", "cl-new", `"claimlayer"`}
	page := func(as string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/garden", nil)
		req.Header.Set("X-Test-Who", as)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		_, _ = io.Copy(&b, resp.Body)
		return resp.StatusCode, b.String()
	}
	get := func(as string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/garden/farm", nil)
		req.Header.Set("X-Test-Who", as)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			b.WriteString(sc.Text())
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /garden/farm as %s = %d: %s", as, resp.StatusCode, b.String())
		}
		if !json.Valid([]byte(b.String())) {
			t.Fatalf("GET /garden/farm as %s is not JSON: %s", as, b.String())
		}
		return b.String()
	}
	has := func(doc string, ids []string) (in, out []string) {
		for _, id := range ids {
			if strings.Contains(doc, id) {
				in = append(in, id)
			} else {
				out = append(out, id)
			}
		}
		return in, out
	}

	// The page: its lists (the hub's in-progress steps) cut to the member's projects, read only, and told it is cut.
	code, adminPage := page("admin")
	if code != http.StatusOK || !strings.Contains(adminPage, "i-gd-step") || !strings.Contains(adminPage, "i-cl-step") {
		t.Fatalf("GET /garden as admin = %d, want both lands' steps in its lists:\n%s", code, adminPage)
	}
	code, minePage := page("claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET /garden as the claimlayer member = %d, want 200", code)
	}
	if !strings.Contains(minePage, "i-cl-step") || strings.Contains(minePage, "gd-step") {
		t.Errorf("the claimlayer member's page lists are not its cut")
	}
	if !strings.Contains(minePage, `data-ro="1"`) || !strings.Contains(minePage, `data-cut="1"`) {
		t.Errorf("the claimlayer member's page is not read only and cut")
	}

	admin := get("admin")
	for _, id := range []string{"gd-plan", "gd-seed", "gd-meadow", "cl-plan", "cl-seed", "cl-meadow"} {
		if !strings.Contains(admin, id) {
			t.Fatalf("the admin farm lacks %s: the fixture does not reach the surface under test\n%s", id, admin)
		}
	}

	mine := get("claimlayer")
	if leaked, _ := has(mine, gardenioIDs); len(leaked) > 0 {
		t.Errorf("the claimlayer member's first paint carries %v", leaked)
	}
	if _, lost := has(mine, []string{"cl-plan", "cl-step", "cl-seed", "cl-meadow"}); len(lost) > 0 {
		t.Errorf("the claimlayer member's first paint lacks its own %v", lost)
	}

	var empty struct {
		Plans, Seeds, Gardeners []json.RawMessage
		Meadow                  struct{ Total int }
	}
	none := get("nogroups")
	if err := json.Unmarshal([]byte(none), &empty); err != nil || empty.Plans == nil || len(empty.Plans)+len(empty.Seeds)+len(empty.Gardeners)+empty.Meadow.Total != 0 {
		t.Errorf("a member with no project groups: %s, want an empty but valid farm", none)
	}
	if strings.Contains(none, "gd-") || strings.Contains(none, "cl-") {
		t.Errorf("the empty farm names a bead: %s", none)
	}

	// The stream: a reset to the member's cut, then the patch for a new step in each land carries only claimlayer's.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/garden/farm/events", nil)
	req.Header.Set("X-Test-Who", "claimlayer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /garden/farm/events as the claimlayer member = %d", resp.StatusCode)
	}
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(prefix string) string {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %q", prefix)
				}
				if leaked, _ := has(l, gardenioIDs); len(leaked) > 0 {
					t.Errorf("the claimlayer stream carries %v: %s", leaked, l)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q line", prefix)
			}
		}
	}
	next("event: reset")
	if d := next("data: "); !strings.Contains(d, "cl-meadow") {
		t.Errorf("the reset is not the member's cut: %s", d)
	}

	src.add(farmBead("gd-new", "gardenio", "task", "open", "gd-plan", false, ""), farmBead("cl-new", "claimlayer", "task", "open", "cl-plan", false, ""))
	farm.Invalidate()
	next("event: patch")
	if d := next("data: "); !strings.Contains(d, "cl-new") {
		t.Errorf("the patch does not carry the new claimlayer step: %s", d)
	}

	// After the change the admin farm has both new steps; the member's first paint has only its own.
	if leaked, _ := has(get("claimlayer"), gardenioIDs); len(leaked) > 0 {
		t.Errorf("the claimlayer member's first paint after the change carries %v", leaked)
	}
	if _, lost := has(get("admin"), append(gardenioIDs[:5:5], claimlayerIDs[:5]...)); len(lost) > 0 {
		t.Errorf("the admin farm lacks %v", lost)
	}
}
