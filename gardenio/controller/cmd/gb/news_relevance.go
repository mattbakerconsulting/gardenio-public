package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"golang.org/x/sync/errgroup"
)

// newsRelevanceWindow is how far back "recently closed" reaches WITHIN the
// caller's own workstream (epic subtree, blockers, shared labels, cross-project
// matches). The fleet-wide Closed section stays on the short --window (default
// 2h) so it is not flooded, but a bead in the CALLER'S scope that closed hours
// ago is exactly the signal the audit found agents missing — rvb's blocker
// escalation ka-0000001499 closed 4h before spawn and was invisible at 2h, and
// thread-1782813189 re-did a task a *sibling had finished a day earlier*
// (ka-0000000723, misses M2/M3/M6). 24h is the widened window the audit
// prescribed for in-scope closures.
const newsRelevanceWindow = 24 * time.Hour

// Per-category caps keep the caller-scoped section tight: it must read as a
// focused "here is what touches YOUR work", not a second firehose. The whole
// point of relevance-scoping is to counter the ~90% irrelevance the audit
// found training crew to treat news as a checkbox.
const (
	relevanceMaxComments = 5
	relevanceMaxRows     = 12
	relevanceMaxCross    = 5
	relevanceKeywordN    = 4
)

// relevanceBlocker is one bead the caller's anchor bead depends on (a "blocks"
// edge: anchor is blocked-by Bead). Cleared marks a blocker that closed within
// the relevance window — the "you may now be unblocked" signal.
type relevanceBlocker struct {
	Bead    *beadsapi.BeadDetail
	Cleared bool
}

// relevanceRow is a scoped bead plus the reason it is relevant to the caller.
type relevanceRow struct {
	Bead   *beadsapi.BeadDetail
	Reason string
}

// relevanceDigest is the caller-scoped view computed from the caller's assigned
// in-progress bead (the "anchor"). It is nil/empty for callers with no claim
// (gardeners, release owners, thread agents) — that cohort already reads rows,
// per the audit; relevance-scoping targets crew who claim one bead and then
// treat news as ritual.
type relevanceDigest struct {
	Anchor       *beadsapi.BeadDetail // the caller's assigned bead
	Container    *beadsapi.BeadDetail // the epic (or parent) scoping the subtree
	NewComments  []beadsapi.Comment   // recent comments on the anchor by others (M6)
	Blockers     []relevanceBlocker   // anchor's blockers, incl. recently cleared (M2/M6)
	Subtree      []relevanceRow       // active/recently-closed work in the anchor's epic (M3)
	SharedLabel  []relevanceRow       // work sharing a topical label with the anchor (M3)
	CrossProject []relevanceRow       // possible-duplicate work in OTHER projects (M2)
}

// empty reports whether the digest carries no caller-scoped signal. Nil-safe so
// callers can gate on a nil digest (no anchor) uniformly.
func (d *relevanceDigest) empty() bool {
	if d == nil {
		return true
	}
	return len(d.NewComments) == 0 && len(d.Blockers) == 0 && len(d.Subtree) == 0 &&
		len(d.SharedLabel) == 0 && len(d.CrossProject) == 0
}

// gatherRelevance builds the caller-scoped digest. It is BEST-EFFORT by design:
// `gb news` is load-bearing at the start of every session, so a failure in any
// sub-query silently omits that category rather than breaking the core digest.
// Returns nil when the caller has no assigned in-progress bead.
func gatherRelevance(ctx context.Context, actor, project string, window time.Duration) *relevanceDigest {
	if daemon == nil || actor == "" || actor == "unknown" {
		return nil
	}
	anchor, err := daemon.ListAssignedTask(ctx, actor)
	if err != nil || anchor == nil {
		return nil
	}

	d := &relevanceDigest{Anchor: anchor}

	// The relevance digest historically issued ~9 daemon round-trips STRICTLY
	// SEQUENTIALLY (comments, deps, blocker batch-get, parent batch-get, optional
	// grandparent hop, subtree list, shared-label list, cross-project SEARCH).
	// Measured 2026-08-29 (ka-0000006233) it dominated `gb news` wall-clock at
	// ~8-9s of the ~11-12s total — the single largest agent inner-loop latency
	// sink, paid at every spawn and before every irreversible action. Split it
	// into a parallel FETCH phase and a sequential COLLECT phase:
	//
	//   Fetch phase — every network round-trip runs concurrently. The three
	//   deps-independent axes (comments, shared-label, cross-project) each need
	//   only the anchor; the deps chain (blockers + container -> subtree) nests
	//   inside its own goroutine. The beadsapi client wraps a concurrency-safe
	//   *http.Client, so wall-clock collapses from the round-trip SUM to the
	//   slowest single chain.
	//
	//   Collect phase — the `seen` cross-category dedup runs SEQUENTIALLY, in the
	//   original priority order (blockers seed `seen`, then subtree > shared-label
	//   > cross-project). Keeping collection single-threaded makes the parallel
	//   change behavior-preserving and race-free: goroutines only ever fetch raw
	//   candidate slices; nothing mutates shared state concurrently.
	var (
		subtreeBeads      []*beadsapi.BeadDetail
		sharedLabelBeads  []*beadsapi.BeadDetail
		crossProjectBeads []*beadsapi.BeadDetail
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		d.NewComments = relevantComments(gctx, anchor, actor, window)
		return nil
	})
	g.Go(func() error {
		sharedLabelBeads = fetchSharedLabelBeads(gctx, anchor, project)
		return nil
	})
	g.Go(func() error {
		crossProjectBeads = fetchCrossProjectBeads(gctx, anchor, project)
		return nil
	})
	g.Go(func() error {
		// The anchor's dependency edges give BOTH its blockers (blocks edges) and
		// its parent chain (parent-child edges) in one round-trip; the two
		// deps-dependent axes then run concurrently.
		deps, _ := daemon.GetDependencies(gctx, anchor.ID)
		blockerIDs, parentIDs := splitRelevanceDeps(deps)
		var ig errgroup.Group
		ig.Go(func() error {
			d.Blockers = relevantBlockers(gctx, blockerIDs, window)
			return nil
		})
		ig.Go(func() error {
			d.Container = relevanceContainer(gctx, parentIDs)
			if d.Container != nil {
				subtreeBeads = fetchSubtreeBeads(gctx, d.Container.ID)
			}
			return nil
		})
		_ = ig.Wait()
		return nil
	})
	_ = g.Wait()

	// Collect phase — sequential, preserving the exact dedup priority the
	// pre-parallelization code had: seed `seen` with the anchor and its blockers,
	// then subtree > shared-label > cross-project.
	seen := map[string]bool{anchor.ID: true}
	for _, b := range d.Blockers {
		seen[b.Bead.ID] = true
	}
	if len(subtreeBeads) > 0 {
		d.Subtree = collectRelevanceRows(subtreeBeads, window, seen, relevanceMaxRows, subtreeReason)
	}
	if len(sharedLabelBeads) > 0 {
		labels := topicalLabels(anchor.Labels)
		d.SharedLabel = collectRelevanceRows(sharedLabelBeads, window, seen, relevanceMaxRows, func(b *beadsapi.BeadDetail) string {
			return sharedLabelReason(b, labels)
		})
	}
	if len(crossProjectBeads) > 0 {
		d.CrossProject = collectRelevanceRows(crossProjectBeads, window, seen, relevanceMaxCross, crossProjectReason)
	}

	return d
}

// splitRelevanceDeps partitions an anchor's forward dependency edges into the
// beads it is blocked-by (blocks edges) and its parents (parent-child edges,
// canonical or legacy). Both encode target = DependsOnID.
func splitRelevanceDeps(deps []beadsapi.Dependency) (blockerIDs, parentIDs []string) {
	for _, dp := range deps {
		if dp.DependsOnID == "" {
			continue
		}
		switch dp.Type {
		case "blocks":
			blockerIDs = append(blockerIDs, dp.DependsOnID)
		case "parent-child", "parent":
			parentIDs = append(parentIDs, dp.DependsOnID)
		}
	}
	return blockerIDs, parentIDs
}

// relevantComments returns recent comments on the anchor authored by someone
// OTHER than the caller, newest first, capped. A fresh respawn that never saw
// the Slack thread reads its gardener's instructions here (audit miss M6).
func relevantComments(ctx context.Context, anchor *beadsapi.BeadDetail, actor string, window time.Duration) []beadsapi.Comment {
	comments, err := daemon.GetComments(ctx, anchor.ID)
	if err != nil || len(comments) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-window)
	actorLower := strings.ToLower(actor)
	var out []beadsapi.Comment
	for _, c := range comments {
		if !c.CreatedAt.IsZero() && c.CreatedAt.Before(cutoff) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(c.Author), actorLower) {
			continue
		}
		out = append(out, c)
	}
	// Newest first, then cap.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > relevanceMaxComments {
		out = out[:relevanceMaxComments]
	}
	return out
}

// relevantBlockers resolves the anchor's blocker beads and keeps: every open
// blocker (still blocking) and any blocker CLOSED within the window (recently
// cleared — the caller may be unblocked). Cleared blockers sort first because
// "you're unblocked" is the more actionable signal.
func relevantBlockers(ctx context.Context, ids []string, window time.Duration) []relevanceBlocker {
	if len(ids) == 0 {
		return nil
	}
	got, err := daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-window)
	var out []relevanceBlocker
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		b := got[id]
		if b == nil {
			continue
		}
		cleared := b.Status == "closed"
		if cleared && !b.UpdatedAt.IsZero() && b.UpdatedAt.Before(cutoff) {
			// An ancient closed blocker is history, not news.
			continue
		}
		out = append(out, relevanceBlocker{Bead: b, Cleared: cleared})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Cleared && !out[j].Cleared
	})
	return out
}

// relevanceContainer resolves the epic that scopes the anchor's subtree. It
// prefers an epic among the anchor's direct parents; failing that it climbs one
// level to catch a task -> sub-epic -> epic shape; otherwise it falls back to
// the first direct parent (a task-parent still scopes a meaningful subtree).
func relevanceContainer(ctx context.Context, parentIDs []string) *beadsapi.BeadDetail {
	if len(parentIDs) == 0 {
		return nil
	}
	got, err := daemon.BatchGetBeads(ctx, parentIDs)
	if err != nil {
		return nil
	}
	var firstParent *beadsapi.BeadDetail
	for _, id := range parentIDs {
		p := got[id]
		if p == nil {
			continue
		}
		if firstParent == nil {
			firstParent = p
		}
		if p.Type == "epic" {
			return p
		}
	}
	if firstParent == nil {
		return nil
	}
	// Climb one level looking for an epic grandparent.
	gdeps, err := daemon.GetDependencies(ctx, firstParent.ID)
	if err == nil {
		_, grandIDs := splitRelevanceDeps(gdeps)
		if len(grandIDs) > 0 {
			if ggot, gerr := daemon.BatchGetBeads(ctx, grandIDs); gerr == nil {
				for _, id := range grandIDs {
					if b := ggot[id]; b != nil && b.Type == "epic" {
						return b
					}
				}
			}
		}
	}
	return firstParent
}

// fetchSubtreeBeads returns the raw active + recently-closed work under the
// container epic (dedup/window/cap are applied later in the sequential collect
// phase). The daemon's transitive Parent filter covers the whole subtree so
// cousins under sibling sub-epics show too.
func fetchSubtreeBeads(ctx context.Context, epicID string) []*beadsapi.BeadDetail {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   epicID,
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress", "closed"},
		Sort:     "-updated_at",
		Limit:    40,
	})
	if err != nil {
		return nil
	}
	return res.Beads
}

// fetchSharedLabelBeads returns raw same-project candidates sharing a topical
// label with the anchor. Empty when the anchor carries no topical label.
func fetchSharedLabelBeads(ctx context.Context, anchor *beadsapi.BeadDetail, project string) []*beadsapi.BeadDetail {
	labels := topicalLabels(anchor.Labels)
	if len(labels) == 0 {
		return nil
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		LabelsAny: labels,
		Kinds:     []string{"issue"},
		Statuses:  []string{"in_progress", "closed"},
		// Same server-side per-run exclusion the in-progress/closed sections apply
		// (newsExcludeLabels): keep the caller's own cron run-instance siblings out
		// even when they share a genuinely topical label (ka-0000006165).
		ExcludeLabels: newsRunInstanceLabels,
		// Narrow to the caller's project server-side (drop rows carrying any OTHER
		// project: label) so LIMIT accounts for it, replacing the post-Limit
		// filterExclusiveProject pass (ka-0000003411). Empty project = no filter.
		ExclusiveProject: project,
		Sort:             "-updated_at",
		Limit:            40,
	})
	if err != nil {
		return nil
	}
	return res.Beads
}

// fetchCrossProjectBeads returns raw possible-duplicate candidates in OTHER
// projects, keyed on distinctive tokens from the anchor's title. Only meaningful
// when the caller is project-scoped (otherwise "other project" is undefined).
func fetchCrossProjectBeads(ctx context.Context, anchor *beadsapi.BeadDetail, project string) []*beadsapi.BeadDetail {
	if project == "" {
		return nil
	}
	kws := keywordsFromTitle(anchor.Title)
	if len(kws) == 0 {
		return nil
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Search:   strings.Join(kws, " "),
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress", "closed"},
		Sort:     "-updated_at",
		Limit:    30,
	})
	if err != nil {
		return nil
	}
	var beads []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b.ID == anchor.ID {
			continue
		}
		if hasOtherProject(b, project) {
			beads = append(beads, b)
		}
	}
	return beads
}

// collectRelevanceRows filters a candidate list to newsworthy rows: skip
// already-seen beads and closed beads older than the window, mark each seen,
// attach a reason, and cap. Mutates `seen` so later categories dedup against it.
func collectRelevanceRows(beads []*beadsapi.BeadDetail, window time.Duration, seen map[string]bool, cap int, reason func(*beadsapi.BeadDetail) string) []relevanceRow {
	cutoff := time.Now().Add(-window)
	var out []relevanceRow
	for _, b := range beads {
		if b == nil || seen[b.ID] {
			continue
		}
		if b.Status == "closed" && !b.UpdatedAt.IsZero() && b.UpdatedAt.Before(cutoff) {
			continue
		}
		seen[b.ID] = true
		out = append(out, relevanceRow{Bead: b, Reason: reason(b)})
		if len(out) >= cap {
			break
		}
	}
	return out
}

func subtreeReason(b *beadsapi.BeadDetail) string {
	if b.Status == "closed" {
		return "closed " + shortAge(b.UpdatedAt)
	}
	if b.Assignee != "" {
		return "in progress @" + b.Assignee
	}
	return "unassigned"
}

func sharedLabelReason(b *beadsapi.BeadDetail, labels []string) string {
	shared := sharedLabelName(b.Labels, labels)
	base := subtreeReason(b)
	if shared != "" {
		return base + " · shares " + shared
	}
	return base
}

func crossProjectReason(b *beadsapi.BeadDetail) string {
	proj := firstProjectLabel(b.Labels)
	state := "in progress"
	if b.Status == "closed" {
		state = "closed " + shortAge(b.UpdatedAt)
	}
	if proj != "" {
		return proj + ", " + state + " · possible duplicate"
	}
	return state + " · possible duplicate"
}

// relevanceNoisePrefixes are label namespaces that are operational, not
// topical: they never mean "these two beads are about the same thing".
//
// `trigger:` belongs here for the same reason `schedule:` does, and its absence
// was a live CRON-EXHAUST leak (rubric class RDY-1/NWS-4, bug ka-0000006165):
// the trigger engine stamps `trigger:<id>` on every per-run bead it spawns, so
// a gardener's own run-instance bead shares that label with all 12 of its prior
// firings. Measured 2026-08-21 01:53 on the news+ready relevance gardener: the
// "Shares a label with your bead (12)" section was 12/12 per-run exhaust of the
// caller's OWN cron, i.e. every row was the reader's own past self. The sibling
// sections exclude that class server-side via newsExcludeLabels; this axis
// filtered on labels only, so it had no such backstop.
var relevanceNoisePrefixes = []string{
	"project:", "role:", "agent:", "source:", "schedule:", "gardener:", "fleet:", "mr:",
	"trigger:",
}

// relevanceNoiseLabels are bare labels that are workflow scaffolding, not topic.
var relevanceNoiseLabels = map[string]bool{
	"plan": true, "no-auto-close": true, "needs-intake": true, "orphaned-open": true,
}

// topicalLabels keeps only labels that plausibly mean "same subject" — dropping
// project/role/source/etc. namespaces and known scaffolding labels — so a
// shared-label query does not fan out on project:gardenio (which every bead has).
func topicalLabels(labels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range labels {
		if relevanceNoiseLabels[l] || seen[l] {
			continue
		}
		noisy := false
		for _, p := range relevanceNoisePrefixes {
			if strings.HasPrefix(l, p) {
				noisy = true
				break
			}
		}
		if noisy {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

func sharedLabelName(beadLabels, want []string) string {
	set := map[string]bool{}
	for _, l := range want {
		set[l] = true
	}
	for _, l := range beadLabels {
		if set[l] {
			return l
		}
	}
	return ""
}

func firstProjectLabel(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "project:") {
			return l
		}
	}
	return ""
}

// hasOtherProject reports whether the bead carries a project label other than
// the caller's — i.e. it belongs to a different project's workstream.
func hasOtherProject(b *beadsapi.BeadDetail, project string) bool {
	want := "project:" + project
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "project:") && l != want {
			return true
		}
	}
	return false
}

var relevanceTokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

// relevanceStopwords are generic tokens that would over-match: command verbs,
// filler, and news's own vocabulary. Dropping them keeps the cross-project
// search's implicit-AND precise instead of matching every "gb ... news" bead.
var relevanceStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "this": true,
	"that": true, "your": true, "into": true, "gb": true, "news": true, "ready": true,
	"task": true, "fix": true, "add": true, "update": true, "bug": true, "gardenio": true,
}

// keywordsFromTitle extracts up to relevanceKeywordN distinctive tokens from a
// title (lowercased, length >= 4, non-stopword), deterministically ordered by
// length then alpha. The daemon's websearch_to_tsquery ANDs the returned terms,
// so a handful of distinctive tokens yields a precise duplicate probe.
func keywordsFromTitle(title string) []string {
	toks := relevanceTokenSplit.Split(strings.ToLower(title), -1)
	seen := map[string]bool{}
	var cand []string
	for _, t := range toks {
		if len(t) < 4 || relevanceStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		cand = append(cand, t)
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if len(cand[i]) != len(cand[j]) {
			return len(cand[i]) > len(cand[j])
		}
		return cand[i] < cand[j]
	})
	if len(cand) > relevanceKeywordN {
		cand = cand[:relevanceKeywordN]
	}
	return cand
}

// printRelevance renders the caller-scoped section. It leads with a blank line
// and mirrors the spacing of the other `gb news` sections so it slots in as the
// FIRST block of the digest.
func printRelevance(d *relevanceDigest, w io.Writer) {
	if d.empty() {
		return
	}
	title := withArms(d.Anchor.Heraldry, truncTitle(d.Anchor.Title, 60))
	fmt.Fprintf(w, "\nRelevant to your work (%s — %s):\n", d.Anchor.ID, title)

	if len(d.NewComments) > 0 {
		fmt.Fprintf(w, "\n  New comments on your bead (%d):\n", len(d.NewComments))
		for _, c := range d.NewComments {
			fmt.Fprintf(w, "    [%s] @%s: %s\n", shortAge(c.CreatedAt), c.Author, truncateComment(c.Text))
		}
	}

	if len(d.Blockers) > 0 {
		cleared := 0
		for _, b := range d.Blockers {
			if b.Cleared {
				cleared++
			}
		}
		heading := fmt.Sprintf("\n  Blockers (%d", len(d.Blockers))
		if cleared > 0 {
			heading += fmt.Sprintf(", %d recently cleared", cleared)
		}
		heading += "):\n"
		fmt.Fprint(w, heading)
		for _, b := range d.Blockers {
			printRelevanceBlocker(b, w)
		}
	}

	if d.Container != nil && len(d.Subtree) > 0 {
		ctitle := withArms(d.Container.Heraldry, truncTitle(d.Container.Title, 44))
		fmt.Fprintf(w, "\n  In your epic (%s — %s) (%d):\n", d.Container.ID, ctitle, len(d.Subtree))
		for _, r := range d.Subtree {
			printRelevanceRow(r, w)
		}
	}

	if len(d.SharedLabel) > 0 {
		fmt.Fprintf(w, "\n  Shares a label with your bead (%d):\n", len(d.SharedLabel))
		for _, r := range d.SharedLabel {
			printRelevanceRow(r, w)
		}
	}

	if len(d.CrossProject) > 0 {
		fmt.Fprintf(w, "\n  Similar work in other projects (%d) — check before re-doing:\n", len(d.CrossProject))
		for _, r := range d.CrossProject {
			printRelevanceRow(r, w)
		}
	}
}

func printRelevanceBlocker(b relevanceBlocker, w io.Writer) {
	title := withArms(b.Bead.Heraldry, truncTitle(b.Bead.Title, 56))
	typeStr := ""
	if b.Bead.Type != "" {
		typeStr = "[" + b.Bead.Type + "] "
	}
	if b.Cleared {
		fmt.Fprintf(w, "    ✓ %s %s— %s  (cleared %s — you may be unblocked)\n", b.Bead.ID, typeStr, title, shortAge(b.Bead.UpdatedAt))
		return
	}
	fmt.Fprintf(w, "    ⏳ %s %s— %s  (%s, still blocking)\n", b.Bead.ID, typeStr, title, b.Bead.Status)
}

func printRelevanceRow(r relevanceRow, w io.Writer) {
	title := withArms(r.Bead.Heraldry, truncTitle(r.Bead.Title, 56))
	typeStr := ""
	if r.Bead.Type != "" {
		typeStr = "[" + r.Bead.Type + "] "
	}
	fmt.Fprintf(w, "    %s %s— %s  (%s)\n", r.Bead.ID, typeStr, title, r.Reason)
}

func truncateComment(text string) string {
	text = strings.TrimSpace(newsDecisionWhitespace.ReplaceAllString(text, " "))
	if len(text) > 90 {
		return text[:89] + "…"
	}
	return text
}

// ── JSON projection ─────────────────────────────────────────────────────────

type newsJSONRelevance struct {
	Anchor       string               `json:"anchor"`
	Container    string               `json:"container,omitempty"`
	NewComments  []newsJSONRelComment `json:"new_comments,omitempty"`
	Blockers     []newsJSONRelBlocker `json:"blockers,omitempty"`
	EpicSubtree  []newsJSONRelRow     `json:"epic_subtree,omitempty"`
	SharedLabel  []newsJSONRelRow     `json:"shared_label,omitempty"`
	CrossProject []newsJSONRelRow     `json:"cross_project,omitempty"`
}

type newsJSONRelComment struct {
	Author    string `json:"author"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at,omitempty"`
}

type newsJSONRelBlocker struct {
	ID      string `json:"id"`
	Type    string `json:"type,omitempty"`
	Title   string `json:"title"`
	Status  string `json:"status,omitempty"`
	Cleared bool   `json:"cleared"`
}

type newsJSONRelRow struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   string `json:"status,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// relevanceToJSON projects the digest into the stable --json shape. Returns nil
// for an empty/absent digest so the `relevant` key is omitted entirely (keeping
// the top-level shape stable for existing `.agents[]` consumers).
func relevanceToJSON(d *relevanceDigest) *newsJSONRelevance {
	if d.empty() {
		return nil
	}
	out := &newsJSONRelevance{Anchor: d.Anchor.ID}
	if d.Container != nil {
		out.Container = d.Container.ID
	}
	for _, c := range d.NewComments {
		var created string
		if !c.CreatedAt.IsZero() {
			created = c.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.NewComments = append(out.NewComments, newsJSONRelComment{
			Author: c.Author, Text: c.Text, CreatedAt: created,
		})
	}
	for _, b := range d.Blockers {
		out.Blockers = append(out.Blockers, newsJSONRelBlocker{
			ID: b.Bead.ID, Type: b.Bead.Type, Title: b.Bead.Title,
			Status: b.Bead.Status, Cleared: b.Cleared,
		})
	}
	out.EpicSubtree = relRowsToJSON(d.Subtree)
	out.SharedLabel = relRowsToJSON(d.SharedLabel)
	out.CrossProject = relRowsToJSON(d.CrossProject)
	return out
}

func relRowsToJSON(rows []relevanceRow) []newsJSONRelRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]newsJSONRelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, newsJSONRelRow{
			ID: r.Bead.ID, Type: r.Bead.Type, Title: r.Bead.Title,
			Status: r.Bead.Status, Assignee: r.Bead.Assignee, Reason: r.Reason,
		})
	}
	return out
}
