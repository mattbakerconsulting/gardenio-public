package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestEscalationCreateAudience(t *testing.T) {
	for flag, want := range map[string]string{"": "", "owner": "owner", " Owner ": "owner"} {
		got, err := escalationCreateAudience(flag)
		if err != nil || got != want {
			t.Errorf("escalationCreateAudience(%q) = %q, %v; want %q", flag, got, err, want)
		}
	}
	// The daemon derives operator and desk; a filer may not pick them.
	for _, flag := range []string{"desk", "operator", "everyone"} {
		if _, err := escalationCreateAudience(flag); err == nil {
			t.Errorf("--audience %s was accepted", flag)
		}
	}
}

// routeStub serves escalations and answers POST /v1/escalations/{id}/audience
// the way kbeads does: an empty audience stamps the derived one.
type routeStub struct {
	mu     sync.Mutex
	beads  []map[string]any
	routes map[string]string // id -> requested audience ("" = derive)
	stamp  map[string]string // id -> what the daemon says it stored
}

func (s *routeStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": s.beads, "total": len(s.beads)})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			for _, b := range s.beads {
				if b["id"] == id {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/audience"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/escalations/"), "/audience")
			var req beadsapi.RouteEscalationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.routes[id] = req.Audience
			got := req.Audience
			if got == "" {
				got = s.stamp[id]
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"escalation": map[string]string{"audience": got}})
		case r.Method == http.MethodPatch:
			http.Error(w, "a generic PATCH of an escalation", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	})
}

func setupRouteStub(t *testing.T, beads ...map[string]any) *routeStub {
	t.Helper()
	stub := &routeStub{beads: beads, routes: map[string]string{}, stamp: map[string]string{}}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return stub
}

func resetEscalationRouteFlags(t *testing.T) {
	t.Helper()
	escalationRouteCmd.ResetFlags()
	escalationRouteCmd.Flags().String("audience", "", "")
	escalationRouteCmd.Flags().String("reason", "", "")
}

func TestEscalationRoute_UsesTheRouteEndpoint(t *testing.T) {
	stub := setupRouteStub(t, makeReflagEscalation("gd-r1", 2, map[string]any{"operation_type": "k8s_rbac"}, nil))
	resetEscalationRouteFlags(t)
	_ = escalationRouteCmd.Flags().Set("audience", "owner")
	_ = escalationRouteCmd.Flags().Set("reason", "needs a policy call")
	escalationRouteCmd.SetContext(context.Background())
	if err := escalationRouteCmd.RunE(escalationRouteCmd, []string{"gd-r1"}); err != nil {
		t.Fatalf("route: %v", err)
	}
	if got, ok := stub.routes["gd-r1"]; !ok || got != "owner" {
		t.Errorf("routed %v, want gd-r1 -> owner", stub.routes)
	}
}

func TestEscalationRoute_RefusesHidingAForcedOwnerEscalation(t *testing.T) {
	for name, f := range map[string]map[string]any{
		"secret":    {"operation_type": "aws_iam", "sensitivity": "secret"},
		"approvers": {"operation_type": "create", "approvers": []any{"oidc:owner@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			stub := setupRouteStub(t, makeReflagEscalation("gd-r2", 2, f, nil))
			resetEscalationRouteFlags(t)
			_ = escalationRouteCmd.Flags().Set("audience", "operator")
			escalationRouteCmd.SetContext(context.Background())
			err := escalationRouteCmd.RunE(escalationRouteCmd, []string{"gd-r2"})
			if err == nil || !strings.Contains(err.Error(), "always goes to the owner") {
				t.Fatalf("route of a forced escalation away from the owner: err = %v", err)
			}
			if len(stub.routes) != 0 {
				t.Errorf("refused route still wrote: %v", stub.routes)
			}
		})
	}
}

func TestEscalationRoute_RejectsUnknownAudience(t *testing.T) {
	stub := setupRouteStub(t, makeReflagEscalation("gd-r3", 2, nil, nil))
	resetEscalationRouteFlags(t)
	_ = escalationRouteCmd.Flags().Set("audience", "everyone")
	escalationRouteCmd.SetContext(context.Background())
	if err := escalationRouteCmd.RunE(escalationRouteCmd, []string{"gd-r3"}); err == nil {
		t.Fatal("route accepted an unknown audience")
	}
	if len(stub.routes) != 0 {
		t.Errorf("wrote %v", stub.routes)
	}
}

// TestEscalationBackfillAudience: backfill asks the daemon to stamp its
// derived audience (an empty route) on each escalation whose stored field is
// not already its effective audience, and never PATCHes the field.
func TestEscalationBackfillAudience(t *testing.T) {
	stub := setupRouteStub(t,
		makeReflagEscalation("gd-merge", 1, map[string]any{"operation_type": "gitlab_mr_merge"}, nil),
		makeReflagEscalation("gd-helm", 1, map[string]any{"operation_type": "helm_chart_change"}, nil),
		makeReflagEscalation("gd-cred", 1, map[string]any{"operation_type": "aws_iam", "audience": "operator", "credential_requirements": []any{map[string]any{"type": "aws"}}}, nil),
		makeReflagEscalation("gd-chose", 1, map[string]any{"operation_type": "gitlab_mr_merge", "audience": "owner"}, nil),
		makeReflagEscalation("gd-done", 1, map[string]any{"operation_type": "k8s_apply", "audience": "operator"}, nil),
	)
	stub.stamp = map[string]string{"gd-merge": "desk", "gd-helm": "operator", "gd-cred": "owner"}
	escalationBackfillAudienceCmd.SetContext(context.Background())
	if err := escalationBackfillAudienceCmd.RunE(escalationBackfillAudienceCmd, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(stub.routes) != 3 {
		t.Errorf("routed %v, want gd-merge, gd-helm and gd-cred", stub.routes)
	}
	for _, id := range []string{"gd-merge", "gd-helm", "gd-cred"} {
		if got, ok := stub.routes[id]; !ok || got != "" {
			t.Errorf("%s: routed %q (present %v), want an empty (derive) route", id, got, ok)
		}
	}
}

// createStub answers escalation creates, recording each POSTed fields map, and
// refuses the audience field the way a daemon older than it does when old.
type createStub struct {
	mu    sync.Mutex
	old   bool
	posts []map[string]any
}

func (s *createStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.posts = append(s.posts, body.Fields)
			s.mu.Unlock()
			if _, ok := body.Fields["audience"]; ok && s.old {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": `invalid fields: audience: unknown field "audience" (run kd fields escalation)`})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-aud-esc"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	})
}

func runAudienceCreate(t *testing.T, stub *createStub, audience string) error {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = old })
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Pick the IdP for Lands")
	_ = cmd.Flags().Set("operation-type", "policy_choice")
	_ = cmd.Flags().Set("audience", audience)
	t.Cleanup(func() {
		_ = cmd.Flags().Set("title", "")
		_ = cmd.Flags().Set("operation-type", "")
		_ = cmd.Flags().Set("audience", "")
	})
	return cmd.RunE(cmd, nil)
}

func TestEscalationCreate_AsksForOwnerOnly(t *testing.T) {
	stub := &createStub{}
	if err := runAudienceCreate(t, stub, "owner"); err != nil {
		t.Fatal(err)
	}
	if len(stub.posts) != 1 || stub.posts[0]["audience"] != "owner" {
		t.Fatalf("posted %v, want one create with audience=owner", stub.posts)
	}
	stub = &createStub{}
	if err := runAudienceCreate(t, stub, ""); err != nil {
		t.Fatal(err)
	}
	if a, ok := stub.posts[0]["audience"]; ok {
		t.Errorf("no --audience still sent audience=%v; the daemon derives it", a)
	}
}

func TestEscalationCreate_OldDaemonFallsBackWithoutAudience(t *testing.T) {
	stub := &createStub{old: true}
	if err := runAudienceCreate(t, stub, "owner"); err != nil {
		t.Fatalf("create against an old daemon failed: %v", err)
	}
	if len(stub.posts) != 2 {
		t.Fatalf("got %d creates, want the refused one and its retry", len(stub.posts))
	}
	if _, ok := stub.posts[1]["audience"]; ok {
		t.Errorf("the retry still carried audience: %v", stub.posts[1])
	}
}
