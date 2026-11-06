package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/jqout"
	"gardenio/kbeads/internal/model"
)

// compiledJQ holds the parsed --jq filter, compiled once in the root
// PersistentPreRunE. nil when --jq was not passed. emitJSON consults it.
var compiledJQ *jqout.Filter

// emitJSON is the single choke-point for every `--json` printer. When a --jq
// filter is set it projects v in-process (jq -r semantics) instead of dumping
// the raw indented JSON; otherwise it prints indented JSON exactly as before.
// Routing all JSON printers through here is what makes `--jq` universal across
// kd list/show/search/fields/config without per-command plumbing.
func emitJSON(v any) {
	if compiledJQ != nil {
		if err := compiledJQ.Apply(os.Stdout, v); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		return
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

func printBeadJSON(bead *model.Bead) {
	emitJSON(bead)
}

func printBeadTable(bead *model.Bead) {
	fmt.Printf("ID:          %s\n", bead.ID)
	fmt.Printf("Slug:        %s\n", bead.Slug)
	fmt.Printf("Title:       %s\n", bead.Title)
	if bead.ShortTitle != "" {
		fmt.Printf("Short title: %s\n", bead.ShortTitle)
	}
	printArms(os.Stdout, bead.Heraldry)
	fmt.Printf("Type:        %s\n", bead.Type)
	fmt.Printf("Kind:        %s\n", bead.Kind)
	fmt.Printf("Status:      %s\n", bead.Status)
	fmt.Printf("Priority:    %d\n", bead.Priority)
	fmt.Printf("Assignee:    %s\n", bead.Assignee)
	fmt.Printf("Owner:       %s\n", bead.Owner)
	if bead.Description != "" {
		fmt.Printf("Description: %s\n", bead.Description)
	}
	if len(bead.Labels) > 0 {
		fmt.Printf("Labels:      %s\n", strings.Join(bead.Labels, ", "))
	}
	if len(bead.Fields) > 0 {
		// Unmarshal into map[string]any, not map[string]string: a single
		// non-string value (a boolean like gate_passed / ephemeral / pod_ready,
		// a number, or a JSON object like on_complete) makes a string-typed
		// unmarshal FAIL, which silently drops the ENTIRE Fields section from
		// `kd show` — so a task carrying gate_passed=true would render no fields
		// at all (ka-0000002579 acceptance #5). map[string]any accepts every
		// typed field; formatFieldValue renders each one for display.
		var fields map[string]any
		if json.Unmarshal(bead.Fields, &fields) == nil && len(fields) > 0 {
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Println("Fields:")
			for _, k := range keys {
				fmt.Printf("  %-24s %s\n", k+":", formatFieldValue(fields[k]))
			}
		}
	}
	fmt.Printf("Created By:  %s\n", bead.CreatedBy)
	if !bead.CreatedAt.IsZero() {
		fmt.Printf("Created At:  %s\n", bead.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	if !bead.UpdatedAt.IsZero() {
		fmt.Printf("Updated At:  %s\n", bead.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
}

// formatFieldValue renders a single typed field value for the `kd show` Fields
// section. Strings print verbatim (no surrounding quotes, matching the prior
// map[string]string behavior); booleans/numbers print via their natural Go
// formatting; and composite values (arrays/objects, e.g. on_complete /
// phase_order) fall back to their compact JSON so the section never drops a
// field it can't stringify. json.Unmarshal into `any` yields string, bool,
// float64, []any, or map[string]any — this covers all of them.
func formatFieldValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// Print integers without a trailing ".0"; keep fractions otherwise.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		// Arrays, objects, and anything else: compact JSON so the value is at
		// least legible rather than a Go %v dump.
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", t)
	}
}

func printBeadListJSON(beads []*model.Bead) {
	emitJSON(beads)
}

// printBeadListJSONWithTotal prints the bare JSON array to stdout (exactly like
// printBeadListJSON, so jq pipelines and consumers that index resp[i] are
// unaffected), then — when the result set was truncated (total > len(beads))
// AND the caller is NOT emitting machine-readable JSON — emits a one-line hint
// to STDERR.
//
// The truncation hint was a stopgap for the silent-truncation gap where
// `kd list --json | jq length` reports a page size with no signal that more
// beads exist (ka-0000003137). But that hint on stderr breaks the live agent
// idiom `kd list --json 2>&1 | jq`: merging stderr into the pipe feeds jq a
// non-JSON "note:" line, so jq throws a parse error and the pipeline exits 5
// (ka-0000008174 / design ka-0000000863 §4). Under `--json` the caller is
// asking for machine-readable output, so the jsonOutput flag now suppresses the
// hint entirely — the scriptable way to learn the true total is `kd count` with
// the same filter flags, which transfers no bead bodies. In non-JSON (table)
// mode the total is already surfaced by the "N beads (M total)" footer, so the
// guarded hint is only ever meaningful for a hypothetical non-JSON caller of
// this JSON printer; it is kept as a defensive affordance.
//
// STDOUT stays a bare array by design — a JSON envelope was explicitly rejected
// (ka-0000007254) because it breaks every `kd list --json | jq '.[]'` consumer
// and the `kd children` JSON schema contract.
func printBeadListJSONWithTotal(beads []*model.Bead, total int, jsonOutput bool) {
	printBeadListJSON(beads)
	if !jsonOutput && total > len(beads) {
		fmt.Fprintf(os.Stderr,
			"note: returned %d of %d beads — raise --limit or use --offset to page\n",
			len(beads), total)
	}
}

// beadListEnvelope is the opt-in (`--json-envelope`) JSON shape for `kd list`.
// It wraps the page of beads with the pagination metadata a scripted caller
// needs to tell a `--limit`-capped page apart from a complete result set — the
// one thing the bare-array default cannot express (ka-0000001033). The default
// stays a bare array (printBeadListJSON) because a top-level envelope was
// explicitly rejected for `--json` (ka-0000007254): it breaks every
// `kd list --json | jq '.[]'` consumer and the `kd children` schema contract.
// The envelope is therefore only ever emitted when the caller opts in.
//
//   - Beads   — this page (never null: an empty match marshals as []).
//   - Count   — rows in this page (== len(Beads)); == `jq length` of the array.
//   - Total   — full matching count (server COUNT(*) OVER()), independent of --limit.
//   - HasMore — true when rows exist beyond this page: offset+count < total.
//   - Offset/Limit — the pagination window this page was drawn from, so a
//     caller can compute the next request (offset += limit) without re-deriving it.
type beadListEnvelope struct {
	Beads   []*model.Bead `json:"beads"`
	Count   int           `json:"count"`
	Total   int           `json:"total"`
	HasMore bool          `json:"has_more"`
	Offset  int           `json:"offset"`
	Limit   int           `json:"limit"`
}

// printBeadListJSONEnvelope emits the opt-in enveloped JSON shape to stdout
// (through emitJSON, so a `--jq` filter still applies — e.g. `--jq '.total'`).
// has_more is derived from the pagination window and the server's full Total:
// there are more rows iff the rows already consumed (offset + this page) fall
// short of the total. Unlike the bare-array printer, this shape lets a scripted
// `--json` caller distinguish a limit-capped page from the whole set in a single
// call.
func printBeadListJSONEnvelope(beads []*model.Bead, total, offset, limit int) {
	if beads == nil {
		beads = []*model.Bead{}
	}
	count := len(beads)
	emitJSON(beadListEnvelope{
		Beads:   beads,
		Count:   count,
		Total:   total,
		HasMore: offset+count < total,
		Offset:  offset,
		Limit:   limit,
	})
}

func printBeadListTable(beads []*model.Bead, total int, scope ...string) {
	// The BLOCKED BY column appears only when the rows actually carry
	// open-blocker data (`kd list --with-blockers`, ka-0000000945).
	// OpenBlockers is nil on every other path, so search / view / fleet /
	// watch — and a plain `kd list` — print the exact same six columns as
	// before, byte for byte.
	//
	// Keyed on the field being POPULATED (non-nil), not on any bead actually
	// being blocked: when --with-blockers returns an all-clear page, the
	// reader must SEE the column full of "-" confirming it was checked, not
	// silently get the old six columns back.
	showBlockers := false
	for _, b := range beads {
		if b.OpenBlockers != nil {
			showBlockers = true
			break
		}
	}

	// The ARMS column (the emoji run, heraldry.go) appears only when some
	// row bears arms, so a table without any is unchanged. It sits before
	// SHORT, the run followed by the short title (heraldry spec §4.2).
	showArms := anyArms(beads)
	wide := newWideCells(os.Stdout)
	w := tabwriter.NewWriter(wide, 0, 0, 2, ' ', 0)
	header := "ID\tSTATUS\tTYPE\tSHORT\tPRIORITY\tTITLE\tASSIGNEE"
	if showArms {
		header = "ID\tSTATUS\tTYPE\tARMS\tSHORT\tPRIORITY\tTITLE\tASSIGNEE"
	}
	if showBlockers {
		header += "\tBLOCKED BY"
	}
	fmt.Fprintln(w, header)
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		typeCell := string(b.Type)
		if showArms {
			typeCell += "\t" + wide.cell(armsRun(b.Heraldry))
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s",
			b.ID,
			b.Status,
			typeCell,
			shortTitleCell(b.ShortTitle),
			b.Priority,
			title,
			b.Assignee,
		)
		if showBlockers {
			row += "\t" + formatOpenBlockers(b.OpenBlockers)
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
	wide.Flush()
	fmt.Printf("\n%s\n", listFooter(len(beads), total, scopeArg(scope)))
}

// shortTitleCellMax is the SHORT column width: model.ShortTitleMax, so a
// valid short_title is never cut.
const shortTitleCellMax = model.ShortTitleMax

// shortTitleCell renders the SHORT cell: "-" when unset (so `kd list
// --missing-short-title` reads as checked, not as a blank column), otherwise
// the short title cut to shortTitleCellMax runes.
func shortTitleCell(s string) string {
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > shortTitleCellMax {
		return string(r[:shortTitleCellMax-3]) + "..."
	}
	return s
}

// openBlockerSummaryMax caps how many blocker ids the table cell names before
// eliding, so one bead waiting on a dozen edges cannot blow out the row.
const openBlockerSummaryMax = 3

// formatOpenBlockers renders the BLOCKED BY cell as "ka-x [open], ka-y
// [in_progress]" — the id AND its status, because the status is what tells a
// triage reader whether the gate is moving. An unblocked row renders "-"
// rather than empty so a reader can tell "nothing blocks this" apart from a
// column that failed to populate.
//
// An out-of-scope blocker (cross-project, title redacted; see
// model.OpenBlocker.OutOfScope) renders "ka-x [open, out-of-scope]" so the
// reader sees the row IS gated even though the blocker's title is withheld —
// it must never render as unblocked (ka-0000007462).
func formatOpenBlockers(blockersPtr *[]*model.OpenBlocker) string {
	if blockersPtr == nil || len(*blockersPtr) == 0 {
		return "-"
	}
	blockers := *blockersPtr
	parts := make([]string, 0, openBlockerSummaryMax)
	for _, b := range blockers[:min(len(blockers), openBlockerSummaryMax)] {
		if b.OutOfScope {
			parts = append(parts, fmt.Sprintf("%s [%s, out-of-scope]", b.ID, b.Status))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s [%s]", b.ID, b.Status))
	}
	out := strings.Join(parts, ", ")
	if extra := len(blockers) - openBlockerSummaryMax; extra > 0 {
		out += fmt.Sprintf(" +%d more", extra)
	}
	return out
}

// listFooter formats the trailing summary line for bead-list output. When
// scope is non-empty it is appended inside the parenthetical so the reader can
// see which project filter (if any) the query applied. This closes the
// silent-scope gap where `kd list --type=task` (project-scoped) and
// `kd list --type=config` (unscoped) printed identical "N beads (M total)"
// footers even though they searched different corpora (ka-0000007254).
func listFooter(shown, total int, scope string) string {
	if scope == "" {
		return fmt.Sprintf("%d beads (%d total)", shown, total)
	}
	return fmt.Sprintf("%d beads (%d total; %s)", shown, total, scope)
}

// scopeArg returns the first scope descriptor from a variadic scope argument,
// or "" when none was supplied. The list printers take scope variadically so
// their existing two-arg call sites (search, view, fleet, watch) keep printing
// the plain footer while only `kd list` threads its computed scope note.
func scopeArg(scope []string) string {
	if len(scope) > 0 {
		return scope[0]
	}
	return ""
}

// resolvedDep pairs a dependency with its optionally-resolved target bead.
type resolvedDep struct {
	Dep  *model.Dependency
	Bead *model.Bead // nil if fetch failed
}

// fetchAndResolveDeps fetches dependencies for a bead and resolves each target.
// If types is non-empty, only dependencies matching one of the given types are included.
func fetchAndResolveDeps(ctx context.Context, c client.BeadsClient, beadID string, types []string) ([]resolvedDep, error) {
	deps, err := c.GetDependencies(ctx, beadID)
	if err != nil {
		return nil, err
	}
	return resolveBeadDeps(ctx, c, deps, types), nil
}

// resolveBeadDeps takes an existing dependency slice, filters by type, and resolves each target bead.
func resolveBeadDeps(ctx context.Context, c client.BeadsClient, deps []*model.Dependency, types []string) []resolvedDep {
	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}

	var resolved []resolvedDep
	for _, d := range deps {
		if len(typeSet) > 0 && !typeSet[string(d.Type)] {
			continue
		}
		rd := resolvedDep{Dep: d}
		fetchedBead, err := c.GetBead(ctx, d.DependsOnID)
		if err == nil {
			rd.Bead = fetchedBead
		}
		resolved = append(resolved, rd)
	}
	return resolved
}

// printDepSubSection prints resolved dependencies as indented lines.
func printDepSubSection(deps []resolvedDep, fields []string) {
	if len(deps) == 0 {
		return
	}
	if len(fields) == 0 {
		fields = []string{"id", "title", "status"}
	}
	for _, rd := range deps {
		if rd.Bead != nil {
			vals := make([]string, len(fields))
			for i, f := range fields {
				vals[i] = beadField(rd.Bead, f)
			}
			fmt.Printf("    %s: %s\n", string(rd.Dep.Type), strings.Join(vals, " | "))
		} else {
			fmt.Printf("    %s: %s (unresolved)\n", string(rd.Dep.Type), rd.Dep.DependsOnID)
		}
	}
}

// printBeadTableFiltered prints bead detail fields, restricted to the given whitelist.
// If fields is nil or empty, all fields are printed (delegates to printBeadTable).
func printBeadTableFiltered(bead *model.Bead, fields []string) {
	if len(fields) == 0 {
		printBeadTable(bead)
		return
	}
	fieldSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldSet[strings.ToLower(f)] = true
	}
	type fieldRow struct {
		label string
		key   string
		value string
	}
	rows := []fieldRow{
		{"ID", "id", bead.ID},
		{"Slug", "slug", bead.Slug},
		{"Title", "title", bead.Title},
		{"Short title", "short_title", bead.ShortTitle},
		{"Type", "type", string(bead.Type)},
		{"Kind", "kind", string(bead.Kind)},
		{"Status", "status", string(bead.Status)},
		{"Priority", "priority", fmt.Sprintf("%d", bead.Priority)},
		{"Assignee", "assignee", bead.Assignee},
		{"Owner", "owner", bead.Owner},
	}
	for _, r := range rows {
		if fieldSet[r.key] {
			fmt.Printf("%-13s%s\n", r.label+":", r.value)
		}
	}
	if fieldSet["heraldry"] || fieldSet["arms"] {
		printArms(os.Stdout, bead.Heraldry)
	}
	if fieldSet["description"] && bead.Description != "" {
		fmt.Printf("%-13s%s\n", "Description:", bead.Description)
	}
	if fieldSet["labels"] && len(bead.Labels) > 0 {
		fmt.Printf("%-13s%s\n", "Labels:", strings.Join(bead.Labels, ", "))
	}
	if fieldSet["created_by"] {
		fmt.Printf("%-13s%s\n", "Created By:", bead.CreatedBy)
	}
	if fieldSet["created_at"] && !bead.CreatedAt.IsZero() {
		fmt.Printf("%-13s%s\n", "Created At:", bead.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	if fieldSet["updated_at"] && !bead.UpdatedAt.IsZero() {
		fmt.Printf("%-13s%s\n", "Updated At:", bead.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
}

// printComments prints bead comments in a standard format.
func printComments(comments []*model.Comment) {
	if len(comments) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("Comments:")
	for _, c := range comments {
		ts := ""
		if !c.CreatedAt.IsZero() {
			ts = c.CreatedAt.Format("2006-01-02 15:04:05")
		}
		fmt.Printf("  [%s] %s: %s\n", ts, c.Author, c.Text)
	}
}
