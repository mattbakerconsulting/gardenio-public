package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// showMentionsOnly suppresses header/comments/deps and prints only the
	// two mention sections — handy for piping into grep/awk. Phase 3 of E3
	// (gardenio/plans/typed-mentions-graph-edges-and-cli.md).
	showMentionsOnly bool

	// showViewerURL is the base URL for OSC 8 hyperlinks wrapped around
	// ka-IDs in the Mentions / Mentioned-by sections when stdout is a TTY.
	// Empty disables linkification (plain IDs). Defaults to BEADS_VIEWER_URL.
	showViewerURL string

	// showAllComments opts into rendering the full comment history. By
	// default the table view caps display to the last commentDisplayLimit
	// entries and prints a suppressed-count footer; --json always emits the
	// full list (ka-0000001542).
	showAllComments bool

	// showDescription (flag --description, alias --body) suppresses the
	// header/deps/comments/mentions and prints ONLY the bead description —
	// the mirror of --mentions-only for the description field. Handy for
	// piping a handoff brief into a pager or another command without
	// scrubbing surrounding chrome (ka-0000002381). Unlike --mentions-only
	// it DOES fetch the bead so a missing id still errors non-zero.
	showDescription bool
)

// commentDisplayLimit caps how many comments `kd show` renders in the
// table view by default. Long-lived beads (schedules, archived epics)
// can accumulate hundreds of comments and dump tens of KB into agent
// terminals; 30 covers the recent-context use case.
const commentDisplayLimit = 30

// salvageCommentPrefix is the stable leading text of the machine-generated
// involuntary-termination salvage notice the controller appends when it
// kills an agent mid-work. A flapping loop's plan bead accumulates dozens
// of these identical notices as its MOST-RECENT comments, so a count-based
// cap spends its whole window on the salvage flood and buries the older
// high-signal comments (STACK-TIP pointers, acceptance evidence). We fold
// runs of these before the cap so the window is spent on distinct signal
// (ka-0000003686 / ka-0000007338). Prefix-match (not exact) because the
// notice carries a variable worktree count / S3 path / agent id tail.
const salvageCommentPrefix = "Killed mid-work (involuntary_termination)."

var showCmd = &cobra.Command{
	Use:     "show <id>...",
	Short:   "Show details of one or more beads",
	GroupID: "beads",
	Long: `Show details of one or more beads.

With a single id the output is unchanged: a human-readable table, or a single
JSON object with --json.

With multiple ids each bead is rendered in sequence — blank-line separated in
table mode; a JSON array (one object per bead) with --json. Fetching loops over
the ids, so an id that cannot be fetched is reported on stderr and the command
exits non-zero, but every bead that WAS found is still rendered (a missing id
never aborts the batch). This kills the N+1 shell loop callers used to write
because ` + "`kd show`" + ` only accepted a single id.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		linker := newIDLinker(showViewerURL, isStdoutTTY())

		if showMentionsOnly {
			// Skip GetBead/header/comments/deps entirely — emit just the
			// mention sections so callers can pipe to grep/awk without
			// scrubbing surrounding noise. One block per id.
			//
			// By design this branch does NOT participate in the batch
			// missing-id contract below: it never fetches the bead (that is
			// the whole point of --mentions-only), so it cannot tell a
			// genuinely-absent id from one with no mentions — both render
			// nothing and exit zero. This matches the pre-batch single-id
			// behavior and keeps the cheap-pipe promise intact.
			for _, id := range args {
				printMentionSections(ctx, beadsClient, id, os.Stdout, linker)
			}
			return nil
		}

		// multi selects the batch surface: a JSON array (vs a single object)
		// and blank-line separators between table blocks. Single-id output
		// stays byte-identical to the pre-batch command on both surfaces.
		multi := len(args) > 1
		jsonValues := make([]any, 0, len(args))
		var missing []string
		var shownIDs []string
		printed := 0

		for _, id := range args {
			bead, err := beadsClient.GetBead(ctx, id)
			if err != nil {
				if !multi {
					// Preserve the single-id error surface byte-for-byte:
					// the "getting bead <id>: " prefix is a contract
					// (show_multi_test.go). The kd search hint is a suffix
					// appended only when the arg isn't id-shaped, so
					// ka-prefixed misses stay unchanged.
					return fmt.Errorf("getting bead %s: %w%s", id, err, showSearchHint(id, err))
				}
				// Batch mode: a missing/unfetchable id must not abort the
				// rest. Report it on stderr and keep going; a summary error
				// at the end drives the non-zero exit.
				fmt.Fprintf(os.Stderr, "kd show: %s: %v%s\n", id, err, showSearchHint(id, err))
				missing = append(missing, id)
				continue
			}
			shownIDs = append(shownIDs, bead.ID)

			if showDescription {
				// Print ONLY the description. Unlike --mentions-only this
				// still fetched the bead above, so it honors the batch
				// missing-id contract below. JSON shape is a
				// {"id","description"} object (single) or an array of them
				// (batch), matching the multi-array surface the other paths
				// use. Text mode is the bare description + trailing newline,
				// blank-line separated between beads in batch mode.
				if jsonOutput {
					v := map[string]string{"id": bead.ID, "description": bead.Description}
					if multi {
						jsonValues = append(jsonValues, v)
					} else {
						printBeadJSONValue(v)
					}
				} else {
					if printed > 0 {
						fmt.Println()
					}
					fmt.Fprintln(os.Stdout, bead.Description)
					printed++
				}
				continue
			}

			// Count parent-child CHILDREN (incoming parent-child edges). An
			// epic with children stores them as reverse edges the header/deps
			// sections never surfaced, so the children were invisible from
			// `kd show` (ka-0000003337). Best-effort: a transport without
			// reverse-dep lookup (gRPC) or any error yields count 0 and the
			// line/field is omitted.
			childCount := countParentChildChildren(ctx, id)

			// Denormalize each dependency edge with its neighbor's status
			// and title so a frontier scan ("are my blockers closed?") reads
			// the answer off this one response instead of issuing a second
			// call per edge (ka-0000000053). One batched round-trip for all
			// edges; best-effort, and free for a bead with no edges.
			depEdges := expandDepEdges(ctx, bead.Dependencies)

			if jsonOutput {
				if multi {
					jsonValues = append(jsonValues, beadJSONValue(bead, childCount, depEdges))
				} else {
					printBeadJSONWithChildren(bead, childCount, depEdges)
				}
				continue
			}

			if printed > 0 {
				// Separate consecutive bead blocks with a blank line in batch
				// mode; on the single-id path printed==0 for the only bead so
				// nothing is emitted and the output stays byte-identical.
				fmt.Println()
			}
			renderBeadText(ctx, bead, childCount, depEdges, id, linker)
			printed++
		}

		if jsonOutput && multi {
			printBeadJSONValue(jsonValues)
		}

		recordSeenBeadIDs(shownIDs)

		if len(missing) > 0 {
			return fmt.Errorf("%d of %d bead(s) not found: %s",
				len(missing), len(args), strings.Join(missing, ", "))
		}
		return nil
	},
}

// showSearchHint returns an actionable suffix for a `kd show` not-found error.
// For a non-ID-shaped arg (no idgen prefix), it points at `kd search` — agent
// names, slugs, and titles land on this path constantly, and a bare 404 there
// used to get masked with 2>/dev/null (ka-0000006651). For an ID-shaped arg
// that was recently seen (returned by a `kd show`/`list`/`search` call within
// the last recentBeadsTTL, ADR docs/adr/taints.md §9.4), it instead returns
// the read-gate hint: the id is real but the caller just lost visibility into
// it (most likely a taint applied between when it was shown and now), not a
// typo. Every other case — an ID-shaped arg never recently seen, or an error
// that isn't the not-found signal (auth failures, timeouts, …) — returns "" so
// the byte-stable single-id error surface for real bead IDs is untouched.
func showSearchHint(arg string, err error) string {
	if !isBeadNotFound(err) {
		return ""
	}
	if !strings.HasPrefix(arg, idgen.DefaultPrefix) {
		return fmt.Sprintf(" (not a bead ID — for agents or titles try: kd search '%s')", arg)
	}
	if wasRecentlySeen(arg) {
		return " (not found or not visible to you)"
	}
	return ""
}

// isBeadNotFound reports whether err is the daemon's "no such bead" signal
// across transports: HTTP surfaces a *client.APIError with StatusCode 404;
// gRPC surfaces a status with codes.NotFound. A message fallback covers
// wrapped errors from transports that flatten the typed error away.
func isBeadNotFound(err error) bool {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	if status.Code(err) == codes.NotFound {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// renderBeadText prints the full human-readable `kd show` rendering for one
// bead: the header table, the Depends On section, the Children summary line,
// the capped comment history, and the typed-mention sections. It is the
// per-bead unit both the single-id and batch paths reuse, so their output
// stays identical bead-for-bead.
//
// Mentions / Mentioned by are best-effort: if the server is older or the
// lookups fail, printMentionSections omits the section and emits a brief
// warning so `kd show` keeps working against pre-typed-mention deployments.
// See plan E3 in gardenio/plans/typed-mentions-graph-edges-and-cli.md.
func renderBeadText(ctx context.Context, bead *model.Bead, childCount int, edges []*depEdgeView, id string, linker idLinker) {
	printBeadTable(bead)
	printAgentSpawnBadge(ctx, bead)
	if len(edges) > 0 {
		fmt.Println()
		fmt.Println("Depends On:")
		for _, d := range edges {
			fmt.Printf("  %s\n", formatDepEdgeLine(d))
		}
	}
	if childCount > 0 {
		fmt.Printf("Children:    %d (`kd dep children %s`)\n", childCount, id)
	}
	printShowComments(os.Stdout, bead.Comments, showAllComments)
	printMentionSections(ctx, beadsClient, id, os.Stdout, linker)
}

func init() {
	showCmd.Flags().BoolVar(&showMentionsOnly, "mentions-only", false,
		"print only the Mentions / Mentioned-by sections (suppresses header, comments, deps)")
	showCmd.Flags().StringVar(&showViewerURL, "viewer-url", os.Getenv("BEADS_VIEWER_URL"),
		"base URL for OSC 8 hyperlinks wrapping ka-IDs in mention sections (TTY only); defaults to BEADS_VIEWER_URL")
	showCmd.Flags().BoolVar(&showAllComments, "all-comments", false,
		fmt.Sprintf("render full comment history (default caps display to last %d; --json always emits all)", commentDisplayLimit))
	// --description and --body both bind the one showDescription var: cobra
	// registers two distinct flags each wrapping the same *bool, so setting
	// either flips it true. --body is a memorable alias for --description.
	showCmd.Flags().BoolVar(&showDescription, "description", false,
		"print only the bead description (suppresses header, deps, comments, mentions)")
	showCmd.Flags().BoolVar(&showDescription, "body", false, "alias for --description")
}

// printShowComments renders the Comments: section for `kd show`. When
// all is false, runs of machine-generated salvage boilerplate are first
// folded (see foldSalvageComments) so the display cap is spent on distinct
// signal, then — if the folded count still exceeds commentDisplayLimit —
// only the last commentDisplayLimit entries are rendered and a footer
// announces how many older entries were suppressed and how to view them.
// When all is true, every comment is rendered raw (no fold, no cap).
func printShowComments(w io.Writer, comments []*model.Comment, all bool) {
	if len(comments) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Comments:")

	lines := make([]string, 0, len(comments))
	if all {
		for _, c := range comments {
			lines = append(lines, formatShowComment(c))
		}
	} else {
		lines = foldSalvageComments(comments)
	}

	start := 0
	suppressed := 0
	if !all && len(lines) > commentDisplayLimit {
		start = len(lines) - commentDisplayLimit
		suppressed = start
	}
	for _, line := range lines[start:] {
		fmt.Fprintln(w, line)
	}
	if suppressed > 0 {
		fmt.Fprintf(w, "  … %d older comments suppressed. Use --all-comments to view full history.\n", suppressed)
	}
}

// formatShowComment renders a single comment as one display line.
func formatShowComment(c *model.Comment) string {
	ts := c.CreatedAt.Format("2006-01-02 15:04:05")
	return fmt.Sprintf("  [%s] %s: %s", ts, c.Author, c.Text)
}

// foldSalvageComments collapses each maximal run of K>=2 consecutive
// involuntary-termination salvage notices into a single synthetic line so
// the count-based display cap is not consumed by the salvage flood. Runs of
// length 1 and every non-salvage comment render normally. The returned slice
// preserves comment order, so downstream windowing keeps its "last N" meaning.
//
// TODO(ka-0000003686): spawn/close lifecycle echoes are the next fold
// candidates once a stable recognizer for them lands — extend this pass with
// additional template prefixes rather than building a general clustering engine.
func foldSalvageComments(comments []*model.Comment) []string {
	lines := make([]string, 0, len(comments))
	for i := 0; i < len(comments); {
		if !isSalvageComment(comments[i]) {
			lines = append(lines, formatShowComment(comments[i]))
			i++
			continue
		}
		// Extend the run over consecutive salvage notices.
		j := i + 1
		for j < len(comments) && isSalvageComment(comments[j]) {
			j++
		}
		run := comments[i:j]
		if len(run) >= 2 {
			first := run[0].CreatedAt.Format("2006-01-02 15:04:05")
			last := run[len(run)-1].CreatedAt.Format("2006-01-02 15:04:05")
			lines = append(lines, fmt.Sprintf(
				"  [%s–%s] %d× involuntary_termination salvage notices (kd show --all-comments to expand)",
				first, last, len(run)))
		} else {
			lines = append(lines, formatShowComment(run[0]))
		}
		i = j
	}
	return lines
}

// isSalvageComment reports whether c is a machine-generated
// involuntary-termination salvage notice, matched by its stable prefix.
func isSalvageComment(c *model.Comment) bool {
	return c != nil && strings.HasPrefix(c.Text, salvageCommentPrefix)
}

// countParentChildChildren returns how many beads are children of id via a
// parent-child edge (incoming parent-child reverse deps). It reuses the same
// reverse-dependency lookup `kd dep children` is built on, filtered to
// parent-child so blocks/related dependents don't inflate the count. It is
// best-effort: a nil client, a transport without reverse-dep support (gRPC),
// or any error yields 0 so `kd show` never fails on the auxiliary count.
func countParentChildChildren(ctx context.Context, id string) int {
	if beadsClient == nil {
		return 0
	}
	rev, err := beadsClient.GetReverseDependencies(ctx, id)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range rev {
		if d != nil && d.Type == model.DepParentChild {
			n++
		}
	}
	return n
}

// printBeadJSONWithChildren emits the bead as JSON, adding a top-level
// "children_count" field when it is > 0 (ka-0000003337 read-surface). We wrap
// rather than add a field to model.Bead so its stored schema stays untouched
// and every other printBeadJSON caller is unaffected.
func printBeadJSONWithChildren(bead *model.Bead, childCount int, edges []*depEdgeView) {
	printBeadJSONValue(beadJSONValue(bead, childCount, edges))
}

// beadJSONShowView is the `kd show --json` per-bead encoding: the whole bead
// plus the read-surface fields this command adds on top of the stored schema.
// All are omitempty, so a bead with no children and no edges encodes
// byte-identically to a bare *model.Bead.
type beadJSONShowView struct {
	*model.Bead
	// Dependencies SHADOWS the embedded Bead.Dependencies: it carries the
	// same edges denormalized with each neighbor's status/title
	// (ka-0000000053). Both marshal to "dependencies", and encoding/json
	// resolves that conflict by depth — this field sits at depth 0 and the
	// embedded one at depth 1, so the enriched slice wins. The enrichment is
	// purely additive per edge, so `.dependencies` stays the canonical key
	// with its original shape intact.
	Dependencies []*depEdgeView `json:"dependencies,omitempty"`
	// DependsOn duplicates Dependencies under the name the text view prints
	// ("Depends On:") and `kd dep list --json` uses ("depends_on_id").
	// Three vocabularies for one concept meant agents who read the text label
	// and scripted the obvious `.depends_on` path got a silent jq null —
	// absent keys are not an error (ka-0000007268). `.dependencies` stays as
	// the canonical key; this is a pure additive alias.
	DependsOn []*depEdgeView `json:"depends_on,omitempty"`
	// Comments SHADOWS the embedded Bead.Comments the same way Dependencies
	// does (depth 0 beats depth 1), carrying the identical comments with an
	// additive "body" alias of the canonical "text" key. The conventional
	// `.comments[] | "\(.author): \(.body)"` printed "<author>: null" before
	// this — see show_comment_view.go for the full defect note
	// (ka-0000002877).
	Comments      []*commentView `json:"comments,omitempty"`
	ChildrenCount int            `json:"children_count,omitempty"`
}

// beadJSONValue returns the JSON-shaped value for a single bead: the bare
// *model.Bead when there is nothing to add (byte-identical to printBeadJSON),
// or a beadJSONShowView carrying the "depends_on" alias (ka-0000007268) and
// the "children_count" read-surface (ka-0000003337). The batch path collects
// these into a slice and marshals them as a JSON array; the single-id path
// marshals the one value directly, so both surfaces share the exact per-bead
// encoding.
func beadJSONValue(bead *model.Bead, childCount int, edges []*depEdgeView) any {
	// A nil bead cannot be embedded — marshaling the wrapper would panic
	// dereferencing it for the promoted fields. GetBead over gRPC returns
	// (nil, nil) when the response carries no bead, so this is reachable.
	if bead == nil {
		return bead
	}
	// Comments participate in the "nothing to add" test: a bead with comments
	// but no edges and no children still needs the wrapper, or its comments
	// would promote from the embedded bead WITHOUT the "body" alias — the
	// exact silent null this fix removes (ka-0000002877).
	if childCount <= 0 && len(bead.Dependencies) == 0 && len(bead.Comments) == 0 {
		return bead
	}
	return beadJSONShowView{
		Bead:          bead,
		Dependencies:  edges,
		DependsOn:     edges,
		Comments:      commentViews(bead.Comments),
		ChildrenCount: childCount,
	}
}

// printBeadJSONValue marshals v with the same indentation printBeadJSON uses
// and prints it. v is either a single bead value (from beadJSONValue) or a
// []any of them for the multi-id array surface.
func printBeadJSONValue(v any) {
	emitJSON(v)
}

// isStdoutTTY is overridable in tests via the package-level hook; the default
// implementation consults golang.org/x/term against fd 1.
var isStdoutTTY = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// idLinker wraps ka-IDs in OSC 8 hyperlinks pointing at a beads-viewer URL.
// It is a no-op when disabled (non-TTY, no viewer URL, or the NO_COLOR
// convention applies through ui.ShouldUseColor) — callers always emit the
// raw ID then, suitable for grep/awk.
//
// OSC 8 reference: https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda
type idLinker struct {
	enabled bool
	baseURL string
}

// newIDLinker returns a linker that emits OSC 8 escapes only when stdout is
// a TTY *and* a non-empty viewer URL is configured. We deliberately reuse
// ui.ShouldUseColor's NO_COLOR/CLICOLOR semantics so users who suppress
// color also suppress hyperlinks (Terminal.app, dumb pipes, etc.).
func newIDLinker(baseURL string, tty bool) idLinker {
	enabled := tty && strings.TrimSpace(baseURL) != "" && ui.ShouldUseColor()
	return idLinker{enabled: enabled, baseURL: strings.TrimRight(baseURL, "/")}
}

// wrap emits the OSC 8 sequence around id. Format:
//
//	ESC ] 8 ; ; URL ESC \ TEXT ESC ] 8 ; ; ESC \
//
// Non-TTY (or empty viewer URL) callers get the unwrapped id, so existing
// pipelines and golden-file tests stay byte-stable.
func (l idLinker) wrap(id string) string {
	if !l.enabled || l.baseURL == "" {
		return id
	}
	url := l.baseURL + "/" + id
	return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", url, id)
}

// mentionPageLimit is the per-call cap when assembling the show summary.
// The summary groups by verb and renders a few exemplar IDs per group; we do
// not need to fetch hundreds of rows. Matches the default refs page size.
const mentionPageLimit = 200

// mentionExemplarsPerGroup caps the ka-IDs printed per verb group on the
// summary line; the count is always shown, so callers can run
// `kd refs mentions <id> --type=<verb>` to see the rest.
const mentionExemplarsPerGroup = 5

func printMentionSections(ctx context.Context, c client.BeadsClient, id string, w io.Writer, linker idLinker) {
	mentions, mErr := c.ListBeadReferencesFrom(ctx, id, mentionPageLimit, 0, nil)
	mentionedBy, mbErr := c.ListBeadsMentioning(ctx, id, mentionPageLimit, 0, nil)

	// If both calls failed, the server likely doesn't speak the references
	// API at all — stay quiet. If only one failed, emit a single hint so
	// the operator knows the partial output isn't authoritative.
	if mErr != nil && mbErr != nil {
		return
	}
	if mErr != nil {
		fmt.Fprintf(w, "\n(mentions lookup failed: %v)\n", mErr)
	}
	if mbErr != nil {
		fmt.Fprintf(w, "\n(mentioned-by lookup failed: %v)\n", mbErr)
	}

	if mentions != nil && len(mentions.References) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Mentions:")
		writeMentionGroups(w, groupMentionsByType(mentions.References, "ID"), linker)
	}
	if mentionedBy != nil && len(mentionedBy.References) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Mentioned by:")
		writeMentionGroups(w, groupMentionsByType(mentionedBy.References, "ID"), linker)
	}
}

// mentionGroup is one bucket of references that share the same
// (canonicalized) relationship_type. Members are deduped — a single source
// referenced via multiple source_fields collapses to one ID per group.
type mentionGroup struct {
	verb string // canonical key, "untyped" for nil relationship_type
	ids  []string
}

// groupMentionsByType buckets references by relationship_type, dedupes IDs
// within each bucket, and returns the groups in a stable display order:
//   - typed verbs in alphabetical order (parent, blocks, related, …)
//   - "untyped" last
//
// idField selects which column from BeadReferenceSummary to bucket. Mentions
// (forward) and mentioned-by (reverse) both ship `ID` as the "other side" of
// the edge, so callers always pass "ID" today; the parameter is here as a
// hook for future additions.
func groupMentionsByType(refs []*model.BeadReferenceSummary, idField string) []mentionGroup {
	_ = idField // currently unused; reserved for forward/reverse parity.
	buckets := map[string][]string{}
	seen := map[string]map[string]struct{}{}

	for _, r := range refs {
		if r == nil {
			continue
		}
		key := "untyped"
		if r.RelationshipType != nil && *r.RelationshipType != "" {
			key = *r.RelationshipType
		}
		if _, ok := seen[key]; !ok {
			seen[key] = map[string]struct{}{}
		}
		if _, dup := seen[key][r.ID]; dup {
			continue
		}
		seen[key][r.ID] = struct{}{}
		buckets[key] = append(buckets[key], r.ID)
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		// "untyped" sorts last so typed verbs are visually grouped first.
		if keys[i] == "untyped" {
			return false
		}
		if keys[j] == "untyped" {
			return true
		}
		return keys[i] < keys[j]
	})

	out := make([]mentionGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, mentionGroup{verb: k, ids: buckets[k]})
	}
	return out
}

// writeMentionGroups renders one indented line per verb:
//
//	blocks: 2 (ka-AAA, ka-BBB)
//	parent: 1 (ka-CCC)
//	untyped: 4 (ka-DDD, ka-EEE, ...)
//
// IDs beyond mentionExemplarsPerGroup collapse to a trailing "..." so wide
// graphs don't blow out the terminal. The full count is always shown.
//
// When linker is enabled, each ka-ID is wrapped in an OSC 8 hyperlink so
// terminal emulators that support it (iTerm2, kitty, WezTerm, modern
// gnome-terminal) render the IDs as clickable links into beads-viewer.
func writeMentionGroups(w io.Writer, groups []mentionGroup, linker idLinker) {
	for _, g := range groups {
		shown := g.ids
		ellipsis := ""
		if len(shown) > mentionExemplarsPerGroup {
			shown = shown[:mentionExemplarsPerGroup]
			ellipsis = ", ..."
		}
		linked := make([]string, len(shown))
		for i, id := range shown {
			linked[i] = linker.wrap(id)
		}
		fmt.Fprintf(w, "  %s: %d (%s%s)\n", g.verb, len(g.ids), strings.Join(linked, ", "), ellipsis)
	}
}
