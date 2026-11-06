package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRootSilencesUsageOnRuntimeError is the regression guard for bug
// ka-0000003634 / fix ka-0000001023: the gb root command's PersistentPreRunE
// must set SilenceUsage on the running command so that RunE failures — the
// `gb stop` decision-gate refusal, any server-side 4xx — print only their
// one-line cause, NOT cobra's ~25-line usage/flags dump that buries it.
// Genuine flag-parse / arg-count errors fire EARLIER (cobra runs ParseFlags +
// ValidateArgs before invoking PersistentPreRunE), so those still surface the
// usage block — covered by TestRootShowsUsageOnParseError below.
func TestRootSilencesUsageOnRuntimeError(t *testing.T) {
	if rootCmd.PersistentPreRunE == nil {
		t.Fatal("gb rootCmd has no PersistentPreRunE — SilenceUsage cannot be set per-run")
	}

	// The hook sets the package-global daemon client as a side effect; save and
	// restore it so this test does not leak a client into sibling tests.
	saved := daemon
	t.Cleanup(func() { daemon = saved })

	probe := &cobra.Command{Use: "probe"}
	if probe.SilenceUsage {
		t.Fatal("probe command unexpectedly starts with SilenceUsage=true")
	}

	// Ignore the returned error: operated-by validation / client resolution may
	// fail in a bare test env, but SilenceUsage must be set UNCONDITIONALLY as
	// the first action of the hook, before any fallible work. That ordering is
	// the whole fix.
	_ = rootCmd.PersistentPreRunE(probe, nil)

	if !probe.SilenceUsage {
		t.Error("gb root PersistentPreRunE did not set SilenceUsage=true; runtime/RPC errors will dump the cobra usage block and bury the cause")
	}
}

// TestRootShowsUsageOnParseError asserts the complementary half of the
// contract: a genuine flag-parse error on the gb root still produces cobra's
// usage block, because ParseFlags fails before PersistentPreRunE can silence
// usage. This keeps `gb --nope` helpful while RunE/RPC failures stay quiet.
func TestRootShowsUsageOnParseError(t *testing.T) {
	saved := daemon
	t.Cleanup(func() { daemon = saved })

	// Build a throwaway command tree that mirrors the root's PersistentPreRunE
	// wiring so we exercise cobra's real error→usage path without dialing the
	// daemon or invoking the network-touching RunE on the production rootCmd.
	root := &cobra.Command{
		Use:               "gb",
		PersistentPreRunE: rootCmd.PersistentPreRunE,
		RunE:              func(cmd *cobra.Command, args []string) error { return nil },
	}
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--nope"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for an unknown flag, got nil")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage block for a flag-parse error, got:\n%s", out.String())
	}
}
