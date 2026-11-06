package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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

// TestViewerURLForHost: the viewer fallback is empty with no configured host.
func TestViewerURLForHost(t *testing.T) {
	if got := viewerURLForHost(""); got != "" {
		t.Errorf("viewerURLForHost(\"\") = %q, want empty", got)
	}
	if got := viewerURLForHost("gardenio.example.com"); got != "https://gardenio.example.com/beads" {
		t.Errorf("viewerURLForHost(host) = %q", got)
	}
}
