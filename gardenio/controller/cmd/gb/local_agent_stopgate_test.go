package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/controller/internal/beadsapi"
)

// localAgentTokenEnv clears every token variable defaultToken reads and sets
// BEADS_AUTH_TOKEN to tok, as localAgentEnv exports it.
func localAgentTokenEnv(t *testing.T, tok string) {
	t.Helper()
	for _, k := range []string{tokenFileEnv, "GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_DAEMON_TOKEN", oidclogin.NoSessionEnv} {
		t.Setenv(k, "")
	}
	t.Setenv("BEADS_AUTH_TOKEN", tok)
}

// A local agent session's gb (the stop-gate hook included) sends the agent's
// own token, even with the owner signed in for the same daemon, and prints
// no session warning when the owner is signed in for another one
// (gd-17BiVe2oC6).
func TestResolveBearer_LocalAgentSessionUsesOwnToken(t *testing.T) {
	useTempGBLoginFile(t)
	if err := oidclogin.Save(&oidclogin.Session{Daemon: testGBDaemonHost, AccessToken: "owner-session", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	oldURL, oldTok := httpURL, tokenFlag
	t.Cleanup(func() { httpURL, tokenFlag = oldURL, oldTok })
	tokenFlag = ""
	localAgentTokenEnv(t, "agent-token")
	t.Setenv(localAgentStartPIDEnv, "4242")

	for _, url := range []string{testGBDaemonURL, "http://127.0.0.1:8080"} {
		httpURL = url
		var warn bytes.Buffer
		if got := resolveBearer(context.Background(), &warn); got != "agent-token" {
			t.Errorf("%s: bearer = %q, want the agent's own token", url, got)
		}
		if warn.Len() != 0 {
			t.Errorf("%s: warned %q", url, warn.String())
		}
	}
}

// A local agent started on the owner's login exports no token and keeps
// acting on that session (spec D6); outside a local agent session the login
// session still comes before the environment's token.
func TestResolveBearer_OwnerLoginPrecedenceKept(t *testing.T) {
	useTempGBLoginFile(t)
	if err := oidclogin.Save(&oidclogin.Session{Daemon: testGBDaemonHost, AccessToken: "owner-session", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	oldURL, oldTok := httpURL, tokenFlag
	t.Cleanup(func() { httpURL, tokenFlag = oldURL, oldTok })
	httpURL, tokenFlag = testGBDaemonURL, ""

	localAgentTokenEnv(t, "")
	t.Setenv(localAgentStartPIDEnv, "4242")
	if got := resolveBearer(context.Background(), &bytes.Buffer{}); got != "owner-session" {
		t.Errorf("owner-login local agent: bearer = %q, want the owner session", got)
	}

	localAgentTokenEnv(t, "env-token")
	t.Setenv(localAgentStartPIDEnv, "")
	if got := resolveBearer(context.Background(), &bytes.Buffer{}); got != "owner-session" {
		t.Errorf("outside a local agent: bearer = %q, want the owner session", got)
	}
}

// gateReleaseDaemon serves the escalation bead and records the decision-gate
// writes releaseExecutorStopGate makes on agent gd-agent.
func gateReleaseDaemon(t *testing.T, escFields string) (satisfied *bool, satisfiedBy *string, mu *sync.Mutex) {
	t.Helper()
	satisfied, satisfiedBy, mu = new(bool), new(string), &sync.Mutex{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-esc":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-esc", "type": "escalation", "fields": json.RawMessage(escFields)})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/gd-agent/gates/decision/satisfy":
			mu.Lock()
			*satisfied = true
			mu.Unlock()
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-agent":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent", "fields": json.RawMessage(`{}`)})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/gd-agent":
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*satisfiedBy, _ = body.Fields["gate_satisfied_by"].(string)
			mu.Unlock()
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = old })
	return satisfied, satisfiedBy, mu
}

// The local agent an escalation was delivered to may end the turn that
// closed it without filing a decision; any other closer keeps the gate
// (gd-17BiVe2oC6).
func TestReleaseExecutorStopGate(t *testing.T) {
	cases := []struct {
		name      string
		startPID  string
		agent     string
		escFields string
		want      bool
	}{
		{"executor in its local session", "4242", "laptop-agent", `{"prompt_delivered_to":"laptop-agent"}`, true},
		{"not a local agent session", "", "laptop-agent", `{"prompt_delivered_to":"laptop-agent"}`, false},
		{"delivered to another agent", "4242", "laptop-agent", `{"prompt_delivered_to":"other-agent"}`, false},
		{"never delivered", "4242", "laptop-agent", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			satisfied, by, mu := gateReleaseDaemon(t, tc.escFields)
			t.Setenv(localAgentStartPIDEnv, tc.startPID)
			t.Setenv("KD_AGENT_ID", "gd-agent")
			t.Setenv("BOAT_AGENT", tc.agent)

			releaseExecutorStopGate(context.Background(), "gd-esc")

			mu.Lock()
			defer mu.Unlock()
			if *satisfied != tc.want {
				t.Errorf("gate satisfied = %v, want %v", *satisfied, tc.want)
			}
			if tc.want && *by != "yield" {
				t.Errorf("gate_satisfied_by = %q, want yield (consumed by the next Stop)", *by)
			}
		})
	}
}
