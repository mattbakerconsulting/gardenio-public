package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestResolveGardenioHost verifies the env-aware default-host resolution:
// $GARDENIO_DEFAULT_HOST overrides the build-time default, schemes/slashes are
// stripped, and there is no compiled-in zero-config fallback.
func TestResolveGardenioHost(t *testing.T) {
	t.Setenv("GARDENIO_DEFAULT_HOST", "")
	if got := resolveGardenioHost(); got != "" {
		t.Errorf("zero-config: got %q, want empty (no compiled default)", got)
	}
	t.Setenv("GARDENIO_DEFAULT_HOST", "gardenio.poc.example")
	if got := resolveGardenioHost(); got != "gardenio.poc.example" {
		t.Errorf("env override: got %q", got)
	}
	// Tolerate a scheme prefix and trailing slash in the override.
	t.Setenv("GARDENIO_DEFAULT_HOST", "https://gardenio.poc.example/")
	if got := resolveGardenioHost(); got != "gardenio.poc.example" {
		t.Errorf("scheme/slash strip: got %q", got)
	}
}

// TestDefaultHTTPURLEmptyWithoutConfig verifies there is no compiled-in daemon
// host: with no env and no override, defaultHTTPURL is "" and the root command
// refuses to run (spec D2, public-ready scrub). GARDENIO_DEFAULT_HOST still works.
func TestDefaultHTTPURLEmptyWithoutConfig(t *testing.T) {
	t.Setenv("BEADS_HTTP_URL", "")
	t.Setenv("BEADS_HTTP_ADDR", "")
	t.Setenv("GARDENIO_DEFAULT_HOST", "")
	if got := defaultHTTPURL(); got != "" {
		t.Errorf("defaultHTTPURL() = %q, want empty with no configuration", got)
	}
	t.Setenv("GARDENIO_DEFAULT_HOST", "gardenio.example.com")
	if got := daemonURLForHost(resolveGardenioHost()); got != "https://gardenio.example.com/kd" {
		t.Errorf("daemonURLForHost(env host) = %q", got)
	}
	if got := defaultHTTPURL(); got != "https://gardenio.example.com/kd" {
		t.Errorf("defaultHTTPURL() with env host = %q", got)
	}
}

// TestDefaultGardenioHostIsEmpty replaces the split-brain regression lock
// (ka-0000001598): the stronger guarantee is that an unconfigured client runs
// NOTHING, rather than targeting a hostname that may be dead or wrong.
func TestDefaultGardenioHostIsEmpty(t *testing.T) {
	if defaultGardenioHost != "" {
		t.Errorf("compiled default host = %q, want empty (no dead-host default)", defaultGardenioHost)
	}
	if strings.Contains(defaultGardenioHost, "e2e") {
		t.Errorf("compiled default host %q must not point at the legacy e2e daemon", defaultGardenioHost)
	}
}

// TestRequireDaemonURL pins which commands may run unconfigured.
func TestRequireDaemonURL(t *testing.T) {
	for _, name := range []string{"version", "help", "completion"} {
		cmd := &cobra.Command{Use: name}
		if err := requireDaemonURL(cmd, ""); err != nil {
			t.Errorf("%s with empty url: unexpected error %v", name, err)
		}
	}
	// `completion bash` is named "bash"; its parent exempts it.
	completion := &cobra.Command{Use: "completion"}
	bash := &cobra.Command{Use: "bash"}
	completion.AddCommand(bash)
	if err := requireDaemonURL(bash, ""); err != nil {
		t.Errorf("completion bash with empty url: unexpected error %v", err)
	}
	// A bare group only prints its help.
	group := &cobra.Command{Use: "admin"}
	group.AddCommand(&cobra.Command{Use: "child"})
	if err := requireDaemonURL(group, ""); err != nil {
		t.Errorf("bare group with empty url: unexpected error %v", err)
	}
	// `kd migrate` runs in the pre-upgrade Job with only BEADS_DATABASE_URL set:
	// a refusal there fails every helm upgrade (2026.304.2, 2026-10-31).
	if err := requireDaemonURL(migrateCmd, ""); err != nil {
		t.Errorf("migrate with empty url: unexpected error %v", err)
	}
	annotated := &cobra.Command{Use: "parent", Annotations: map[string]string{noDaemonAnnotation: "true"}}
	annotatedChild := &cobra.Command{Use: "child"}
	annotated.AddCommand(annotatedChild)
	if err := requireDaemonURL(annotatedChild, ""); err != nil {
		t.Errorf("child of a no-daemon command with empty url: unexpected error %v", err)
	}
	// The exemption is the annotation, not the name: `kd admin migrate ...` still dials.
	adminMig := &cobra.Command{Use: "migrate"}
	adminMigChild := &cobra.Command{Use: "advice-targeting"}
	adminMig.AddCommand(adminMigChild)
	if err := requireDaemonURL(adminMigChild, ""); err == nil {
		t.Errorf("admin migrate child with empty url: want refusal, got nil")
	}
	cmd := &cobra.Command{Use: "list"}
	err := requireDaemonURL(cmd, "")
	want := "no gardenio daemon URL configured: set BEADS_HTTP_URL (for example https://gardenio.example.com/kd) or pass --http-url"
	if err == nil || err.Error() != want {
		t.Errorf("list with empty url: got %v, want %q", err, want)
	}
	if err := requireDaemonURL(cmd, "http://127.0.0.1:8080"); err != nil {
		t.Errorf("list with url: unexpected error %v", err)
	}
}
