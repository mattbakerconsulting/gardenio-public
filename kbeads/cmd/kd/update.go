package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// updateArgs enforces the single <id> positional, but when EXTRA positionals
// arrive alongside --add-label/--remove-label it replaces cobra's opaque
// "accepts 1 arg(s), received N" with the actual cause: those flags take ONE
// label per occurrence, so `kd update <id> --remove-label a b` binds only "a"
// and leaves "b" a stray positional (ka-0000006934). The generic arg-count
// error sent callers hunting for a bad id instead of repeating the flag; this
// prints the corrected form (repeat the flag, or comma-join) verbatim.
func updateArgs(cmd *cobra.Command, args []string) error {
	if len(args) <= 1 {
		return cobra.ExactArgs(1)(cmd, args)
	}
	extra := args[1:]
	for _, flag := range []string{"remove-label", "add-label"} {
		if !cmd.Flags().Changed(flag) {
			continue
		}
		// Reconstruct the full intent: the one value the flag DID bind plus the
		// strays cobra dropped to positionals, so the suggested form is complete.
		have, _ := cmd.Flags().GetStringSlice(flag)
		all := append(append([]string{}, have...), extra...)
		return fmt.Errorf(
			"kd update takes exactly one bead id, but also got: %s\n"+
				"       --%s takes ONE label per flag, so those became stray positionals, not labels.\n"+
				"       Repeat the flag, or comma-join, to name several labels:\n"+
				"         --%s %s\n"+
				"         --%s %s",
			strings.Join(extra, " "),
			flag,
			flag, strings.Join(all, " --"+flag+" "),
			flag, strings.Join(all, ","))
	}
	return cobra.ExactArgs(1)(cmd, args)
}

var updateCmd = &cobra.Command{
	Use:     "update <id>",
	Short:   "Update a bead",
	GroupID: "beads",
	Args:    updateArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		req := &client.UpdateBeadRequest{}

		if cmd.Flags().Changed("title") {
			v, _ := cmd.Flags().GetString("title")
			resolved, err := resolveTextValue(v)
			if err != nil {
				return fmt.Errorf("--title: %w", err)
			}
			req.Title = strPtr(resolved)
		}
		if cmd.Flags().Changed("short-title") {
			v, _ := cmd.Flags().GetString("short-title")
			req.ShortTitle = strPtr(v)
		}
		if cmd.Flags().Changed("heraldry") {
			v, _ := cmd.Flags().GetString("heraldry")
			if err := checkArmsFlag(v); err != nil {
				return err
			}
			req.Heraldry = strPtr(v)
		}
		if onlyIfEmpty, _ := cmd.Flags().GetBool("only-if-short-title-empty"); onlyIfEmpty {
			if req.ShortTitle == nil {
				return fmt.Errorf("--only-if-short-title-empty needs --short-title")
			}
			req.OnlyIfShortTitleEmpty = true
		}
		if cmd.Flags().Changed("description") {
			v, _ := cmd.Flags().GetString("description")
			resolved, err := resolveTextValue(v)
			if err != nil {
				return fmt.Errorf("--description: %w", err)
			}
			req.Description = strPtr(resolved)
		}
		if cmd.Flags().Changed("status") {
			v, _ := cmd.Flags().GetString("status")
			req.Status = strPtr(v)
		}
		if cmd.Flags().Changed("priority") {
			v, _ := cmd.Flags().GetInt("priority")
			req.Priority = intPtr(v)
		}
		if cmd.Flags().Changed("assignee") {
			v, _ := cmd.Flags().GetString("assignee")
			req.Assignee = strPtr(v)
		}
		if cmd.Flags().Changed("owner") {
			v, _ := cmd.Flags().GetString("owner")
			req.Owner = strPtr(v)
		}
		if cmd.Flags().Changed("notes") {
			v, _ := cmd.Flags().GetString("notes")
			resolved, err := resolveTextValue(v)
			if err != nil {
				return fmt.Errorf("--notes: %w", err)
			}
			req.Notes = strPtr(resolved)
		}
		var fieldPairs []string
		var unsetKeys []string
		if cmd.Flags().Changed("unset-field") {
			unsetKeys, _ = cmd.Flags().GetStringArray("unset-field")
		}
		if cmd.Flags().Changed("field") {
			fieldPairs, _ = cmd.Flags().GetStringArray("field")
		}
		if len(fieldPairs) > 0 || len(unsetKeys) > 0 {
			fieldsJSON, err := parseFieldsWithUnset(fieldPairs, unsetKeys)
			if err != nil {
				return fmt.Errorf("parsing fields: %w", err)
			}
			req.Fields = fieldsJSON
		}

		// Refuse to blank text that is the bead's content (gd-EgvTGAozYB). The
		// usual cause is not intent: `-d "$(cat body.md)"` on a resumed session
		// whose scratchpad was wiped expands to "" and the update erases a plan
		// narrative or a bundle's content. --allow-empty is the deliberate path.
		if allowEmpty, _ := cmd.Flags().GetBool("allow-empty"); !allowEmpty {
			if err := refuseEmptyUpdate(req, fieldPairs); err != nil {
				return err
			}
		}

		// Non-clobbering sub-field edits for SHARED AGGREGATE fields
		// (ka-0000007767). These edit ONE key of a JSON-map field or ONE element
		// of a CSV field via the daemon's atomic edit endpoint, so they never
		// replace the whole aggregate the way a bare -f does. Parsed up front so
		// a malformed spec aborts before any write.
		subEdits, err := collectSubFieldEdits(func(name string) []string {
			if !cmd.Flags().Changed(name) {
				return nil
			}
			v, _ := cmd.Flags().GetStringArray(name)
			return v
		})
		if err != nil {
			return err
		}

		// A whole-bead PATCH change is any scalar/typed-field/labels mutation.
		// When ONLY sub-field edits are requested we skip UpdateBead entirely so
		// we don't fire a spurious empty PATCH (which would bump updated_at and
		// could trip the write-authz ownership gate). Any other invocation keeps
		// the existing behavior of always calling UpdateBead.
		reqHasChange := req.Title != nil || req.ShortTitle != nil || req.Heraldry != nil || req.Description != nil || req.Status != nil ||
			req.Priority != nil || req.Assignee != nil || req.Owner != nil ||
			req.Notes != nil || req.Fields != nil
		labelChange := cmd.Flags().Changed("add-label") || cmd.Flags().Changed("remove-label")
		callUpdate := reqHasChange || labelChange || len(subEdits) == 0

		ctx := context.Background()

		// Lint deprecation warnings for type=schedule. We need the bead's
		// current type to decide whether to lint; fetch it cheaply when
		// fields are being touched.
		if len(fieldPairs) > 0 {
			if cur, err := beadsClient.GetBead(ctx, id); err == nil && cur != nil {
				lintScheduleFields(os.Stderr, string(cur.Type), fieldPairs)
				lintTriggerFields(os.Stderr, string(cur.Type), fieldPairs)
			}
		}
		escalate, _ := cmd.Flags().GetBool("escalate")
		if escalate {
			ctx = client.WithEscalate(ctx)
		}

		var bead *model.Bead
		if callUpdate {
			var err error
			updateCtx, warnings := client.WithWarnings(ctx)
			bead, err = beadsClient.UpdateBead(updateCtx, id, req)
			if err != nil {
				var esc *client.EscalationCreated
				if errors.As(err, &esc) {
					fmt.Printf("Escalation created: %s\n", esc.EscalationID)
					fmt.Printf("  Action: %s on type %s\n", esc.DeniedAction, esc.BeadType)
					fmt.Println("  Status: pending operator approval")
					return nil
				}
				return fmt.Errorf("updating bead %s: %w", id, err)
			}
			printWarnings(cmd, warnings)
		}

		// Apply non-clobbering sub-field edits after the core UpdateBead so a
		// -f/--status change and a --field-set can be expressed in one call. Each
		// edit is atomic server-side (ka-0000007767).
		if len(subEdits) > 0 {
			edited, err := applySubFieldEdits(ctx, id, subEdits)
			if err != nil {
				return err
			}
			if edited != nil {
				bead = edited
			}
		}

		// Label mutations, mirroring `kd label add/remove` (label.go). These run
		// after the core UpdateBead so a --title/--status change and a label edit
		// can be expressed in one invocation — the natural `kd update <id> --label x`
		// that agents reach for instead of the separate `kd label` subcommand.
		if cmd.Flags().Changed("add-label") {
			addLabels, _ := cmd.Flags().GetStringSlice("add-label")
			for _, label := range addLabels {
				if _, err := beadsClient.AddLabel(ctx, id, label); err != nil {
					return fmt.Errorf("adding label %q: %w", label, err)
				}
			}
		}
		if cmd.Flags().Changed("remove-label") {
			removeLabels, _ := cmd.Flags().GetStringSlice("remove-label")
			for _, label := range removeLabels {
				// `kd update --remove-label` is a declarative reconcile ("make
				// these labels absent"), so a 0-match is the desired end state,
				// not a failure — the rows-affected signal is intentionally
				// discarded here (ka-0000000424). Interactive `kd label remove`
				// is the surface that reports a no-op.
				if _, err := beadsClient.RemoveLabel(ctx, id, label); err != nil {
					return fmt.Errorf("removing label %q: %w", label, err)
				}
			}
		}
		// Re-fetch so the printed bead reflects the label mutations, which
		// UpdateBead's returned bead predates.
		if cmd.Flags().Changed("add-label") || cmd.Flags().Changed("remove-label") {
			if refreshed, err := beadsClient.GetBead(ctx, id); err == nil && refreshed != nil {
				bead = refreshed
			}
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			printBeadTable(bead)
		}
		return nil
	},
}

func init() {
	updateCmd.Flags().String("title", "", "bead title (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	updateCmd.Flags().String("short-title", "", model.ShortTitleHelp)
	updateCmd.Flags().String("heraldry", "", model.HeraldryHelp)
	updateCmd.Flags().Bool("only-if-short-title-empty", false, "with --short-title: set it only when the bead has no short_title yet (a fill, never a replace)")
	updateCmd.Flags().StringP("description", "d", "", "bead description (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	updateCmd.Flags().StringP("status", "s", "", "bead status")
	updateCmd.Flags().IntP("priority", "p", 0, "bead priority")
	updateCmd.Flags().String("assignee", "", "assignee")
	updateCmd.Flags().String("owner", "", "owner")
	updateCmd.Flags().String("notes", "", "notes (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	updateCmd.Flags().StringArrayP("field", "f", nil, "typed field (key=value, repeatable; key=@file reads value verbatim from file as a string, key=@- reads stdin). WARNING: on a SHARED AGGREGATE field (JSON map like channel_roles, CSV like slack_channel) this REPLACES the whole value, clobbering entries other agents/operators set — use --field-set/--field-append instead")
	updateCmd.Flags().StringArray("unset-field", nil, "delete a typed field entirely so it is absent, not empty/null (key, repeatable)")
	// Non-clobbering sub-field edits for shared aggregate fields (ka-0000007767).
	// Each targets ONE key/element via the daemon's atomic edit endpoint, so it
	// preserves entries other writers set — unlike bare -f, which replaces the
	// whole aggregate.
	updateCmd.Flags().StringArray("field-set", nil, "set ONE key of a JSON-map field without clobbering its other keys: <field>.<key>=<value> (e.g. channel_roles.C123=ops-lead; repeatable)")
	updateCmd.Flags().StringArray("field-unset-key", nil, "delete ONE key of a JSON-map field without clobbering its other keys: <field>.<key> (e.g. channel_roles.C123; repeatable)")
	updateCmd.Flags().StringArray("field-append", nil, "append ONE element to a CSV field without clobbering its other elements: <field>=<element> (e.g. slack_channel=C123; idempotent, repeatable)")
	updateCmd.Flags().StringArray("field-remove", nil, "remove ONE element from a CSV field without clobbering its other elements: <field>=<element> (e.g. slack_channel=C123; repeatable)")
	updateCmd.Flags().Bool("allow-empty", false, "allow -d/--description, --notes or a -f key= value to be set to an empty (or blank) string; without it kd update refuses, so a -d \"$(cat missing.md)\" that expanded to nothing cannot erase a bead's content (use -d @file instead, which fails on a missing file)")
	updateCmd.Flags().Bool("escalate", false, "request elevated access if write is denied")
	updateCmd.Flags().StringSlice("add-label", nil, "add a label — ONE per flag; repeat or comma-join for several ('--add-label a,b'). '--add-label a b' adds only 'a'. Same as kd label add")
	updateCmd.Flags().StringSlice("remove-label", nil, "remove a label — ONE per flag; repeat or comma-join for several ('--remove-label a,b'). '--remove-label a b' removes only 'a'. Same as kd label remove")
	aliasUpdateLabelFlags(updateCmd)
}

// aliasUpdateLabelFlags normalizes the label-flag spellings agents naturally
// reach for on `kd update` — `--label`/`--labels`/`--add-labels` → `--add-label`
// and `--remove-labels` → `--remove-label` — so the guess works instead of a
// bare cobra "unknown flag" usage dump (bug ka-0000002558). This mirrors
// aliasLabelFlags (label_alias.go, bug ka-0000000987) but targets update's
// add/remove split; it is attached PER COMMAND, never global, so no other
// command's flags are shadowed.
func aliasUpdateLabelFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "label", "labels", "add-labels":
			name = "add-label"
		case "remove-labels":
			name = "remove-label"
		}
		return pflag.NormalizedName(name)
	})
}

// refuseEmptyUpdate returns an error when the update would set the
// description, the notes or any -f field to an empty or whitespace-only
// string. fieldPairs are the raw -f key=value args; their resolved values are
// read back from req.Fields so a -f key=@file whose file is empty is caught
// too (gd-EgvTGAozYB).
func refuseEmptyUpdate(req *client.UpdateBeadRequest, fieldPairs []string) error {
	const hint = "a shell substitution like \"$(cat file)\" expands to nothing when the file is missing; pass @file instead (-d @body.md, -f key=@file), which fails on a missing file"
	for _, t := range []struct {
		flag string
		v    *string
	}{{"--description", req.Description}, {"--notes", req.Notes}} {
		if t.v != nil && strings.TrimSpace(*t.v) == "" {
			return fmt.Errorf("refusing to set %s to an empty value: %s. To blank it on purpose, add --allow-empty", t.flag, hint)
		}
	}
	if len(fieldPairs) == 0 || req.Fields == nil {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		return fmt.Errorf("decoding fields: %w", err)
	}
	for _, p := range fieldPairs {
		k, _, _ := splitField(p)
		if s, ok := fields[k].(string); ok && strings.TrimSpace(s) == "" {
			return fmt.Errorf("refusing to set field %q to an empty value: %s. To delete the field use --unset-field %s; to store an empty string on purpose, add --allow-empty", k, hint, k)
		}
	}
	return nil
}
