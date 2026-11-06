package main

import (
	"context"
	"testing"
)

// Tests for legacy config:auth compat. The e2e fixtures (and unmigrated
// deployments) label projects on a config:auth bead rather than on account
// beads: accounts chain via the `fallback` field but carry no `project:`
// label. The failover path must still discover the chain from the config:auth
// bead, but must not write it back: the controller's machine principal has no
// config:auth grant and the in-memory runtime is the failover source of truth.

// accountNoProject constructs an account bead with no project: label —
// matches the e2e fixture pattern where the project lives on a config:auth
// bead instead.
func accountNoProject(id, title string, fields map[string]any) *testBead {
	if fields == nil {
		fields = map[string]any{}
	}
	return &testBead{
		ID: id, Title: title, Type: "account", Status: "open",
		Fields: fields,
	}
}

// authConfig constructs a legacy config:auth bead labeled with a project and
// a description pointing to the given account name.
func authConfig(id, project, account string) *testBead {
	return &testBead{
		ID: id, Title: "auth", Type: "config", Status: "open",
		Labels:      []string{"project:" + project},
		Fields:      map[string]any{},
		Description: `{"account":"` + account + `"}`,
	}
}

// TestAuthFailover_LegacyConfigAuth_DoesNotWriteDescription verifies that
// authFailover.Handle discovers the chain through the config:auth bead when
// the account beads lack project labels, and leaves the config:auth
// description untouched.
func TestAuthFailover_LegacyConfigAuth_DoesNotWriteDescription(t *testing.T) {
	store := newMockStore(
		authConfig("gd-cfg", "legacy-proj", "acct-a"),
		accountNoProject("gd-a", "acct-a", map[string]any{
			"provider": "anthropic", "fallback": "acct-b", "status": "active",
			"throttle_status": "allowed",
		}),
		accountNoProject("gd-b", "acct-b", map[string]any{
			"provider": "anthropic", "status": "active",
			"throttle_status": "allowed",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "legacy-proj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	state, ok := f.runtime.Get("legacy-proj")
	if !ok {
		t.Fatal("expected runtime state for legacy-proj")
	}
	if state.Primary != "acct-a" {
		t.Errorf("Primary = %q, want acct-a", state.Primary)
	}
	if state.Current != "acct-b" {
		t.Errorf("Current = %q, want acct-b", state.Current)
	}

	// The controller no longer writes the legacy bead (decision gd-7oRiFouRVY).
	if got := store.description("gd-cfg"); got != `{"account":"acct-a"}` {
		t.Errorf("config:auth description = %q, want it unchanged", got)
	}
}

// TestProbeEventListener_FindProjectsForAccount_LegacyConfig verifies the
// probe listener's findProjectsForAccount finds projects whose serving
// account is defined on a legacy config:auth bead instead of project-labeled
// account beads.
func TestProbeEventListener_FindProjectsForAccount_LegacyConfig(t *testing.T) {
	store := newMockStore(
		authConfig("gd-cfg1", "legacy-proj", "acct-a"),
		authConfig("gd-cfg2", "other-proj", "acct-z"),
		accountNoProject("gd-a", "acct-a", map[string]any{"fallback": "acct-b"}),
		accountNoProject("gd-b", "acct-b", map[string]any{}),
		accountNoProject("gd-z", "acct-z", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	client := newClient(t, srv)
	f := newAuthFailover(client, nil, testFailoverLogger(), 0, 0)
	l := newProbeEventListener(client, f, testFailoverLogger())

	projects, err := l.findProjectsForAccount(context.Background(), "acct-a")
	if err != nil {
		t.Fatalf("findProjectsForAccount: %v", err)
	}
	if len(projects) != 1 || projects[0] != "legacy-proj" {
		t.Errorf("findProjectsForAccount(acct-a) = %v, want [legacy-proj]", projects)
	}

	// Unrelated account should not match this project.
	projects, err = l.findProjectsForAccount(context.Background(), "acct-b")
	if err != nil {
		t.Fatalf("findProjectsForAccount: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("findProjectsForAccount(acct-b) = %v, want [] (acct-b is a fallback, not the serving account)", projects)
	}
}
