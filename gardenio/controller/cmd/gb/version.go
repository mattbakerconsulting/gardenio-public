package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gardenio/controller/internal/buildinfo"
	"github.com/spf13/cobra"
)

// Build-time version vars. These match the ldflag targets the gardenio build
// files already set (`-X main.version=… -X main.commit=…` in gardenio/Makefile,
// controller/Makefile, and the Dockerfiles). Before this file, gb declared no
// such vars, so those -X flags were silently dropped by the linker and
// `gb version` did not exist (ka-0000002965). They are seeded into buildinfo in
// init() so buildinfo is the single source of truth for `gb version`,
// `gb --version`, and the skew signal.
var (
	version = ""
	commit  = ""
)

func init() {
	buildinfo.Seed(version, commit)

	// Wire `gb --version` off the same buildinfo source. cobra prints the
	// Version string for a bare --version with no subcommand.
	info := buildinfo.Get()
	rootCmd.Version = info.Version
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:     "version",
	Short:   "Print gb build version (commit, branch, build date)",
	GroupID: "session",
	// No daemon round-trip: version must work offline and against any daemon.
	// Override the root PersistentPreRunE (which dials the daemon) with a
	// no-op that still silences the usage dump on error.
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

// warnOnVersionSkew prints a one-line stderr warning when the daemon reports a
// build materially newer than this binary. Wired as the beadsapi client's
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
