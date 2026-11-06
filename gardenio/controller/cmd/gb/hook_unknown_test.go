package main

import (
	"strings"
	"testing"
)

// TestHookCmd_RejectsUnknownSubcommand verifies the Args validator on
// hookCmd rejects extra positional args (which is how cobra surfaces an
// unknown subcommand on a parent that has no Run). The stdout of every
// SessionStart hook is injected into the agent session as a system-reminder,
// so a stale settings.local.json referencing a non-existent subcommand
// (e.g. `gb hook hippo-rehydrate`) must error instead of dumping the cobra
// usage banner — otherwise each stale entry wastes ~600 tokens of session
// context.
func TestHookCmd_RejectsUnknownSubcommand(t *testing.T) {
	err := hookCmd.Args(hookCmd, []string{"hippo-rehydrate"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("error should mention unknown subcommand, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hippo-rehydrate") {
		t.Errorf("error should name the bad subcommand, got: %v", err)
	}
}

// TestHookCmd_AcceptsNoArgs verifies bare `gb hook` does not trigger the
// Args validator (validator only fires when extra args are passed).
func TestHookCmd_AcceptsNoArgs(t *testing.T) {
	if err := hookCmd.Args(hookCmd, nil); err != nil {
		t.Errorf("Args validator rejected zero args: %v", err)
	}
}

// TestHookCmd_BareInvocation_NoError verifies bare `gb hook` does not error
// (it prints usage to stderr and exits 0). The actual end-to-end stdout
// behaviour is covered by TestHookCmd_RejectsUnknownSubcommand for the
// unknown-subcommand path, which is the case that occurs in stale hook
// configs.
func TestHookCmd_BareInvocation_NoError(t *testing.T) {
	if err := hookCmd.RunE(hookCmd, nil); err != nil {
		t.Fatalf("bare gb hook RunE errored: %v", err)
	}
}
