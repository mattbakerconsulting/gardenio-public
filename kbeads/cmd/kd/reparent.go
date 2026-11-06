package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/references"
	"github.com/spf13/cobra"
)

// reparentResult is the JSON-emitted summary of a reparent operation.
type reparentResult struct {
	Bead            string   `json:"bead"`
	OldParents      []string `json:"old_parents"`
	NewParent       string   `json:"new_parent"`
	RemovedEdges    []string `json:"removed_edges"`
	AddedEdge       string   `json:"added_edge"`
	MentionsRewrote int      `json:"mentions_rewrote"`
	DescriptionEdit bool     `json:"description_edited"`
	RelatedAdded    []string `json:"related_added,omitempty"`
}

var reparentCmd = &cobra.Command{
	Use:     "reparent <bead-id> --to <new-parent-id>",
	Short:   "Move a bead to a new parent epic in one safe operation",
	GroupID: "beads",
	Long: `Re-parent a bead: remove its old parent-child edge(s), add the new one, and
rewrite the ` + "`parent:`" + ` typed mentions in the bead's text so a later write
does not resurrect the old edge.

Re-parenting by hand is a fussy multi-step dance. Typed
mentions like ` + "`parent: ka-X`" + ` auto-materialize a parent-child dep row on
every write, additively — so ` + "`kd dep remove`" + ` alone is not durable: the next
description or comment write re-extracts the still-present ` + "`parent: ka-X`" + ` text
and re-creates the edge. reparent does all of it atomically from the client:

  1. add the new parent-child edge (server enforces the cycle guard);
  2. rewrite every parent-edge mention of the old parent across the title,
     description, AND notes (` + "`parent(ka-OLD)`" + `, ` + "`parent: ka-OLD`" + `, and the
     inverse ` + "`child-of` forms" + `) to point at the new parent — this is what
     stops resurrection;
  3. remove the stale old parent-child edge(s).

Only parent-edge typed mentions are rewritten — related:, blocks:, and parent
mentions of other beads are left untouched.

Examples:
  kd reparent ka-0000003755 --to ka-0000002510
  kd reparent ka-0000003755 --to ka-0000002510 --keep-related`,
	Args: cobra.ExactArgs(1),
	RunE: runReparent,
}

func runReparent(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	newParent, _ := cmd.Flags().GetString("to")
	keepRelated, _ := cmd.Flags().GetBool("keep-related")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if newParent == "" {
		return fmt.Errorf("--to <new-parent-id> is required")
	}
	if newParent == beadID {
		return fmt.Errorf("cannot reparent %s onto itself", beadID)
	}

	ctx := context.Background()

	// Fetch the bead so we can rewrite its description and validate it exists.
	bead, err := beadsClient.GetBead(ctx, beadID)
	if err != nil {
		return fmt.Errorf("fetching bead %s: %w", beadID, err)
	}

	// Discover the current parent-child edge(s). A bead should have at most
	// one parent, but reparent tolerates (and cleans up) a bead that ended up
	// with several stale parent edges from prior manual churn.
	deps, err := beadsClient.GetDependencies(ctx, beadID)
	if err != nil {
		return fmt.Errorf("listing dependencies of %s: %w", beadID, err)
	}
	var oldParents []string
	for _, d := range deps {
		if d.Type == model.DepParentChild && d.DependsOnID != newParent {
			oldParents = append(oldParents, d.DependsOnID)
		}
	}

	// Rewrite parent-edge typed mention(s) across ALL text fields the write
	// path re-extracts — title, description, AND notes. replaceBeadReferences
	// runs the typed parser over all three on every write
	// (queries_references.go), so a stale `parent: ka-OLD` in the title or
	// notes resurrects the edge exactly as one in the description does — the
	// description-only scrub would silently miss it (source-verified,
	// ka-0000007756). The rewrite is driven off what the parser (ExtractTyped)
	// actually surfaces as a parent-edge mention — `parent` and its inverse
	// `child-of`, both of which materialize the SAME parent-child dep row —
	// NOT off the live edge list, so it also scrubs a text-only stale parent
	// whose edge was already removed manually (the resurrection footgun).
	newTitle, newDesc, newNotes := bead.Title, bead.Description, bead.Notes
	totalRewrote := 0
	var rewrittenOldParents []string
	seenOldParent := map[string]struct{}{}
	for _, fp := range []*string{&newTitle, &newDesc, &newNotes} {
		for _, oldP := range references.ParentEdgeTargetsExcept(*fp, newParent) {
			r := references.RewriteParentMention(*fp, oldP, newParent)
			if r.Rewrote == 0 {
				continue
			}
			*fp = r.Text
			totalRewrote += r.Rewrote
			if _, dup := seenOldParent[oldP]; !dup {
				seenOldParent[oldP] = struct{}{}
				rewrittenOldParents = append(rewrittenOldParents, oldP)
			}
		}
	}

	// --keep-related: append one `related: <oldParent>` back-reference to the
	// description per rewritten old parent, unless the bead already relates to
	// it in some field (idempotent). The back-ref preserves the now
	// non-parental association without re-materializing a parent-child edge.
	var relatedAdded []string
	if keepRelated {
		for _, oldP := range rewrittenOldParents {
			if references.HasRelatedMention(newTitle, oldP) ||
				references.HasRelatedMention(newDesc, oldP) ||
				references.HasRelatedMention(newNotes, oldP) {
				continue
			}
			newDesc = references.AppendRelatedMention(newDesc, oldP)
			relatedAdded = append(relatedAdded, oldP)
		}
	}

	// Residual guard (belt-and-suspenders): after rewriting, no parent-edge
	// mention of any bead OTHER than the new parent may survive in ANY field —
	// if one does, the next write would resurrect that edge. This catches an
	// unhandled surface form loudly instead of silently. In practice
	// ParentEdgeTargetsExcept + RewriteParentMention cover the same verb set,
	// so this should never fire; it exists so a future divergence fails a test
	// rather than a production bead.
	var residual []string
	for _, f := range []string{newTitle, newDesc, newNotes} {
		residual = append(residual, references.ParentEdgeTargetsExcept(f, newParent)...)
	}
	if len(residual) > 0 {
		return fmt.Errorf(
			"internal: text still names parent-edge target(s) %v after rewrite — refusing to proceed (would resurrect edge). This is a bug in kd reparent; please report with bead %s",
			residual, beadID)
	}

	titleEdit := newTitle != bead.Title
	descEdit := newDesc != bead.Description
	notesEdit := newNotes != bead.Notes
	anyTextEdit := titleEdit || descEdit || notesEdit

	result := reparentResult{
		Bead:            beadID,
		OldParents:      oldParents,
		NewParent:       newParent,
		MentionsRewrote: totalRewrote,
		DescriptionEdit: anyTextEdit,
		RelatedAdded:    relatedAdded,
	}

	if dryRun {
		result.AddedEdge = newParent + " (not applied: --dry-run)"
		result.RemovedEdges = oldParents
		return emitReparentResult(result, newDesc, bead.Description, true)
	}

	// --- Apply, in a fail-safe order. ---
	//
	// 1. Add the new parent-child edge FIRST. The server runs the cycle guard
	//    here (ka-0000006899); if the move would create a cycle we abort
	//    before touching anything else, leaving the bead on its old parent.
	if _, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
		BeadID:      beadID,
		DependsOnID: newParent,
		Type:        string(model.DepParentChild),
		CreatedBy:   actor,
	}); err != nil {
		return fmt.Errorf("adding new parent-child edge %s -> %s: %w", beadID, newParent, err)
	}
	result.AddedEdge = newParent

	// 2. Rewrite the text SECOND, before removing the old edge, so the stale
	//    `parent: ka-OLD` text is gone before any future write can
	//    re-materialize it. If this fails we still remove the old edge below,
	//    but we surface the error so the operator knows the text is stale.
	var descErr error
	if anyTextEdit {
		req := &client.UpdateBeadRequest{}
		if titleEdit {
			req.Title = strPtr(newTitle)
		}
		if descEdit {
			req.Description = strPtr(newDesc)
		}
		if notesEdit {
			req.Notes = strPtr(newNotes)
		}
		if _, err := beadsClient.UpdateBead(ctx, beadID, req); err != nil {
			descErr = fmt.Errorf("rewriting text fields of %s: %w", beadID, err)
		}
	}

	// 3. Remove the stale old parent-child edge(s) LAST.
	for _, oldP := range oldParents {
		// Discard rows-affected: the old edge being already gone is a fine
		// outcome for a reparent (the desired end state is "not parented to
		// oldP"), so only a transport/store error is fatal here.
		if _, err := beadsClient.RemoveDependency(ctx, beadID, oldP, string(model.DepParentChild)); err != nil {
			return fmt.Errorf("removing old parent-child edge %s -> %s: %w (new edge and description already updated)", beadID, oldP, err)
		}
		result.RemovedEdges = append(result.RemovedEdges, oldP)
	}

	if descErr != nil {
		// The edges are consistent, but the text still names the old parent —
		// warn loudly because the next write would resurrect the edge.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %v\n         the old parent text is still present; re-run reparent or fix the bead text manually.\n",
			descErr)
	}

	return emitReparentResult(result, newDesc, bead.Description, false)
}

// emitReparentResult prints the reparent outcome as JSON (when --json) or a
// human-readable summary.
func emitReparentResult(r reparentResult, newDesc, oldDesc string, dryRun bool) error {
	if jsonOutput {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	if dryRun {
		fmt.Printf("DRY RUN — no changes applied.\n\n")
	}
	fmt.Printf("Bead:            %s\n", r.Bead)
	if len(r.OldParents) == 0 {
		fmt.Printf("Old parent(s):   (none found — adding new parent edge only)\n")
	} else {
		fmt.Printf("Old parent(s):   %v\n", r.OldParents)
	}
	fmt.Printf("New parent:      %s\n", r.NewParent)
	if !dryRun {
		fmt.Printf("Added edge:      %s -> %s (parent-child)\n", r.Bead, r.NewParent)
		if len(r.RemovedEdges) > 0 {
			fmt.Printf("Removed edge(s): %v\n", r.RemovedEdges)
		}
	}
	fmt.Printf("Parent mentions rewritten (title/description/notes): %d\n", r.MentionsRewrote)
	if len(r.RelatedAdded) > 0 {
		fmt.Printf("related: mentions added for: %v\n", r.RelatedAdded)
	}
	if dryRun && newDesc != oldDesc {
		fmt.Fprintf(os.Stdout, "\n--- description (new) ---\n%s\n", newDesc)
	}
	return nil
}

func init() {
	reparentCmd.Flags().String("to", "", "new parent bead id (required)")
	reparentCmd.Flags().Bool("keep-related", false, "add a `related:` mention back to the old parent after reparenting")
	reparentCmd.Flags().Bool("dry-run", false, "show what would change without applying it")
	_ = reparentCmd.MarkFlagRequired("to")
}
