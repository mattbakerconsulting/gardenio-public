package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

func TestScopeFromRequest(t *testing.T) {
	cases := []struct {
		name         string
		roles        []string
		groups       []string
		wantScoped   bool
		wantProjects []string
	}{
		{"project member with groups", []string{oidcauth.RoleProjectMember}, []string{"/project-claimlayer", "/project-acme", "/staff"}, true, []string{"acme", "claimlayer"}},
		{"group without leading slash", []string{oidcauth.RoleProjectMember}, []string{"project-claimlayer"}, true, []string{"claimlayer"}},
		{"duplicate groups collapse", []string{oidcauth.RoleProjectMember}, []string{"/project-a", "/project-a"}, true, []string{"a"}},
		{"project member, no groups claim: scoped, admits nothing", []string{oidcauth.RoleProjectMember}, nil, true, nil},
		{"bare prefix is not a project", []string{oidcauth.RoleProjectMember}, []string{"/project-"}, true, nil},
		{"land-role subgroups name their land once", []string{oidcauth.RoleProjectMember}, []string{"/project-claimlayer/owners", "/project-claimlayer", "/project-acme/guests"}, true, []string{"acme", "claimlayer"}},
		{"admin who is also a project member is unrestricted", []string{oidcauth.RoleProjectMember, oidcauth.RoleAdmin}, []string{"/project-claimlayer"}, false, nil},
		{"operator without project-member is unrestricted", []string{oidcauth.RoleOperator}, []string{"/project-claimlayer"}, false, nil},
		{"no roles at all (dev bypass, probes) is unrestricted", nil, nil, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := requestAs("GET", "/", principal{email: "u@example.com", roles: c.roles, groups: c.groups})
			got := scopeFromRequest(r)
			if got.Scoped() != c.wantScoped {
				t.Fatalf("Scoped() = %v, want %v (scope %+v)", got.Scoped(), c.wantScoped, got)
			}
			if !slices.Equal(got.Projects, c.wantProjects) {
				t.Fatalf("Projects = %v, want %v", got.Projects, c.wantProjects)
			}
		})
	}
}

// A raw group header must not be able to widen a scope: only the verified
// context is read.
func TestScopeFromRequest_IgnoresRawHeaders(t *testing.T) {
	r := requestAs("GET", "/", scopedPrincipal())
	r.Header.Set("X-Auth-Request-Groups", "/project-gardenio")
	r.Header.Set("X-Auth-Request-Roles", oidcauth.RoleAdmin)
	got := scopeFromRequest(r)
	if !got.Scoped() || !slices.Equal(got.Projects, []string{"claimlayer"}) {
		t.Fatalf("scope = %+v, want scoped to [claimlayer] regardless of headers", got)
	}
}

// Land roles (Lands Commons 4.2): the role in each land comes from the same
// groups, and may() applies the action table to it.
func TestViewerScope_LandRoles(t *testing.T) {
	r := requestAs("GET", "/", principal{email: "u@example.com", roles: []string{oidcauth.RoleProjectMember},
		groups: []string{"/project-own/owners", "/project-mem", "/project-gst/guests"}})
	sc := scopeFromRequest(r)
	for land, want := range map[string]conversation.LandRole{
		"own": conversation.LandRoleOwner, "mem": conversation.LandRoleMember,
		"gst": conversation.LandRoleGuest, "other": conversation.LandRoleNone,
	} {
		if got := sc.landRole(land); got != want {
			t.Errorf("landRole(%s) = %q, want %q", land, got, want)
		}
	}
	for _, c := range []struct {
		land   string
		action conversation.Action
		want   bool
	}{
		{"gst", conversation.ActionSay, true},
		{"gst", conversation.ActionReply, true},
		{"gst", conversation.ActionSpawn, false},
		{"gst", conversation.ActionDecisionRespond, false},
		{"mem", conversation.ActionSpawn, true},
		{"mem", conversation.ActionChannelManage, false},
		{"own", conversation.ActionSpawn, true},
		{"own", conversation.ActionChannelManage, true},
		{"other", conversation.ActionSay, false},
	} {
		if got := sc.may(c.land, c.action); got != c.want {
			t.Errorf("may(%s, %s) = %v, want %v", c.land, c.action, got, c.want)
		}
	}
	admin := viewerScope{Admin: true}
	if admin.landRole("any") != conversation.LandRoleOperator || !admin.may("any", conversation.ActionMemberManage) {
		t.Error("an unrestricted caller must act as operator in every land")
	}

	// Channel membership (Lands Commons 4.8): a user channel takes posts from
	// its members only, whatever the land role; the operator is exempt.
	const self = "oidc:u@example.com"
	design := conversation.Channel{Kind: "user", Members: []string{"oidc:x@example.com"}}
	joined := conversation.Channel{Kind: "user", Members: []string{self}}
	general := conversation.Channel{Kind: "general"}
	for _, c := range []struct {
		land string
		ch   conversation.Channel
		want bool
	}{
		{"own", design, false},
		{"own", joined, true},
		{"gst", general, true},
		{"gst", joined, true},
		{"other", joined, false},
	} {
		if got := sc.mayInChannel(c.land, self, c.ch, conversation.ActionSay); got != c.want {
			t.Errorf("mayInChannel(%s, %+v, say) = %v, want %v", c.land, c.ch, got, c.want)
		}
	}
	if !admin.mayInChannel("any", "", design, conversation.ActionSay) {
		t.Error("the operator must post in any channel")
	}
}

func TestViewerScope_AllowsAny(t *testing.T) {
	sc := viewerScope{Projects: []string{"claimlayer"}}
	if !sc.allowsAny([]string{"role:crew", "project:claimlayer"}) {
		t.Error("bead in own project not admitted")
	}
	if sc.allowsAny([]string{"project:gardenio"}) {
		t.Error("bead in another project admitted")
	}
	if sc.allowsAny([]string{"role:crew"}) {
		t.Error("bead with no project label admitted to a scoped principal")
	}
	if !(viewerScope{Admin: true}).allowsAny(nil) {
		t.Error("unrestricted scope must admit anything")
	}
}

// scopedDaemon emulates the daemon's read cut for the tests below: a request
// whose bearer is registered in scopes sees only beads labelled with one of those
// projects (kbeads does this off the token's groups); any other bearer, or none,
// is unrestricted. The viewer's own scope on the context is deliberately NOT
// consulted, so these tests exercise "reads go out as the user", not "the viewer
// filtered".
type scopedDaemon struct {
	*fakeDaemon
	scopes map[string][]string
}

const (
	scopedBearer = "tok-claimlayer"
	adminBearer  = "tok-admin"
)

func newScopedDaemon(f *fakeDaemon) *scopedDaemon {
	return &scopedDaemon{fakeDaemon: f, scopes: map[string][]string{scopedBearer: {"claimlayer"}}}
}

func (d *scopedDaemon) projectsFor(ctx context.Context) ([]string, bool) {
	p, ok := d.scopes[beadsapi.UserBearerFromContext(ctx)]
	return p, ok
}

func (d *scopedDaemon) visible(ctx context.Context, labels []string) bool {
	projects, scoped := d.projectsFor(ctx)
	if !scoped {
		return true
	}
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok && slices.Contains(projects, p) {
			return true
		}
	}
	return false
}

func (d *scopedDaemon) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	b, err := d.fakeDaemon.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if !d.visible(ctx, b.Labels) {
		return nil, &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized to read beads in this project"}
	}
	return b, nil
}

func (d *scopedDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res, err := d.fakeDaemon.ListBeadsFiltered(ctx, q)
	if err != nil || res == nil {
		return res, err
	}
	out := &beadsapi.ListBeadsResult{}
	for _, b := range res.Beads {
		if b != nil && d.visible(ctx, b.Labels) {
			out.Beads = append(out.Beads, b)
		}
	}
	out.Total = len(out.Beads)
	return out, nil
}

func (d *scopedDaemon) ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error) {
	all, err := d.fakeDaemon.ListProjectBeads(ctx)
	if err != nil {
		return nil, err
	}
	projects, scoped := d.projectsFor(ctx)
	if !scoped {
		return all, nil
	}
	out := map[string]beadsapi.ProjectInfo{}
	for name, info := range all {
		if slices.Contains(projects, name) {
			out[name] = info
		}
	}
	return out, nil
}

func (d *scopedDaemon) ListGlobalEvents(ctx context.Context, req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error) {
	resp, err := d.fakeDaemon.ListGlobalEvents(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	out := *resp
	out.Events = nil
	for _, e := range resp.Events {
		var w struct {
			Bead struct {
				Labels []string `json:"labels"`
			} `json:"bead"`
		}
		_ = json.Unmarshal(e.Payload, &w)
		if d.visible(ctx, w.Bead.Labels) {
			out.Events = append(out.Events, e)
		}
	}
	return &out, nil
}

// twoProjectFake seeds one bead per project, plus the project registry.
func twoProjectFake() *fakeDaemon {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"cl-1": {ID: "cl-1", Title: "Claimlayer thing", Type: "task", Status: "open", Labels: []string{"project:claimlayer"}},
		"gd-1": {ID: "gd-1", Title: "Gardenio secret thing", Type: "task", Status: "open", Labels: []string{"project:gardenio"}},
	}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"claimlayer": {Name: "claimlayer"},
		"gardenio":   {Name: "gardenio"},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["cl-1"], f.beads["gd-1"]},
		Total: 2,
	}
	return f
}

func feedEventFor(t *testing.T, id, project string) beadsapi.SSEEvent {
	t.Helper()
	data, err := json.Marshal(map[string]any{"bead": map[string]any{
		"id": id, "type": "task", "title": id + " title", "status": "open",
		// The payload's own labels are deliberately wrong: the hub must read
		// the project from the daemon, not trust the event.
		"labels": []string{"project:" + "wrong"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return beadsapi.SSEEvent{Event: "beads.bead.created", Data: data}
}

// drain collects what a subscription delivers within a short window.
func drain(ch <-chan beadsapi.SSEEvent) []string {
	var ids []string
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return ids
			}
			var w struct {
				Bead struct {
					ID string `json:"id"`
				} `json:"bead"`
			}
			_ = json.Unmarshal(e.Data, &w)
			ids = append(ids, w.Bead.ID)
		case <-deadline:
			return ids
		}
	}
}

func TestFeedSSE_ScopedSubscriberSeesOnlyOwnProject(t *testing.T) {
	fake := twoProjectFake()
	hub := NewBeadFeedHub(fake, testLogger())

	// Published before anyone subscribes: the warm-paint ring must be cut too.
	hub.broadcast(feedEventFor(t, "gd-1", "gardenio"))
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scoped, stopScoped := hub.SubscribeFor(ctx, viewerScope{Projects: []string{"claimlayer"}})
	defer stopScoped()
	admin, stopAdmin := hub.SubscribeFor(ctx, viewerScope{Admin: true})
	defer stopAdmin()

	// And live, after both subscribed. The unattributable ones fail closed.
	hub.broadcast(feedEventFor(t, "gd-1", "gardenio"))
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	hub.broadcast(feedEventFor(t, "zz-404", "")) // unknown bead: lookup fails
	hub.broadcast(beadsapi.SSEEvent{Event: "beads.bead.created", Data: json.RawMessage(`{"nope":1}`)})

	if got := drain(scoped); !slices.Equal(got, []string{"cl-1", "cl-1"}) {
		t.Errorf("scoped subscriber received %v, want only cl-1 (ring + live)", got)
	}
	got := drain(admin)
	for _, want := range []string{"gd-1", "cl-1"} {
		if !slices.Contains(got, want) {
			t.Errorf("admin subscriber missed %s: got %v", want, got)
		}
	}
}

// The project lookup runs once per bead id, not once per event.
func TestFeedSSE_ScopedProjectLookupIsMemoized(t *testing.T) {
	fake := twoProjectFake()
	hub := NewBeadFeedHub(fake, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop := hub.SubscribeFor(ctx, viewerScope{Projects: []string{"claimlayer"}})
	defer stop()
	for range 5 {
		hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	}
	if got := drain(ch); len(got) != 5 {
		t.Fatalf("delivered %d events, want 5", len(got))
	}
	fake.getBeadMu.Lock()
	calls := fake.getBeadCalls
	fake.getBeadMu.Unlock()
	if calls != 1 {
		t.Errorf("daemon GetBead calls = %d, want 1 (memoized per bead id)", calls)
	}
}

// A failed lookup drops the event and is not memoized: once the daemon answers,
// the same bead's next event is delivered.
func TestFeedSSE_ScopedLookupFailureDropsThenRecovers(t *testing.T) {
	fake := twoProjectFake()
	hub := NewBeadFeedHub(fake, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop := hub.SubscribeFor(ctx, viewerScope{Projects: []string{"claimlayer"}})
	defer stop()

	fake.getBeadMu.Lock()
	fake.getBeadErr = context.DeadlineExceeded
	fake.getBeadMu.Unlock()
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("event delivered despite failed lookup: %v", got)
	}

	fake.getBeadMu.Lock()
	fake.getBeadErr = nil
	fake.getBeadMu.Unlock()
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	if got := drain(ch); !slices.Equal(got, []string{"cl-1"}) {
		t.Fatalf("after recovery got %v, want [cl-1]", got)
	}
}

// End to end through the handler: the ring is shared, the rendered SSE is not.
func TestFeedSSE_HandlerCutsPerPrincipal(t *testing.T) {
	fake := twoProjectFake()
	hub := NewBeadFeedHub(fake, testLogger())
	hub.broadcast(feedEventFor(t, "gd-1", "gardenio"))
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetBeadFeedHub(hub)

	run := func(p principal) string {
		ctx, cancel := context.WithTimeout(requestAs("GET", "/feed/events", p).Context(), 300*time.Millisecond)
		defer cancel()
		ctx = withViewerScope(ctx, scopeFromRequest(requestAs("GET", "/", p)))
		req := httptest.NewRequest("GET", "/feed/events", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.handleFeedEvents(rec, req)
		return rec.Body.String()
	}
	scoped, admin := run(scopedPrincipal()), run(adminPrincipal())
	if !strings.Contains(scoped, "cl-1") || strings.Contains(scoped, "gd-1") {
		t.Errorf("scoped SSE body should carry cl-1 only:\n%s", scoped)
	}
	if !strings.Contains(admin, "cl-1") || !strings.Contains(admin, "gd-1") {
		t.Errorf("admin SSE body should carry both:\n%s", admin)
	}
}

// The telepathy feed has no per-project cut, so it is simply off for a scoped
// principal, even if it somehow reached the handler.
func TestThinkingHub_OffForScopedPrincipal(t *testing.T) {
	hub := &ThinkingHub{}
	ch, stop := hub.SubscribeFor(context.Background(), viewerScope{Projects: []string{"claimlayer"}})
	defer stop()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("scoped thinking subscription delivered an event")
		}
	case <-time.After(time.Second):
		t.Fatal("scoped thinking subscription should be an already-closed channel")
	}
}

// adminCtx / scopedCtx carry both the viewer scope and the matching bearer, as
// the middleware chain leaves them.
func adminCtx() context.Context {
	return withViewerScope(beadsapi.WithUserBearer(context.Background(), adminBearer), viewerScope{Admin: true})
}

func scopedCtx() context.Context {
	return withViewerScope(beadsapi.WithUserBearer(context.Background(), scopedBearer), viewerScope{Projects: []string{"claimlayer"}})
}

// TestHubs_ScopedCachesAreKeyedByScope: every cache and hub that holds data
// fetched on a shared path serves a scoped principal only its own cut. Each case
// fills the cache as an admin, then asks as the scoped principal and requires the
// answer to come from a fresh daemon read (which the daemon narrows), and the
// reverse: a scoped fill must not be served to the admin.
func TestHubs_ScopedCachesAreKeyedByScope(t *testing.T) {
	t.Run("planProjectCache", func(t *testing.T) {
		d := newScopedDaemon(twoProjectFake())
		srv := NewServer(d, testLogger(), "", "", "", &fakeScreenClient{})
		all, err := srv.listProjectBeadsCached(adminCtx())
		if err != nil || len(all) != 2 {
			t.Fatalf("admin projects = %v, %v; want both", all, err)
		}
		mine, err := srv.listProjectBeadsCached(scopedCtx())
		if err != nil {
			t.Fatal(err)
		}
		if _, leaked := mine["gardenio"]; leaked || len(mine) != 1 {
			t.Errorf("scoped principal was served %v from the admin-filled cache", mine)
		}
		// A scoped fill must not poison the admin: expire nothing, ask again.
		again, _ := srv.listProjectBeadsCached(adminCtx())
		if len(again) != 2 {
			t.Errorf("admin was served the scoped cut %v", again)
		}
	})

	t.Run("planEnrichListCache", func(t *testing.T) {
		d := newScopedDaemon(twoProjectFake())
		srv := NewServer(d, testLogger(), "", "", "", &fakeScreenClient{})
		q := beadsapi.ListBeadsQuery{Limit: 400}
		all, err := srv.listBeadsFilteredCached(adminCtx(), planEnrichListKeyAssistants, q)
		if err != nil || len(all.Beads) != 2 {
			t.Fatalf("admin list = %v, %v; want both", all, err)
		}
		mine, err := srv.listBeadsFilteredCached(scopedCtx(), planEnrichListKeyAssistants, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(mine.Beads) != 1 || mine.Beads[0].ID != "cl-1" {
			t.Errorf("scoped principal was served %d beads from the admin-filled cache", len(mine.Beads))
		}
		again, _ := srv.listBeadsFilteredCached(adminCtx(), planEnrichListKeyAssistants, q)
		if len(again.Beads) != 2 {
			t.Errorf("admin was served the scoped cut (%d beads)", len(again.Beads))
		}
	})

	t.Run("feedSeedCache", func(t *testing.T) {
		f := twoProjectFake()
		t0 := time.Now().Add(-time.Hour)
		payload := func(id, project string) json.RawMessage {
			data, _ := json.Marshal(map[string]any{"bead": map[string]any{
				"id": id, "type": "task", "title": id + " seed", "status": "open", "labels": []string{"project:" + project},
			}})
			return data
		}
		f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: payload("gd-1", "gardenio")},
			{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(time.Minute), Payload: payload("cl-1", "claimlayer")},
		}}
		srv := NewServer(newScopedDaemon(f), testLogger(), "", "", "", &fakeScreenClient{})
		render := func(ctx context.Context) string {
			cards, _ := srv.feedSeedCardsAndUsers(ctx, feedBackfillLimit)
			var sb strings.Builder
			for _, c := range cards {
				sb.WriteString(string(c.HTML))
			}
			return sb.String()
		}
		adminHTML := render(adminCtx())
		if !strings.Contains(adminHTML, "gd-1 seed") || !strings.Contains(adminHTML, "cl-1 seed") {
			t.Fatalf("admin seed should carry both beads:\n%s", adminHTML)
		}
		scopedHTML := render(scopedCtx())
		if strings.Contains(scopedHTML, "gd-1 seed") || !strings.Contains(scopedHTML, "cl-1 seed") {
			t.Errorf("scoped seed was served from the admin-filled cache:\n%s", scopedHTML)
		}
		// Second scoped render must not have filled the shared cache either.
		if again := render(adminCtx()); !strings.Contains(again, "gd-1 seed") {
			t.Errorf("admin seed lost gd-1 after a scoped render:\n%s", again)
		}
	})

	t.Run("operatorSlugCache", func(t *testing.T) {
		f := twoProjectFake()
		srv := NewServer(newScopedDaemon(f), testLogger(), "", "", "", &fakeScreenClient{})
		srv.operatorSlugCache = newOperatorSlugCache()
		srv.operatorSlugCache.set(7, "matt")
		if got := srv.resolveOperatorSlug(scopedCtx(), 7); got == "matt" {
			t.Errorf("scoped principal read the admin-filled slug cache: %q", got)
		}
		if got := srv.resolveOperatorSlug(adminCtx(), 7); got != "matt" {
			t.Errorf("admin lost its cached slug: %q", got)
		}
	})

	t.Run("gardenerOwnerCache", func(t *testing.T) {
		f := twoProjectFake()
		srv := NewServer(newScopedDaemon(f), testLogger(), "", "", "", &fakeScreenClient{})
		now := srv.now()
		srv.gardenerOwnerCache.setUserBead("ub-1", gardenerOwnerResolution{display: "Matt Baker", slug: "matt", ok: true}, now)
		srv.gardenerOwnerCache.setSlack("U123", gardenerOwnerResolution{display: "Matt Slack", slug: "matt", ok: true}, now)
		if d, _, ok := srv.ownerFromUserBead(scopedCtx(), "ub-1"); ok && d == "Matt Baker" {
			t.Errorf("scoped principal read the user-bead owner cache: %q", d)
		}
		if d, _, ok := srv.ownerFromSlackUserID(scopedCtx(), "U123"); ok && d == "Matt Slack" {
			t.Errorf("scoped principal read the slack owner cache: %q", d)
		}
		if d, _, ok := srv.ownerFromUserBead(adminCtx(), "ub-1"); !ok || d != "Matt Baker" {
			t.Errorf("admin lost its cached owner: %q %v", d, ok)
		}
	})

	t.Run("dashboardHubCache", func(t *testing.T) {
		f := twoProjectFake()
		srv := NewServer(newScopedDaemon(f), testLogger(), "", "", "", &fakeScreenClient{})
		srv.dashboardHubCache.set(dashboardHubData{TotalDashboards: 3, SectionCount: 1}, srv.now())
		if got := srv.dashboardHub(scopedCtx()); got.TotalDashboards == 3 {
			t.Errorf("scoped principal was served the admin-filled dashboard hub")
		}
		if got := srv.dashboardHub(adminCtx()); got.TotalDashboards != 3 {
			t.Errorf("admin lost its cached dashboard hub: %+v", got)
		}
	})
}

// TestScopedViewer_BeadPageOtherProject403: the viewer does not hide the other
// project's bead itself; the daemon refuses the user's bearer and the viewer
// shows its answer as a plain not-found page, the same as a bead that does not
// exist.
func TestScopedViewer_BeadPageOtherProject403(t *testing.T) {
	d := newScopedDaemon(twoProjectFake())
	srv := NewServer(d, testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })

	get := func(id string, bearer string, p principal) *httptest.ResponseRecorder {
		r := requestAs("GET", "/"+id, p)
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	denied := get("gd-1", scopedBearer, scopedPrincipal())
	missing := get("zz-404", scopedBearer, scopedPrincipal())
	if denied.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("other-project bead = %d, missing bead = %d; both want 404", denied.Code, missing.Code)
	}
	if denied.Body.String() != missing.Body.String() {
		t.Errorf("denied page %q differs from the not-found page %q: the viewer confirms the bead exists",
			denied.Body.String(), missing.Body.String())
	}
	if strings.Contains(denied.Body.String(), "Gardenio secret thing") {
		t.Error("denied page leaked the bead title")
	}

	// The daemon, not the viewer, made that call: the same request as an
	// unrestricted principal reads the bead.
	if rec := get("gd-1", adminBearer, adminPrincipal()); rec.Code != http.StatusOK {
		t.Errorf("admin GET /gd-1 = %d, want 200", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "Gardenio secret thing") {
		t.Error("admin page does not show the bead")
	}
	if rec := get("cl-1", scopedBearer, scopedPrincipal()); rec.Code != http.StatusOK {
		t.Errorf("scoped GET /cl-1 = %d, want 200", rec.Code)
	}
}

// TestScopedViewer_NoOtherProjectInNavigation renders the front page through the
// full scope chain for both audiences: the project switcher, the first-paint
// "recent" cards and the rail carry only the scoped principal's project, while
// the admin page shows both.
func TestScopedViewer_NoOtherProjectInNavigation(t *testing.T) {
	f := twoProjectFake()
	t0 := time.Now().Add(-time.Hour)
	payload := func(id, project string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"bead": map[string]any{
			"id": id, "type": "task", "title": id + " recent", "status": "open", "labels": []string{"project:" + project},
		}})
		return data
	}
	f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{Events: []beadsapi.Event{
		{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: payload("gd-1", "gardenio")},
		{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(time.Minute), Payload: payload("cl-1", "claimlayer")},
	}}
	// Semantic search is off in this fixture, so /search takes its lexical path
	// through the (scoped) list call.
	f.semanticErr = errors.New("semantic search disabled")
	srv := NewServer(newScopedDaemon(f), testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })

	page := func(path, bearer string, p principal) string {
		r := requestAs("GET", path, p)
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as %v = %d: %s", path, p.roles, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Admin first, so every shared cache is warm with the full picture before
	// the scoped principal asks.
	admin := page("/", adminBearer, adminPrincipal())
	for _, want := range []string{"claimlayer", "gardenio", "gd-1 recent", "cl-1 recent"} {
		if !strings.Contains(admin, want) {
			t.Fatalf("admin front page missing %q: the fixture does not reach the surface under test", want)
		}
	}

	for _, path := range []string{"/", "/feed", "/search?q=thing", "/active"} {
		// The search form's example filter and the nav's product wordmark (the
		// Dawn Meadow sign, S1j) are static copy, not data.
		body := strings.ReplaceAll(page(path, scopedBearer, scopedPrincipal()), `placeholder="project:gardenio"`, "")
		body = strings.ReplaceAll(body, `<a class="gd-sign" href="/feed">gardenio</a>`, "")
		for _, leaked := range []string{"gardenio", "gd-1", "Gardenio secret thing"} {
			if strings.Contains(body, leaked) {
				t.Errorf("scoped GET %s leaks %q", path, leaked)
			}
		}
	}
	if body := page("/", scopedBearer, scopedPrincipal()); !strings.Contains(body, "claimlayer") || !strings.Contains(body, "cl-1 recent") {
		t.Error("scoped front page lost its own project's switcher entry or recent card")
	}
}

// The farm document's cut follows the viewer scope: an unrestricted caller (and a context with no scope) gets the
// whole farm, a scoped principal its projects, an applicant an empty set (gd-6UG1OGlqYg).
func TestGardenFarmCut(t *testing.T) {
	req := func(sc *viewerScope) *http.Request {
		r := httptest.NewRequest("GET", "/garden/farm", nil)
		if sc != nil {
			r = r.WithContext(withViewerScope(r.Context(), *sc))
		}
		return r
	}
	if c := gardenFarmCut(req(nil)); c.Scoped {
		t.Errorf("no scope on the context: %+v, want the whole farm", c)
	}
	if c := gardenFarmCut(req(&viewerScope{Admin: true})); c.Scoped {
		t.Errorf("admin: %+v, want the whole farm", c)
	}
	if c := gardenFarmCut(req(&viewerScope{Projects: []string{"claimlayer", "zeta"}})); !c.Scoped || !slices.Equal(c.Projects, []string{"claimlayer", "zeta"}) {
		t.Errorf("member: %+v, want scoped to claimlayer and zeta", c)
	}
	if c := gardenFarmCut(req(&viewerScope{Applicant: true})); !c.Scoped || len(c.Projects) != 0 {
		t.Errorf("applicant: %+v, want scoped to nothing", c)
	}
}
