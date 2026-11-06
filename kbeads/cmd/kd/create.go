package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// renderDedupAckRejection prints the dedup-ack gate rejection (epic
// ka-0000004599) in a human- and agent-legible form: the open list the caller
// must look at, then the exact retry command. The exit is the returned error
// (non-zero); this only writes the guidance to stderr so it never contaminates
// a `kd create --json`-style stdout capture.
func renderDedupAckRejection(cmd *cobra.Command, e *client.DedupAckRequiredError) {
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "dedup-ack required: %d open %s(s) already exist in project %q.\n",
		e.TotalOpen, e.BeadType, e.Project)
	fmt.Fprintln(w, "Look at the existing list before creating another:")
	for _, o := range e.Open {
		fmt.Fprintf(w, "  %s  %s  (%s old)\n", o.ID, o.Title, humanizeAge(o.AgeSeconds))
	}
	if e.Truncated {
		fmt.Fprintf(w, "  … and %d more (list capped)\n", e.TotalOpen-len(e.Open))
	}
	fmt.Fprintln(w, "If this is genuinely NOT a duplicate, re-run with the acknowledgement flag:")
	fmt.Fprintf(w, "  kd create … %s\n", e.RetryWith)
	fmt.Fprintln(w, "(or pass --force to bypass the gate entirely).")
}

// printWarnings echoes the daemon's advisory X-Beads-Warning headers as
// "warning: …" lines on stderr, leaving stdout (and --json) untouched.
func printWarnings(cmd *cobra.Command, w *client.Warnings) {
	for _, msg := range w.List() {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", msg)
	}
}

// humanizeAge renders an age in seconds as a compact human string.
func humanizeAge(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

// defaultParentTask returns the agent's current task id from $BOAT_TASK_ID.
// Used as the default for `kd create --parent-task` so agent-spawned shells
// inherit their parent-task without the agent having to thread the value
// through every kd invocation.
func defaultParentTask() string {
	return os.Getenv("BOAT_TASK_ID")
}

// agentRequiredFields lists the fields the gardenio controller requires for
// type=agent beads.  Without them the SSE-triggered spawn silently skips the
// bead (buildEvent returns false when role==” || name==”).
var agentRequiredFields = []string{"agent", "role", "project"}

// validateAgentFields returns an error if any required agent field is missing
// from the provided key=value pairs.
func validateAgentFields(pairs []string) error {
	present := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		k, _, ok := splitField(p)
		if ok {
			present[k] = true
		}
	}
	var missing []string
	for _, f := range agentRequiredFields {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"type=agent requires fields: %v\n"+
				"provide them with -f flags (or --role for the role field), e.g.:\n"+
				"  kd create <title> --type agent --role crew -f agent=<name> -f project=<project>",
			missing,
		)
	}
	return nil
}

// parseFields converts -f key=value pairs into a JSON object (bytes).
// Values that look like JSON (start with { [ " or are true/false/null/number)
// are embedded as-is; everything else is quoted as a string. A value of the
// form "@path" (or "@-" for stdin) reads the value verbatim from a file as a
// literal string — bypassing JSON auto-detection so JSON-object text can be
// stored in a string field, and so multiline values survive intact.
func parseFields(pairs []string) ([]byte, error) {
	return parseFieldsWithUnset(pairs, nil)
}

// parseFieldsWithUnset is parseFields plus a set of keys to DELETE. Each unset
// key is emitted as an explicit JSON null in the patch; the daemon applies RFC
// 7386 merge-patch semantics and deletes the key from the stored fields object
// (see internal/server/beads.go updateBead). This is the discoverable path to
// restoring a field to "absent" — distinct from empty string or a stored null
// — and the fix for ka-0000007246. A key given in both `pairs` (a -f set) and
// `unset` is an error: the two intents contradict.
func parseFieldsWithUnset(pairs, unset []string) ([]byte, error) {
	if len(pairs) == 0 && len(unset) == 0 {
		return nil, nil
	}
	m := make(map[string]any, len(pairs)+len(unset))
	for _, p := range pairs {
		k, v, ok := splitField(p)
		if !ok {
			return nil, fmt.Errorf("invalid field %q: expected key=value", p)
		}
		val, err := resolveFieldValue(v)
		if err != nil {
			return nil, err
		}
		m[k] = val
	}
	for _, k := range unset {
		if k == "" {
			return nil, fmt.Errorf("invalid --unset-field: empty key")
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("field %q cannot be both set with -f and cleared with --unset-field", k)
		}
		m[k] = nil // JSON null → daemon deletes the key (RFC 7386)
	}
	b, err := jsonMarshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding fields: %w", err)
	}
	return b, nil
}

var createCmd = &cobra.Command{
	Use:     "create <title>",
	Short:   "Create a new bead",
	GroupID: "beads",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		title := args[0]
		shortTitle, _ := cmd.Flags().GetString("short-title")
		arms, _ := cmd.Flags().GetString("heraldry")
		if err := checkArmsFlag(arms); err != nil {
			return err
		}

		description, _ := cmd.Flags().GetString("description")
		// Expand curl-style @file / @- / @@ on -d, matching `kd update -d` and
		// `-f key=@file` (ka-0000003715). Without this a bare "@/path/to/body.md"
		// is silently stored as the literal description instead of the file body.
		description, err := resolveTextValue(description)
		if err != nil {
			return fmt.Errorf("--description: %w", err)
		}
		beadType, _ := cmd.Flags().GetString("type")
		kind, _ := cmd.Flags().GetString("kind")
		priority, _ := cmd.Flags().GetInt("priority")
		labels, _ := cmd.Flags().GetStringSlice("label")
		assignee, _ := cmd.Flags().GetString("assignee")
		owner, _ := cmd.Flags().GetString("owner")
		role, _ := cmd.Flags().GetString("role")

		fieldPairs, _ := cmd.Flags().GetStringArray("field")

		if role != "" {
			labels = append(labels, "role:"+role)
			// For agent beads, --role also satisfies the required "role" field
			// so the user doesn't need to pass both --role and -f role=<value>.
			if beadType == "agent" {
				hasRoleField := false
				for _, p := range fieldPairs {
					k, _, ok := splitField(p)
					if ok && k == "role" {
						hasRoleField = true
						break
					}
				}
				if !hasRoleField {
					fieldPairs = append(fieldPairs, "role="+role)
				}
			}
		}

		if beadType == "agent" {
			if err := validateAgentFields(fieldPairs); err != nil {
				return err
			}
		}

		// --format is a discoverability shorthand for the type=doc `format`
		// field (ka-0000006824). Thread it into the field pairs unless the
		// caller already supplied `-f format=…`, in which case error rather
		// than silently picking a winner. The server defaults format=md for
		// type=doc when neither is set, so --format stays optional.
		if format, _ := cmd.Flags().GetString("format"); format != "" {
			for _, p := range fieldPairs {
				if k, _, ok := splitField(p); ok && k == "format" {
					return fmt.Errorf("--format and -f format=… both set; use one")
				}
			}
			fieldPairs = append(fieldPairs, "format="+format)
		}

		// type=bundle carries a REQUIRED `kind` FIELD (the display
		// classification: plan|post-mortem|spec|report|other), distinct from
		// the top-level bead kind (issue/data/config) that every type derives
		// from its type config. The top-level bead kind is server-derived and
		// the request's Kind is ignored entirely (createBead sets
		// bead.Kind = tc.Kind), so `-k/--kind` never reached the bundle's
		// required field and EVERY `kd create --type=bundle` 400'd with
		// "kind: is required" (ka-0000002997). Route --kind into fields.kind
		// for bundles — mirroring how --format threads into the type=doc
		// `format` field above — so the documented CLI path works without
		// hand-rolling `-f kind=…`. Error rather than silently pick a winner
		// when both --kind and -f kind=… are set; the server validates the
		// enum vocabulary and surfaces it on a bad value.
		if beadType == "bundle" && kind != "" {
			for _, p := range fieldPairs {
				if k, _, ok := splitField(p); ok && k == "kind" {
					return fmt.Errorf("--kind and -f kind=… both set; use one")
				}
			}
			fieldPairs = append(fieldPairs, "kind="+kind)
			// The top-level request Kind is server-ignored for every type
			// (bead.Kind is always derived from the type config), so clear the
			// local copy: for a bundle the operator's intent is the field, and
			// leaving Kind set would send a misleading, dead top-level value.
			kind = ""
		}

		if err := rejectScheduleCreate(beadType); err != nil {
			return err
		}
		lintTriggerFields(os.Stderr, beadType, fieldPairs)

		// Apply --project label and cross-project guard.
		project, _ := cmd.Flags().GetString("project")
		force, _ := cmd.Flags().GetBool("force")
		if project != "" {
			labels = append(labels, "project:"+project)
			if !force {
				if ap := agentProject(); ap != "" && ap != project {
					return fmt.Errorf("cross-project create: you are in project %q but creating in %q — use --force to override", ap, project)
				}
			}
		}

		fieldsJSON, err := parseFields(fieldPairs)
		if err != nil {
			return fmt.Errorf("parsing fields: %w", err)
		}

		idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")

		// Dedup-ack: the acknowledged open-list hash is type-specific, but the
		// server input carries a single saw_open_hash — the type disambiguates
		// which queue it acknowledges. Take whichever flag matches the bead
		// type (decision → --saw-open-decisions, escalation →
		// --saw-open-escalations); the other is irrelevant for this create.
		sawDecisions, _ := cmd.Flags().GetString("saw-open-decisions")
		sawEscalations, _ := cmd.Flags().GetString("saw-open-escalations")
		sawOpenHash := sawDecisions
		if sawOpenHash == "" {
			sawOpenHash = sawEscalations
		}

		req := &client.CreateBeadRequest{
			Title:          title,
			ShortTitle:     shortTitle,
			Heraldry:       arms,
			Description:    description,
			Type:           beadType,
			Kind:           kind,
			Priority:       priority,
			Labels:         labels,
			Assignee:       assignee,
			Owner:          owner,
			CreatedBy:      actor,
			Fields:         fieldsJSON,
			IdempotencyKey: idempotencyKey,
			SawOpenHash:    sawOpenHash,
			// --force also bypasses the server-side decision/escalation
			// content guards (ack-only guard, dedup-ack gate) — the same flag
			// that bypasses the client-side cross-project check above.
			Force: force,
		}

		ctx := context.Background()
		escalate, _ := cmd.Flags().GetBool("escalate")
		if escalate {
			ctx = client.WithEscalate(ctx)
		}

		parentTask, _ := cmd.Flags().GetString("parent-task")
		noParent, _ := cmd.Flags().GetBool("no-parent")
		// A parentless type=doc silently auto-parents to the agent's task
		// instead of the bundle it was written for, producing an empty
		// bundle page with no error anywhere (ka-0000000755).
		lintOrphanDoc(os.Stderr, beadType, cmd.Flags().Changed("parent-task"), noParent)
		if noParent {
			ctx = client.WithNoAutoParent(ctx)
		} else if parentTask != "" {
			// Hand the parent to the daemon as parent_bead_id so it wires the
			// parent-child dep inside the create transaction (D5,
			// ka-0000007622). The server then skips its auto-parent step, and —
			// the point of gd-kcCrpdrlm2 — create-time gates such as
			// BEADS_HERALDRY_GATE see the new bead's parent, which a dep wired
			// after the create hid from them. WithNoAutoParent stays as a belt
			// for a daemon that predates parent_bead_id.
			req.ParentBeadID = parentTask
			ctx = client.WithNoAutoParent(ctx)
		}

		ctx, warnings := client.WithWarnings(ctx)
		bead, err := beadsClient.CreateBead(ctx, req)
		if err != nil && req.ParentBeadID != "" && isUnknownParentError(err) {
			// --parent-task defaults to $BOAT_TASK_ID, so a stale or mistyped
			// parent must not cost the create: before parent_bead_id the dep
			// was a separate, non-fatal write. Keep that — create unparented
			// and warn so the user can retry with `kd dep add`.
			fmt.Fprintf(os.Stderr, "warning: parent-task %s not found; creating without a parent-child dep: %v\n", req.ParentBeadID, err)
			req.ParentBeadID = ""
			bead, err = beadsClient.CreateBead(ctx, req)
		}
		if err != nil {
			var esc *client.EscalationCreated
			if errors.As(err, &esc) {
				fmt.Printf("Escalation created: %s\n", esc.EscalationID)
				fmt.Printf("  Action: %s on type %s\n", esc.DeniedAction, esc.BeadType)
				fmt.Println("  Status: pending operator approval")
				return nil
			}
			var dedup *client.DedupAckRequiredError
			if errors.As(err, &dedup) {
				renderDedupAckRejection(cmd, dedup)
				return dedup
			}
			return fmt.Errorf("creating bead: %w", err)
		}
		printWarnings(cmd, warnings)

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			printBeadTable(bead)
		}
		return nil
	},
}

// isUnknownParentError reports whether a create was refused because its
// parent_bead_id names no bead (the daemon's inputError in createBead). Matched
// on the message because it reaches kd as either an HTTP 400 or a gRPC
// InvalidArgument, and both carry the same text.
func isUnknownParentError(err error) bool {
	return strings.Contains(err.Error(), "parent_bead_id refers to unknown bead")
}

func init() {
	createCmd.Flags().String("short-title", "", model.ShortTitleHelp)
	createCmd.Flags().String("heraldry", "", model.HeraldryHelp)
	createCmd.Flags().StringP("description", "d", "", "bead description (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	createCmd.Flags().StringP("type", "t", "task", "bead type")
	createCmd.Flags().StringP("kind", "k", "", "bead kind (inferred from type for issue/data/config beads; for --type bundle sets the required kind field: plan|post-mortem|spec|report|other)")
	createCmd.Flags().IntP("priority", "p", 2, "bead priority")
	createCmd.Flags().StringSliceP("label", "l", nil, "labels (repeatable)")
	createCmd.Flags().String("assignee", "", "assignee")
	createCmd.Flags().String("owner", "", "owner")
	createCmd.Flags().String("role", "", "role label to assign (adds role:<value> label; for --type agent also sets the role field)")
	createCmd.Flags().StringArrayP("field", "f", nil, "typed field (key=value, repeatable; key=@file reads value verbatim from file as a string, key=@- reads stdin)")
	createCmd.Flags().String("format", "", "for --type doc: content format (md|diff|csv|code|image|link|html); server defaults to md when unset")
	createCmd.Flags().String("project", defaultProject(), "project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	createCmd.Flags().Bool("force", false, "bypass cross-project check")
	createCmd.Flags().Bool("escalate", false, "request elevated access if write is denied")
	createCmd.Flags().String("parent-task", defaultParentTask(), "parent-child dep target (alias: --parent; default: $BOAT_TASK_ID; set to empty to disable explicit wiring while still allowing server-side auto-parent)")
	createCmd.Flags().Bool("no-parent", false, "suppress both --parent-task and server-side auto-parent dep injection")
	createCmd.Flags().String("idempotency-key", "", "dedup key: if a bead in this project already carries it, that bead is returned instead of creating a duplicate (no new bead, no create event)")
	// Dedup-ack gate (epic ka-0000004599): for --type decision/escalation, when
	// open beads of that type already exist in the project the daemon rejects
	// the create and returns the open list + a hash. Re-run with the matching
	// flag set to that hash to confirm you looked at the existing list first.
	createCmd.Flags().String("saw-open-decisions", "", "dedup-ack: hash of the open-decisions list you acknowledged (from a prior dedup_ack_required rejection); required to create a decision when open decisions exist")
	createCmd.Flags().String("saw-open-escalations", "", "dedup-ack: hash of the open-escalations list you acknowledged (from a prior dedup_ack_required rejection); required to create an escalation when open escalations exist")
	// Accept --labels as an alias of --label so the common agent typo no longer
	// aborts with a bare "unknown flag" (ka-0000000987).
	aliasLabelFlags(createCmd)
	// Accept --parent as an alias of --parent-task so the spelling agents reach
	// for (kd list has --parent) no longer aborts with a bare "unknown flag"
	// (ka-0000003191). Must run AFTER aliasLabelFlags — it chains onto that
	// command's existing normalize func rather than replacing it.
	aliasParentTaskFlag(createCmd)
}
