package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/bridge"
)

// mr_security.go wires the security bar (bundle ka-0000005053 §2.3; task
// ka-0000007861) into the `gb mr` command group:
//
//   gb mr security-gate <bead-id-or-mr-url>   — the aggregate gate: named
//       security status checks green + a well-formed Security-Scan: block whose
//       recorded scanner versions match gardenio/locks/security-tools.lock + (if the diff
//       touches sensitive paths) a high-effort /security-review triaged-to-zero.
//       Exit 0 = green, exit 3 = blocked, mirroring gb mr relevance-gate.
//
//   gb mr security-tier <bead-id-or-mr-url>   — classify the two-tier
//       /security-review effort (low everywhere, high on auth/secret/crypto/
//       deser/dependency path globs) from the MR's changed files. Prints the
//       effort + the sensitive paths that triggered high. Exit 0 always (advice,
//       not a gate); --json for machine output.
//
// Both resolve their arg via resolveMRURL and talk to GitLab through the shared
// newMRGitLabClient, exactly like the sibling gate commands in mr.go. The pure
// decisions live in internal/bridge/security_gate.go and are table-tested there.

var (
	mrSecurityLockPath string
	mrSecurityTierOnly bool
)

var mrSecurityGateCmd = &cobra.Command{
	Use:   "security-gate <bead-id-or-mr-url>",
	Short: "Evaluate the aggregate security bar for an MR (status checks + Security-Scan block + lock match)",
	Long: `Aggregate security gate for a dark MR (bundle ka-0000005053 §2.3).

Reports GREEN iff ALL of:
  - the named security status checks are success:
      CI / Gitleaks, CI / Go-Vuln, CI / Cargo-Audit, CI / Cargo-Deny, CI / OSV-Scan
  - the MR description carries a machine-parseable ` + "`Security-Scan:`" + ` block
  - every scanner version recorded in that block MATCHES the pin in
    gardenio/locks/security-tools.lock (version drift = block; an MR cannot claim a scan it
    ran against a stale/unpinned scanner)
  - if the MR touches a sensitive path glob (auth/secret/crypto/deser/dependency
    manifests), the block records ` + "`security-review=high=0-findings`" + `

The gate does NOT re-run scanners — it AGGREGATES their recorded verdicts (the
externally posted commit statuses + the Security-Scan block), the way the coverage-patch
gate aggregates patch-cov-result.json. The reachability-diff itself runs in CI
(scripts/security/govulncheck-diff.py) and is surfaced here as CI / Go-Vuln.

Exit 0 = green (may merge); exit 3 = blocked (a human/agent must resolve the
listed reasons). --json for machine-readable output.`,
	Args: cobra.ExactArgs(1),
	RunE: runMRSecurityGate,
}

var mrSecurityTierCmd = &cobra.Command{
	Use:   "security-tier <bead-id-or-mr-url>",
	Short: "Classify the two-tier /security-review effort (low/high) from an MR's changed files",
	Long: `Classifies the /security-review effort for an MR's diff (bundle
ka-0000005053 §2.3): effort=low on every MR, escalated to effort=high when any
changed file matches a sensitive path glob (auth/token/credential/crypto/
deserialization code, .gitleaks.toml, securityContext templates, or any
dependency manifest/lockfile: go.mod/go.sum, Cargo.toml/Cargo.lock, bun.lock,
package.json/lock).

Prints the classified effort and the sensitive paths that triggered a high
classification. Advisory (always exit 0) — it tells the agent which effort to run
` + "`/security-review`" + ` at; the security-gate is what enforces that a high-tier diff
actually recorded a high-effort review. --json for machine output.`,
	Args: cobra.ExactArgs(1),
	RunE: runMRSecurityTier,
}

func init() {
	mrSecurityGateCmd.Flags().StringVar(&mrSecurityLockPath, "lock", "gardenio/locks/security-tools.lock",
		"Path to the security-tools.lock pinning expected scanner versions")
	mrSecurityGateCmd.Flags().BoolVar(&mrSecurityTierOnly, "skip-tier", false,
		"Do not fetch the MR diff / classify tier (treat the diff as low-tier)")
	mrCmd.AddCommand(mrSecurityGateCmd)
	mrCmd.AddCommand(mrSecurityTierCmd)
}

// loadSecurityLock reads and parses the security-tools.lock at the given path.
func loadSecurityLock(path string) (map[string]string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // operator-supplied lock path
	if err != nil {
		return nil, fmt.Errorf("reading security-tools.lock at %s: %w (pass --lock <path>)", path, err)
	}
	pins := bridge.ParseSecurityToolsLock(string(content))
	if len(pins) == 0 {
		return nil, fmt.Errorf("no scanner version pins found in %s", path)
	}
	return pins, nil
}

func runMRSecurityGate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	lockPins, err := loadSecurityLock(mrSecurityLockPath)
	if err != nil {
		return err
	}

	mr, err := client.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
	}

	// Named security status checks — resolve each name to its latest verdict
	// from the head SHA's commit statuses (an external CI posts these as commit
	// statuses; see ListCommitStatusesByPath's doc). Latest-wins per name.
	statusChecks := map[string]string{}
	if mr.SHA != "" {
		statuses, sErr := client.ListCommitStatusesByPath(ctx, ref.ProjectPath, mr.SHA)
		if sErr != nil {
			return fmt.Errorf("listing commit statuses for %s: %w", mr.SHA, sErr)
		}
		for _, s := range statuses {
			statusChecks[s.Name] = s.Status
		}
	}

	// Security-Scan: block from the MR description.
	scanBlock := bridge.ParseSecurityScanBlock(mr.Description)

	// Two-tier classification from the changed files (unless suppressed).
	tier := bridge.SecurityReviewTier{Effort: "low"}
	if !mrSecurityTierOnly {
		files, fErr := client.ListMergeRequestChangedFiles(ctx, ref.ProjectPath, ref.IID)
		if fErr != nil {
			return fmt.Errorf("fetching MR changed files: %w", fErr)
		}
		tier = bridge.ClassifySecurityReviewEffort(files)
	}

	decision := bridge.EvaluateSecurityGate(bridge.SecurityGateInput{
		StatusChecks: statusChecks,
		ScanBlock:    scanBlock,
		LockPins:     lockPins,
		Tier:         tier,
	})

	if jsonOutput {
		printJSON(map[string]any{
			"mr_iid":          mr.IID,
			"head_sha":        mr.SHA,
			"pass":            decision.Pass,
			"reasons":         decision.Reasons,
			"review_effort":   tier.Effort,
			"sensitive_paths": tier.SensitivePaths,
			"scan_present":    scanBlock.Present,
		})
	} else if decision.Pass {
		cmd.Printf("GREEN — security gate passes for !%d (review tier: %s)\n", mr.IID, tier.Effort)
	} else {
		cmd.Printf("BLOCKED — security gate fails for !%d (review tier: %s):\n", mr.IID, tier.Effort)
		for _, r := range decision.Reasons {
			cmd.Printf("  - %s\n", r)
		}
	}

	if !decision.Pass {
		// Distinct non-zero exit (3) so a caller's shell branches on "blocked"
		// vs. a real error (1), matching gb mr relevance-gate / dispatch-gate.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

func runMRSecurityTier(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	files, err := client.ListMergeRequestChangedFiles(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR changed files: %w", err)
	}
	tier := bridge.ClassifySecurityReviewEffort(files)

	if jsonOutput {
		printJSON(map[string]any{
			"mr_iid":          ref.IID,
			"review_effort":   tier.Effort,
			"sensitive_paths": tier.SensitivePaths,
			"changed_files":   len(files),
		})
		return nil
	}
	cmd.Printf("security-review effort: %s (from %d changed file(s))\n", strings.ToUpper(tier.Effort), len(files))
	if len(tier.SensitivePaths) > 0 {
		sort.Strings(tier.SensitivePaths)
		cmd.Println("sensitive paths triggering high tier:")
		for _, p := range tier.SensitivePaths {
			cmd.Printf("  - %s\n", p)
		}
	}
	return nil
}
