package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// gd-SyEYkqe601: the agent page shows the live-vs-approved ServiceAccount
// permission diff for an agent replayed from a privileged spawn escalation,
// flagging widening without blocking, and an unknown probe as unknown.

var replayedAgentFields = map[string]string{
	"role":                "k8s-live",
	"spawned_by":          "gd-esc1",
	"spawn_approval_mode": "human",
}

func permissionDiffFake(agentFields map[string]string) *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-agent1": {
				ID:     "gd-agent1",
				Title:  "gardenio-some-agent-abc",
				Kind:   "config",
				Type:   "agent",
				Status: "open",
				Fields: agentFields,
			},
		},
	}
}

func getPermissionDiffFragment(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/b/gd-agent1/permission-diff", nil)
	w := httptest.NewRecorder()
	mkServer(t, f).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestAgentSpawnEscalationID(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"nil", nil, ""},
		{"non-agent", &beadsapi.BeadDetail{Type: "task", Fields: replayedAgentFields}, ""},
		{"ordinary spawn names its spawner", &beadsapi.BeadDetail{Type: "agent", Fields: map[string]string{"spawned_by": "thread-abc"}}, ""},
		{"escalation replay", &beadsapi.BeadDetail{Type: "agent", Fields: replayedAgentFields}, "gd-esc1"},
	}
	for _, tc := range cases {
		if got := agentSpawnEscalationID(tc.bead); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildAgentPermissionDiffView_OrdinarySpawnSkipsDaemon(t *testing.T) {
	f := permissionDiffFake(map[string]string{"spawned_by": "thread-abc"})
	s := &Server{daemon: f, logger: testLogger()}
	if view := s.buildAgentPermissionDiffView(context.Background(), f.beads["gd-agent1"]); view.Available {
		t.Errorf("expected Available=false, got %+v", view)
	}
	if f.permissionDiffCalls != 0 {
		t.Errorf("expected no daemon call, got %d", f.permissionDiffCalls)
	}
}

func TestBuildAgentPermissionDiffView_DaemonErrorDegrades(t *testing.T) {
	f := permissionDiffFake(replayedAgentFields)
	f.permissionDiffErr = errors.New("daemon down")
	s := &Server{daemon: f, logger: testLogger()}
	if view := s.buildAgentPermissionDiffView(context.Background(), f.beads["gd-agent1"]); view.Available {
		t.Errorf("expected Available=false on daemon error, got %+v", view)
	}
}

func TestBuildAgentPermissionDiffView_Success(t *testing.T) {
	f := permissionDiffFake(replayedAgentFields)
	f.permissionDiffOK = true
	f.permissionDiffResp = &beadsapi.SpawnPermissionDiff{
		SnapshotAt:     "2026-10-27T00:00:00Z",
		ServiceAccount: "system:serviceaccount:gardenio:gc-k8s-live",
		Widened:        true,
		Namespaces:     []beadsapi.NamespacePermissionDiff{{Namespace: "gc-dogfood", State: "widened"}},
	}
	s := &Server{daemon: f, logger: testLogger()}
	view := s.buildAgentPermissionDiffView(context.Background(), f.beads["gd-agent1"])
	if !view.Available || !view.Widened || len(view.Namespaces) != 1 || view.ServiceAccount == "" {
		t.Errorf("expected a populated widened view, got %+v", view)
	}
	if f.permissionDiffLastID != "gd-esc1" {
		t.Errorf("expected the spawned_by escalation id, got %q", f.permissionDiffLastID)
	}
}

func TestPermissionDiffRuleLabel(t *testing.T) {
	cases := map[string]beadsapi.PermissionDiffRow{
		"pods":                   {Resource: "pods"},
		"jobs.batch":             {APIGroup: "batch", Resource: "jobs"},
		"secrets [a b]":          {Resource: "secrets", ResourceNames: []string{"a", "b"}},
		"/healthz":               {NonResourceURL: "/healthz"},
		"deployments/scale.apps": {APIGroup: "apps", Resource: "deployments/scale"},
	}
	for want, row := range cases {
		if got := permissionDiffRuleLabel(row); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestHandleAgentPermissionDiff_WidenedFlagged(t *testing.T) {
	f := permissionDiffFake(replayedAgentFields)
	f.permissionDiffOK = true
	f.permissionDiffResp = &beadsapi.SpawnPermissionDiff{
		SnapshotAt:     "2026-10-27T00:00:00Z",
		ServiceAccount: "system:serviceaccount:gardenio:gc-k8s-live",
		Widened:        true,
		Namespaces: []beadsapi.NamespacePermissionDiff{{
			Namespace: "gardenio",
			State:     "widened",
			Rows: []beadsapi.PermissionDiffRow{
				{Resource: "secrets", ResourceNames: []string{"app-token"}, ApprovedVerbs: []string{"get"}},
				{Resource: "secrets", LiveVerbs: []string{"get", "list"}, AddedVerbs: []string{"get", "list"}},
			},
		}},
	}
	body := getPermissionDiffFragment(t, f)
	for _, want := range []string{
		`id="agent-permission-diff"`,
		`hx-get="/b/gd-agent1/permission-diff"`,
		`hx-trigger="every 20s`,
		`hx-swap="outerHTML"`,
		"agent-permission-diff-widened",
		"widened since approval",
		"secrets [app-token]",
		"permission-diff-row-widened",
		"+get +list",
		"system:serviceaccount:gardenio:gc-k8s-live",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// An errored probe must read as unknown, never as "no drift".
func TestHandleAgentPermissionDiff_UnknownNotUnchanged(t *testing.T) {
	f := permissionDiffFake(replayedAgentFields)
	f.permissionDiffOK = true
	f.permissionDiffResp = &beadsapi.SpawnPermissionDiff{
		ServiceAccount: "system:serviceaccount:gardenio:gc-k8s-live",
		Unknown:        true,
		Namespaces: []beadsapi.NamespacePermissionDiff{{
			Namespace: "gc-dogfood",
			State:     "unknown",
			Reason:    "the live probe failed: no ServiceAccount token mounted in the beads daemon",
		}},
	}
	body := getPermissionDiffFragment(t, f)
	for _, want := range []string{"permission-diff-badge-unknown", "permission-diff-ns-unknown", "no ServiceAccount token mounted"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "agent-permission-diff-widened") || strings.Contains(body, "No permissions.") {
		t.Errorf("an unknown namespace must not render as widened or empty:\n%s", body)
	}
}

// Unavailable renders an empty section with no hx attributes, so the
// page's lazy shell is replaced and polling stops.
func TestHandleAgentPermissionDiff_UnavailableStopsPolling(t *testing.T) {
	body := getPermissionDiffFragment(t, permissionDiffFake(replayedAgentFields))
	if !strings.Contains(body, `id="agent-permission-diff"`) {
		t.Errorf("expected the section id kept for the swap, got:\n%s", body)
	}
	if strings.Contains(body, "hx-get") || strings.Contains(body, "<h2>") {
		t.Errorf("expected an empty, non-polling section, got:\n%s", body)
	}
}

func TestHandleAgentPermissionDiff_ReadAuthzShortCircuit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"404", &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}, http.StatusNotFound},
		{"403 answers as missing", &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "forbidden"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.getBeadErr = tc.err
			req := httptest.NewRequest("GET", "/b/gd-aaa/permission-diff", nil)
			w := httptest.NewRecorder()
			mkServer(t, f).ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d", tc.wantStatus, w.Code)
			}
			if f.permissionDiffCalls != 0 {
				t.Errorf("expected no diff fetch on a %d, got %d calls", tc.wantStatus, f.permissionDiffCalls)
			}
		})
	}
}

// The agent page embeds only the lazy shell: it never calls the daemon's
// live probe while rendering.
func TestAgentBeadShow_EmbedsLazyPermissionDiffShell(t *testing.T) {
	f := permissionDiffFake(replayedAgentFields)
	f.permissionDiffOK = true
	f.permissionDiffResp = &beadsapi.SpawnPermissionDiff{}
	body := getSpawnBadgeBody(t, mkServer(t, f), "/gd-agent1")
	if !strings.Contains(body, `id="agent-permission-diff"`) || !strings.Contains(body, `hx-trigger="load"`) {
		t.Errorf("expected the lazy permission-diff shell on the agent card, got:\n%s", body)
	}
	if f.permissionDiffCalls != 0 {
		t.Errorf("page render called the diff %d times; it must load lazily", f.permissionDiffCalls)
	}
}

func TestAgentBeadShow_OmitsPermissionDiffForOrdinarySpawn(t *testing.T) {
	f := permissionDiffFake(map[string]string{"role": "crew", "spawned_by": "thread-abc"})
	body := getSpawnBadgeBody(t, mkServer(t, f), "/gd-agent1")
	if strings.Contains(body, `id="agent-permission-diff"`) {
		t.Errorf("expected no permission-diff section for an ordinary spawn, got:\n%s", body)
	}
}
