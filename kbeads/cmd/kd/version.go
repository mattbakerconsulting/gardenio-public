package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gardenio/kbeads/internal/buildinfo"
	"github.com/spf13/cobra"
)

// warnOnVersionSkew prints a one-line stderr warning when the daemon reports a
// build materially newer than this binary. Wired as the HTTP client's
// once-only daemon-version callback (ka-0000002965). Suppressed under
// BEADS_NO_VERSION_WARN / --json so it never corrupts machine-readable output.
func warnOnVersionSkew(daemonVersion string) {
	if os.Getenv("BEADS_NO_VERSION_WARN") != "" || jsonOutput {
		return
	}
	if msg, skewed := buildinfo.SkewCheck(daemonVersion); skewed {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// Build-time version vars. These match the ldflag targets the kd build paths
// already set (`-X main.Version=… -X main.Build=… -X main.Commit=…` in
// the kbeads Dockerfile and the CI build). They are seeded
// into the buildinfo package in init() so buildinfo stays the single source of
// truth for both `kd version` and the daemon's version header (ka-0000002965).
// Build is the older alias for the commit sha; Commit wins when both are set.
var (
	Version = ""
	Build   = ""
	Commit  = ""
)

func init() {
	commit := Commit
	if commit == "" {
		commit = Build
	}
	buildinfo.Seed(Version, commit)

	// Wire `kd --version` off the same buildinfo source. cobra prints the
	// Version string for a bare --version with no subcommand.
	info := buildinfo.Get()
	rootCmd.Version = info.Version
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:     "version",
	Short:   "Print kd build version (commit, branch, build date)",
	GroupID: "system",
	// No server round-trip: this must work offline and against any daemon.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { cmd.SilenceUsage = true; return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		info := buildinfo.Get()
		if jsonOutput {
			data, err := json.MarshalIndent(info, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}
		fmt.Print(info.String())
		return nil
	},
}
