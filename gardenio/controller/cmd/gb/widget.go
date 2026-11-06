// Command surface for Incident Intelligence widgets.
//
// Widgets are modeled as two bead types (defined in kbeads `internal/server/config.go`):
//
//   - `widget-type`     — registered renderer; identified by `slug` (+ optional
//     `widget_version`); carries a JSON-Schema `schema` field.
//   - `widget-instance` — one concrete widget on a `rich-report` page; references
//     a `widget_type` slug and carries the rendered `data`
//     payload. Parented to the rich-report bead via the
//     typed-mention `parent(<rich-report-id>)`.
//
// The CLI is the agent-facing surface that gardener/crew use to land widgets
// without hand-rolling `kd create` invocations. See
// `docs/research/incident-intelligence/synthesis.md §Phase 2`.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var widgetCmd = &cobra.Command{
	Use:   "widget",
	Short: "DEPRECATED — manage Incident Intelligence widget types and instances",
	Long: `DEPRECATED (2026-08-02): widgets and reports are deprecated in favor of
bundles. Publish user-facing deliverables as a 'type=bundle' with 'type=doc'
children — see 'gb bundle create --help' and advice ka-0000007697.

Widget payloads are thin JSON blobs rendered into fixed cards, which pushed
authors toward filling required slots rather than communicating a finding. A
bundle carries prose you write for a human, in an order you choose.

The read-only subcommands (list/show/similar/lint/preview/validate) remain
useful for inspecting already-published widgets. The authoring subcommands
(emit, scaffold-htmx) warn and require --i-know-widgets-are-deprecated.

Create, register, and discover Incident Intelligence widgets.

Widgets back the rich-report rendering surface (see Phase 0/1 of the
incident-intelligence epic). Two bead types underpin the system:

  widget-type     — a registered renderer (slug + optional widget_version,
                    summary, JSON Schema describing the data payload).
  widget-instance — a concrete widget rendered on a rich-report page;
                    parented to the rich-report bead and carrying the
                    payload the renderer interprets.

Subcommands:
  emit     Create a widget-instance on a rich-report bead.
  list     List registered widget-types.
  show     Show a widget-type's schema, sample, and version history.
  similar  Find existing widget-types similar to a description.

Use 'gb widget similar' before drafting a new widget-type — reuse beats
reinvention.`,
	GroupID: "orchestration",
}

// ── widget emit ────────────────────────────────────────────────────────

var widgetEmitCmd = &cobra.Command{
	Use:   "emit <widget-type-slug>",
	Short: "Create a widget-instance on a rich-report bead",
	Long: `Create a widget-instance bead and parent it under a rich-report bead.

The slug must match an existing widget-type bead's 'slug' field. The data
payload is best-effort validated against the widget-type's JSON Schema
when one is registered; missing-required-field errors fail the command,
unknown-field warnings are printed but do not block.

Examples:
  # Inline payload
  gb widget emit incident-summary --report ka-99Ocl0 \
      --data '{"severity":"P2","headline":"db lag spike"}'

  # Payload from file
  gb widget emit incident-summary --report ka-99Ocl0 --data @summary.json

  # Pinned widget-version + slot position
  gb widget emit incident-summary --report ka-99Ocl0 --version 2 \
      --position 10 --headline "Incident Summary" --data @summary.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkWidgetDeprecated(cmd, "widget-instance"); err != nil {
			return err
		}
		slug := args[0]
		report, _ := cmd.Flags().GetString("report")
		dataArg, _ := cmd.Flags().GetString("data")
		version, _ := cmd.Flags().GetString("version")
		position, _ := cmd.Flags().GetInt("position")
		headline, _ := cmd.Flags().GetString("headline")
		title, _ := cmd.Flags().GetString("title")
		project, _ := cmd.Flags().GetString("project")
		skipValidation, _ := cmd.Flags().GetBool("skip-validation")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		draftOK, _ := cmd.Flags().GetBool("draft-ok")
		allowDeprecated, _ := cmd.Flags().GetBool("allow-deprecated")

		if report == "" {
			return fmt.Errorf("--report is required (rich-report bead id)")
		}
		if !(strings.HasPrefix(report, "gd-") || strings.HasPrefix(report, "ka-")) {
			return fmt.Errorf("--report must be a bead id like ka-0000001713, got %q", report)
		}

		// Resolve the widget-type bead (and verify it exists).
		wt, err := findWidgetType(cmd, slug, version)
		if err != nil {
			return err
		}

		// Stage gating: reject deprecated unless --allow-deprecated; warn
		// on draft unless --draft-ok.
		if wt != nil {
			stage := stageFromLabels(wt.Labels)
			switch stage {
			case "deprecated":
				if !allowDeprecated {
					replaced := wt.Fields["replaced_by"]
					if replaced != "" {
						return fmt.Errorf("widget-type %s is deprecated (replaced_by=%s); pass --allow-deprecated to override", slug, replaced)
					}
					return fmt.Errorf("widget-type %s is deprecated; pass --allow-deprecated to override", slug)
				}
				fmt.Fprintf(os.Stderr, "warning: emitting on deprecated widget-type %s (replaced_by=%s)\n", slug, wt.Fields["replaced_by"])
			case "draft":
				if !draftOK {
					return fmt.Errorf("widget-type %s is widget-stage:draft; pass --draft-ok to emit on a draft type", slug)
				}
				fmt.Fprintf(os.Stderr, "warning: emitting draft widget-type %s — instance will render with a DRAFT chip\n", slug)
			}
		}

		// Read the payload (inline JSON, @file, or - for stdin).
		payload, err := readDataArg(dataArg)
		if err != nil {
			return fmt.Errorf("reading --data: %w", err)
		}

		var dataAny any
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &dataAny); err != nil {
				return fmt.Errorf("--data is not valid JSON: %w", err)
			}
		}

		// Best-effort validate against the widget-type's schema.
		if !skipValidation {
			if msgs := validateAgainstSchema(wt, dataAny); len(msgs) > 0 {
				for _, m := range msgs {
					if m.fatal {
						return fmt.Errorf("payload validation failed: %s", m.msg)
					}
					fmt.Fprintf(os.Stderr, "warning: %s\n", m.msg)
				}
			}
		}

		// Resolve the rich-report bead so we can confirm the parent exists
		// before we create a dangling child. Best-effort: a 404 still creates
		// the bead but warns; non-404 errors are fatal so we don't create
		// orphans on transient issues.
		if rep, err := daemon.GetBead(cmd.Context(), report); err != nil {
			if isNotFound(err) {
				fmt.Fprintf(os.Stderr, "warning: rich-report bead %s not found; widget-instance will be created with a dangling parent reference\n", report)
			} else {
				return fmt.Errorf("checking rich-report bead %s: %w", report, err)
			}
		} else if rep != nil && rep.Type != "rich-report" {
			fmt.Fprintf(os.Stderr, "warning: parent %s is type=%s, not rich-report; rendering may not pick this up\n", report, rep.Type)
		}

		instanceTitle := title
		if instanceTitle == "" {
			if headline != "" {
				instanceTitle = fmt.Sprintf("%s — %s", slug, headline)
			} else {
				instanceTitle = fmt.Sprintf("%s widget on %s", slug, report)
			}
		}

		// Description carries the parent typed-mention so the kbeads
		// reference extractor records the parent-child edge.
		description := fmt.Sprintf("Widget instance for %s on rich-report parent(%s).", slug, report)

		fields := map[string]any{
			"widget_type": slug,
		}
		if version != "" {
			fields["widget_version"] = version
		}
		if position != 0 {
			fields["position"] = position
		}
		if headline != "" {
			fields["headline"] = headline
		}
		if dataAny != nil {
			fields["data"] = json.RawMessage(payload)
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		var labels []string
		if project != "" {
			labels = append(labels, "project:"+project)
		}

		if dryRun {
			out := map[string]any{
				"title":       instanceTitle,
				"type":        "widget-instance",
				"kind":        "data",
				"description": description,
				"labels":      labels,
				"fields":      fields,
			}
			if jsonOutput {
				printJSON(out)
			} else {
				fmt.Printf("[dry-run] would create widget-instance for widget_type=%s on rich-report %s\n", slug, report)
				fmt.Printf("  title:       %s\n", instanceTitle)
				if len(labels) > 0 {
					fmt.Printf("  labels:      %s\n", strings.Join(labels, ", "))
				}
				b, _ := json.MarshalIndent(fields, "  ", "  ")
				fmt.Printf("  fields:      %s\n", string(b))
			}
			return nil
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:       instanceTitle,
			Type:        "widget-instance",
			Kind:        "data",
			Description: description,
			Labels:      labels,
			CreatedBy:   actor,
			Fields:      fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating widget-instance: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"id":          id,
				"widget_type": slug,
				"version":     version,
				"report":      report,
				"position":    position,
				"headline":    headline,
			})
		} else {
			fmt.Printf("Created widget-instance %s (widget_type=%s) on rich-report %s\n", id, slug, report)
		}
		return nil
	},
}

// ── widget list ────────────────────────────────────────────────────────

var widgetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered widget-type beads",
	Long: `List widget-type beads. By default only widget-stage:stable beads
are shown — pass --all-stages to include drafts and deprecated, or
--stage <name> to filter to a single stage.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		stage, _ := cmd.Flags().GetString("stage")
		labels, _ := cmd.Flags().GetStringSlice("label")
		allStages, _ := cmd.Flags().GetBool("all-stages")
		withPreview, _ := cmd.Flags().GetBool("with-preview")

		var stageFilter string
		switch {
		case stage != "":
			stageFilter = stage
		case allStages:
			stageFilter = ""
		default:
			stageFilter = "stable"
		}

		statuses := []string{"open", "in_progress"}
		if all, _ := cmd.Flags().GetBool("all"); all {
			statuses = nil
		}

		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"widget-type"},
			Kinds:    []string{"data"},
			Labels:   labels,
			Statuses: statuses,
			Limit:    500,
		})
		if err != nil {
			return fmt.Errorf("listing widget-types: %w", err)
		}

		beads := result.Beads
		if stageFilter != "" {
			beads = filterByStage(beads, stageFilter)
		}

		// Stable display: sort by slug then version.
		sort.SliceStable(beads, func(i, j int) bool {
			si, sj := beads[i].Fields["slug"], beads[j].Fields["slug"]
			if si != sj {
				return si < sj
			}
			return beads[i].Fields["widget_version"] < beads[j].Fields["widget_version"]
		})

		if jsonOutput {
			printJSON(beads)
			return nil
		}
		printWidgetTypeListExtended(beads, len(beads), withPreview)
		return nil
	},
}

// ── widget show ────────────────────────────────────────────────────────

var widgetShowCmd = &cobra.Command{
	Use:   "show <slug>",
	Short: "Show a widget-type's schema, sample payload, and version history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		slug := args[0]
		version, _ := cmd.Flags().GetString("version")

		// Walk all matching widget-types so we can show the version timeline
		// along with the chosen one.
		all, err := listWidgetTypesBySlug(cmd, slug)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return fmt.Errorf("no widget-type with slug %q", slug)
		}

		picked := pickWidgetTypeVersion(all, version)
		if picked == nil {
			return fmt.Errorf("no widget-type %q with version %q (have: %s)", slug, version, formatVersions(all))
		}

		if jsonOutput {
			printJSON(map[string]any{
				"picked":   picked,
				"versions": all,
			})
			return nil
		}

		fmt.Printf("ID:        %s\n", picked.ID)
		fmt.Printf("Slug:      %s\n", picked.Fields["slug"])
		if v := picked.Fields["widget_version"]; v != "" {
			fmt.Printf("Version:   %s\n", v)
		}
		fmt.Printf("Status:    %s\n", picked.Status)
		if v := picked.Title; v != "" {
			fmt.Printf("Title:     %s\n", v)
		}
		if v := picked.Fields["summary"]; v != "" {
			fmt.Printf("Summary:   %s\n", v)
		}
		if len(picked.Labels) > 0 {
			fmt.Printf("Labels:    %s\n", strings.Join(picked.Labels, ", "))
		}

		fmt.Println()
		fmt.Println("Schema:")
		if v := picked.Fields["schema"]; v != "" && v != "null" {
			fmt.Println(prettyJSON(v))
		} else {
			fmt.Println("  (none registered — payload validation will be skipped)")
		}

		if sample := sampleFromSchema(picked); sample != "" {
			fmt.Println()
			fmt.Println("Sample payload (scaffolded from schema):")
			fmt.Println(sample)
		}

		// Free-htmx template body: print fenced for direct copy/paste.
		if picked.Fields["renderer_kind"] == "htmx" {
			tpl := picked.Fields["template_source"]
			if strings.TrimSpace(tpl) != "" {
				fmt.Println()
				fmt.Println("Template (renderer_kind=htmx):")
				fmt.Println("```html")
				fmt.Print(tpl)
				if !strings.HasSuffix(tpl, "\n") {
					fmt.Println()
				}
				fmt.Println("```")
			}
		}

		if len(all) > 1 {
			fmt.Println()
			fmt.Println("Version history:")
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "  VERSION\tID\tSTATUS\tTITLE\n")
			for _, v := range all {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
					stringOr(v.Fields["widget_version"], "(unset)"),
					v.ID,
					v.Status,
					v.Title,
				)
			}
			tw.Flush()
		}

		return nil
	},
}

// ── widget similar ─────────────────────────────────────────────────────

var widgetSimilarCmd = &cobra.Command{
	Use:   "similar <description>",
	Short: "Find existing widget-types similar to a description",
	Long: `Search existing widget-types semantically before drafting a new one.

Reuse beats reinvention; the catalog stays small if agents land payloads
into existing renderers when the shape fits.

If the embedder is disabled (BEADS_EMBED_BACKEND=noop) this falls back
to a lexical search over widget-type beads.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := args[0]
		limit, _ := cmd.Flags().GetInt("limit")
		if limit <= 0 {
			limit = 10
		}

		includeDraft, _ := cmd.Flags().GetBool("include-draft")
		includeDeprecated, _ := cmd.Flags().GetBool("include-deprecated")

		filterStages := func(in []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
			out := make([]*beadsapi.BeadDetail, 0, len(in))
			for _, b := range in {
				stage := stageFromLabels(b.Labels)
				switch stage {
				case "deprecated":
					if includeDeprecated {
						out = append(out, b)
					}
				case "draft":
					if includeDraft {
						out = append(out, b)
					}
				default:
					// stable or unstaged — include
					out = append(out, b)
				}
			}
			return out
		}

		// Try semantic search first.
		semResp, semErr := daemon.SemanticSearch(cmd.Context(), beadsapi.SemanticSearchRequest{
			Query: query,
			Type:  []string{"widget-type"},
			Kind:  []string{"data"},
			Limit: limit,
		})
		if semErr == nil && len(semResp.Results) > 0 {
			beads := make([]*beadsapi.BeadDetail, 0, len(semResp.Results))
			for _, r := range semResp.Results {
				beads = append(beads, r.Bead)
			}
			beads = filterStages(beads)
			if jsonOutput {
				printJSON(semResp.Results)
				return nil
			}
			if len(beads) == 0 {
				fmt.Println("No widget-types matched (after stage filter). Pass --include-draft / --include-deprecated to widen.")
				return nil
			}
			fmt.Printf("Semantic matches (model=%s):\n", semResp.Model)
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "ID\tSCORE\tSLUG\tVERSION\tSTAGE\tTITLE\n")
			scoreByID := map[string]float64{}
			for _, r := range semResp.Results {
				scoreByID[r.Bead.ID] = r.Score
			}
			for _, b := range beads {
				fmt.Fprintf(tw, "%s\t%.2f\t%s\t%s\t%s\t%s\n",
					b.ID, scoreByID[b.ID],
					stringOr(b.Fields["slug"], "(no slug)"),
					stringOr(b.Fields["widget_version"], ""),
					stringOr(stageFromLabels(b.Labels), "stable"),
					b.Title,
				)
			}
			tw.Flush()
			return nil
		}

		// Fallback: lexical filter.
		if semErr != nil {
			fmt.Fprintf(os.Stderr, "semantic search unavailable (%v); falling back to lexical match\n", semErr)
		}
		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"widget-type"},
			Kinds:    []string{"data"},
			Search:   query,
			Statuses: []string{"open", "in_progress"},
			Limit:    limit,
		})
		if err != nil {
			return fmt.Errorf("lexical search: %w", err)
		}

		filtered := filterStages(result.Beads)
		if jsonOutput {
			printJSON(filtered)
			return nil
		}
		if len(filtered) == 0 {
			fmt.Println("No widget-types matched. Consider drafting a new widget-type bead.")
			return nil
		}
		fmt.Println("Lexical matches:")
		printWidgetTypeListExtended(filtered, len(filtered), false)
		return nil
	},
}

// ── helpers ────────────────────────────────────────────────────────────

// widgetDeprecationFlag is the opt-out every widget *authoring* command
// requires. Read-only subcommands (list/show/similar/lint/preview/validate)
// are unaffected — inspecting already-published widgets stays free.
const widgetDeprecationFlag = "i-know-widgets-are-deprecated"

// checkWidgetDeprecated blocks widget-authoring commands unless the caller
// explicitly acknowledges the deprecation.
//
// Operator directive 2026-08-02 (Matt Baker, Slack thread ka-0000004862):
// "formally deprecate the use of widgets and reports in favor of bundles" —
// low-quality widget-based reports were reaching operators. A widget instance
// is a thin JSON payload in a fixed card, so authors filled required slots
// instead of communicating a finding. Bundles carry prose instead.
//
// This fails LOUD rather than warning-and-continuing: a warning on stderr is
// invisible to an agent that only checks the exit code, and the whole point is
// to stop new widget beads from being created. Tracking: ka-0000007561.
func checkWidgetDeprecated(cmd *cobra.Command, creates string) error {
	if ok, _ := cmd.Flags().GetBool(widgetDeprecationFlag); ok {
		fmt.Fprintf(os.Stderr,
			"warning: %s is deprecated (creates a %s bead); prefer 'gb bundle create --kind report'\n",
			cmd.CommandPath(), creates)
		return nil
	}
	return fmt.Errorf(`%s is DEPRECATED and creates a %s bead.

Widgets and reports are deprecated in favor of bundles (operator directive
2026-08-02, advice ka-0000007697). Publish the deliverable as a bundle:

  gb bundle create <origin-bead-id> --title "..." --kind report \
      --headline "one-line takeaway" --summary "the paragraph a VP reads"
  gb bundle doc <bundle-id> --title "Root cause" --format md --content @rca.md

If you genuinely still need a widget (maintaining an already-published
widget surface), re-run with --%s`,
		cmd.CommandPath(), creates, widgetDeprecationFlag)
}

func init() {
	rootCmd.AddCommand(widgetCmd)
	// Authoring
	widgetCmd.AddCommand(widgetScaffoldHTMXCmd)
	widgetCmd.AddCommand(widgetLintCmd)
	widgetCmd.AddCommand(widgetPreviewCmd)
	widgetCmd.AddCommand(widgetValidateCmd)
	widgetCmd.AddCommand(widgetEmitCmd)
	// Lifecycle (operator-only)
	widgetCmd.AddCommand(widgetPublishCmd)
	widgetCmd.AddCommand(widgetPromoteCmd)
	widgetCmd.AddCommand(widgetDeprecateCmd)
	// Discovery
	widgetCmd.AddCommand(widgetListCmd)
	widgetCmd.AddCommand(widgetShowCmd)
	widgetCmd.AddCommand(widgetSimilarCmd)

	// Deprecation opt-out on the two authoring commands that create beads.
	widgetEmitCmd.Flags().Bool(widgetDeprecationFlag, false, "acknowledge the widget deprecation and emit anyway")
	widgetScaffoldHTMXCmd.Flags().Bool(widgetDeprecationFlag, false, "acknowledge the widget deprecation and scaffold anyway")

	// Emit
	widgetEmitCmd.Flags().String("report", "", "rich-report bead id (gd-…) to parent the widget-instance under (required)")
	widgetEmitCmd.Flags().String("data", "", "payload as inline JSON, @file.json, or - for stdin")
	widgetEmitCmd.Flags().String("version", "", "widget version to pin (matches widget-type.widget_version)")
	widgetEmitCmd.Flags().Int("position", 0, "display order among siblings on the rich-report (lower first)")
	widgetEmitCmd.Flags().String("headline", "", "short headline rendered above the widget body")
	widgetEmitCmd.Flags().String("title", "", "override the bead title (defaults to '<slug> — <headline>')")
	widgetEmitCmd.Flags().String("project", "", "shorthand for --label project:<value>")
	widgetEmitCmd.Flags().Bool("skip-validation", false, "skip best-effort JSON-Schema validation against the widget-type")
	widgetEmitCmd.Flags().Bool("dry-run", false, "show the widget-instance bead body without creating it")
	widgetEmitCmd.Flags().Bool("draft-ok", false, "allow emitting against widget-stage:draft types (instance renders with a DRAFT chip)")
	widgetEmitCmd.Flags().Bool("allow-deprecated", false, "allow emitting against widget-stage:deprecated types")
	_ = widgetEmitCmd.MarkFlagRequired("report")

	// List
	widgetListCmd.Flags().String("stage", "", "filter by widget stage label (draft|stable|deprecated)")
	widgetListCmd.Flags().StringSliceP("label", "l", nil, "filter by label (repeatable)")
	widgetListCmd.Flags().Bool("all", false, "include closed widget-type beads")
	widgetListCmd.Flags().Bool("all-stages", false, "include all stages (default lists only widget-stage:stable)")
	widgetListCmd.Flags().Bool("with-preview", false, "include a one-line preview URL hint per row")

	// Show
	widgetShowCmd.Flags().String("version", "", "specific version to show (defaults to highest)")

	// Similar
	widgetSimilarCmd.Flags().Int("limit", 10, "max number of matches to return")
	widgetSimilarCmd.Flags().Bool("include-draft", false, "include widget-stage:draft types in matches")
	widgetSimilarCmd.Flags().Bool("include-deprecated", false, "include widget-stage:deprecated types in matches")

	// Scaffold-htmx
	widgetScaffoldHTMXCmd.Flags().String("out", "", "write the scaffolded artifact to this file (default: stdout)")
	widgetScaffoldHTMXCmd.Flags().String("in", "", "read an artifact from this file when --commit is set (default: stdin)")
	widgetScaffoldHTMXCmd.Flags().Bool("commit", false, "create a widget-type bead from the artifact instead of printing")
	widgetScaffoldHTMXCmd.Flags().String("summary", "", "short summary used in the Summary section")
	widgetScaffoldHTMXCmd.Flags().String("with-fields", "", "comma-separated 'name:type' field list (string|integer|number|boolean|array|object)")
	widgetScaffoldHTMXCmd.Flags().Bool("with-live", false, "scaffold an hx-trigger='load, every 30s' live-poll panel")
	widgetScaffoldHTMXCmd.Flags().Bool("with-action", false, "scaffold a Refresh button using actionURL")
	widgetScaffoldHTMXCmd.Flags().String("from", "", "copy schema + template from an existing widget-type slug")
	widgetScaffoldHTMXCmd.Flags().String("project", "", "label project:<value> on the new widget-type bead (--commit)")
	widgetScaffoldHTMXCmd.Flags().String("version", "", "widget_version field value (default \"1\")")

	// Lint (no extra flags beyond global --json)

	// Preview
	widgetPreviewCmd.Flags().String("payload", "", "JSON payload for render (inline, @file, -; defaults to the doc's Sample section)")
	widgetPreviewCmd.Flags().String("out", "", "write rendered HTML to this file (default: stdout)")
	widgetPreviewCmd.Flags().Bool("browser", false, "print a one-shot beads-viewer URL instead of rendering locally")
	widgetPreviewCmd.Flags().Bool("operator", false, "request an operator-scoped preview URL (for --browser)")
	widgetPreviewCmd.Flags().Bool("no-sanitize", false, "skip the FreeHtmxPolicy sanitizer (diagnostic only)")
	widgetPreviewCmd.Flags().Bool("with-csp", false, "wrap output with the FreeHtmxPolicy CSP headers")

	// Validate
	widgetValidateCmd.Flags().Bool("strict", false, "enforce the publish gate (widget_version, summary, sample-schema, sample-render)")
	widgetValidateCmd.Flags().Bool("json", false, "emit the structured result on stdout")

	// Publish (operator-only)
	widgetPublishCmd.Flags().String("note", "", "note recorded in the audit comment")
	widgetPublishCmd.Flags().Bool("force", false, "skip validate --strict (still records an audit note)")

	// Promote (operator-only) — canonical Stage-B alias for publish.
	widgetPromoteCmd.Flags().String("note", "", "note recorded in the audit comment")
	widgetPromoteCmd.Flags().Bool("force", false, "skip validate --strict (still records an audit note)")

	// Deprecate (operator-only)
	widgetDeprecateCmd.Flags().String("replaced-by", "", "slug of the replacement widget-type (required)")
	widgetDeprecateCmd.Flags().String("note", "", "operator rationale recorded in the audit comment (required)")
	_ = widgetDeprecateCmd.MarkFlagRequired("replaced-by")
	_ = widgetDeprecateCmd.MarkFlagRequired("note")
}

// stageFromLabels returns the first widget-stage:* suffix found in
// labels, or "" when no stage label is set.
func stageFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "widget-stage:"); ok {
			return v
		}
	}
	return ""
}

// filterByStage keeps only beads whose widget-stage:* label matches stage.
// Beads without a stage label are dropped when stage is non-empty.
func filterByStage(beads []*beadsapi.BeadDetail, stage string) []*beadsapi.BeadDetail {
	out := make([]*beadsapi.BeadDetail, 0, len(beads))
	for _, b := range beads {
		if stageFromLabels(b.Labels) == stage {
			out = append(out, b)
		}
	}
	return out
}

// printWidgetTypeListExtended is the same as printWidgetTypeList but adds
// a KIND column (htmx|builtin|—) and an optional preview URL hint.
func printWidgetTypeListExtended(beads []*beadsapi.BeadDetail, total int, withPreview bool) {
	if len(beads) == 0 {
		fmt.Println("No widget-type beads found.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "ID\tSLUG\tVERSION\tSTAGE\tKIND\tSCHEMA\tTITLE"
	if withPreview {
		header += "\tPREVIEW"
	}
	fmt.Fprintln(tw, header)
	previewBase := strings.TrimRight(os.Getenv("BEADS_VIEWER_URL"), "/")
	for _, b := range beads {
		schema := "—"
		if v := b.Fields["schema"]; v != "" && v != "null" {
			schema = "✓"
		}
		kind := stringOr(b.Fields["renderer_kind"], "—")
		stage := stringOr(stageFromLabels(b.Labels), "—")
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			b.ID,
			stringOr(b.Fields["slug"], "(no slug)"),
			stringOr(b.Fields["widget_version"], "—"),
			stage,
			kind,
			schema,
			b.Title,
		)
		if withPreview {
			url := fmt.Sprintf("/widget-types/%s", stringOr(b.Fields["slug"], b.ID))
			if previewBase != "" {
				url = previewBase + url
			}
			row += "\t" + url
		}
		fmt.Fprintln(tw, row)
	}
	tw.Flush()
	fmt.Printf("\n%d widget-type(s) (%d total)\n", len(beads), total)
}

// readDataArg expands a --data flag into raw JSON bytes. Supports inline
// JSON, @file paths, and "-" for stdin.
func readDataArg(arg string) ([]byte, error) {
	if arg == "" {
		return nil, nil
	}
	if arg == "-" {
		return io.ReadAll(os.Stdin)
	}
	if path, ok := strings.CutPrefix(arg, "@"); ok {
		return os.ReadFile(path)
	}
	return []byte(arg), nil
}

// findWidgetType resolves a (slug, version) pair to a widget-type bead.
// Empty version picks the highest-versioned open bead.
func findWidgetType(cmd *cobra.Command, slug, version string) (*beadsapi.BeadDetail, error) {
	all, err := listWidgetTypesBySlug(cmd, slug)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		// Don't fail emit — agents may emit ahead of registration. Surface
		// a warning and return nil so the validator no-ops.
		fmt.Fprintf(os.Stderr, "warning: no widget-type registered for slug %q — payload validation will be skipped\n", slug)
		return nil, nil
	}
	picked := pickWidgetTypeVersion(all, version)
	if picked == nil {
		return nil, fmt.Errorf("widget-type %q has no version %q (have: %s)", slug, version, formatVersions(all))
	}
	return picked, nil
}

// listWidgetTypesBySlug returns every widget-type bead matching slug,
// ordered by widget_version (lexicographic — fine for v1 because versions
// are integer strings).
func listWidgetTypesBySlug(cmd *cobra.Command, slug string) ([]*beadsapi.BeadDetail, error) {
	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types: []string{"widget-type"},
		Kinds: []string{"data"},
		Limit: 500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing widget-types: %w", err)
	}
	out := make([]*beadsapi.BeadDetail, 0, 4)
	for _, b := range result.Beads {
		if b.Fields["slug"] == slug {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Fields["widget_version"] < out[j].Fields["widget_version"]
	})
	return out, nil
}

// pickWidgetTypeVersion picks the matching version from a sorted slice.
// Empty version picks the last (highest) entry.
func pickWidgetTypeVersion(all []*beadsapi.BeadDetail, version string) *beadsapi.BeadDetail {
	if len(all) == 0 {
		return nil
	}
	if version == "" {
		return all[len(all)-1]
	}
	for _, b := range all {
		if b.Fields["widget_version"] == version {
			return b
		}
	}
	return nil
}

func formatVersions(all []*beadsapi.BeadDetail) string {
	vs := make([]string, 0, len(all))
	for _, b := range all {
		vs = append(vs, stringOr(b.Fields["widget_version"], "(unset)"))
	}
	return strings.Join(vs, ", ")
}

// validationMsg captures one schema-validation finding. fatal=true halts
// emit; otherwise it's surfaced as a warning.
type validationMsg struct {
	msg   string
	fatal bool
}

// validateAgainstSchema is a deliberately small JSON-Schema subset:
// it enforces `required`, type for top-level fields, and warns on
// additional properties. Anything more elaborate belongs server-side
// once the registry is shared between renderers and CLI.
func validateAgainstSchema(wt *beadsapi.BeadDetail, payload any) []validationMsg {
	if wt == nil {
		return nil
	}
	raw := wt.Fields["schema"]
	if raw == "" || raw == "null" {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		return []validationMsg{{msg: fmt.Sprintf("widget-type %s schema is not valid JSON: %v", wt.ID, err)}}
	}
	obj, ok := payload.(map[string]any)
	if !ok {
		// Top-level non-object payloads are accepted unconditionally —
		// schemas in v1 only constrain object payloads.
		return nil
	}

	var msgs []validationMsg

	// required: ["a","b"]
	if reqRaw, ok := schema["required"].([]any); ok {
		for _, r := range reqRaw {
			name, _ := r.(string)
			if name == "" {
				continue
			}
			if _, present := obj[name]; !present {
				msgs = append(msgs, validationMsg{
					msg:   fmt.Sprintf("missing required field %q", name),
					fatal: true,
				})
			}
		}
	}

	// properties: {a: {type: "string"}, ...}
	if props, ok := schema["properties"].(map[string]any); ok {
		// Type check known fields.
		for name, def := range props {
			val, present := obj[name]
			if !present {
				continue
			}
			defMap, ok := def.(map[string]any)
			if !ok {
				continue
			}
			if want, ok := defMap["type"].(string); ok {
				if !jsonTypeMatches(val, want) {
					msgs = append(msgs, validationMsg{
						msg:   fmt.Sprintf("field %q: expected type %s, got %T", name, want, val),
						fatal: true,
					})
				}
			}
		}
		// Warn on unknown properties when the schema declares any properties
		// at all (otherwise we can't tell intent).
		additional := schema["additionalProperties"]
		if additional == nil || additional == true {
			// Permissive: don't warn.
		} else {
			for name := range obj {
				if _, ok := props[name]; !ok {
					msgs = append(msgs, validationMsg{
						msg: fmt.Sprintf("field %q is not declared by the widget-type schema", name),
					})
				}
			}
		}
	}

	return msgs
}

func jsonTypeMatches(v any, want string) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	// Unknown / unsupported type keyword — accept.
	return true
}

// sampleFromSchema produces a small JSON object that satisfies the schema's
// `required` list, populated with `example` values when present and
// type-appropriate zero values otherwise. Best-effort scaffolding only.
func sampleFromSchema(wt *beadsapi.BeadDetail) string {
	if wt == nil {
		return ""
	}
	raw := wt.Fields["schema"]
	if raw == "" || raw == "null" {
		return ""
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		return ""
	}
	out := map[string]any{}
	required, _ := schema["required"].([]any)
	props, _ := schema["properties"].(map[string]any)
	for _, r := range required {
		name, _ := r.(string)
		if name == "" {
			continue
		}
		var def map[string]any
		if props != nil {
			def, _ = props[name].(map[string]any)
		}
		out[name] = sampleValueFromDef(def)
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(out, "  ", "  ")
	if err != nil {
		return ""
	}
	return "  " + string(b)
}

func sampleValueFromDef(def map[string]any) any {
	if def == nil {
		return nil
	}
	if ex, ok := def["example"]; ok {
		return ex
	}
	switch def["type"] {
	case "string":
		return ""
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return nil
}

func stringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func prettyJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "  " + raw
	}
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return "  " + raw
	}
	return "  " + string(b)
}

// isNotFound returns true when err looks like a daemon 404. The beadsapi
// client wraps errors with surrounding context, so we sniff for the
// status-code substring rather than asserting on a typed error to keep
// this best-effort.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") || strings.Contains(s, "not found")
}
