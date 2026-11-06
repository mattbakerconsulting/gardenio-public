package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plangardener"
)

// Scoped acceptance for the Commons read model (Lands Commons I step 8,
// gd-LCtVzVbuWM; spec §6.2, §6.3): a scoped principal sees only its land. The
// request meets the production chain: scopeMiddleware and viewerRouteClasses in
// front of the registered routes, the handler main builds (newCommonsHandler),
// and a daemon that cuts reads by bearer the way kbeads does, including
// handing a scoped bearer the beads that carry no project label at all. Each
// refusal is paired with the unrestricted caller on the same request, so a row
// cannot pass because the route is broken.

func newCommonsAcceptViewer(t *testing.T) http.Handler {
	t.Helper()
	d := commonstest.TwoLands(time.Now().Add(-20 * time.Minute))
	d.Scopes[acceptNoGroupBearer] = nil
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	// Presence (step 4.3): a person in each land's #general.
	presence := commons.NewPresence(nil, nil, nil)
	presence.Join("oidc:cl-person@example.com", commons.Spot{Land: "claimlayer", Channel: "cl-chgen"})
	presence.Join("oidc:gd-secret-person@example.com", commons.Spot{Land: "gardenio", Channel: "gd-chgen"})
	srv.SetCommonsHandler(newCommonsHandler(d, nil, presence, "", nil))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
}

func commonsDo(h http.Handler, path, bearer string, p principal, timeout time.Duration) (int, string) {
	r := requestAs("GET", path, p)
	ctx := beadsapi.WithUserBearer(r.Context(), bearer)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r.WithContext(ctx))
	return rec.Code, rec.Body.String()
}

func TestCommonsScopedAcceptance(t *testing.T) {
	h := newCommonsAcceptViewer(t)
	leaks := append([]string{`data-land="gardenio"`, "gd-secret-person"}, commonstest.Leaks...)

	rows := []struct {
		name       string
		path       string
		wantScoped int
		wantAdmin  int
		adminHas   string // what the control must carry, so the row is not vacuous
		scopedHas  string
	}{
		{name: "the channel list goes to the steward", path: "/garden/c", wantScoped: 302, wantAdmin: 302,
			adminHas: "/garden/c/s", scopedHas: "/garden/c/s"},
		{name: "own channel goes to its land's steward", path: "/garden/c/claimlayer/general", wantScoped: 302, wantAdmin: 302,
			adminHas: "/garden/c/s/claimlayer", scopedHas: "/garden/c/s/claimlayer"},
		{name: "own thread", path: "/garden/c/claimlayer/general/t/cl-wish1", wantScoped: 200, wantAdmin: 200,
			adminHas: "on it", scopedHas: "on it"},
		{name: "own message link goes to its thread's node", path: "/garden/c/m/cl-rep1", wantScoped: 302, wantAdmin: 302,
			adminHas: "/garden/c/n/cl-wish1", scopedHas: "/garden/c/n/cl-wish1"},
		{name: "another land's channel is refused at the gate", path: "/garden/c/gardenio/general", wantScoped: 403, wantAdmin: 302,
			adminHas: "/garden/c/s/gardenio"},
		{name: "another land's thread is refused at the gate", path: "/garden/c/gardenio/general/t/gd-wish1", wantScoped: 403, wantAdmin: 200,
			adminHas: "gd-rep1"},
		{name: "another land's channel under its own land is not found", path: "/garden/c/claimlayer/gd-chgen/t/gd-wish1", wantScoped: 404, wantAdmin: 404},
		{name: "another land's message is not found", path: "/garden/c/m/gd-msg1", wantScoped: 404, wantAdmin: 302,
			adminHas: "/garden/c/n/gd-msg1"},
		{name: "own channel's presence", path: "/garden/c/claimlayer/general/presence", wantScoped: 200, wantAdmin: 200,
			adminHas: "cl-person", scopedHas: "cl-person"},
		{name: "another land's presence is refused at the gate", path: "/garden/c/gardenio/general/presence", wantScoped: 403, wantAdmin: 200,
			adminHas: "gd-secret-person"},
		{name: "a label-less message is nobody's", path: "/garden/c/m/nl-msg1", wantScoped: 404, wantAdmin: 404},
		{name: "a dual-land message is nobody's", path: "/garden/c/m/xx-msg1", wantScoped: 404, wantAdmin: 404},
		{name: "own bead is sent to its thread", path: "/garden/c/bead/cl-rep1", wantScoped: 303, wantAdmin: 303,
			adminHas: "/garden/c/claimlayer/general/t/cl-wish1", scopedHas: "/garden/c/claimlayer/general/t/cl-wish1"},
		{name: "another land's bead has no place", path: "/garden/c/bead/gd-rep1", wantScoped: 404, wantAdmin: 303,
			adminHas: "/garden/c/gardenio/general/t/gd-wish1"},
		{name: "unread counts cover only claimlayer", path: "/garden/c/unread", wantScoped: 200, wantAdmin: 200,
			adminHas: `"land":"gardenio"`, scopedHas: `"land":"claimlayer"`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			code, body := commonsDo(h, r.path, adminBearer, adminPrincipal(), 0)
			if code != r.wantAdmin {
				t.Errorf("control: admin GET %s = %d, want %d", r.path, code, r.wantAdmin)
			}
			if r.adminHas != "" && !strings.Contains(body, r.adminHas) {
				t.Errorf("control: admin GET %s lacks %q; the row is vacuous", r.path, r.adminHas)
			}

			code, body = commonsDo(h, r.path, acceptScopedBearer, scopedPrincipal(), 0)
			if code != r.wantScoped {
				t.Fatalf("scoped GET %s = %d, want %d: %s", r.path, code, r.wantScoped, body)
			}
			for _, l := range leaks {
				if strings.Contains(body, l) {
					t.Errorf("scoped GET %s leaks %q", r.path, l)
				}
			}
			if r.scopedHas != "" && !strings.Contains(body, r.scopedHas) {
				t.Errorf("scoped GET %s lost its own land's %q", r.path, r.scopedHas)
			}
		})
	}

	t.Run("read position: own land only", func(t *testing.T) {
		post := func(path, bearer string, p principal) int {
			r := requestAs("POST", path, p)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
			return rec.Code
		}
		if code := post("/garden/c/claimlayer/general/read?m=cl-msg1", acceptScopedBearer, scopedPrincipal()); code != http.StatusNoContent {
			t.Errorf("scoped mark own channel read = %d, want 204", code)
		}
		if code := post("/garden/c/gardenio/general/read?m=gd-msg1", adminBearer, adminPrincipal()); code != http.StatusNoContent {
			t.Errorf("control: admin mark gardenio read = %d, want 204", code)
		}
		if code := post("/garden/c/gardenio/general/read?m=gd-msg1", acceptScopedBearer, scopedPrincipal()); code != http.StatusForbidden {
			t.Errorf("scoped mark another land read = %d, want 403", code)
		}
	})

	t.Run("scoped, no group: no land at all", func(t *testing.T) {
		code, body := commonsDo(h, "/garden/c", acceptNoGroupBearer, acceptNoGroupPrincipal(), 0)
		if code != http.StatusFound {
			t.Errorf("no-group GET /garden/c = %d, want 302 to the steward:\n%s", code, body)
		}
		for _, l := range append(leaks, "claimlayer", "cl-") {
			if strings.Contains(body, l) {
				t.Errorf("no-group GET /garden/c carries %q", l)
			}
		}
		if code, _ := commonsDo(h, "/garden/c/claimlayer/general", acceptNoGroupBearer, acceptNoGroupPrincipal(), 0); code != http.StatusForbidden {
			t.Errorf("no-group GET claimlayer channel = %d, want 403", code)
		}
		if code, _ := commonsDo(h, "/garden/c/m/cl-msg1", acceptNoGroupBearer, acceptNoGroupPrincipal(), 0); code != http.StatusNotFound {
			t.Errorf("no-group GET claimlayer message = %d, want 404", code)
		}
	})

	t.Run("event stream replays only claimlayer", func(t *testing.T) {
		since := time.Now().Add(-time.Hour + time.Minute).UTC().Format(time.RFC3339)
		path := "/garden/c/events?since=" + since
		_, admin := commonsDo(h, path, adminBearer, adminPrincipal(), 300*time.Millisecond)
		if !strings.Contains(admin, `"id":"gd-msg1"`) || !strings.Contains(admin, `"id":"cl-msg1"`) {
			t.Errorf("control: admin stream should replay both lands:\n%s", admin)
		}
		code, scoped := commonsDo(h, path, acceptScopedBearer, scopedPrincipal(), 300*time.Millisecond)
		if code != http.StatusOK || !strings.Contains(scoped, `"id":"cl-msg1"`) || !strings.Contains(scoped, "event: replayed") {
			t.Errorf("scoped stream = %d, should replay claimlayer:\n%s", code, scoped)
		}
		for _, l := range leaks {
			if strings.Contains(scoped, l) {
				t.Errorf("scoped stream leaks %q", l)
			}
		}
	})
}

// TestCommonsPageScoped: a scoped person's browser navigation to a channel or
// thread of their own land goes to the workbench (gd-ZeiSZV7j2Y), whose page
// carries none of the farm's lists; the workbench's own fetch of a thread
// still gets the fragment, and another land's channel is refused at the gate,
// never redirected. A gatehouse guest's "/" lands on the gatehouse's steward,
// and that navigation gets the workbench's page.
func TestCommonsPageScoped(t *testing.T) {
	hub := gardenhub.New(twoLandFarm(), "", testLogger())
	hub.Start()
	defer hub.Close()
	page := &gardenhub.Handler{Hub: hub, Cut: gardenFarmCut,
		ReadOnly: func(r *http.Request) bool { return viewerScopeFromContext(r.Context()).Scoped() }}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.landsGatehouse = "gatehouse"
	srv.SetGardenHandler(page)
	srv.SetCommonsHandler(newCommonsHandler(commonstest.TwoLands(time.Now().Add(-20*time.Minute)), nil, nil, "", page.ServePage))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	navigate := func(path, bearer string, p principal, fragment bool) *httptest.ResponseRecorder {
		t.Helper()
		r := requestAs("GET", path, p)
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
		r.Header.Set("Accept", "text/html,application/xhtml+xml")
		if fragment {
			r.Header.Set("X-Commons", "fragment")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	isPage := func(body string) bool { return strings.HasPrefix(body, "<!doctype html>") }

	for path, want := range map[string]string{"/garden/c/claimlayer/general": "/garden/c/s/claimlayer",
		"/garden/c/claimlayer/general/t/cl-wish1": "/garden/c/n/cl-wish1", "/garden/c": "/garden/c/s"} {
		for who, rec := range map[string]*httptest.ResponseRecorder{"admin": navigate(path, adminBearer, adminPrincipal(), false),
			"scoped": navigate(path, acceptScopedBearer, scopedPrincipal(), false)} {
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
				t.Errorf("%s navigating to %s = %d to %q, want 302 to %s", who, path, rec.Code, rec.Header().Get("Location"), want)
			}
		}
	}
	rec := navigate("/garden/c/n/cl-wish1", acceptScopedBearer, scopedPrincipal(), false)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !isPage(body) || !strings.Contains(body, "<title>Workbench</title>") || strings.Contains(body, "-step") {
		t.Fatalf("scoped navigation to a node = %d, want the workbench's page and no farm lists:\n%s", rec.Code, body)
	}

	t.Run("the pane's fetch of a thread still gets the fragment", func(t *testing.T) {
		rec := navigate("/garden/c/claimlayer/general/t/cl-wish1", acceptScopedBearer, scopedPrincipal(), true)
		if body := rec.Body.String(); rec.Code != http.StatusOK || isPage(body) || !strings.Contains(body, "on it") {
			t.Fatalf("scoped fragment fetch = %d, want the thread fragment:\n%s", rec.Code, body)
		}
	})

	t.Run("another land's channel is refused, not paged", func(t *testing.T) {
		for _, path := range []string{"/garden/c/gardenio/general", "/garden/c/gardenio/general/t/gd-wish1"} {
			rec := navigate(path, acceptScopedBearer, scopedPrincipal(), false)
			if rec.Code != http.StatusForbidden || isPage(rec.Body.String()) {
				t.Errorf("scoped navigation to %s = %d, want refused at the gate without the page", path, rec.Code)
			}
		}
	})

	t.Run("a gatehouse guest signing in lands on its steward", func(t *testing.T) {
		guest := principal{email: "guest@example.com", roles: []string{oidcauth.RoleProjectMember}, groups: []string{"/project-gatehouse/guests"}}
		rec := navigate("/", "", guest, false)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusFound || loc != "/garden/c/s/gatehouse" {
			t.Fatalf("guest GET / = %d %q, want 302 to the gatehouse's steward", rec.Code, loc)
		}
		rec = navigate(loc, "", guest, false)
		if body := rec.Body.String(); rec.Code != http.StatusOK || !isPage(body) || strings.Contains(body, "-step") {
			t.Fatalf("guest navigation to %s = %d, want the workbench's page:\n%s", loc, rec.Code, body)
		}
	})
}

// TestCommonsWritesScoped walks the write routes (Lands Commons I step 9,
// gd-b4IDsCKWbB) through the same chain: a scoped principal posts, replies and
// joins in its own land on its own bearer, and is refused in another land at
// the gate or by the handler, with nothing written.
func TestCommonsWritesScoped(t *testing.T) {
	d := commonstest.TwoLands(time.Now().Add(-20 * time.Minute))
	d.Selves = map[string]string{acceptScopedBearer: "oidc:cl@claimlayer.example"}
	d.Scopes[acceptScopedBearer] = []string{"claimlayer"}
	// The scoped viewer holds a stream on its land's #general, so it may say it
	// is typing there (Lands Commons III step 4.4).
	presence := commons.NewPresence(nil, nil, nil)
	presence.Join("oidc:cl@claimlayer.example", commons.Spot{Land: "claimlayer", Channel: "cl-chgen"})
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetCommonsHandler(newCommonsHandler(d, nil, presence, "", nil))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	do := func(path, bearer string, p principal) (int, string) {
		r := requestAs("POST", path, p)
		r.Body = io.NopCloser(strings.NewReader("text=hello+there"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
		return rec.Code, rec.Body.String()
	}

	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"say in own land", "/garden/c/claimlayer/general/say", http.StatusCreated},
		{"reply in own land", "/garden/c/t/cl-wish1/reply", http.StatusCreated},
		{"say in another land is refused at the gate", "/garden/c/gardenio/general/say", http.StatusForbidden},
		{"reply in another land is not found", "/garden/c/t/gd-wish1/reply", http.StatusNotFound},
		{"say in a user channel it is not in", "/garden/c/claimlayer/build/say", http.StatusForbidden},
		{"join that channel", "/garden/c/claimlayer/build/join", http.StatusNoContent},
		{"then say there", "/garden/c/claimlayer/build/say", http.StatusCreated},
		{"join in another land is refused at the gate", "/garden/c/gardenio/general/join", http.StatusForbidden},
		// The Lands kill switch is the operator's, even in the member's own land.
		{"freeze its own land", "/garden/c/claimlayer/general/freeze", http.StatusForbidden},
		{"thaw its own land", "/garden/c/claimlayer/general/thaw", http.StatusForbidden},
	} {
		before := len(d.Created) + len(d.Updated)
		code, body := do(c.path, acceptScopedBearer, scopedPrincipal())
		if code != c.want {
			t.Errorf("%s: POST %s = %d, want %d: %s", c.name, c.path, code, c.want, body)
		}
		if wrote := len(d.Created)+len(d.Updated) > before; wrote != (c.want < 300) {
			t.Errorf("%s: wrote=%v for status %d", c.name, wrote, code)
		}
	}
	// Typing writes nothing: through in its own land, refused at the gate in
	// another.
	before := len(d.Created) + len(d.Updated)
	if code, body := do("/garden/c/claimlayer/general/typing", acceptScopedBearer, scopedPrincipal()); code != http.StatusNoContent {
		t.Errorf("typing in own land = %d, want 204: %s", code, body)
	}
	if code, body := do("/garden/c/gardenio/general/typing", acceptScopedBearer, scopedPrincipal()); code != http.StatusForbidden {
		t.Errorf("typing in another land = %d, want 403: %s", code, body)
	}
	if len(d.Created)+len(d.Updated) != before {
		t.Error("a typing signal wrote to the daemon")
	}
	for _, req := range d.Created {
		if req.CreatedBy != "oidc:cl@claimlayer.example" || len(req.Labels) == 0 || req.Labels[0] != "project:claimlayer" {
			t.Errorf("written as %q with labels %v", req.CreatedBy, req.Labels)
		}
	}
	// The unrestricted control writes anywhere.
	if code, body := do("/garden/c/gardenio/general/say", adminBearer, adminPrincipal()); code != http.StatusCreated {
		t.Errorf("control: admin say in gardenio = %d %s", code, body)
	}
}

// The node model finds a plan's steward by the labels its gardener loop
// carries (gd-qlk31NBV6o); the two packages keep one string each.
func TestCommonsPlanGardenerLabelsArePlangardeners(t *testing.T) {
	if got, want := commons.GardenerEpicLabel+"gd-x", plangardener.GardenerEpicLabel("gd-x"); got != want {
		t.Fatalf("commons.GardenerEpicLabel gives %q, plangardener.GardenerEpicLabel %q", got, want)
	}
	if commons.PlanGardenerLabel != plangardener.MarkerLabel {
		t.Fatalf("commons.PlanGardenerLabel = %q, plangardener.MarkerLabel = %q", commons.PlanGardenerLabel, plangardener.MarkerLabel)
	}
}

// The Commons tags a reply for the bridge's thread forward with the label the
// forwarder filters on; the two packages keep one string.
func TestCommonsThreadSayLabelIsTheBridges(t *testing.T) {
	if commons.LabelThreadSay != bridge.ThreadSayLabel {
		t.Fatalf("commons.LabelThreadSay = %q, bridge.ThreadSayLabel = %q", commons.LabelThreadSay, bridge.ThreadSayLabel)
	}
}

// TestCommonsCardActionsScoped: a land member's card button goes through to
// the bridge on the member's own bearer; a guest's is refused at the gate,
// and the bridge never hears of it (Commons II step 5).
func TestCommonsCardActionsScoped(t *testing.T) {
	var bearers []string
	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearers = append(bearers, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"status":"handled","ephemeral":["Dismissed."]}`))
	}))
	defer bridgeSrv.Close()
	d := commonstest.TwoLands(time.Now().Add(-20 * time.Minute))
	d.Selves = map[string]string{acceptScopedBearer: "oidc:cl@claimlayer.example"}
	d.Scopes[acceptScopedBearer] = []string{"claimlayer"}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	ch := newCommonsHandler(d, nil, nil, "", nil)
	ch.Inbound = commons.NewBridgeInbound(bridgeSrv.URL, nil)
	srv.SetCommonsHandler(ch)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	do := func(p principal) (int, string) {
		r := requestAs("POST", "/garden/c/action/cl-dcard1", p)
		r.Body = io.NopCloser(strings.NewReader("action=dismiss"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r.WithContext(beadsapi.WithUserBearer(r.Context(), acceptScopedBearer)))
		return rec.Code, rec.Body.String()
	}
	guest := scopedPrincipal()
	guest.groups = []string{"/project-claimlayer/guests"}
	if code, body := do(guest); code != http.StatusForbidden {
		t.Errorf("guest: %d %s", code, body)
	}
	if len(bearers) != 0 {
		t.Fatalf("the bridge heard a guest's click: %v", bearers)
	}
	if code, body := do(scopedPrincipal()); code != http.StatusOK || !strings.Contains(body, `id="m-cl-dcard1"`) {
		t.Errorf("member: %d %s", code, body)
	}
	if len(bearers) != 1 || bearers[0] != "Bearer "+acceptScopedBearer {
		t.Errorf("the bridge got %v, want the member's bearer once", bearers)
	}
}

// TestCommonsCardVerbsAreTheBridges: every button and lightbox the Commons
// sends the bridge needs the same action-table entry on both sides, so the
// viewer's gate and the bridge's agree on who may press it.
func TestCommonsCardVerbsAreTheBridges(t *testing.T) {
	for _, c := range [][3]string{
		{"action", "decision", "resolve"}, {"submit", "decision", "other"}, {"action", "decision", "dismiss"},
		{"submit", "decision", "promote"}, {"action", "permission", "allow"}, {"action", "permission", "deny"},
		{"action", "agent", "kill"}, {"action", "agent", "restart"},
	} {
		viewer := commons.CardVerb(c[0], c[1], c[2])
		got, ok := bridge.CommonsCardAction(conversation.InboundKind(c[0]), c[1], c[2])
		if viewer == "" || !ok || string(got) != viewer {
			t.Errorf("%v: viewer %q, bridge %q (%v)", c, viewer, got, ok)
		}
	}
}
