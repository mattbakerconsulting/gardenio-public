package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// mr_embed.go — put screenshots INTO a GitLab MR description (ka-0000001730).
//
// WHY. Agents capture evidence with `gb image upload`, which stores the bytes
// in a PRIVATE bucket and prints a viewer-proxied URL. That URL resolves only
// through the viewer's OIDC chain, so `![before](<viewer>/beads/image/…)` in an
// MR description renders as a BROKEN IMAGE for every reviewer — GitLab fetches
// the src anonymously and gets bounced.
//
// This uploads the bytes to GitLab's own per-project markdown store, which
// serves them to anyone who can see the project (i.e. anyone who can see the
// MR), and splices the resulting markdown into a managed, idempotent block in
// the description. No public bucket, no new anonymous surface, and the image
// actually renders.

const (
	mrEvidenceMarkerStart = "<!-- gardenio:evidence:start — managed by `gb mr embed`, edits below are overwritten -->"
	mrEvidenceMarkerEnd   = "<!-- gardenio:evidence:end -->"
)

var mrEmbedCmd = &cobra.Command{
	Use:   "embed <bead-id-or-mr-url> <image>...",
	Short: "Upload images to GitLab and embed them in the MR description",
	Long: `Upload local images to GitLab and embed them inline in an MR description.

A viewer-proxied 'gb image upload' URL renders as a BROKEN IMAGE in GitLab: the
bucket is private and the /beads/image/ proxy is OIDC-gated, so GitLab's
renderer cannot fetch it. This command uploads the bytes to the project's own
markdown-upload store instead, which serves them to exactly the audience that
can already see the MR.

Images land in a managed block at the end of the description. Re-running
REPLACES that block rather than appending, so a re-capture does not stack
duplicate galleries.

Pair filenames as before/after to render a two-column comparison:
  gb mr embed !1987 before.png after.png --pair

Examples:
  gb mr embed https://gitlab.com/g/p/-/merge_requests/12 shot.png
  gb mr embed my-bead-id before.png after.png --pair
  gb mr embed !1987 proof.png --heading "Fix proof (FSM-472)"
  gb mr embed !1987 shot.png --print-only`,
	Args: cobra.MinimumNArgs(2),
	RunE: runMREmbed,
}

func init() {
	mrEmbedCmd.Flags().Bool("pair", false, "render images as before/after pairs in a two-column table")
	mrEmbedCmd.Flags().String("heading", "Evidence", "heading for the managed evidence block")
	mrEmbedCmd.Flags().Bool("print-only", false, "upload and print the markdown without editing the MR description")
	mrCmd.AddCommand(mrEmbedCmd)
}

func runMREmbed(cmd *cobra.Command, args []string) error {
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

	pair, _ := cmd.Flags().GetBool("pair")
	heading, _ := cmd.Flags().GetString("heading")
	printOnly, _ := cmd.Flags().GetBool("print-only")

	paths := args[1:]
	if pair && len(paths)%2 != 0 {
		return fmt.Errorf("--pair needs an even number of images (before/after per component), got %d", len(paths))
	}

	uploads := make([]*bridge.GitLabUpload, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading %s: %w", p, err)
		}
		up, err := client.UploadFile(cmd.Context(), ref.ProjectPath, filepath.Base(p), data)
		if err != nil {
			return fmt.Errorf("uploading %s to %s: %w", p, ref.ProjectPath, err)
		}
		uploads = append(uploads, up)
		fmt.Fprintf(os.Stderr, "uploaded %s → %s\n", filepath.Base(p), up.MarkdownRef(""))
	}

	block := buildEvidenceBlock(heading, altsFor(paths), uploads, pair)

	if printOnly {
		fmt.Println(block)
		return nil
	}

	mr, err := client.GetMergeRequestByPath(cmd.Context(), ref.ProjectPath, ref.IID)
	if err != nil {
		return fmt.Errorf("fetching MR !%d: %w", ref.IID, err)
	}

	updated := spliceEvidenceBlock(mr.Description, block)
	if updated == mr.Description {
		fmt.Fprintln(os.Stderr, "MR description already carries this evidence block — nothing to do.")
		return nil
	}
	if err := client.UpdateMergeRequestDescription(cmd.Context(), ref.ProjectPath, ref.IID, updated); err != nil {
		return fmt.Errorf("updating MR !%d description: %w", ref.IID, err)
	}

	fmt.Printf("Embedded %d image(s) in %s!%d\n", len(uploads), ref.ProjectPath, ref.IID)
	return nil
}

// altsFor derives alt text from each file's basename (extension stripped), so
// "before-403.png" reads as "before-403" rather than a bare "screenshot".
func altsFor(paths []string) []string {
	alts := make([]string, len(paths))
	for i, p := range paths {
		base := filepath.Base(p)
		alts[i] = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return alts
}

// buildEvidenceBlock renders the managed markdown block. In --pair mode
// consecutive uploads become before/after table rows, which is how a reviewer
// actually reads a fix proof: the two states side by side, not stacked.
func buildEvidenceBlock(heading string, alts []string, uploads []*bridge.GitLabUpload, pair bool) string {
	var sb strings.Builder
	sb.WriteString(mrEvidenceMarkerStart)
	sb.WriteString("\n\n## ")
	sb.WriteString(heading)
	sb.WriteString("\n\n")

	if pair {
		sb.WriteString("| Before | After |\n|---|---|\n")
		for i := 0; i+1 < len(uploads); i += 2 {
			// Table cells cannot contain a raw newline, so each cell is a
			// single inline image.
			sb.WriteString("| ")
			sb.WriteString(uploads[i].MarkdownRef(altAt(alts, i)))
			sb.WriteString(" | ")
			sb.WriteString(uploads[i+1].MarkdownRef(altAt(alts, i+1)))
			sb.WriteString(" |\n")
		}
	} else {
		for i, up := range uploads {
			sb.WriteString(up.MarkdownRef(altAt(alts, i)))
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(mrEvidenceMarkerEnd)
	return sb.String()
}

func altAt(alts []string, i int) string {
	if i < len(alts) {
		return alts[i]
	}
	return ""
}

// spliceEvidenceBlock replaces an existing managed evidence block, or appends
// one when none is present.
//
// Idempotent by construction: re-running after a re-capture swaps the block
// instead of stacking a second gallery, which is what a naive append would do
// every time an agent retried a proof.
func spliceEvidenceBlock(description, block string) string {
	start := strings.Index(description, mrEvidenceMarkerStart)
	end := strings.Index(description, mrEvidenceMarkerEnd)
	if start >= 0 && end > start {
		return description[:start] + block + description[end+len(mrEvidenceMarkerEnd):]
	}
	if strings.TrimSpace(description) == "" {
		return block
	}
	return strings.TrimRight(description, "\n") + "\n\n" + block
}
