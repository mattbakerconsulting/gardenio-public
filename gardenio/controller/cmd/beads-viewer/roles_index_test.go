package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// rolesIndexFake routes ListBeadsFiltered by query SHAPE so a buildRolesIndex
// test can seed the type:role config-bead list and the cross-project live-agent
// list independently. The shared fakeDaemon collapses every active-agent query
// onto liveAgentsResp and routes Types=[role] to listResp, which would make the
// two indistinguishable — so the roles index needs its own router. Embeds
// *fakeDaemon for the rest of the surface (ListRoleBeads from rolesResp,
// ListProjectBeads from projectsResp, ListAccountBeadsByProject from
// accountsByProjectResp).
type rolesIndexFake struct {
	*fakeDaemon

	roleConfigResp *beadsapi.ListBeadsResult // Types=[role]
	agentsResp     *beadsapi.ListBeadsResult // Types=[agent]

	roleConfigErr error
	agentsErr     error
}

func (f *rolesIndexFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	switch {
	case slices.Contains(q.Types, "role"):
		return orEmpty(f.roleConfigResp), f.roleConfigErr
	case slices.Contains(q.Types, "agent"):
		return orEmpty(f.agentsResp), f.agentsErr
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func rolesIndexServer(f *rolesIndexFake) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) })
	return srv
}

func roleConfigBeadFixture(id, title string, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "role",
		Status: "open",
		Title:  title,
		Fields: fields,
	}
}

// TestBuildRolesIndex_Union covers the core discovery behavior: the index
// unions configured roles (type:role beads) with live-agent roles, counts
// agents per role across projects, flags drift (live role with no config bead),
// and computes a validity badge from the config bead's raw fields.
func TestBuildRolesIndex_Union(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	// Two configured roles; "crew" is healthy, "tester" has a malformed secrets
	// field (silently dropped → red badge).
	f.roleConfigResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		roleConfigBeadFixture("gd-role-crew", "crew", map[string]string{"image": "registry/agent:crew"}),
		roleConfigBeadFixture("gd-role-tester", "tester", map[string]string{"secrets": "{not-an-array}"}),
	}}
	// Agents: 2 crew (one in gardenio, one in kbeads), 1 gardener with NO config
	// bead (drift), 1 untyped (excluded from the vocabulary).
	f.agentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		agentBead("a1", "crew-alpha", "crew", "gardenio", "working"),
		agentBead("a2", "crew-beta", "crew", "kbeads", "working"),
		agentBead("a3", "cap-one", "gardener", "gardenio", "working"),
		{ID: "a4", Type: "agent", Status: "in_progress", Fields: map[string]string{"agent": "untyped-x", "agent_state": "working"}},
	}}

	view := rolesIndexServer(f).buildRolesIndex(context.Background())

	if view.Error != "" {
		t.Fatalf("unexpected page error: %q", view.Error)
	}
	// Vocabulary = {crew, tester, gardener}; untyped agent contributes nothing.
	if view.TotalRoles != 3 {
		t.Fatalf("TotalRoles = %d, want 3 (crew, tester, gardener)", view.TotalRoles)
	}
	if view.ConfiguredRoles != 2 {
		t.Errorf("ConfiguredRoles = %d, want 2", view.ConfiguredRoles)
	}
	if view.UnconfiguredRoles != 1 {
		t.Errorf("UnconfiguredRoles = %d, want 1 (gardener drift)", view.UnconfiguredRoles)
	}

	cards := map[string]roleIndexCard{}
	for _, c := range view.Cards {
		cards[c.Name] = c
	}
	if got := cards["crew"].AgentCount; got != 2 {
		t.Errorf("crew AgentCount = %d, want 2", got)
	}
	if cards["crew"].Validity.Badge() != "green" {
		t.Errorf("crew badge = %q, want green", cards["crew"].Validity.Badge())
	}
	if cards["crew"].ConfigBeadID != "gd-role-crew" {
		t.Errorf("crew ConfigBeadID = %q, want gd-role-crew", cards["crew"].ConfigBeadID)
	}
	if cards["tester"].Validity.Badge() != "red" {
		t.Errorf("tester badge = %q, want red (malformed secrets)", cards["tester"].Validity.Badge())
	}
	if got := cards["gardener"].AgentCount; got != 1 {
		t.Errorf("gardener AgentCount = %d, want 1", got)
	}
	if cards["gardener"].HasConfigBead {
		t.Errorf("gardener should be drift (no config bead)")
	}
}

// TestBuildRolesIndex_SortOrder: configured roles sort before drift, then by
// agent count desc, then name asc.
func TestBuildRolesIndex_SortOrder(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	f.roleConfigResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		roleConfigBeadFixture("gd-role-crew", "crew", nil),
		roleConfigBeadFixture("gd-role-triage", "triage", nil),
	}}
	f.agentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		agentBead("a1", "crew-a", "crew", "gardenio", "working"),
		agentBead("a2", "crew-b", "crew", "gardenio", "working"),
		agentBead("a3", "triage-a", "triage", "gardenio", "working"),
		agentBead("a4", "drift-a", "zzz-drift", "gardenio", "working"),
	}}

	view := rolesIndexServer(f).buildRolesIndex(context.Background())
	got := make([]string, 0, len(view.Cards))
	for _, c := range view.Cards {
		got = append(got, c.Name)
	}
	// crew (configured, 2 agents) > triage (configured, 1) > zzz-drift (drift).
	want := []string{"crew", "triage", "zzz-drift"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestBuildRolesIndex_DuplicateTitleGuard: a duplicate type:role title resolves
// to the lowest bead ID, matching ListRoleBeads (ka-0000007118).
func TestBuildRolesIndex_DuplicateTitleGuard(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	f.roleConfigResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		roleConfigBeadFixture("gd-role-zzz", "crew", map[string]string{"image": "loser"}),
		roleConfigBeadFixture("gd-role-aaa", "crew", map[string]string{"image": "winner"}),
	}}
	view := rolesIndexServer(f).buildRolesIndex(context.Background())
	if len(view.Cards) != 1 {
		t.Fatalf("len(Cards) = %d, want 1 (deduped)", len(view.Cards))
	}
	if view.Cards[0].ConfigBeadID != "gd-role-aaa" {
		t.Errorf("ConfigBeadID = %q, want gd-role-aaa (lowest ID wins)", view.Cards[0].ConfigBeadID)
	}
}

// TestBuildRolesIndex_BothFail: when both the registry and the agent list fail,
// the view carries a page-level error rather than a blank index.
func TestBuildRolesIndex_BothFail(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	f.roleConfigErr = errors.New("registry down")
	f.agentsErr = errors.New("agents down")
	view := rolesIndexServer(f).buildRolesIndex(context.Background())
	if view.Error == "" {
		t.Errorf("expected page-level error when both registry and agents fail")
	}
}

// TestBuildRolesIndex_AgentFailDegrades: agent fetch fails but the registry
// still populates the index (counts degrade to zero, no page error).
func TestBuildRolesIndex_AgentFailDegrades(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	f.roleConfigResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		roleConfigBeadFixture("gd-role-crew", "crew", nil),
	}}
	f.agentsErr = errors.New("agents down")
	view := rolesIndexServer(f).buildRolesIndex(context.Background())
	if view.Error != "" {
		t.Fatalf("unexpected page error: %q", view.Error)
	}
	if view.TotalRoles != 1 || view.Cards[0].Name != "crew" {
		t.Errorf("expected crew card from registry despite agent failure, got %+v", view.Cards)
	}
	if view.Cards[0].AgentCount != 0 {
		t.Errorf("AgentCount = %d, want 0 (agent fetch failed)", view.Cards[0].AgentCount)
	}
}

// TestHandleRolesIndex_Page renders the full /roles page end-to-end: the role
// card links to /role/<name>, the validity badge renders, and a drift role
// shows the "no role bead" hint.
func TestHandleRolesIndex_Page(t *testing.T) {
	f := &rolesIndexFake{fakeDaemon: newFake()}
	f.roleConfigResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		roleConfigBeadFixture("gd-role-crew", "crew", map[string]string{"image": "registry/agent:crew"}),
	}}
	f.agentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		agentBead("a1", "crew-alpha", "crew", "gardenio", "working"),
		agentBead("a2", "drift-one", "wanderer", "gardenio", "working"),
	}}

	srv := rolesIndexServer(f)
	r := httptest.NewRequest(http.MethodGet, "/roles", nil)
	w := httptest.NewRecorder()
	srv.handleRolesIndex(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/role/crew"`) {
		t.Errorf("expected link to /role/crew\nbody=%s", body)
	}
	if !strings.Contains(body, `href="/role/wanderer"`) {
		t.Errorf("expected link to /role/wanderer (drift role still links)\nbody=%s", body)
	}
	if !strings.Contains(body, "no role bead") {
		t.Errorf("expected drift hint 'no role bead'\nbody=%s", body)
	}
	if !strings.Contains(body, "validity-green") {
		t.Errorf("expected green validity badge for crew\nbody=%s", body)
	}
}

// TestNavLink_RolesPresent verifies the layout nav carries a /roles link so the
// discovery index is reachable from any page.
func TestNavLink_RolesPresent(t *testing.T) {
	// Roles is a vibe-only nav link after gb-ux #3 (ka-0000003340); render as a
	// vibe-mode viewer so the gated link is present.
	f := newFake()
	f.userResp = vibeUserResp()
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/roles"`) {
		t.Errorf("layout missing /roles nav link\nbody=%s", w.Body.String())
	}
}
