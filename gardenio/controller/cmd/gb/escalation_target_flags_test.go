package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// P7 Task 13: --target-runner and --spawn-role carry the fields the daemon
// requires for execution_target local and service_account; --target-owner is
// the alternative to --target-runner (P4 Task 16).
func TestEscalationCreate_TargetFlags(t *testing.T) {
	var fields map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				Fields map[string]any `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if fields == nil {
				fields = req.Fields
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-target-flags"})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	t.Cleanup(srv.Close)
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := escalationCreateCmd
	cmd.SetContext(t.Context())
	set := map[string]string{
		"title": "Apply the RBAC fix", "operation-type": "k8s_apply",
		"execution-target": "local", "target-runner": "gd-laptop", "target-owner": "oidc:a@example.com", "spawn-role": "k8s-live",
	}
	for k, v := range set {
		cmd.Flags().Set(k, v)
	}
	t.Cleanup(func() {
		for k := range set {
			cmd.Flags().Set(k, "")
		}
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("escalation create: %v", err)
	}
	if fields["target_runner"] != "gd-laptop" || fields["target_owner"] != "oidc:a@example.com" || fields["spawn_role"] != "k8s-live" || fields["execution_target"] != "local" {
		t.Errorf("fields = %v", fields)
	}
}

// gd-iNgsblfqCx: --execution-target local with no runner is refused before
// anything reaches the daemon, a runner flag alone implies local, and a bare
// --target-owner gains the oidc: prefix a runner's owner is stored with.
func TestResolveEscalationLocalTarget(t *testing.T) {
	cases := []struct {
		name, target, runner, owner string
		wantTarget, wantOwner       string
		wantImplied, wantErr        bool
	}{
		{name: "local with no runner is refused", target: "local", wantErr: true},
		{name: "local with a runner", target: "local", runner: "gd-r1", wantTarget: "local"},
		{name: "local with an owner", target: "local", owner: "oidc:a@example.com", wantTarget: "local", wantOwner: "oidc:a@example.com"},
		{name: "a runner implies local", runner: "gd-r1", wantTarget: "local", wantImplied: true},
		{name: "a bare owner gets the prefix and implies local", owner: "a@example.com", wantTarget: "local", wantOwner: "oidc:a@example.com", wantImplied: true},
		{name: "no target stays unset for the policy to route", wantTarget: ""},
		{name: "server is left alone", target: "server", wantTarget: "server"},
		{name: "an explicit non-local target with a runner is left alone", target: "service_account", runner: "gd-r1", wantTarget: "service_account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, owner, implied, err := resolveEscalationLocalTarget(c.target, c.runner, c.owner)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if !strings.Contains(err.Error(), "--target-runner") || !strings.Contains(err.Error(), "Leave --execution-target unset") {
					t.Errorf("refusal does not say what to file instead: %v", err)
				}
				return
			}
			if target != c.wantTarget || owner != c.wantOwner || implied != c.wantImplied {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", target, owner, implied, c.wantTarget, c.wantOwner, c.wantImplied)
			}
		})
	}
}

// The refusal happens in gb: no create reaches the daemon. Positive control:
// the same filing with --target-runner and no --execution-target is created
// as execution_target=local.
func TestEscalationCreate_LocalTargetNeedsRunner(t *testing.T) {
	var creates int
	var fields map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			creates++
			var req struct {
				Fields map[string]any `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			fields = req.Fields
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-local-target"})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	t.Cleanup(srv.Close)
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := escalationCreateCmd
	cmd.SetContext(t.Context())
	set := func(kv map[string]string) {
		for k, v := range kv {
			cmd.Flags().Set(k, v)
		}
	}
	reset := map[string]string{"title": "", "operation-type": "", "execution-target": "", "target-runner": ""}
	t.Cleanup(func() { set(reset) })

	set(map[string]string{"title": "Open the browser session", "operation-type": "manual_operator_action", "execution-target": "local"})
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "--target-runner") {
		t.Fatalf("local with no runner: err = %v, want the refusal", err)
	}
	if creates != 0 {
		t.Fatalf("a refused filing reached the daemon %d time(s)", creates)
	}

	set(map[string]string{"execution-target": "", "target-runner": "gd-r1"})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("with --target-runner: %v", err)
	}
	if creates != 1 || fields["execution_target"] != "local" || fields["target_runner"] != "gd-r1" {
		t.Errorf("creates = %d, fields = %v; want one local create naming gd-r1", creates, fields)
	}
}
