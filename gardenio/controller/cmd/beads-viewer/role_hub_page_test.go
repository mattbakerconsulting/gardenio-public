package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// roleHubPageServer builds a real Server (templates loaded) over a roleHubFake
// so the page + fragment handlers render end-to-end. SetNow pins the clock so
// "ago" rendering is deterministic.
func roleHubPageServer(f *roleHubFake) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) })
	return srv
}

func roleHubReq(role string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/role/"+role, nil)
	r.SetPathValue("role", role)
	return r
}

func roleHubAgentsReq(role string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/b/role/"+role+"/agents", nil)
	r.SetPathValue("role", role)
	return r
}

// TestHandleRoleHub_Page renders the full role hub page for a registered role:
// the "Role:" scope header, the resolved overrides (secrets by name only), the
// scoped config + advice rows, and the lazy agents shell that fetches the
// fragment URL on reveal. The eager page must NOT inline the agent list.
func TestHandleRoleHub_Page(t *testing.T) {
	resume := true
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	f := &roleHubFake{
		fakeDaemon: newFake(),
		// Agents seeded but the eager page must not render them (lazy).
		agentsByLabel: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a1", "crew-alpha", "crew", "gardenio", "working"),
		}},
		configResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-cfg1", Type: "config", Status: "open", Title: "crew nudge prompt", UpdatedAt: now.Add(-2 * time.Hour)},
		}},
		adviceResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-adv1", Type: "advice", Status: "open", Title: "crew advice", UpdatedAt: now.Add(-30 * time.Minute)},
		}},
	}
	f.rolesResp = map[string]beadsapi.RoleInfo{
		"crew": {
			Name:           "crew",
			Image:          "registry/agent:crew",
			Model:          "opus",
			SessionResume:  &resume,
			Secrets:        []beadsapi.SecretEntry{{Env: "FOO", Secret: "s", Key: "k"}},
			ClaudeConfig:   map[string]json.RawMessage{"hooks": json.RawMessage(`{}`)},
			EnabledSkills:  []string{"onboarding", "video"},
			DisabledSkills: []string{"deploy"},
		},
	}

	rec := httptest.NewRecorder()
	roleHubPageServer(f).handleRoleHub(rec, roleHubReq("crew"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Scope header (reuses advice/matching BuildScopeHeader).
	if !strings.Contains(body, "Role: crew") {
		t.Errorf("body missing scope header %q\n%s", "Role: crew", body)
	}
	// Resolved overrides + secret BY NAME + skill toggle panels.
	for _, want := range []string{
		"registry/agent:crew", "opus", "FOO", "hooks", "gd-cfg1", "crew nudge prompt", "gd-adv1", "crew advice",
		"Skills enabled", "onboarding", "video", "Skills disabled", "deploy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// Lazy agents shell wires the fragment URL; the eager page must NOT inline
	// the agent row itself.
	if !strings.Contains(body, "/b/role/crew/agents") {
		t.Errorf("body missing lazy agents URL\n%s", body)
	}
	if !strings.Contains(body, `hx-trigger="intersect once"`) {
		t.Errorf("body missing intersect-once lazy trigger\n%s", body)
	}
	if strings.Contains(body, "crew-alpha") {
		t.Errorf("eager page inlined the lazy agent list (crew-alpha present)\n%s", body)
	}
}

// TestHandleRoleHub_UnregisteredRole renders a role with no type:role bead: the
// "no role bead" badge shows, overrides degrade to the fallback note, and the
// page still 200s (agents + scoped config render below).
func TestHandleRoleHub_UnregisteredRole(t *testing.T) {
	f := &roleHubFake{fakeDaemon: newFake()}
	f.rolesResp = map[string]beadsapi.RoleInfo{} // no "thread" role

	rec := httptest.NewRecorder()
	roleHubPageServer(f).handleRoleHub(rec, roleHubReq("thread"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Role: thread") {
		t.Errorf("body missing scope header\n%s", body)
	}
	if !strings.Contains(body, "no role bead") {
		t.Errorf("unregistered role missing the 'no role bead' badge\n%s", body)
	}
}

// TestHandleRoleHub_MissingRole guards the empty path: a blank role segment is a
// 400 (defensive — the route never matches an empty segment).
func TestHandleRoleHub_MissingRole(t *testing.T) {
	f := &roleHubFake{fakeDaemon: newFake()}
	rec := httptest.NewRecorder()
	roleHubPageServer(f).handleRoleHub(rec, roleHubReq(""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for blank role", rec.Code)
	}
}

// TestHandleRoleHubAgents_Fragment renders the lazy cross-project agents
// fragment: agents grouped by project, each group cross-linking back to the
// project hub, and the deduped count headline.
func TestHandleRoleHubAgents_Fragment(t *testing.T) {
	f := &roleHubFake{
		fakeDaemon: newFake(),
		agentsByLabel: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a1", "crew-alpha", "crew", "gardenio", "working"),
			agentBead("a2", "crew-beta", "crew", "kbeads", "working"),
		}},
		// a2 reappears via the field query (dedup) + a3 added.
		agentsByField: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			agentBead("a2", "crew-beta", "crew", "kbeads", "working"),
			agentBead("a3", "crew-gamma", "crew", "gardenio", "idle"),
		}},
	}

	rec := httptest.NewRecorder()
	roleHubPageServer(f).handleRoleHubAgents(rec, roleHubAgentsReq("crew"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"crew-alpha", "crew-beta", "crew-gamma", "/p/gardenio", "/p/kbeads", "/agents?role=crew"} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %q\n%s", want, body)
		}
	}
}

// TestHandleRoleHubAgents_Empty renders the quiet empty-state when no agent
// carries the role (best-effort: never a 500).
func TestHandleRoleHubAgents_Empty(t *testing.T) {
	f := &roleHubFake{fakeDaemon: newFake()}
	rec := httptest.NewRecorder()
	roleHubPageServer(f).handleRoleHubAgents(rec, roleHubAgentsReq("ghost"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No active agents") {
		t.Errorf("empty fragment missing empty-state\n%s", rec.Body.String())
	}
}
