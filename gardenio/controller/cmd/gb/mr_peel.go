package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// mr_peel.go wires the peel scaffolder into the `gb mr` command group:
//
//	gb mr peel <dark-bead> --concern <slice> [--operator …] [--bead …] [-o file]
//
// It is the deterministic, human-guided front door to a monorepo-dark trickle
// slice (task ka-0000001847, R3 catalog /bundle/ka-0000005053 §2.5 / follow-up
// #26; master epic ka-0000005560). It resolves the dark bead's extractable facts
// (title, operator, dark-MR IID from mr_url) from the daemon and renders a
// pre-filled trust-dossier MR body via bridge.ScaffoldPeelDossier.
//
// SAFETY / SCOPE (R3 §2.5, "may stay human-guided"): this command is READ-ONLY.
// It NEVER pushes a branch and NEVER opens an MR — the risky judgment (which
// files form the single-concern slice, the Why/Alternatives/Risk prose) stays
// with a human. It prints a filled skeleton + the exact next steps (materialize
// the slice on peel/<slug>, fill the TODO sections, then open a Draft MR via
// deploy-values/gitlab-dark/peel/peel-mr-cmd.sh). The upstream-write path
// (peel-mr-cmd.sh) is the only thing that ever touches upstream, and it is a
// separate, guardrailed step a human drives.
var (
	mrPeelConcern  string
	mrPeelOperator string
	mrPeelBead     string
	mrPeelSlug     string
	mrPeelDarkMR   string
	mrPeelTitle    string
	mrPeelOutput   string
)

var mrPeelCmd = &cobra.Command{
	Use:   "peel <dark-bead> --concern <slice>",
	Short: "Scaffold a trust-dossier MR that peels one concern from an already-merged dark bead",
	Long: `Scaffold a monorepo-dark trickle MR (task ka-0000001847, R3 catalog
/bundle/ka-0000005053 §2.5 / follow-up #26; master epic ka-0000005560).

Given a dark bead (already-merged dark work) and a single --concern slice, this
PRE-FILLS a trust-dossier MR body from the deterministic, extractable facts and
hands you a filled skeleton to finish:

  * Summary       seeded from the dark bead title
  * Provenance    Dark bead + Dark-MR (from the bead's mr_url) anchors — bead id
                  + MR IID, NEVER a SHA (the rebase gardener rewrites dark SHAs)
  * Operator:     from --operator, else $BEADS_OPERATED_BY, else the bead
  * Bead:         from --bead (peel-request bead), else the dark bead itself
  * concern boundary in the "What this is NOT" section
  * Why / Alternatives / Risk / Evidence   left as explicit TODO stubs

This is DELIBERATELY human-guided (R3 flags single-concern slicing of merged dark
state as the riskiest piece). The command is READ-ONLY: it NEVER pushes a branch
and NEVER opens an MR. The judgment sections stay TODO so the trust-dossier lint
(scripts/lint-trust-dossier.sh) stays RED until a human fills them — that is the
intended signal, not a bug.

Next steps it prints:
  1. materialize the single-concern slice on the suggested branch peel/<slug>;
  2. fill the Why / Alternatives / Risk / Evidence sections;
  3. open a Draft MR via deploy-values/gitlab-dark/peel/peel-mr-cmd.sh (the only
     guardrailed upstream-write path — a human owns the merge).

Examples:
  gb mr peel ka-ABC123 --concern scaffolder
  gb mr peel ka-ABC123 --concern "viewer render" -o /tmp/peel-body.md
  gb mr peel ka-ABC123 --concern scaffolder --bead ka-REQ999 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runMRPeel,
}

func init() {
	mrPeelCmd.Flags().StringVar(&mrPeelConcern, "concern", "",
		"The single concern this slice delivers (required) — seeds the slug, title, and concern boundary")
	mrPeelCmd.Flags().StringVar(&mrPeelOperator, "operator", "",
		"Operator: trailer value (default: $BEADS_OPERATED_BY, else the dark bead's operator)")
	mrPeelCmd.Flags().StringVar(&mrPeelBead, "bead", "",
		"Peel-request bead for the Bead: trailer, if distinct from the dark bead (default: the dark bead)")
	mrPeelCmd.Flags().StringVar(&mrPeelSlug, "slug", "",
		"Override the derived peel branch slug (default: <concern>-<dark-bead>)")
	mrPeelCmd.Flags().StringVar(&mrPeelDarkMR, "dark-mr", "",
		"Dark MR IID (digits) to anchor on, when the dark bead carries no mr_url")
	mrPeelCmd.Flags().StringVar(&mrPeelTitle, "title", "",
		"Override the dark bead title used to seed Summary + the MR title (default: fetched from the bead)")
	mrPeelCmd.Flags().StringVarP(&mrPeelOutput, "output", "o", "",
		"Write the scaffolded MR body to this file instead of stdout")

	mrCmd.AddCommand(mrPeelCmd)
}

func runMRPeel(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(mrPeelConcern) == "" {
		return fmt.Errorf("--concern <slice> is required (the single concern this slice delivers)")
	}
	darkBeadID := strings.TrimSpace(args[0])

	in := bridge.PeelScaffoldInput{
		DarkBeadID:    darkBeadID,
		Concern:       mrPeelConcern,
		Operator:      strings.TrimSpace(mrPeelOperator),
		RequestBead:   strings.TrimSpace(mrPeelBead),
		DarkMRIID:     strings.TrimSpace(mrPeelDarkMR),
		DarkBeadTitle: strings.TrimSpace(mrPeelTitle),
		SlugOverride:  strings.TrimSpace(mrPeelSlug),
	}

	// Best-effort enrichment from the dark bead. The command still scaffolds when
	// the daemon is unreachable or the bead does not resolve — every fetched
	// field is optional and degrades to a TODO stub or a flag override.
	if bead := fetchDarkBead(cmd.Context(), darkBeadID); bead != nil {
		if in.DarkBeadTitle == "" {
			in.DarkBeadTitle = bead.Title
		}
		if in.Operator == "" {
			in.Operator = darkBeadOperator(bead)
		}
		if in.DarkMRIID == "" {
			in.DarkMRIID = darkMRIIDFromBead(bead)
		}
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"peel: could not fetch dark bead %s — scaffolding from flags/placeholders only\n", darkBeadID)
	}

	scaffold, err := bridge.ScaffoldPeelDossier(in)
	if err != nil {
		return err
	}

	if mrPeelOutput != "" {
		if err := os.WriteFile(mrPeelOutput, []byte(scaffold.Description), 0o644); err != nil { //nolint:gosec // operator-supplied output path
			return fmt.Errorf("writing scaffold to %s: %w", mrPeelOutput, err)
		}
	}

	if jsonOutput {
		printJSON(map[string]any{
			"dark_bead":   darkBeadID,
			"concern":     mrPeelConcern,
			"slug":        scaffold.Slug,
			"branch":      scaffold.Branch,
			"title":       scaffold.Title,
			"operator":    in.Operator,
			"dark_mr_iid": in.DarkMRIID,
			"bead":        firstNonEmpty(in.RequestBead, darkBeadID),
			"output_file": mrPeelOutput,
			"description": scaffold.Description,
		})
		return nil
	}

	printPeelScaffold(cmd, scaffold, in, darkBeadID)
	return nil
}

// fetchDarkBead resolves the dark bead, returning nil on any failure (daemon
// unset in a unit context, network error, or not-found) so the caller can
// degrade gracefully rather than abort.
func fetchDarkBead(ctx context.Context, id string) *beadsapi.BeadDetail {
	if daemon == nil || !(strings.HasPrefix(id, "gd-") || strings.HasPrefix(id, "ka-")) {
		return nil
	}
	bead, err := daemon.GetBead(ctx, id)
	if err != nil {
		return nil
	}
	return bead
}

// darkBeadOperator picks the most human-readable operator identity from a bead's
// operator fields, preferring a display name, then email, then the raw id.
func darkBeadOperator(bead *beadsapi.BeadDetail) string {
	for _, v := range []string{bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBy, bead.Fields["operated_by"]} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// darkMRIIDFromBead extracts the dark MR IID (digits only) from the bead's
// mr_url field, if present. Anchoring on the IID (not a SHA) is what survives the
// rebase gardener's per-cycle SHA rewrites.
func darkMRIIDFromBead(bead *beadsapi.BeadDetail) string {
	raw := strings.TrimSpace(bead.Fields["mr_url"])
	if raw == "" {
		return ""
	}
	if ref := bridge.ParseMRURL(raw); ref != nil && ref.IID > 0 {
		return fmt.Sprintf("%d", ref.IID)
	}
	return ""
}

// printPeelScaffold renders the human-facing output: the suggested branch/title,
// the pre-filled body (unless it was written to a file), and the explicit
// human-guided next steps.
func printPeelScaffold(cmd *cobra.Command, s bridge.PeelScaffold, in bridge.PeelScaffoldInput, darkBeadID string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Peel scaffold for concern %q from %s\n", in.Concern, darkBeadID)
	fmt.Fprintf(out, "  suggested branch : %s\n", s.Branch)
	fmt.Fprintf(out, "  suggested title  : %s\n", s.Title)
	if op := strings.TrimSpace(in.Operator); op != "" {
		fmt.Fprintf(out, "  operator         : %s\n", op)
	} else {
		fmt.Fprintf(out, "  operator         : (unset — fill the Operator: trailer)\n")
	}
	if iid := strings.TrimSpace(in.DarkMRIID); iid != "" {
		fmt.Fprintf(out, "  dark MR anchor   : !%s\n", iid)
	} else {
		fmt.Fprintf(out, "  dark MR anchor   : (none — dark bead has no mr_url; pass --dark-mr)\n")
	}

	if mrPeelOutput != "" {
		fmt.Fprintf(out, "\nMR body written to %s\n", mrPeelOutput)
	} else {
		fmt.Fprintln(out, "\n--- trust-dossier MR body (review, then fill the TODO sections) ---")
		fmt.Fprint(out, s.Description)
		fmt.Fprintln(out, "--- end MR body ---")
	}

	fmt.Fprintln(out, "\nHuman-guided next steps (this command pushed nothing and opened no MR):")
	fmt.Fprintf(out, "  1. materialize the single `%s` concern on branch %s\n", in.Concern, s.Branch)
	fmt.Fprintln(out, "  2. fill the Why / Alternatives considered / Risk assessment / Evidence sections")
	fmt.Fprintln(out, "     (scripts/lint-trust-dossier.sh stays RED until Why is non-trivial and != Summary)")
	fmt.Fprintln(out, "  3. open a Draft MR via deploy-values/gitlab-dark/peel/peel-mr-cmd.sh open-draft-mr \\")
	fmt.Fprintf(out, "        %s <peel-tip> %q <body-file>\n", s.Slug, s.Title)
	fmt.Fprintln(out, "     (the only guardrailed upstream-write path — a human owns the merge)")
}
