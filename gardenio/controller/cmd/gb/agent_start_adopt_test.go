package main

import (
	"strings"
	"testing"

	"gardenio/clicreds/oidclogin"
)

// gb agent start --local --agent-id (gl agent start, gd-6yW5zvZSbz): run as a
// land agent the land registered, on its own token, never its person's login.

func TestCheckAdoptedAgent(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		local     bool
		named     bool
		noSession string
		want      string // "" = admitted; else a substring of the error
	}{
		{name: "no agent id", local: true},
		{name: "admitted", id: "or-Agt7kL2mNp", local: true, named: true, noSession: "1"},
		{name: "docker", id: "or-Agt7kL2mNp", named: true, noSession: "1", want: "only supported with --local"},
		{name: "no name", id: "or-Agt7kL2mNp", local: true, noSession: "1", want: "needs --name"},
		{name: "login session allowed", id: "or-Agt7kL2mNp", local: true, named: true, want: oidclogin.NoSessionEnv + "=1"},
		{name: "login session not 1", id: "or-Agt7kL2mNp", local: true, named: true, noSession: "true", want: oidclogin.NoSessionEnv + "=1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkAdoptedAgent(c.id, c.local, c.named, c.noSession)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("err = %v, want admitted", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one naming %q", err, c.want)
			}
		})
	}
}

func TestAdoptedAgentCreds(t *testing.T) {
	c, err := adoptedAgentCreds("https://api.example.com/land/orchard", "", "agent-tok")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPURL != "https://api.example.com/land/orchard" || c.AuthToken != "agent-tok" || c.OwnerLogin {
		t.Fatalf("creds = %+v", c)
	}
	if c, _ := adoptedAgentCreds("https://api.example.com/land/orchard", "flag-tok", "agent-tok"); c.AuthToken != "flag-tok" {
		t.Errorf("--token does not win over the env: %+v", c)
	}
	if _, err := adoptedAgentCreds("https://api.example.com/land/orchard", "", ""); err == nil {
		t.Error("no token admitted; an agent must not start on another credential")
	}
	if _, err := adoptedAgentCreds("", "", "agent-tok"); err == nil {
		t.Error("no daemon URL admitted")
	}
}

// The agent's kd and gb inherit the no-login switch and get the agent's token
// under every name they read.
func TestLocalAgentEnv_AdoptedAgentKeepsNoLoginSession(t *testing.T) {
	t.Setenv(oidclogin.NoSessionEnv, "1")
	t.Setenv("BEADS_AUTH_TOKEN", "person-tok")
	creds, err := adoptedAgentCreds("https://api.example.com/land/orchard", "", "agent-tok")
	if err != nil {
		t.Fatal(err)
	}
	env := localAgentEnv("studio-mm4k", "or-Agt7kL2mNp", "lands-gardener", "/tmp", "http://127.0.0.1:8080", "", creds)
	if got := findEnv(env, oidclogin.NoSessionEnv); got != "1" {
		t.Errorf("%s = %q, want 1", oidclogin.NoSessionEnv, got)
	}
	for _, k := range []string{"BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"} {
		if got := findEnv(env, k); got != "agent-tok" {
			t.Errorf("%s = %q, want the agent's token", k, got)
		}
	}
	if got := findEnv(env, "KD_AGENT_ID"); got != "or-Agt7kL2mNp" {
		t.Errorf("KD_AGENT_ID = %q", got)
	}
}
