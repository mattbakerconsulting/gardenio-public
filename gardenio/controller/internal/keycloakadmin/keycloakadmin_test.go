package keycloakadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeKeycloak is an in-memory Keycloak admin API covering the endpoints the
// provisioner client uses. Every non-GET request is recorded in writes.
type fakeKeycloak struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	nextID     int
	groups     map[string]*fakeGroup // id → group
	users      map[string]*fakeUser  // id → user
	realmRoles map[string]string     // name → id
	unmappable []string              // realm roles map-role does not cover
	tokens     int
	writes     []string // "METHOD path"
	// looseAttrSearch makes q=github_id:N also return users whose github_id
	// merely starts with N, like a server that does not honour exact.
	looseAttrSearch bool
	// looseEmailSearch makes email=X also return users whose email merely
	// contains X, as Keycloak's search does without exact.
	looseEmailSearch bool
	// conflictOnCreate makes POST /groups answer 409 after creating the
	// group, as if a concurrent provisioner won the race.
	conflictOnCreate bool
	// expireNextAdminCall answers the next admin call 401 once.
	expireNextAdminCall bool
	// listsHidden answers every list and search 200 [], as Keycloak 26.7
	// does when the client's admin permissions v2 grants use a client
	// policy: the partial evaluator behind list filtering skips client
	// policies. Calls by id are unaffected (gd-7peNzGjJ3C).
	listsHidden bool
}

type fakeGroup struct {
	id, name, path string
	parent         string // parent id, "" for top level
	attrs          map[string][]string
	realmRoles     []string
	clientRoles    map[string][]string
	members        map[string]bool
}

type fakeUser struct {
	id, username, email string
	emailVerified       bool
	attrs               map[string][]string
	links               []string // identity provider aliases
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	fk := &fakeKeycloak{
		t:      t,
		groups: map[string]*fakeGroup{},
		users:  map[string]*fakeUser{},
		realmRoles: map[string]string{
			ProjectMemberRole: "role-pm",
			"gardenio-viewer": "role-viewer",
			"gardenio-admin":  "role-admin",
		},
	}
	fk.srv = httptest.NewTLSServer(http.HandlerFunc(fk.serve))
	t.Cleanup(fk.srv.Close)
	return fk
}

// id returns a fresh UUID-shaped id; zero padding keeps sort order numeric.
func (fk *fakeKeycloak) id() string {
	fk.nextID++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", fk.nextID)
}

func (fk *fakeKeycloak) addGroup(name, parent string, realmRoles ...string) *fakeGroup {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	path := "/" + name
	if parent != "" {
		path = fk.groups[parent].path + "/" + name
	}
	g := &fakeGroup{id: fk.id(), name: name, path: path, parent: parent, attrs: map[string][]string{}, realmRoles: realmRoles, clientRoles: map[string][]string{}, members: map[string]bool{}}
	fk.groups[g.id] = g
	return g
}

// addLandGroup adds a top-level project-<land> group carrying the land marker,
// as EnsureProjectGroup would create it.
func (fk *fakeKeycloak) addLandGroup(land string, realmRoles ...string) *fakeGroup {
	g := fk.addGroup(GroupPrefix+land, "", realmRoles...)
	g.attrs[LandAttribute] = []string{land}
	return g
}

func (fk *fakeKeycloak) addUser(username string, githubID string) *fakeUser {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	u := &fakeUser{id: fk.id(), username: username, email: username + "@example.com", attrs: map[string][]string{}}
	if githubID != "" {
		u.attrs[GitHubIDAttribute] = []string{githubID}
	}
	fk.users[u.id] = u
	return u
}

func (fk *fakeKeycloak) groupByName(name string) *fakeGroup {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	for _, g := range fk.groups {
		if g.parent == "" && g.name == name {
			return g
		}
	}
	return nil
}

func (fk *fakeKeycloak) writeLog() []string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return append([]string(nil), fk.writes...)
}

func (fk *fakeKeycloak) client(t *testing.T, logger *slog.Logger) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: fk.srv.URL + "/", Realm: "gardenio", ClientID: "gardenio-lands-provisioner", ClientSecret: "s3cret", HTTPClient: fk.srv.Client()}, logger)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (fk *fakeKeycloak) serve(w http.ResponseWriter, r *http.Request) {
	fk.mu.Lock()
	defer fk.mu.Unlock()

	if r.URL.Path == "/realms/gardenio/protocol/openid-connect/token" {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" ||
			r.Form.Get("client_id") != "gardenio-lands-provisioner" || r.Form.Get("client_secret") != "s3cret" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized_client"})
			return
		}
		fk.tokens++
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "tok-" + strconv.Itoa(fk.tokens), "expires_in": 300})
		return
	}

	const admin = "/admin/realms/gardenio"
	if !strings.HasPrefix(r.URL.Path, admin+"/") {
		http.NotFound(w, r)
		return
	}
	if fk.expireNextAdminCall {
		fk.expireNextAdminCall = false
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if want := "Bearer tok-" + strconv.Itoa(fk.tokens); r.Header.Get("Authorization") != want {
		fk.t.Errorf("%s %s: Authorization %q, want %q", r.Method, r.URL.Path, r.Header.Get("Authorization"), want)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, admin)
	if r.Method != http.MethodGet {
		fk.writes = append(fk.writes, r.Method+" "+path)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	q := r.URL.Query()

	if fk.listsHidden && r.Method == http.MethodGet && (path == "/groups" || path == "/users" ||
		(len(parts) == 3 && parts[0] == "groups" && (parts[2] == "children" || parts[2] == "members"))) {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/groups":
		fk.searchGroups(w, q.Get("search"), q.Get("exact") == "true")
	case r.Method == http.MethodPost && path == "/groups":
		var g groupRepresentation
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil || g.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, ex := range fk.groups {
			if ex.parent == "" && ex.name == g.Name {
				writeJSON(w, http.StatusConflict, map[string]string{"errorMessage": "Top level group named '" + g.Name + "' already exists."})
				return
			}
		}
		attrs := g.Attributes
		if attrs == nil {
			attrs = map[string][]string{}
		}
		ng := &fakeGroup{id: fk.id(), name: g.Name, path: "/" + g.Name, attrs: attrs, clientRoles: map[string][]string{}, members: map[string]bool{}}
		fk.groups[ng.id] = ng
		if fk.conflictOnCreate {
			writeJSON(w, http.StatusConflict, map[string]string{"errorMessage": "exists"})
			return
		}
		w.Header().Set("Location", fk.srv.URL+admin+"/groups/"+ng.id)
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 3 && parts[0] == "groups" && parts[2] == "children" && r.Method == http.MethodGet:
		parent := fk.groups[parts[1]]
		if parent == nil {
			http.NotFound(w, r)
			return
		}
		out := []map[string]any{}
		for _, g := range fk.sortedGroups() {
			if g.parent == parent.id && (q.Get("search") == "" || g.name == q.Get("search") || (q.Get("exact") != "true" && strings.Contains(g.name, q.Get("search")))) {
				out = append(out, map[string]any{"id": g.id, "name": g.name, "path": g.path})
			}
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "groups" && parts[2] == "children" && r.Method == http.MethodPost:
		parent := fk.groups[parts[1]]
		if parent == nil {
			http.NotFound(w, r)
			return
		}
		var g groupRepresentation
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil || g.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, ex := range fk.groups {
			if ex.parent == parent.id && ex.name == g.Name {
				writeJSON(w, http.StatusConflict, map[string]string{"errorMessage": "Sibling group named '" + g.Name + "' already exists."})
				return
			}
		}
		attrs := g.Attributes
		if attrs == nil {
			attrs = map[string][]string{}
		}
		ng := &fakeGroup{id: fk.id(), name: g.Name, path: parent.path + "/" + g.Name, parent: parent.id, attrs: attrs, clientRoles: map[string][]string{}, members: map[string]bool{}}
		fk.groups[ng.id] = ng
		w.Header().Set("Location", fk.srv.URL+admin+"/groups/"+ng.id)
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "groups" && r.Method == http.MethodGet:
		u := fk.users[parts[1]]
		if u == nil {
			http.NotFound(w, r)
			return
		}
		out := []map[string]any{}
		for _, g := range fk.sortedGroups() {
			if g.members[u.id] {
				out = append(out, map[string]any{"id": g.id, "name": g.name, "path": g.path})
			}
		}
		first, _ := strconv.Atoi(q.Get("first"))
		if first >= len(out) {
			out = []map[string]any{}
		} else {
			out = out[first:]
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 2 && parts[0] == "groups" && r.Method == http.MethodGet:
		g := fk.groups[parts[1]]
		if g == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": g.id, "name": g.name, "path": g.path, "attributes": g.attrs})
	case len(parts) == 3 && parts[0] == "groups" && parts[2] == "role-mappings" && r.Method == http.MethodGet:
		g := fk.groups[parts[1]]
		if g == nil {
			http.NotFound(w, r)
			return
		}
		m := map[string]any{}
		var realm []roleRepresentation
		for _, name := range g.realmRoles {
			realm = append(realm, roleRepresentation{ID: fk.realmRoles[name], Name: name})
		}
		if len(realm) > 0 {
			m["realmMappings"] = realm
		}
		if len(g.clientRoles) > 0 {
			cm := map[string]any{}
			for client, roles := range g.clientRoles {
				var rr []roleRepresentation
				for _, name := range roles {
					rr = append(rr, roleRepresentation{ID: "c-" + name, Name: name})
				}
				cm[client] = map[string]any{"client": client, "mappings": rr}
			}
			m["clientMappings"] = cm
		}
		writeJSON(w, http.StatusOK, m)
	case len(parts) == 4 && parts[0] == "groups" && parts[2] == "role-mappings" && parts[3] == "realm" && r.Method == http.MethodPost:
		g := fk.groups[parts[1]]
		if g == nil {
			http.NotFound(w, r)
			return
		}
		var roles []roleRepresentation
		if err := json.NewDecoder(r.Body).Decode(&roles); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, role := range roles {
			if fk.realmRoles[role.Name] != role.ID {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			g.realmRoles = append(g.realmRoles, role.Name)
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "groups" && parts[2] == "members" && r.Method == http.MethodGet:
		g := fk.groups[parts[1]]
		if g == nil {
			http.NotFound(w, r)
			return
		}
		var ids []string
		for id := range g.members {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		first, _ := strconv.Atoi(q.Get("first"))
		maxN, _ := strconv.Atoi(q.Get("max"))
		out := []map[string]any{}
		for i := first; i < len(ids) && i < first+maxN; i++ {
			out = append(out, fk.userJSON(fk.users[ids[i]]))
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 2 && parts[0] == "roles" && r.Method == http.MethodGet:
		// Live: the provisioner holds no view-realm, so a role read by name
		// is refused (gd-AQ9iCiKxML attempt 4).
		w.WriteHeader(http.StatusForbidden)
	case len(parts) == 5 && parts[0] == "groups" && parts[2] == "role-mappings" && parts[3] == "realm" && parts[4] == "available" && r.Method == http.MethodGet:
		g := fk.groups[parts[1]]
		if g == nil {
			http.NotFound(w, r)
			return
		}
		out := []roleRepresentation{}
		for name, id := range fk.realmRoles {
			if !slices.Contains(g.realmRoles, name) && !slices.Contains(fk.unmappable, name) {
				out = append(out, roleRepresentation{ID: id, Name: name})
			}
		}
		slices.SortFunc(out, func(a, b roleRepresentation) int { return strings.Compare(a.Name, b.Name) })
		writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == "/users" && q.Has("email"):
		out := []map[string]any{}
		for _, u := range fk.sortedUsers() {
			if u.email == q.Get("email") || (fk.looseEmailSearch && strings.Contains(u.email, q.Get("email"))) {
				out = append(out, fk.userJSON(u))
			}
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "federated-identity" && r.Method == http.MethodGet:
		u := fk.users[parts[1]]
		if u == nil {
			http.NotFound(w, r)
			return
		}
		out := []map[string]any{}
		for _, alias := range u.links {
			out = append(out, map[string]any{"identityProvider": alias, "userId": "1", "userName": u.username})
		}
		writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == "/users":
		key, val, _ := strings.Cut(q.Get("q"), ":")
		out := []map[string]any{}
		for _, u := range fk.sortedUsers() {
			for _, v := range u.attrs[key] {
				if v == val || (fk.looseAttrSearch && strings.HasPrefix(v, val)) {
					out = append(out, fk.userJSON(u))
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 4 && parts[0] == "users" && parts[2] == "groups" && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
		u, g := fk.users[parts[1]], fk.groups[parts[3]]
		if u == nil || g == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			g.members[u.id] = true
		} else {
			delete(g.members, u.id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fk.t.Errorf("fake keycloak: unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// searchGroups mimics Keycloak: it returns top-level groups whose own name or
// any subgroup's name matches, with the matching subgroups nested.
func (fk *fakeKeycloak) searchGroups(w http.ResponseWriter, search string, exact bool) {
	match := func(name string) bool {
		if exact {
			return name == search
		}
		return strings.Contains(name, search)
	}
	out := []map[string]any{}
	for _, g := range fk.sortedGroups() {
		if g.parent != "" {
			continue
		}
		var subs []map[string]any
		for _, s := range fk.sortedGroups() {
			if s.parent == g.id && match(s.name) {
				subs = append(subs, map[string]any{"id": s.id, "name": s.name, "path": s.path})
			}
		}
		if match(g.name) || len(subs) > 0 {
			out = append(out, map[string]any{"id": g.id, "name": g.name, "path": g.path, "subGroups": subs})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (fk *fakeKeycloak) userJSON(u *fakeUser) map[string]any {
	return map[string]any{"id": u.id, "username": u.username, "email": u.email, "emailVerified": u.emailVerified, "attributes": u.attrs}
}

func (fk *fakeKeycloak) sortedGroups() []*fakeGroup {
	var ids []string
	for id := range fk.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*fakeGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, fk.groups[id])
	}
	return out
}

func (fk *fakeKeycloak) sortedUsers() []*fakeUser {
	var ids []string
	for id := range fk.users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*fakeUser, 0, len(ids))
	for _, id := range ids {
		out = append(out, fk.users[id])
	}
	return out
}

// assertWritesScoped fails if any write touched a group other than a
// project- group or lands-tenants, or mapped a role other than
// gardenio-project-member. It is the B-D6 boundary checked on the wire.
// isLandOrTenantsGroup reports whether g is a top-level project- group, its
// owners or guests subgroup, or lands-tenants. Callers hold fk.mu.
func (fk *fakeKeycloak) isLandOrTenantsGroup(g *fakeGroup) bool {
	if g.parent == "" {
		return strings.HasPrefix(g.name, GroupPrefix) || g.name == TenantsGroup || g.name == InvitedGroup
	}
	p := fk.groups[g.parent]
	return p != nil && p.parent == "" && strings.HasPrefix(p.name, GroupPrefix) && (g.name == "owners" || g.name == "guests")
}

func (fk *fakeKeycloak) assertWritesScoped(t *testing.T) {
	t.Helper()
	fk.mu.Lock()
	defer fk.mu.Unlock()
	for _, w := range fk.writes {
		_, path, _ := strings.Cut(w, " ")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case path == "/groups":
		case len(parts) == 3 && parts[0] == "groups" && parts[2] == "children":
			g := fk.groups[parts[1]]
			if g == nil || g.parent != "" || !strings.HasPrefix(g.name, GroupPrefix) {
				t.Errorf("subgroup created outside a project- group: %s", w)
			}
		case len(parts) == 4 && parts[0] == "users" && parts[2] == "groups":
			g := fk.groups[parts[3]]
			if g == nil || !fk.isLandOrTenantsGroup(g) {
				t.Errorf("membership write outside project-*[/owners|/guests]/lands-tenants: %s", w)
			}
		case len(parts) == 4 && parts[0] == "groups" && parts[2] == "role-mappings":
			g := fk.groups[parts[1]]
			if g == nil || !strings.HasPrefix(g.name, GroupPrefix) {
				t.Errorf("role mapping on a non-project group: %s", w)
			}
		default:
			t.Errorf("write outside the provisioner's scope: %s", w)
		}
	}
	for _, g := range fk.groups {
		if !strings.HasPrefix(g.name, GroupPrefix) {
			continue
		}
		for _, r := range g.realmRoles {
			if r != ProjectMemberRole {
				t.Errorf("group %s holds %s", g.name, r)
			}
		}
	}
}

func TestGroupName(t *testing.T) {
	for _, land := range []string{"acme", "a", "my-land-2", strings.Repeat("a", 58)} {
		if got, err := GroupName(land); err != nil || got != "project-"+land {
			t.Errorf("GroupName(%q) = %q, %v", land, got, err)
		}
	}
	for _, land := range []string{"", "Claimlayer", "-x", "x-", "a/b", "../gardenio-admins", "a b", "a_b", "/lands-tenants", strings.Repeat("a", 59), "x?y"} {
		if _, err := GroupName(land); !errors.Is(err, ErrInvalidLand) {
			t.Errorf("GroupName(%q) err = %v, want ErrInvalidLand", land, err)
		}
	}
}

func TestNew_RequiresConfig(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://x", Realm: "r", ClientID: "c"}, nil); err == nil {
		t.Fatal("New with no secret: want error")
	}
}

func TestNew_RequiresHTTPS(t *testing.T) {
	for _, base := range []string{"http://auth.example.com", "auth.example.com", "https://", "ftp://auth.example.com", "://x"} {
		if _, err := New(Config{BaseURL: base, Realm: "gardenio", ClientID: "c", ClientSecret: "s"}, nil); err == nil {
			t.Errorf("New(BaseURL %q): want error", base)
		}
	}
	if _, err := New(Config{BaseURL: "https://auth.example.com/auth", Realm: "gardenio", ClientID: "c", ClientSecret: "s"}, nil); err != nil {
		t.Errorf("https base URL: %v", err)
	}
}

// Plain http is refused for every host, the in-cluster Keycloak Service the
// http exception once allowed (decision gd-F060vJkxoM) included: Keycloak now
// serves https in-cluster (gd-XRsosVFAau).
func TestNew_HTTPSOnlyInCluster(t *testing.T) {
	for base, ok := range map[string]bool{
		"https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth": true,
		"https://gardenio-keycloak-runtime.gardenio.svc:8443/auth":               true,

		"http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/auth":  false,
		"http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080":       false,
		"http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth":  false,
		"https://u:p@gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443":  false,
		"https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/?x=1": false,
		"https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/#f":   false,
		"http://10.43.0.10:8080/auth":                                            false,
	} {
		_, err := New(Config{BaseURL: base, Realm: "gardenio", ClientID: "c", ClientSecret: "s"}, nil)
		if ok && err != nil {
			t.Errorf("New(BaseURL %q): %v, want accepted", base, err)
		}
		if !ok && err == nil {
			t.Errorf("New(BaseURL %q): accepted, want error", base)
		}
	}
}

func TestNew_RefusesMasterRealm(t *testing.T) {
	for _, realm := range []string{"master", "Master", "MASTER"} {
		if _, err := New(Config{BaseURL: "https://auth.example.com", Realm: realm, ClientID: "c", ClientSecret: "s"}, nil); err == nil {
			t.Errorf("New(Realm %q): want error", realm)
		}
	}
}

func TestEnsureProjectGroup_CreatesWithOnlyMemberRole_Idempotent(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()

	id, err := c.EnsureProjectGroup(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	g := fk.groupByName("project-acme")
	if g == nil || g.id != id {
		t.Fatalf("group not created or wrong id: %+v, id %q", g, id)
	}
	if len(g.realmRoles) != 1 || g.realmRoles[0] != ProjectMemberRole {
		t.Fatalf("realm roles = %v, want [%s]", g.realmRoles, ProjectMemberRole)
	}
	first := fk.writeLog()

	id2, err := c.EnsureProjectGroup(ctx, "acme")
	if err != nil || id2 != id {
		t.Fatalf("second Ensure = %q, %v; want %q", id2, err, id)
	}
	if again := fk.writeLog(); len(again) != len(first) {
		t.Fatalf("second Ensure wrote %v", again[len(first):])
	}
	fk.assertWritesScoped(t)
}

func TestEnsureProjectGroup_CreatesWithLandMarker(t *testing.T) {
	fk := newFakeKeycloak(t)
	if _, err := fk.client(t, nil).EnsureProjectGroup(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	g := fk.groupByName("project-acme")
	if v := g.attrs[LandAttribute]; len(v) != 1 || v[0] != "acme" {
		t.Fatalf("%s = %v, want [acme]", LandAttribute, v)
	}
}

// A group this client created whose role mapping never landed (a crash
// between the two calls) gets the role on the next run.
func TestEnsureProjectGroup_OwnGroupGetsMissingRole(t *testing.T) {
	fk := newFakeKeycloak(t)
	g := fk.addLandGroup("acme")
	c := fk.client(t, nil)
	id, err := c.EnsureProjectGroup(context.Background(), "acme")
	if err != nil || id != g.id {
		t.Fatalf("Ensure = %q, %v", id, err)
	}
	if len(g.realmRoles) != 1 || g.realmRoles[0] != ProjectMemberRole {
		t.Fatalf("realm roles = %v", g.realmRoles)
	}
	for _, w := range fk.writeLog() {
		if w == "POST /groups" {
			t.Fatal("re-created an existing group")
		}
	}
	fk.assertWritesScoped(t)
}

// Without map-role on the member role, Keycloak leaves it out of the
// group's available list: Ensure fails naming the permission and maps
// nothing, rather than guessing a role id.
func TestEnsureProjectGroup_MemberRoleNotMappable(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.unmappable = []string{ProjectMemberRole}
	_, err := fk.client(t, nil).EnsureProjectGroup(context.Background(), "acme")
	if err == nil || !strings.Contains(err.Error(), "map-role") {
		t.Fatalf("Ensure err = %v, want the missing map-role permission named", err)
	}
	if g := fk.groupByName("project-acme"); g != nil && len(g.realmRoles) != 0 {
		t.Fatalf("realm roles = %v, want none", g.realmRoles)
	}
}

func TestEnsureProjectGroup_RefusesForeignRoles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(g *fakeGroup)
	}{
		{"global realm role", func(g *fakeGroup) { g.realmRoles = []string{ProjectMemberRole, "gardenio-viewer"} }},
		{"only a global role", func(g *fakeGroup) { g.realmRoles = []string{"gardenio-admin"} }},
		{"client role", func(g *fakeGroup) { g.clientRoles["realm-management"] = []string{"manage-users"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := newFakeKeycloak(t)
			g := fk.addLandGroup("acme")
			tc.setup(g)
			_, err := fk.client(t, nil).EnsureProjectGroup(context.Background(), "acme")
			if !errors.Is(err, ErrForeignRoles) {
				t.Fatalf("err = %v, want ErrForeignRoles", err)
			}
			if w := fk.writeLog(); len(w) != 0 {
				t.Fatalf("wrote %v on a refused group", w)
			}
		})
	}
}

func TestEnsureProjectGroup_ConcurrentCreateConflict(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.conflictOnCreate = true
	id, err := fk.client(t, nil).EnsureProjectGroup(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if g := fk.groupByName("project-acme"); g == nil || g.id != id || len(g.realmRoles) != 1 {
		t.Fatalf("after 409: group %+v, id %q", g, id)
	}
}

// A realm whose list calls hide the provisioner's groups (admin permissions
// v2 granted through a client policy, gd-7peNzGjJ3C) must fail on the first
// create with ErrGroupHidden, never with ErrGroupNotFound, which close reads
// as "gone". Once lists are visible (a user policy), the same land provisions
// and re-provisions without a write.
func TestEnsureProjectGroup_ListsHidden(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.listsHidden = true
	c := fk.client(t, nil)
	ctx := context.Background()
	for _, run := range []string{"create", "re-run (409)"} {
		_, err := c.EnsureProjectGroup(ctx, "acme")
		if !errors.Is(err, ErrGroupHidden) || errors.Is(err, ErrGroupNotFound) {
			t.Fatalf("%s with hidden lists: %v, want ErrGroupHidden only", run, err)
		}
	}
	g := fk.groupByName("project-acme")
	if g == nil || len(g.realmRoles) != 0 {
		t.Fatalf("group after the hidden runs = %+v; want it created, unmapped", g)
	}

	fk.listsHidden = false
	id, err := c.EnsureProjectGroup(ctx, "acme")
	if err != nil || id != g.id || len(g.realmRoles) != 1 {
		t.Fatalf("EnsureProjectGroup once lists are visible = %q, %v (roles %v)", id, err, g.realmRoles)
	}
	ownersID, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	before := fk.writeLog()
	if again, err := c.EnsureProjectGroup(ctx, "acme"); err != nil || again != id {
		t.Fatalf("second EnsureProjectGroup = %q, %v", again, err)
	}
	if again, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner); err != nil || again != ownersID {
		t.Fatalf("second EnsureLandRoleGroup = %q, %v", again, err)
	}
	if after := fk.writeLog(); len(after) != len(before) {
		t.Fatalf("the second runs wrote %v", after[len(before):])
	}
	fk.assertWritesScoped(t)
}

func TestEnsureProjectGroup_IgnoresSubgroupOfSameName(t *testing.T) {
	fk := newFakeKeycloak(t)
	parent := fk.addGroup("gardenio-admins", "")
	sub := fk.addGroup("project-acme", parent.id)
	c := fk.client(t, nil)
	id, err := c.EnsureProjectGroup(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if id == sub.id || id == parent.id {
		t.Fatalf("Ensure returned the nested decoy %q", id)
	}
	if len(sub.realmRoles) != 0 || len(parent.realmRoles) != 0 {
		t.Fatal("mapped a role on the decoy")
	}
	fk.assertWritesScoped(t)
}

func TestInvalidLand_NoRequests(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	u := fk.addUser("alice", "1")
	bad := "../gardenio-admins"
	if _, err := c.EnsureProjectGroup(ctx, bad); !errors.Is(err, ErrInvalidLand) {
		t.Errorf("Ensure: %v", err)
	}
	if err := c.AddProjectMember(ctx, bad, u.id); !errors.Is(err, ErrInvalidLand) {
		t.Errorf("Add: %v", err)
	}
	if err := c.RemoveProjectMember(ctx, bad, u.id); !errors.Is(err, ErrInvalidLand) {
		t.Errorf("Remove: %v", err)
	}
	if _, err := c.ProjectMembers(ctx, bad); !errors.Is(err, ErrInvalidLand) {
		t.Errorf("Members: %v", err)
	}
	if fk.tokens != 0 {
		t.Fatalf("an invalid land reached Keycloak (%d token calls)", fk.tokens)
	}
}

func TestMembership_AddRemoveIdempotentAndAudited(t *testing.T) {
	fk := newFakeKeycloak(t)
	var logs bytes.Buffer
	c := fk.client(t, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()
	tenants := fk.addGroup(TenantsGroup, "")
	alice := fk.addUser("alice", "101")
	if _, err := c.EnsureProjectGroup(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	g := fk.groupByName("project-acme")

	for range 2 {
		if err := c.AddProjectMember(ctx, "acme", alice.id); err != nil {
			t.Fatal(err)
		}
		if err := c.AddTenant(ctx, alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if !g.members[alice.id] || !tenants.members[alice.id] {
		t.Fatalf("alice not a member: project %v tenants %v", g.members, tenants.members)
	}
	members, err := c.ProjectMembers(ctx, "acme")
	if err != nil || len(members) != 1 || members[0].ID != alice.id || members[0].GitHubID != 101 {
		t.Fatalf("ProjectMembers = %+v, %v", members, err)
	}

	for range 2 {
		if err := c.RemoveProjectMember(ctx, "acme", alice.id); err != nil {
			t.Fatal(err)
		}
		if err := c.RemoveTenant(ctx, alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if g.members[alice.id] || tenants.members[alice.id] {
		t.Fatal("alice still a member after removal")
	}

	out := logs.String()
	for _, want := range []string{
		`action=add group=project-acme`,
		`action=add group=lands-tenants`,
		`action=remove group=project-acme`,
		`action=remove group=lands-tenants`,
		"user_id=" + alice.id,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log missing %q:\n%s", want, out)
		}
	}
	fk.assertWritesScoped(t)
}

func TestMembership_NeverCreatesGroups(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	alice := fk.addUser("alice", "101")
	if err := c.AddTenant(ctx, alice.id); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("AddTenant without lands-tenants: %v", err)
	}
	if err := c.AddProjectMember(ctx, "acme", alice.id); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("AddProjectMember without the group: %v", err)
	}
	if _, err := c.ProjectMembers(ctx, "acme"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("ProjectMembers without the group: %v", err)
	}
	if w := fk.writeLog(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
}

func TestMembership_UnknownUser(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.addGroup(TenantsGroup, "")
	c := fk.client(t, nil)
	ctx := context.Background()
	if err := c.AddTenant(ctx, "00000000-0000-4000-8000-999999999999"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Add unknown user: %v", err)
	}
	if err := c.RemoveTenant(ctx, "00000000-0000-4000-8000-999999999999"); err != nil {
		t.Errorf("Remove unknown user should be a no-op: %v", err)
	}
}

// Finding MEDIUM (#704): a user id is a path segment, so only a Keycloak UUID
// may reach the wire. ".." used to pass the "/?#" check.
func TestMembership_RejectsNonUUIDUserID(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.addGroup(TenantsGroup, "")
	fk.addLandGroup("acme", ProjectMemberRole)
	c := fk.client(t, nil)
	ctx := context.Background()
	for _, bad := range []string{
		"", "..", ".", "x/../../roles", "a?b", "no-such-user", "%2e%2e",
		"00000000-0000-4000-8000-00000000000A",  // upper case
		"00000000-0000-4000-8000-0000000000001", // too long
		" 00000000-0000-4000-8000-000000000001",
	} {
		for name, call := range map[string]func() error{
			"AddTenant":           func() error { return c.AddTenant(ctx, bad) },
			"RemoveTenant":        func() error { return c.RemoveTenant(ctx, bad) },
			"AddProjectMember":    func() error { return c.AddProjectMember(ctx, "acme", bad) },
			"RemoveProjectMember": func() error { return c.RemoveProjectMember(ctx, "acme", bad) },
		} {
			if err := call(); !errors.Is(err, ErrInvalidUserID) {
				t.Errorf("%s(%q) = %v, want ErrInvalidUserID", name, bad, err)
			}
		}
	}
	if fk.tokens != 0 {
		t.Fatalf("an invalid user id reached Keycloak (%d token calls)", fk.tokens)
	}
}

// Finding HIGH (#704): a project- group the client did not create (no land
// marker, or another land's) is never adopted, joined, listed or left, even
// when it holds only gardenio-project-member like the chart's project groups.
func TestUnmarkedProjectGroup_Refused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string][]string
	}{
		{"no marker", nil},
		{"other land", map[string][]string{LandAttribute: {"other"}}},
		{"two values", map[string][]string{LandAttribute: {"acme", "acme"}}},
		{"empty value", map[string][]string{LandAttribute: {""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := newFakeKeycloak(t)
			g := fk.addGroup("project-acme", "", ProjectMemberRole)
			for k, v := range tc.attrs {
				g.attrs[k] = v
			}
			member := fk.addUser("member", "1")
			g.members[member.id] = true
			newcomer := fk.addUser("newcomer", "2")
			c := fk.client(t, nil)
			ctx := context.Background()

			if _, err := c.EnsureProjectGroup(ctx, "acme"); !errors.Is(err, ErrNotLandGroup) {
				t.Errorf("Ensure: %v, want ErrNotLandGroup", err)
			}
			if err := c.AddProjectMember(ctx, "acme", newcomer.id); !errors.Is(err, ErrNotLandGroup) {
				t.Errorf("Add: %v, want ErrNotLandGroup", err)
			}
			if err := c.RemoveProjectMember(ctx, "acme", member.id); !errors.Is(err, ErrNotLandGroup) {
				t.Errorf("Remove: %v, want ErrNotLandGroup", err)
			}
			if _, err := c.ProjectMembers(ctx, "acme"); !errors.Is(err, ErrNotLandGroup) {
				t.Errorf("Members: %v, want ErrNotLandGroup", err)
			}
			if w := fk.writeLog(); len(w) != 0 {
				t.Fatalf("wrote %v to a group it does not own", w)
			}
			if !g.members[member.id] || g.members[newcomer.id] || len(g.realmRoles) != 1 {
				t.Fatalf("group changed: members %v roles %v", g.members, g.realmRoles)
			}
		})
	}
}

// Finding HIGH (#704): a land whose group the chart renders is refused before
// any request, so a land named claimlayer never reaches ClaimLayer's group.
func TestReservedLand_NoRequests(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.addGroup(TenantsGroup, "")
	chart := fk.addGroup("project-claimlayer", "", ProjectMemberRole)
	chart.attrs[LandAttribute] = []string{"claimlayer"} // even a forged marker does not help
	deployed := fk.addLandGroup("extra")
	u := fk.addUser("alice", "1")
	c, err := New(Config{
		BaseURL: fk.srv.URL, Realm: "gardenio", ClientID: "gardenio-lands-provisioner", ClientSecret: "s3cret",
		ReservedGroups: []string{"project-extra"}, HTTPClient: fk.srv.Client(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, land := range []string{"claimlayer", "extra"} {
		if _, err := c.EnsureProjectGroup(ctx, land); !errors.Is(err, ErrReservedLand) {
			t.Errorf("Ensure(%s): %v", land, err)
		}
		if err := c.AddProjectMember(ctx, land, u.id); !errors.Is(err, ErrReservedLand) {
			t.Errorf("Add(%s): %v", land, err)
		}
		if err := c.RemoveProjectMember(ctx, land, u.id); !errors.Is(err, ErrReservedLand) {
			t.Errorf("Remove(%s): %v", land, err)
		}
		if _, err := c.ProjectMembers(ctx, land); !errors.Is(err, ErrReservedLand) {
			t.Errorf("Members(%s): %v", land, err)
		}
	}
	if _, err := GroupName("claimlayer"); !errors.Is(err, ErrReservedLand) {
		t.Errorf("GroupName(claimlayer): %v", err)
	}
	if fk.tokens != 0 || len(deployed.members) != 0 || len(chart.members) != 0 {
		t.Fatalf("a reserved land reached Keycloak (%d token calls)", fk.tokens)
	}
}

// TestReservedGroups_CoverChart keeps ReservedGroups in step with every group
// the chart renders, by default and as deployed, so a new chart group cannot
// silently become a land.
func TestReservedGroups_CoverChart(t *testing.T) {
	reserved := map[string]bool{}
	for _, g := range ReservedGroups {
		reserved[g] = true
	}
	for _, file := range []string{"../../../helm/gardenio/values.yaml", "../../../../deploy-values/gardenio-k3s.yaml"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Keycloak struct {
				Groups        []struct{ Name string } `yaml:"groups"`
				ProjectGroups []struct{ Name string } `yaml:"projectGroups"`
			} `yaml:"keycloak"`
		}
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var names []string
		for _, g := range v.Keycloak.Groups {
			names = append(names, g.Name)
		}
		for _, g := range v.Keycloak.ProjectGroups {
			names = append(names, GroupPrefix+g.Name)
		}
		if len(names) == 0 {
			t.Errorf("%s: no keycloak.groups found; did the values layout move?", file)
		}
		for _, n := range names {
			if !reserved[n] {
				t.Errorf("%s renders group %q, which is not in ReservedGroups", file, n)
			}
		}
	}
}

func TestProjectMembers_Pages(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	if _, err := c.EnsureProjectGroup(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	g := fk.groupByName("project-big")
	for i := range 230 {
		g.members[fk.addUser(fmt.Sprintf("u%d", i), "").id] = true
	}
	got, err := c.ProjectMembers(ctx, "big")
	if err != nil || len(got) != 230 {
		t.Fatalf("ProjectMembers = %d users, %v; want 230", len(got), err)
	}
	seen := map[string]bool{}
	for _, u := range got {
		if seen[u.ID] {
			t.Fatalf("duplicate %s", u.ID)
		}
		seen[u.ID] = true
	}
}

func TestFindUserByGitHubID(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.looseAttrSearch = true
	alice := fk.addUser("alice", "12")
	fk.addUser("bob", "123") // a loose server match for 12
	fk.addUser("carol", "")
	fk.addUser("dup1", "77")
	fk.addUser("dup2", "77")
	c := fk.client(t, nil)
	ctx := context.Background()

	u, err := c.FindUserByGitHubID(ctx, 12)
	if err != nil || u.ID != alice.id || u.GitHubID != 12 || u.Username != "alice" {
		t.Fatalf("Find(12) = %+v, %v; want alice", u, err)
	}
	if _, err := c.FindUserByGitHubID(ctx, 999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Find(999): %v", err)
	}
	if _, err := c.FindUserByGitHubID(ctx, 77); !errors.Is(err, ErrAmbiguousUser) {
		t.Errorf("Find(77): %v", err)
	}
	if _, err := c.FindUserByGitHubID(ctx, 0); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Find(0): %v", err)
	}
	if w := fk.writeLog(); len(w) != 0 {
		t.Fatalf("a lookup wrote %v", w)
	}
}

func TestFindUserByEmail(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.looseEmailSearch = true
	alice := fk.addUser("alice", "12")
	alice.emailVerified, alice.links = true, []string{GitHubProvider}
	fk.addUser("malice", "") // a loose server match for alice@example.com
	carol := fk.addUser("carol", "")
	dup1, dup2 := fk.addUser("dup1", ""), fk.addUser("dup2", "")
	dup1.email, dup2.email = "dup@example.com", "dup@example.com"
	c := fk.client(t, nil)
	ctx := context.Background()

	u, err := c.FindUserByEmail(ctx, "alice@example.com")
	if err != nil || u.ID != alice.id || !u.EmailVerified || !slices.Equal(u.IdentityProviders, []string{GitHubProvider}) || u.GitHubID != 12 {
		t.Fatalf("Find(alice) = %+v, %v", u, err)
	}
	u, err = c.FindUserByEmail(ctx, "carol@example.com")
	if err != nil || u.ID != carol.id || u.EmailVerified || u.IdentityProviders == nil || len(u.IdentityProviders) != 0 {
		t.Fatalf("Find(carol) = %+v, %v; want unverified with no links", u, err)
	}
	for _, email := range []string{"nobody@example.com", "ALICE@example.com", "", "a b@example.com", "example.com"} {
		if _, err := c.FindUserByEmail(ctx, email); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("Find(%q): %v, want ErrUserNotFound", email, err)
		}
	}
	if _, err := c.FindUserByEmail(ctx, "dup@example.com"); !errors.Is(err, ErrAmbiguousUser) {
		t.Errorf("Find(dup): %v", err)
	}
	if w := fk.writeLog(); len(w) != 0 {
		t.Fatalf("a lookup wrote %v", w)
	}
}

func TestAddInvited_JoinsTheChartGroupOnly(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	u := fk.addUser("alice", "")
	if err := c.AddInvited(ctx, u.id); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("AddInvited with no invited group: %v, want ErrGroupNotFound", err)
	}
	if fk.groupByName(InvitedGroup) != nil || len(fk.writeLog()) != 0 {
		t.Fatal("AddInvited created the group or wrote")
	}
	g := fk.addGroup(InvitedGroup, "")
	for range 2 {
		if err := c.AddInvited(ctx, u.id); err != nil {
			t.Fatal(err)
		}
	}
	if !g.members[u.id] {
		t.Fatal("not in invited")
	}
	if err := c.AddInvited(ctx, "../x"); !errors.Is(err, ErrInvalidUserID) {
		t.Errorf("AddInvited(../x): %v", err)
	}
	fk.assertWritesScoped(t)
}

func TestInvitedMembers(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	if _, err := c.InvitedMembers(ctx); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("InvitedMembers with no invited group: %v, want ErrGroupNotFound", err)
	}
	g := fk.addGroup(InvitedGroup, "")
	alice, bob := fk.addUser("alice", ""), fk.addUser("bob", "")
	fk.addUser("carol", "")
	g.members[alice.id], g.members[bob.id] = true, true
	got, err := c.InvitedMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range got {
		ids = append(ids, u.ID)
	}
	want := []string{alice.id, bob.id}
	slices.Sort(ids)
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("InvitedMembers = %v, want %v", ids, want)
	}
	if len(fk.writeLog()) != 0 {
		t.Fatal("InvitedMembers wrote")
	}
}

func TestToken_CachedAndRefreshedOn401(t *testing.T) {
	fk := newFakeKeycloak(t)
	fk.addGroup(TenantsGroup, "")
	alice := fk.addUser("alice", "1")
	c := fk.client(t, nil)
	ctx := context.Background()
	for range 3 {
		if err := c.AddTenant(ctx, alice.id); err != nil {
			t.Fatal(err)
		}
	}
	if fk.tokens != 1 {
		t.Fatalf("token calls = %d, want 1 (cached)", fk.tokens)
	}
	fk.expireNextAdminCall = true
	if err := c.AddTenant(ctx, alice.id); err != nil {
		t.Fatalf("after a 401: %v", err)
	}
	if fk.tokens != 2 {
		t.Fatalf("token calls = %d, want 2 (refreshed once)", fk.tokens)
	}
}

// Finding LOW (#704): a redirect on the token POST is not followed, so
// client_secret is never re-sent to wherever it points (a 307/308 would
// replay the form body).
func TestToken_DoesNotFollowRedirect(t *testing.T) {
	var leaked []string
	var mu sync.Mutex
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		leaked = append(leaked, r.Method+" "+r.URL.Path+" secret="+r.Form.Get("client_secret"))
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "stolen", "expires_in": 300})
	}))
	defer sink.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, sink.URL+r.URL.Path, code)
		}))
		// One client trusts both test certificates and would follow by default.
		hc := redirector.Client()
		hc.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(sink.Certificate())
		c, err := New(Config{BaseURL: redirector.URL, Realm: "gardenio", ClientID: "gardenio-lands-provisioner", ClientSecret: "s3cret", HTTPClient: hc}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.EnsureProjectGroup(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("%d: err = %v, want a token error", code, err)
		}
		if hc.CheckRedirect != nil {
			t.Errorf("New changed the caller's HTTPClient")
		}
		redirector.Close()
	}
	if len(leaked) != 0 {
		t.Fatalf("followed a redirect: %v", leaked)
	}
}

func TestToken_BadSecret(t *testing.T) {
	fk := newFakeKeycloak(t)
	c, err := New(Config{BaseURL: fk.srv.URL, Realm: "gardenio", ClientID: "gardenio-lands-provisioner", ClientSecret: "wrong", HTTPClient: fk.srv.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureProjectGroup(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a token error", err)
	}
	if fk.groupByName("project-acme") != nil {
		t.Fatal("created a group without a token")
	}
}

// childByName returns the subgroup name of parent, or nil.
func (fk *fakeKeycloak) childByName(parent *fakeGroup, name string) *fakeGroup {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	for _, g := range fk.groups {
		if g.parent == parent.id && g.name == name {
			return g
		}
	}
	return nil
}

// Land roles (Commons 4.2): owners and guests are the land group's owners and
// guests subgroups, marked like the land group and holding no role.
func TestEnsureLandRoleGroup_CreatesMarkedSubgroup_Idempotent(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	if _, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("before the land group: %v, want ErrGroupNotFound", err)
	}
	if _, err := c.EnsureProjectGroup(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	land := fk.groupByName("project-acme")
	id, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	owners := fk.childByName(land, "owners")
	if owners == nil || owners.id != id || owners.path != "/project-acme/owners" {
		t.Fatalf("owners subgroup = %+v, id %q", owners, id)
	}
	if v := owners.attrs[LandAttribute]; len(v) != 1 || v[0] != "acme" {
		t.Fatalf("owners marker = %v", v)
	}
	if len(owners.realmRoles) != 0 {
		t.Fatalf("owners holds roles %v", owners.realmRoles)
	}
	first := fk.writeLog()
	if id2, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner); err != nil || id2 != id {
		t.Fatalf("second Ensure = %q, %v", id2, err)
	}
	if again := fk.writeLog(); len(again) != len(first) {
		t.Fatalf("second Ensure wrote %v", again[len(first):])
	}
	if _, err := c.EnsureLandRoleGroup(ctx, "acme", RoleMember); err == nil {
		t.Fatal("RoleMember has no subgroup; want an error")
	}
	fk.assertWritesScoped(t)
}

func TestEnsureLandRoleGroup_RefusesUnmarkedOrRoleHoldingSubgroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(g *fakeGroup)
		want  error
	}{
		{"unmarked", func(g *fakeGroup) {}, ErrNotLandGroup},
		{"other land", func(g *fakeGroup) { g.attrs[LandAttribute] = []string{"other"} }, ErrNotLandGroup},
		{"holds a role", func(g *fakeGroup) {
			g.attrs[LandAttribute] = []string{"acme"}
			g.realmRoles = []string{"gardenio-viewer"}
		}, ErrForeignRoles},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := newFakeKeycloak(t)
			land := fk.addLandGroup("acme", ProjectMemberRole)
			owners := fk.addGroup("owners", land.id)
			tc.setup(owners)
			alice := fk.addUser("alice", "1")
			c := fk.client(t, nil)
			ctx := context.Background()
			if _, err := c.EnsureLandRoleGroup(ctx, "acme", RoleOwner); !errors.Is(err, tc.want) {
				t.Fatalf("Ensure: %v, want %v", err, tc.want)
			}
			if tc.want == ErrNotLandGroup {
				if err := c.AddLandMember(ctx, "acme", RoleOwner, alice.id); !errors.Is(err, ErrNotLandGroup) {
					t.Fatalf("Add: %v, want ErrNotLandGroup", err)
				}
			}
			if w := fk.writeLog(); len(w) != 0 {
				t.Fatalf("wrote %v", w)
			}
		})
	}
}

func TestLandMembers_RolesAndRemoveFromLand(t *testing.T) {
	fk := newFakeKeycloak(t)
	c := fk.client(t, nil)
	ctx := context.Background()
	alice := fk.addUser("alice", "1") // owner
	bob := fk.addUser("bob", "2")     // member
	carol := fk.addUser("carol", "3") // guest
	if _, err := c.EnsureProjectGroup(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []LandRole{RoleOwner, RoleGuest} {
		if _, err := c.EnsureLandRoleGroup(ctx, "acme", role); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct {
		u    *fakeUser
		role LandRole
	}{{alice, RoleOwner}, {bob, RoleMember}, {carol, RoleGuest}, {alice, RoleOwner}} {
		if err := c.AddLandMember(ctx, "acme", m.role, m.u.id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.LandMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range got {
		roles = append(roles, m.Username+"="+string(m.Role))
	}
	if want := "alice=owner bob=member carol=guest"; strings.Join(roles, " ") != want {
		t.Fatalf("LandMembers = %v, want %s", roles, want)
	}
	lands, err := c.UserLands(ctx, alice.id)
	if err != nil || len(lands) != 1 || lands[0] != "acme" {
		t.Fatalf("UserLands(alice) = %v, %v", lands, err)
	}

	for _, u := range []*fakeUser{alice, bob, carol, alice} {
		if err := c.RemoveFromLand(ctx, "acme", u.id); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := c.LandMembers(ctx, "acme"); err != nil || len(got) != 0 {
		t.Fatalf("after RemoveFromLand: %+v, %v", got, err)
	}
	if lands, err := c.UserLands(ctx, alice.id); err != nil || len(lands) != 0 {
		t.Fatalf("UserLands(alice) after removal = %v, %v", lands, err)
	}
	fk.assertWritesScoped(t)
}

func TestUserLands_ReadsPathsOnly(t *testing.T) {
	fk := newFakeKeycloak(t)
	a := fk.addLandGroup("acme", ProjectMemberRole)
	b := fk.addGroup("project-claimlayer", "", ProjectMemberRole)
	owners := fk.addGroup("owners", b.id)
	admins := fk.addGroup("gardenio-admins", "")
	u := fk.addUser("dana", "4")
	a.members[u.id], owners.members[u.id], admins.members[u.id] = true, true, true
	c := fk.client(t, nil)
	lands, err := c.UserLands(context.Background(), u.id)
	if err != nil || strings.Join(lands, ",") != "acme,claimlayer" {
		t.Fatalf("UserLands = %v, %v", lands, err)
	}
	if _, err := c.UserLands(context.Background(), "../x"); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("bad id: %v", err)
	}
}
