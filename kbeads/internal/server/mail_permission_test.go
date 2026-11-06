package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The slack bridge answers a Claude Code tool-approval prompt with a
// control:permission mail (bridge MailPermissionVerdict) carrying request_id
// and behavior. type:mail did not declare them, so every Slack Allow/Deny
// create 400'd "unknown field" (PR #1705 send-back). The bridge tests use a
// fake daemon that accepts any field, so these run the exact create through
// the real createBead: field validation and the control-mail guard together.

const (
	wireMailPermissionRequestID = "request_id"
	wireMailPermissionBehavior  = "behavior"
)

// permissionVerdictInput is the create MailPermissionVerdict sends to
// local-laptop's agent.
func permissionVerdictInput(t *testing.T) createBeadInput {
	t.Helper()
	fields, err := json.Marshal(map[string]string{
		wireMailPermissionRequestID: "req-42",
		wireMailPermissionBehavior:  "allow",
	})
	if err != nil {
		t.Fatal(err)
	}
	return createBeadInput{
		Title:       "Permission req-42: allow",
		Type:        "mail",
		Kind:        "data",
		Description: "Permission decision from Slack for request req-42: allow",
		Assignee:    "local-laptop",
		Labels:      []string{"from:slack-bridge", "control:permission"},
		CreatedBy:   "slack-bridge",
		Fields:      fields,
	}
}

func TestMailPermission_FieldsDeclaredOnBuiltin(t *testing.T) {
	tc := parseBuiltinSchema(t, "mail")
	for _, name := range []string{wireMailPermissionRequestID, wireMailPermissionBehavior} {
		f := fieldByName(tc, name)
		if f == nil {
			t.Fatalf("type:mail does not declare %q: every Slack permission verdict 400s \"unknown field\"", name)
		}
		if f.Type != model.FieldTypeString {
			t.Errorf("type:mail field %q type=%q, want %q", name, f.Type, model.FieldTypeString)
		}
	}
}

// The slack bridge's exact create lands with both fields; the runner that
// owns the recipient and a per-agent token are refused, so no agent can
// answer its own prompts.
func TestMailPermission_CreateBead_BridgeAllowedRunnerAndAgentRefused(t *testing.T) {
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ms.beadsMu.Lock()
	ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: "open", Fields: []byte(`{"agent":"local-laptop","runtime_id":"gd-runner1"}`)}
	ms.beadsMu.Unlock()

	bridge := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, "slack-bridge")
	got, err := s.createBead(bridge, permissionVerdictInput(t))
	if err != nil {
		t.Fatalf("slack bridge's control:permission mail refused: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		t.Fatal(err)
	}
	if stored[wireMailPermissionRequestID] != "req-42" || stored[wireMailPermissionBehavior] != "allow" {
		t.Errorf("stored fields = %v, want request_id=req-42 behavior=allow", stored)
	}

	// Built as runnerOwnerWriteContext builds it: owner stamped as the
	// operator. The runner may still send its own agent control:kill, so the
	// refusal below is the permission guard's and not attribution's.
	runner := WithOperatedByHeader(WithRunnerID(ctxWithIdentity(AuthIdentity{Type: "runner", Subject: "oidc:owner@example.com"}, ""), "gd-runner1"), "oidc:owner@example.com")
	kill := permissionVerdictInput(t)
	kill.Labels, kill.Fields = []string{"from:local-runner", "control:kill"}, nil
	if _, err := s.createBead(runner, kill); err != nil {
		t.Fatalf("runner's control:kill to its own agent refused: %v", err)
	}

	for name, ctx := range map[string]context.Context{
		"runner owning the recipient": runner,
		"recipient's own agent token": ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "local-laptop"}, "agents"),
	} {
		_, err := s.createBead(ctx, permissionVerdictInput(t))
		var denial *AuthzDenial
		if !errors.As(err, &denial) {
			t.Errorf("%s: control:permission must be refused with AuthzDenial, got %v", name, err)
		}
	}
}

// TestMailPermission_MatchesMailprotoConstants fails if the controller renames
// mailproto.FieldPermissionRequestID or FieldPermissionBehavior without the
// schema following.
func TestMailPermission_MatchesMailprotoConstants(t *testing.T) {
	abs, err := filepath.Abs(mailprotoSrcRelPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		t.Skipf("gardenio sibling source %s not checked out", mailprotoSrcRelPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	for constName, want := range map[string]string{
		"FieldPermissionRequestID": wireMailPermissionRequestID,
		"FieldPermissionBehavior":  wireMailPermissionBehavior,
	} {
		match := regexp.MustCompile(constName + `\s*=\s*"([^"]*)"`).FindSubmatch(src)
		if match == nil {
			t.Fatalf("%s no longer declares %s; update this guard and the type:mail schema together", mailprotoSrcRelPath, constName)
		}
		if got := string(match[1]); got != want {
			t.Errorf("mailproto.%s = %q, want %q: the type:mail schema no longer declares the wire name", constName, got, want)
		}
	}
}
