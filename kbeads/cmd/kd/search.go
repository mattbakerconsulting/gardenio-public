package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search beads by text query with field filters",
	Long: `Search beads by text query, field filters, and structured output.

Unlike 'kd list', search does NOT inject a default kind=issue filter, so
escalation and decision beads (kind=data) are already eligible in results —
there is no --include-operational flag here. Add -s open to restrict to open
operational beads, or -t escalation -t decision to see only those types.

Examples:
  kd search "deployment issue"                    # text search
  kd search -t agent --where project=gardenio      # field equality
  kd search -t agent --where project=             # field is empty/absent
  kd search -t agent --fields project,role,mode   # custom columns
  kd search -s '!closed'                          # exclude closed beads
  kd search -t escalation -t decision -s open     # open operational beads
  kd search --semantic "failed login retries"     # vector similarity search
  kd search --semantic "auth" --min-score 0.6     # drop low-confidence matches
  kd search --mentions ka-0000004973               # beads whose text references this id`,
	GroupID: "beads",
	Args:    cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.Join(args, " ")
		statusFlags, _ := cmd.Flags().GetStringSlice("status")
		beadType, _ := cmd.Flags().GetStringSlice("type")
		kind, _ := cmd.Flags().GetStringSlice("kind")
		limit, _ := cmd.Flags().GetInt("limit")
		whereFlags, _ := cmd.Flags().GetStringSlice("where")
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		labels, _ := cmd.Flags().GetStringSlice("label")
		labelsAny, _ := cmd.Flags().GetStringSlice("label-any")
		assignee, _ := cmd.Flags().GetString("assignee")

		// Same pipe-OR footgun guard as `kd list` (ka-0000002308): a "|" in a
		// label value matches literally and never ORs, so warn (without aborting)
		// and point at --label-any. Shared helper keeps the guard off list-only.
		warnOnPipeLabels(os.Stderr, "-l/--label", labels)
		warnOnPipeLabels(os.Stderr, "--label-any", labelsAny)
		sort, _ := cmd.Flags().GetString("sort")
		semantic, _ := cmd.Flags().GetBool("semantic")
		minScore, _ := cmd.Flags().GetFloat64("min-score")
		mentions, _ := cmd.Flags().GetString("mentions")

		// Validate --type against the known type registry so an unknown value
		// fails loud instead of silently matching nothing (exit 0) — mirroring
		// the create path's "unknown bead type" 400 (ka-0000006244). Applies to
		// both the structured and --semantic search paths below. Reassign
		// beadType to the NORMALIZED (trimmed) values: the server matches the
		// type column verbatim, so the raw pflag splits would silently miss.
		beadType, err := validateTypeFilter(cmd.Context(), beadType)
		if err != nil {
			return err
		}

		// Process status negation: "!closed" → all statuses except "closed".
		statuses, err := resolveStatuses(statusFlags)
		if err != nil {
			return err
		}

		if semantic {
			if query == "" {
				return fmt.Errorf("--semantic requires a non-empty query")
			}
			return runSemanticSearch(cmd.Context(), &client.SemanticSearchRequest{
				Query:    query,
				Status:   statuses,
				Type:     beadType,
				Kind:     kind,
				Labels:   labels,
				Assignee: assignee,
				Limit:    limit,
				MinScore: minScore,
			}, fieldsFlag)
		}

		// Parse --where flags into field filters.
		fieldFilters := parseWhereFlags(whereFlags)

		req := &client.ListBeadsRequest{
			Search:       query,
			Status:       statuses,
			Type:         beadType,
			Kind:         kind,
			Labels:       labels,
			LabelsAny:    labelsAny,
			Assignee:     assignee,
			Sort:         sort,
			FieldFilters: fieldFilters,
			Limit:        limit,
			Mentions:     mentions,
		}

		resp, err := beadsClient.ListBeads(cmd.Context(), req)
		if err != nil {
			return fmt.Errorf("searching beads: %w", err)
		}
		recordSeenBeadIDs(listedBeadIDs(resp.Beads))

		// A leading relevance SCORE column is shown only when a text search is
		// active (query non-empty) — that is when the daemon populates
		// model.Bead.RelevanceScore (ka-0000003832 / Option 2). Pure
		// field-filter listings (`kd search -t agent --where …`) carry no score
		// and render unchanged. JSON always includes relevance_score via the
		// model json tag (omitted when zero), so the JSON path is unconditional.
		showScore := query != ""

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else if fieldsFlag != "" {
			columns := strings.Split(fieldsFlag, ",")
			if showScore {
				printBeadListCustomWithScore(resp.Beads, resp.Total, columns)
			} else {
				printBeadListCustom(resp.Beads, resp.Total, columns)
			}
		} else if showScore {
			printBeadListTableWithScore(resp.Beads, resp.Total)
		} else {
			printBeadListTable(resp.Beads, resp.Total)
		}

		// When a text lexical search returns 0 rows, nudge toward recovery.
		// Lexical search ANDs every literal token, so a long natural-language
		// query can silently intersect to empty even though each token matches
		// plenty of beads individually (ka-0000007281). A weaker reader stops at
		// "0 beads -> no prior art"; the hint converts that dead-end into a
		// recovery path. Gated to human (non-JSON) output with an actual query,
		// and suppressed under --semantic (that path returns earlier anyway, and
		// its own empty message already reads as terminal).
		//
		// The message is tailored to the LIKELY cause so it does not misattribute
		// the zero (ka-0000003751): the "requires ALL terms" advice only holds
		// for a multi-token query, and a structured filter (--where/--type/…)
		// alongside a non-empty query is a distinct culprit — the query alone
		// might match plenty, so pointing at term-dropping sends the reader the
		// wrong way. --semantic stays offered in every case.
		if !jsonOutput && query != "" && len(resp.Beads) == 0 {
			hasFilter := len(statuses) > 0 || len(beadType) > 0 || len(kind) > 0 ||
				len(labels) > 0 || len(labelsAny) > 0 || assignee != "" ||
				len(fieldFilters) > 0 || mentions != ""
			var hint string
			switch {
			case hasFilter:
				hint = "hint: 0 matches — a filter (--where/--status/--type/--label/…), not the query terms, may have emptied this. Try relaxing filters, or --semantic for fuzzy/conceptual matching."
			case len(strings.Fields(query)) > 1:
				hint = "hint: 0 matches — lexical search requires ALL terms. Try fewer terms, or --semantic for fuzzy/conceptual matching."
			default:
				hint = "hint: 0 matches. Try --semantic for fuzzy/conceptual matching."
			}
			fmt.Fprintln(os.Stderr, hint)
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().StringSliceP("status", "s", nil, "filter by status (repeatable, prefix ! to negate)")
	searchCmd.Flags().StringSliceP("type", "t", nil, "filter by type (repeatable)")
	searchCmd.Flags().StringSliceP("kind", "k", nil, "filter by kind (repeatable)")
	searchCmd.Flags().StringSliceP("label", "l", nil, "filter by label — ANDed: a bead must carry ALL given labels (repeatable/comma). For OR/any-of use --label-any.")
	searchCmd.Flags().StringSlice("label-any", nil, "filter by ANY of these labels (OR; repeatable/comma). Contrast -l/--label which ANDs. Ignored with --semantic.")
	searchCmd.Flags().StringSliceP("where", "w", nil, "field filter as key=value (repeatable, empty value matches absent)")
	searchCmd.Flags().String("fields", "", "comma-separated columns to display (e.g. id,title,project,role)")
	searchCmd.Flags().String("assignee", "", "filter by assignee")
	searchCmd.Flags().String("sort", "", "sort by column (prefix - for descending, e.g. -priority)")
	searchCmd.Flags().Int("limit", 20, "maximum number of results")
	searchCmd.Flags().Bool("semantic", false, "use vector similarity search (POST /v1/search/semantic)")
	searchCmd.Flags().Float64("min-score", 0, "with --semantic, drop results below this cosine similarity (0-1)")
	searchCmd.Flags().String("mentions", "", "filter to beads whose text references this bead id (e.g. ka-0000004973)")
	// Accept the pluralized --labels / --labels-any spellings as aliases of
	// --label / --label-any so the common agent typo no longer aborts with a
	// bare "unknown flag" that reads as a false all-clear (ka-0000000987).
	aliasLabelFlags(searchCmd)
}

// runSemanticSearch dispatches the CLI search request to the semantic-search
// endpoint and renders the scored results. Structured filters (status, type,
// kind, labels, assignee) are forwarded; --where / --sort are ignored because
// the semantic endpoint does not accept arbitrary field filters or sort keys.
func runSemanticSearch(ctx context.Context, req *client.SemanticSearchRequest, fieldsFlag string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := beadsClient.SemanticSearch(ctx, req)
	if err != nil {
		return fmt.Errorf("semantic search: %w", err)
	}
	if jsonOutput {
		data, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	if len(resp.Results) == 0 {
		fmt.Fprintln(os.Stdout, "No matching beads found.")
		return nil
	}
	if fieldsFlag != "" {
		columns := strings.Split(fieldsFlag, ",")
		printSemanticResultsCustom(resp.Results, columns)
	} else {
		printSemanticResultsTable(resp)
	}
	return nil
}

// printSemanticResultsTable prints semantic-search results with a similarity
// score as the leading column.
func printSemanticResultsTable(resp *client.SemanticSearchResponse) {
	beads := make([]*model.Bead, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r != nil && r.Bead != nil {
			beads = append(beads, r.Bead)
		}
	}
	showArms := anyArms(beads)
	wide := newWideCells(os.Stdout)
	w := tabwriter.NewWriter(wide, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tID\tSTATUS\tTYPE\tPRIORITY\t"+armsHeader(showArms)+"TITLE\tASSIGNEE")
	count := 0
	for _, r := range resp.Results {
		if r == nil || r.Bead == nil {
			continue
		}
		b := r.Bead
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%d\t%s%s\t%s\n",
			r.Score, b.ID, b.Status, b.Type, b.Priority, armsCell(wide, showArms, b), title, b.Assignee)
		count++
	}
	w.Flush()
	wide.Flush()
	if resp.Model != "" {
		fmt.Printf("\n%d semantic results (model: %s)\n", count, resp.Model)
	} else {
		fmt.Printf("\n%d semantic results\n", count)
	}
}

// printSemanticResultsCustom prints semantic-search results with user-selected
// columns, always prepending a "score" column.
func printSemanticResultsCustom(results []*client.SemanticSearchResult, columns []string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	headers := append([]string{"SCORE"}, make([]string, len(columns))...)
	for i, c := range columns {
		headers[i+1] = strings.ToUpper(strings.TrimSpace(c))
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, r := range results {
		if r == nil || r.Bead == nil {
			continue
		}
		vals := make([]string, len(columns)+1)
		vals[0] = fmt.Sprintf("%.3f", r.Score)
		for i, c := range columns {
			vals[i+1] = beadColumnValue(r.Bead, strings.TrimSpace(c))
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	w.Flush()
	fmt.Printf("\n%d semantic results\n", len(results))
}

// allStatuses is the set of valid bead statuses. It is the completeness basis
// for "!status" negation in resolveStatuses, so it MUST list every status
// model.Status.IsValid accepts — otherwise a negation like "!closed" silently
// drops beads in an omitted status (e.g. blocked/partial). See ka-0000006476.
var allStatuses = []string{"open", "in_progress", "deferred", "blocked", "partial", "closed"}

// resolveStatuses handles status negation. A status prefixed with "!" means
// "all statuses except this one". Multiple negations are AND'd together.
func resolveStatuses(flags []string) ([]string, error) {
	var positive, negative []string
	for _, s := range flags {
		if strings.HasPrefix(s, "!") {
			negative = append(negative, strings.TrimPrefix(s, "!"))
		} else {
			positive = append(positive, s)
		}
	}
	if len(negative) > 0 && len(positive) > 0 {
		return nil, fmt.Errorf("cannot mix positive and negated statuses")
	}
	if len(negative) > 0 {
		excluded := make(map[string]bool, len(negative))
		for _, n := range negative {
			excluded[n] = true
		}
		var result []string
		for _, s := range allStatuses {
			if !excluded[s] {
				result = append(result, s)
			}
		}
		return result, nil
	}
	return positive, nil
}

// parseWhereFlags parses "key=value" pairs from --where flags.
// An empty value (e.g. "project=") matches beads where the field is absent or empty.
func parseWhereFlags(flags []string) map[string]string {
	if len(flags) == 0 {
		return nil
	}
	m := make(map[string]string, len(flags))
	for _, f := range flags {
		if idx := strings.IndexByte(f, '='); idx >= 0 {
			m[f[:idx]] = f[idx+1:]
		}
	}
	return m
}

// printBeadListCustom prints beads with user-selected columns.
// Supported column names: id, title, status, type, kind, priority, assignee,
// owner, created_by, labels, arms, plus any custom field name (from bead.Fields).
func printBeadListCustom(beads []*model.Bead, total int, columns []string) {
	wide := newWideCells(os.Stdout)
	w := tabwriter.NewWriter(wide, 0, 0, 2, ' ', 0)
	headers := make([]string, len(columns))
	for i, c := range columns {
		headers[i] = strings.ToUpper(strings.TrimSpace(c))
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	for _, b := range beads {
		vals := make([]string, len(columns))
		for i, c := range columns {
			vals[i] = customColumnCell(wide, b, strings.TrimSpace(c))
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	w.Flush()
	wide.Flush()
	fmt.Printf("\n%d beads (%d total)\n", len(beads), total)
}

// printBeadListTableWithScore is printBeadListTable plus a leading relevance
// SCORE column (ka-0000003832 / Option 2). Used by `kd search "<query>"` when a
// text search is active so the most-relevant bead and its score are visible at
// a glance. Mirrors printSemanticResultsTable's score-first layout.
func printBeadListTableWithScore(beads []*model.Bead, total int) {
	showArms := anyArms(beads)
	wide := newWideCells(os.Stdout)
	w := tabwriter.NewWriter(wide, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tID\tSTATUS\tTYPE\tPRIORITY\t"+armsHeader(showArms)+"TITLE\tASSIGNEE")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%d\t%s%s\t%s\n",
			b.RelevanceScore, b.ID, b.Status, b.Type, b.Priority, armsCell(wide, showArms, b), title, b.Assignee)
	}
	w.Flush()
	wide.Flush()
	fmt.Printf("\n%d beads (%d total)\n", len(beads), total)
}

// printBeadListCustomWithScore is printBeadListCustom with a leading SCORE
// column prepended to the user-selected columns, matching
// printSemanticResultsCustom. Used by `kd search "<query>" --fields …`.
func printBeadListCustomWithScore(beads []*model.Bead, total int, columns []string) {
	wide := newWideCells(os.Stdout)
	w := tabwriter.NewWriter(wide, 0, 0, 2, ' ', 0)
	headers := append([]string{"SCORE"}, make([]string, len(columns))...)
	for i, c := range columns {
		headers[i+1] = strings.ToUpper(strings.TrimSpace(c))
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, b := range beads {
		vals := make([]string, len(columns)+1)
		vals[0] = fmt.Sprintf("%.3f", b.RelevanceScore)
		for i, c := range columns {
			vals[i+1] = customColumnCell(wide, b, strings.TrimSpace(c))
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	w.Flush()
	wide.Flush()
	fmt.Printf("\n%d beads (%d total)\n", len(beads), total)
}

// customColumnCell is beadColumnValue for a --fields table, with the arms
// column ("arms" or "heraldry", the emoji run) laid out by its width on
// screen (heraldry.go's wideCells).
func customColumnCell(wide *wideCells, b *model.Bead, col string) string {
	if col == "arms" || col == "heraldry" {
		return wide.cell(armsRun(b.Heraldry))
	}
	return beadColumnValue(b, col)
}

// beadColumnValue extracts a display value for a named column from a bead.
func beadColumnValue(b *model.Bead, col string) string {
	switch col {
	case "score", "relevance_score":
		return fmt.Sprintf("%.3f", b.RelevanceScore)
	case "id":
		return b.ID
	case "title":
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		return title
	case "status":
		return string(b.Status)
	case "type":
		return string(b.Type)
	case "kind":
		return string(b.Kind)
	case "priority":
		return fmt.Sprintf("%d", b.Priority)
	case "assignee":
		return b.Assignee
	case "owner":
		return b.Owner
	case "created_by":
		return b.CreatedBy
	case "labels":
		return strings.Join(b.Labels, ",")
	default:
		// Try custom field from bead.Fields JSON.
		return beadFieldValue(b, col)
	}
}

// beadFieldValue extracts a custom field value from the bead's Fields JSON.
func beadFieldValue(b *model.Bead, key string) string {
	if len(b.Fields) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b.Fields, &fields) != nil {
		return ""
	}
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	// Try to unquote string values; fall back to raw JSON.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
