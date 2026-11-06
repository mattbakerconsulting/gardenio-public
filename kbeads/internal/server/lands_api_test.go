package server

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// The map's closed tables (internal/landmap/tables.go) against the daemon they
// describe, Door D.6 gd-yKOBNCMyNk.

// TestLandsAPI_RouteTableRegistered: every daemon route the map calls is a
// registered route a land member can reach. A route renamed or closed to
// scoped principals here fails the build instead of every tenant call.
func TestLandsAPI_RouteTableRegistered(t *testing.T) {
	patterns := registeredRoutePatterns()
	for _, rt := range landmap.Routes() {
		if !slices.Contains(patterns, rt.String()) {
			t.Errorf("landmap calls %s, which registerRoutes does not register", rt)
			continue
		}
		if c := scopedRouteClasses[rt.String()]; c != classFiltered && c != classScopedSafe {
			t.Errorf("landmap calls %s, which is closed to scoped principals (class %d)", rt, c)
		}
	}
}

// TestLandsAPI_ContractRoutesRegistered: the map's own routes are registered
// under /v1 (where the land door reaches them), one per verb.
func TestLandsAPI_ContractRoutesRegistered(t *testing.T) {
	var lands []string
	for _, p := range registeredRoutePatterns() {
		if _, path, _ := strings.Cut(p, " "); strings.HasPrefix(path, landmap.BaseURL+"/lands/v1/") {
			lands = append(lands, p)
		}
	}
	if len(lands) != len(landmap.Verbs) {
		t.Errorf("%d Lands API routes registered, %d verbs: %v", len(lands), len(landmap.Verbs), lands)
	}
}

// TestLandsAPI_VerbsInActionTable: each write verb's action is one actionTable
// grants a land member and owner on every type it touches, so no verb ships
// that the daemon refuses to every tenant. An owner-only verb's action is the
// owner's and not the member's.
func TestLandsAPI_VerbsInActionTable(t *testing.T) {
	for _, v := range landmap.Verbs {
		if v.Deferred != "" || v.Action == landmap.ActionRead {
			continue
		}
		for _, typ := range v.Types {
			if !tierAllows(tierOwner, typ, v.Action) {
				t.Errorf("verb %s: owner may not %s %s (actionTable)", v.Op, v.Action, typ)
			}
			if member := tierAllows(tierMember, typ, v.Action); member == v.OwnerOnly {
				t.Errorf("verb %s (owner only %v): member may %s %s = %v (actionTable)", v.Op, v.OwnerOnly, v.Action, typ, member)
			}
		}
	}
}

// TestLandsAPI_TypeRowsEveryType: every bead type the daemon knows has a row
// in the map's type table, hidden or visible, so a new type is a decision in
// landmap before a tenant can see it. A tenant-writable type must be visible
// or explicitly hidden; the row says which.
func TestLandsAPI_TypeRowsEveryType(t *testing.T) {
	for _, typ := range allWritableTypeCandidates() {
		if _, ok := landmap.RowFor(typ); !ok {
			t.Errorf("bead type %q has no row in landmap.TypeRows (hidden or visible)", typ)
		}
	}
	for _, r := range landmap.TypeRows {
		if r.Noun == "" {
			continue
		}
		if _, ok := builtinConfigs["type:"+string(r.Type)]; !ok {
			t.Errorf("landmap shows %q, which is not a built-in type", r.Type)
		}
	}
}

// landsDo calls the Lands API as token and returns status, headers and body.
func (f *scopedFixture) landsDo(t *testing.T, token, method, path string, body any) (int, http.Header, string) {
	t.Helper()
	resp := oidcE2ERequest(t, f.ts, method, "/v1/lands/v1/lands"+path, token, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func assertLandsProblem(t *testing.T, what string, code int, body string, wantStatus int, wantCode string) {
	t.Helper()
	if code != wantStatus {
		t.Fatalf("%s: %d %s, want %d", what, code, body, wantStatus)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil || p.Code != wantCode {
		t.Errorf("%s: problem %s, want code %s", what, body, wantCode)
	}
}

// TestLandsAPI_ThroughTheDaemon drives the map end to end: the full HTTP
// chain, a land member's OIDC bearer, the daemon's own scope and actionTable.
func TestLandsAPI_ThroughTheDaemon(t *testing.T) {
	f := newScopedFixture(t)
	// The list path returns stored beads as they are; carry the labels on them.
	for id, l := range map[string]string{beadClaimlayer: "claimlayer", beadGardenio: "gardenio"} {
		f.ms.beads[id].Labels = []string{"project:" + l}
	}

	code, hdr, body := f.landsDo(t, tokScopedWriter, http.MethodGet, "/claimlayer/tickets", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("list body: %v\n%s", err, body)
	}
	if len(page.Items) != 1 || page.Items[0].ID != beadClaimlayer {
		t.Errorf("claimlayer tickets = %s, want only %s", body, beadClaimlayer)
	}
	if hdr.Get("X-Lands-Contract") == "" || hdr.Get("X-Kbeads-Version") != "" || hdr.Get("X-Kbeads-Commit") != "" {
		t.Errorf("headers: %v", hdr)
	}
	for _, leak := range []string{"project:", `"labels"`, `"fields"`, "/v1/"} {
		if strings.Contains(body, leak) {
			t.Errorf("list body carries %q: %s", leak, body)
		}
	}

	// Another land, and another land's ticket through this land, read as absent.
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/gardenio/tickets", nil)
	assertLandsProblem(t, "other land", code, body, http.StatusNotFound, "not_found")
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/claimlayer/tickets/"+beadGardenio, nil)
	assertLandsProblem(t, "other land's ticket", code, body, http.StatusNotFound, "not_found")

	// A member creates a ticket; it lands in the path's land, as the member.
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodPost, "/claimlayer/tickets", map[string]any{"kind": "task", "title": "From gl"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var tk struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &tk); err != nil || tk.ID == "" || tk.Status != "open" {
		t.Fatalf("create body: %s", body)
	}
	stored := f.ms.beads[tk.ID]
	if stored == nil || stored.Type != model.TypeTask || !slices.Contains(f.ms.labels[tk.ID], "project:claimlayer") {
		t.Fatalf("stored bead = %+v labels %v", stored, f.ms.labels[tk.ID])
	}

	// An escalation filed through the map is an operator-run request in the
	// land, which the daemon's escalation gates accept from a member.
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodPost, "/claimlayer/escalations", map[string]any{"title": "Need a domain", "reason": "for the site"})
	if code != http.StatusCreated {
		t.Fatalf("create escalation: %d %s", code, body)
	}
	if !strings.Contains(body, `"status":"open"`) || !strings.Contains(body, `"requested_by":`) {
		t.Errorf("escalation body: %s", body)
	}

	code, _, body = f.landsDo(t, tokScopedOwner, http.MethodGet, "/claimlayer/whoami", nil)
	if code != http.StatusOK || !strings.Contains(body, `"role":"owner"`) || strings.Contains(body, "@") {
		t.Errorf("whoami as owner: %d %s", code, body)
	}

	// A guest may not file tickets in its land: the daemon's actionTable says
	// so, and the map passes the refusal on as a code.
	code, _, body = f.landsDo(t, tokScopedGuest, http.MethodPost, "/claimlayer/tickets", map[string]any{"kind": "task", "title": "nope"})
	assertLandsProblem(t, "guest create", code, body, http.StatusForbidden, "forbidden")
	if strings.Contains(body, "guest") || strings.Contains(body, "may not") {
		t.Errorf("guest refusal carries daemon text: %s", body)
	}
}

// TestLandsAPI_GuardOnTheRealChain: on a Lands API path, a refusal from the
// daemon's own middleware or mux reaches the tenant as the contract's
// problem+json, without the daemon's headers (D.8 gd-enMSApFne1). Other paths
// keep the daemon's answer and its version header.
func TestLandsAPI_GuardOnTheRealChain(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	for _, tc := range []struct {
		method, path, bearer string
		status               int
		code                 string
	}{
		{"GET", "/v1/lands/v1/lands/acme/tickets", "", http.StatusUnauthorized, "unauthenticated"},
		{"GET", "/v1/lands/v1/no-such-route", testStaticToken, http.StatusNotFound, "not_found"},
		{"DELETE", "/v1/lands/v1/lands/acme", testStaticToken, http.StatusNotFound, "not_found"},
	} {
		rec := doBearerJSON(t, h, tc.method, tc.path, tc.bearer, nil, nil)
		what := tc.method + " " + tc.path
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d: %s", what, rec.Code, tc.status, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type %q", what, ct)
		}
		for k := range rec.Header() {
			if !slices.Contains(landmap.ResponseHeaders, k) {
				t.Errorf("%s: header %s reached the tenant", what, k)
			}
		}
		var p struct {
			Code          string  `json:"code"`
			CorrelationID string  `json:"correlation_id"`
			Detail        *string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Code != tc.code || p.CorrelationID == "" || p.Detail != nil {
			t.Errorf("%s: problem %s (%v), want code %s", what, rec.Body, err, tc.code)
		}
	}
	rec := doBearerJSON(t, h, "GET", "/v1/beads", "", nil, nil)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("X-Kbeads-Version") == "" {
		t.Errorf("GET /v1/beads: %d %v, want the daemon's own 401", rec.Code, rec.Header())
	}
}

// TestLandsAPI_CrewAndPlots: the crew is the land's people, each with the land
// role their sign-in recorded, under the same id whoami gives them; the plot is
// the land's repository, by name, never by address.
func TestLandsAPI_CrewAndPlots(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"tenant","git_url":"https://github.com/acme-co/claim-web.git","default_branch":"trunk"}`)
	// A person whose bead predates land_roles, and one in another land only.
	f.seedUser(t, "u-ann", "ann@example.com", []string{"claimlayer"}, model.StatusOpen)
	f.seedUser(t, "u-bob", "bob@example.com", []string{"other"}, model.StatusOpen)
	// Each person's first call provisions their user bead from their groups.
	for _, tok := range []string{tokScopedWriter, tokScopedGuest} {
		if code, _, body := f.landsDo(t, tok, http.MethodGet, "/claimlayer/whoami", nil); code != http.StatusOK {
			t.Fatalf("whoami: %d %s", code, body)
		}
	}

	code, _, body := f.landsDo(t, tokScopedOwner, http.MethodGet, "/claimlayer/crew", nil)
	if code != http.StatusOK {
		t.Fatalf("crew: %d %s", code, body)
	}
	var page struct {
		Items []struct{ ID, Name, Role, Status string } `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("crew body: %v\n%s", err, body)
	}
	roles := map[string]string{}
	ids := map[string]string{}
	for _, m := range page.Items {
		roles[m.Name], ids[m.Name] = m.Role, m.ID
	}
	want := map[string]string{"ann": "other", "clw": "member", "clg": "guest", "clo": "owner"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("crew roles = %v, want %v", roles, want)
	}
	for _, leak := range []string{"@", "oidc:", "principal"} {
		if strings.Contains(body, leak) {
			t.Errorf("crew body carries %q: %s", leak, body)
		}
	}
	_, _, body = f.landsDo(t, tokScopedOwner, http.MethodGet, "/claimlayer/whoami", nil)
	if !strings.Contains(body, `"id":"`+ids["clo"]+`"`) {
		t.Errorf("whoami %s, crew id %s: one person, two ids", body, ids["clo"])
	}
	code, _, body = f.landsDo(t, tokScopedGuest, http.MethodGet, "/claimlayer/crew/"+ids["clw"], nil)
	if code != http.StatusOK || !strings.Contains(body, `"role":"member"`) {
		t.Errorf("getMember: %d %s", code, body)
	}
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/other/crew", nil)
	assertLandsProblem(t, "other land's crew", code, body, http.StatusNotFound, "not_found")

	code, _, body = f.landsDo(t, tokScopedGuest, http.MethodGet, "/claimlayer/plots", nil)
	if code != http.StatusOK || !strings.Contains(body, `"name":"claim-web"`) || !strings.Contains(body, `"default_branch":"trunk"`) {
		t.Fatalf("plots: %d %s", code, body)
	}
	for _, leak := range []string{"github", "acme-co", ".git"} {
		if strings.Contains(body, leak) {
			t.Errorf("plots body carries %q: %s", leak, body)
		}
	}
	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/other/plots", nil)
	assertLandsProblem(t, "other land's plots", code, body, http.StatusNotFound, "not_found")
}

// TestLandsAPI_Budget: the budget is the purse snapshot the watcher stamps on
// the land's account bead, read by the daemon although a member may not read
// account beads; no snapshot is unavailable, never zeros; an operator land
// has no budget, and another land's reads as absent.
func TestLandsAPI_Budget(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	get := func() (int, string) {
		code, _, body := f.landsDo(t, tokScopedWriter, http.MethodGet, "/claimlayer/budget", nil)
		return code, body
	}

	// A tenant land with no account bead yet.
	code, body := get()
	assertLandsProblem(t, "no account", code, body, http.StatusServiceUnavailable, "upstream_unavailable")

	// Labelled with its land as provisioning writes it, so the member below
	// is refused by the tenant type gate, not by a missing project label.
	f.seedTyped("acct-cl", "account", "project:claimlayer")
	acct := f.ms.beads["acct-cl"]
	acct.Title, acct.Kind = "lands-claimlayer", "config"
	stamp := func(fields map[string]string) {
		b, _ := json.Marshal(fields)
		acct.Fields = b
	}
	now := time.Now().UTC()
	fresh, stale := now.Add(-time.Minute).Format(time.RFC3339), now.Add(-20*time.Minute).Format(time.RFC3339)

	// Not stamped yet: no snapshot.
	stamp(map[string]string{"total_cost_usd": "12"})
	code, body = get()
	assertLandsProblem(t, "no snapshot", code, body, http.StatusServiceUnavailable, "upstream_unavailable")

	for _, c := range []struct {
		name, state, used, readAt, want string
	}{
		{"ok", "ok", "30.5", fresh, `{"limit":100,"spent":30.5,"status":"ok","unit":"credits"}`},
		{"wall", "wall", "96", fresh, `{"limit":100,"spent":96,"status":"exhausted","unit":"credits"}`},
		{"unknown", "unknown", "30", fresh, `{"limit":100,"spent":30,"status":"exhausted","unit":"credits"}`},
		{"stale", "ok", "30", stale, `{"limit":100,"spent":30,"status":"exhausted","unit":"credits"}`},
	} {
		stamp(map[string]string{
			"purse_granted_credits": "100", "purse_used_credits": c.used, "purse_state": c.state, "purse_read_at": c.readAt,
		})
		code, body := get()
		if code != http.StatusOK || strings.TrimSpace(body) != c.want {
			t.Errorf("%s: %d %s, want %s", c.name, code, body, c.want)
		}
	}

	// An unknown purse whose spend could not be computed has no used figure.
	stamp(map[string]string{"purse_granted_credits": "100", "purse_used_credits": "", "purse_state": "unknown", "purse_read_at": fresh})
	code, body = get()
	assertLandsProblem(t, "no used figure", code, body, http.StatusServiceUnavailable, "upstream_unavailable")

	// A member may not read the account bead itself.
	if code, _ := f.do(t, tokScopedWriter, http.MethodGet, "/v1/beads/acct-cl", nil); code != http.StatusForbidden {
		t.Errorf("a land member reading the account bead directly: %d, want 403", code)
	}

	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/other/budget", nil)
	assertLandsProblem(t, "other land", code, body, http.StatusNotFound, "not_found")

	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"operator"}`)
	code, body = get()
	assertLandsProblem(t, "operator land", code, body, http.StatusNotFound, "not_found")
}
