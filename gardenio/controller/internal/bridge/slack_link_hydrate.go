package bridge

// slack_link_hydrate.go turns bare bead ids in outgoing Slack text into
// readable links, and appends a References footer when a message names
// several beads.
//
// Before: `P0 (gd-lGvUZtjpRQ) has a live agent` → the linkify transport wraps
// the id as `<viewer/gd-lGvUZtjpRQ|gd-lGvUZtjpRQ>`: a link whose label still
// says nothing.
//
// After (Garden theme): `P0 (🌱 <viewer/gd-lGvUZtjpRQ|forward duration_ms
// through gb hook relay>) has a live agent`, and when the message mentions
// three or more distinct beads, a compact footer — one line per bead, type
// glyph, status glyph, linked name, and a type-specific tail (priority,
// assignee, "awaiting operator") — sorted so the items that need a human come
// first. No bead id appears anywhere; the link carries it.
//
// The pass runs inside the linkify transport (slack_linkify.go) ahead of the
// regex rules, so hydrated spans are protected from them and any id the daemon
// could not resolve still gets today's id-labelled link. It is gated by
// SLACK_LINK_HYDRATE_ENABLED (default off) and degrades to the old behaviour on
// any daemon error or timeout: a Slack post never fails or stalls because of
// it. Design thread: gd-8Gxghn6hkt; spec: gd-jciXhNJYAu.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

const (
	// hydrateFooterMin is the number of distinct resolved beads at which a
	// message gets a References footer. Below it the inline labels suffice.
	hydrateFooterMin = 3
	// hydrateFooterMax caps footer lines; the rest fold into "+N more".
	hydrateFooterMax = 6
	// hydrateMaxIDs bounds how many ids one message may hydrate (also the
	// daemon batch cap, so one round-trip always suffices).
	hydrateMaxIDs = 50
	// hydrateCacheTTL bounds how stale a bead's status/assignee may be in a
	// label. Titles are effectively immutable; status flips within a minute
	// are rare and the next message corrects them.
	hydrateCacheTTL = 60 * time.Second
	// hydrateTimeout bounds the daemon round-trip inside the Slack HTTP path.
	hydrateTimeout = 3 * time.Second
	// hydrateCacheMax caps cache entries. Entries are tiny (render fields
	// only) but ids are unbounded over the bridge's lifetime; at the cap the
	// soonest-to-expire entries are evicted.
	hydrateCacheMax = 2048
	// hydrateMaxSection is the Block Kit section text limit (3000) less
	// headroom. A section text node whose hydrated text would exceed it is
	// left as-is.
	hydrateMaxSection = 2900
	// hydrateMaxContext is the Block Kit limit for context-block elements and
	// section fields (2000) less headroom. Every mrkdwn node that is not a
	// section's own text is held to it, as is the footer.
	hydrateMaxContext = 1900
	// hydrateMaxBlocks is Slack's per-message block cap. The footer adds a
	// block, so a message already at the cap gets inline labels only.
	hydrateMaxBlocks = 50
)

// linkHydrator resolves bead ids to display data for the linkify transport.
// fetch and theme are injected (the Bot's batchGetBeads / themeFor) so the
// transport can be built before the Bot exists and unit-tested without one.
type linkHydrator struct {
	viewerURL string
	fetch     func(ctx context.Context, ids []string) map[string]*beadsapi.BeadDetail
	theme     func(ctx context.Context, channel string) Theme
	logger    *slog.Logger

	mu    sync.Mutex
	cache map[string]hydrateCacheEntry
	now   func() time.Time
}

// hydrateCacheEntry caches a lookup result, including misses (nil bead), so a
// run of messages mentioning an unknown id does not re-query the daemon. The
// bead is a slim copy (see slimBead), never the daemon's full BeadDetail.
type hydrateCacheEntry struct {
	bead    *beadsapi.BeadDetail
	expires time.Time
}

// slimBead keeps only what the renderer reads: id, type, status, priority,
// assignee, the plan label, the arms (heraldry spec §5: the emoji run stands
// in for the type glyph, so dropping it here would drop it before render), and
// the resolved display name (carried in the short_title column, which
// beadDisplayName honours first and which is stable under re-truncation). Titles, descriptions, notes and the fields, the bulk
// of a bead, are dropped so a cached id costs a few hundred bytes rather than
// a bead body.
func slimBead(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if b == nil {
		return nil
	}
	out := &beadsapi.BeadDetail{
		ID:         b.ID,
		Type:       b.Type,
		Status:     b.Status,
		Priority:   b.Priority,
		Assignee:   b.Assignee,
		ShortTitle: beadDisplayName(b),
		Heraldry:   b.Heraldry,
	}
	if beadIsPlan(b) {
		out.Labels = []string{planBadgeUnfurlLabel}
	}
	return out
}

// sweepLocked drops expired entries and, if the cache is still over
// hydrateCacheMax, evicts the soonest-to-expire until it fits. Callers hold
// h.mu.
func (h *linkHydrator) sweepLocked(now time.Time) {
	for id, e := range h.cache {
		if !now.Before(e.expires) {
			delete(h.cache, id)
		}
	}
	if len(h.cache) <= hydrateCacheMax {
		return
	}
	type idExp struct {
		id  string
		exp time.Time
	}
	all := make([]idExp, 0, len(h.cache))
	for id, e := range h.cache {
		all = append(all, idExp{id, e.expires})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].exp.Before(all[j].exp) })
	for _, e := range all[:len(all)-hydrateCacheMax] {
		delete(h.cache, e.id)
	}
}

func newLinkHydrator(viewerURL string, logger *slog.Logger) *linkHydrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &linkHydrator{
		viewerURL: strings.TrimRight(viewerURL, "/"),
		logger:    logger,
		cache:     make(map[string]hydrateCacheEntry),
		now:       time.Now,
	}
}

// ready reports whether the hydrator has everything it needs to run.
func (h *linkHydrator) ready() bool {
	return h != nil && h.viewerURL != "" && h.fetch != nil
}

// resolve returns the beads for ids, from cache where fresh and from one
// daemon batch call for the rest. Unknown ids are absent from the result.
// A fetch failure yields only the cached subset (possibly empty).
func (h *linkHydrator) resolve(ctx context.Context, ids []string) map[string]*beadsapi.BeadDetail {
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	var missing []string
	now := h.now()

	h.mu.Lock()
	for _, id := range ids {
		if e, ok := h.cache[id]; ok && now.Before(e.expires) {
			if e.bead != nil {
				out[id] = e.bead
			}
			continue
		}
		missing = append(missing, id)
	}
	h.mu.Unlock()

	if len(missing) == 0 {
		return out
	}
	fetched := h.fetch(ctx, missing)
	h.mu.Lock()
	// Sweep before inserting: the map is only ever touched here, so this is
	// what keeps a long-lived bridge from accumulating every id it has posted.
	h.sweepLocked(now)
	for _, id := range missing {
		bead := slimBead(fetched[id]) // nil when unknown or when the fetch failed
		h.cache[id] = hydrateCacheEntry{bead: bead, expires: now.Add(hydrateCacheTTL)}
		if bead != nil {
			out[id] = bead
		}
	}
	h.mu.Unlock()
	return out
}

// hydrateForm applies the hydration pass to a parsed chat.* form body. It
// returns true when it changed vals. Everything here is best-effort: any
// failure leaves vals untouched for the regex rules to handle as before.
func (h *linkHydrator) hydrateForm(ctx context.Context, vals url.Values) bool {
	if !h.ready() {
		return false
	}
	text := vals.Get("text")
	blocks := vals.Get("blocks")
	atts := vals.Get("attachments")

	ids := collectHydrateIDs(text, blocks, atts)
	if len(ids) == 0 {
		return false
	}

	rctx, cancel := context.WithTimeout(ctx, hydrateTimeout)
	defer cancel()
	beads := h.resolve(rctx, ids)
	if len(beads) == 0 {
		return false
	}
	th := Garden
	if h.theme != nil {
		th = h.theme(rctx, vals.Get("channel"))
	}
	r := hydrateRenderer{viewerURL: h.viewerURL, theme: th, beads: beads}

	changed := false
	if text != "" {
		if nt := r.inline(text); nt != text {
			vals.Set("text", nt)
			changed = true
		}
	}
	if blocks != "" {
		if nb, ok := r.inlineJSON(blocks); ok {
			vals.Set("blocks", nb)
			blocks = nb
			changed = true
		}
	}
	if atts != "" {
		if na, ok := r.inlineJSON(atts); ok {
			vals.Set("attachments", na)
			changed = true
		}
	}

	// References footer: only for messages naming several beads, and only
	// on the message body (attachments are cards with their own layout).
	if footer := r.footer(ids); footer != "" {
		switch {
		case blocks != "":
			if nb, ok := appendContextBlock(blocks, footer); ok {
				vals.Set("blocks", nb)
				changed = true
			}
		case text != "":
			vals.Set("text", vals.Get("text")+"\n"+quoteLines(footer))
			changed = true
		}
	}
	return changed
}

// collectHydrateIDs gathers the distinct bare bead ids across the three
// user-visible form fields, in first-seen order, skipping ids already inside
// a link or URL (those were authored as links; we leave them alone).
func collectHydrateIDs(text, blocks, atts string) []string {
	seen := make(map[string]bool)
	var ids []string
	add := func(s string) {
		for _, id := range scanBareBeadIDsAll(s) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	add(text)
	for _, j := range []string{blocks, atts} {
		if j == "" {
			continue
		}
		var data any
		if err := json.Unmarshal([]byte(j), &data); err != nil {
			continue
		}
		walkMrkdwn(data, func(s string) string { add(s); return s })
	}
	if len(ids) > hydrateMaxIDs {
		ids = ids[:hydrateMaxIDs]
	}
	return ids
}

// scanBareBeadIDsAll is scanBareBeadIDs without its preview cap.
func scanBareBeadIDsAll(text string) []string {
	if text == "" {
		return nil
	}
	var ids []string
	s := text
	for len(s) > 0 {
		start, length := nextProtectedSpan(s)
		var chunk string
		if start < 0 {
			chunk, s = s, ""
		} else {
			chunk, s = s[:start], s[start+length:]
		}
		ids = append(ids, bareBeadIDPattern.FindAllString(chunk, -1)...)
	}
	return ids
}

// walkMrkdwn visits every mrkdwn text node in decoded Block Kit JSON and
// replaces it with fn's result. Returns true when any node changed.
func walkMrkdwn(node any, fn func(string) string) bool {
	return walkMrkdwnLimit(node, hydrateMaxContext, func(s string, _ int) string { return fn(s) })
}

// walkMrkdwnLimit is walkMrkdwn that also tells fn the Block Kit length limit
// for the node: hydrateMaxSection for a section block's own text field,
// otherwise limit (context elements, section fields, attachment text).
func walkMrkdwnLimit(node any, limit int, fn func(string, int) string) bool {
	changed := false
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			if walkMrkdwnLimit(item, limit, fn) {
				changed = true
			}
		}
	case map[string]any:
		isSection := false
		if t, _ := v["type"].(string); t == "section" {
			isSection = true
		}
		if t, _ := v["type"].(string); t == "mrkdwn" {
			if text, ok := v["text"].(string); ok {
				if nt := fn(text, limit); nt != text {
					v["text"] = nt
					changed = true
				}
			}
		}
		for k, val := range v {
			l := limit
			if isSection && k == "text" {
				l = hydrateMaxSection
			}
			if walkMrkdwnLimit(val, l, fn) {
				changed = true
			}
		}
	}
	return changed
}

// hydrateRenderer renders one message's ids against one resolved bead set
// and one theme.
type hydrateRenderer struct {
	viewerURL string
	theme     Theme
	beads     map[string]*beadsapi.BeadDetail
}

func (r hydrateRenderer) link(id string) string { return r.viewerURL + "/" + id }

// inline rewrites each resolved bare id in s (outside protected spans) as
// `<mark> <url|name>`, the mark being the bead's emoji run or its type glyph
// (inlineChunk). Unresolved ids are left for the regex rules.
func (r hydrateRenderer) inline(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 64)
	runOnLine := false
	rest := s
	for len(rest) > 0 {
		start, length := nextProtectedSpan(rest)
		if start < 0 {
			b.WriteString(r.inlineChunk(rest, &runOnLine))
			break
		}
		b.WriteString(r.inlineChunk(rest[:start], &runOnLine))
		span := rest[start : start+length]
		if strings.Contains(span, "\n") {
			runOnLine = false
		}
		b.WriteString(span)
		rest = rest[start+length:]
	}
	return b.String()
}

// inlineChunk hydrates the ids in one unprotected chunk. runOnLine carries,
// across chunks, whether the current line already shows an emoji run: a line
// shows the run only for the first bead with arms it names (heraldry spec
// R10), and later beads on that line take their type glyph as before.
func (r hydrateRenderer) inlineChunk(chunk string, runOnLine *bool) string {
	var b strings.Builder
	last := 0
	for _, m := range bareBeadIDPattern.FindAllStringIndex(chunk, -1) {
		between := chunk[last:m[0]]
		if strings.Contains(between, "\n") {
			*runOnLine = false
		}
		b.WriteString(between)
		last = m[1]
		id := chunk[m[0]:m[1]]
		bead := r.beads[id]
		if bead == nil {
			b.WriteString(id)
			continue
		}
		label := fmt.Sprintf("<%s|%s>", r.link(id), slackMrkdwnLabelEscaper.Replace(beadDisplayName(bead)))
		g := r.inlineGlyph(bead)
		if run := armsRun(bead.Heraldry); run != "" && !*runOnLine {
			g = run
			*runOnLine = true
		}
		if g != "" {
			b.WriteString(g + " ")
		}
		b.WriteString(label)
	}
	tail := chunk[last:]
	if strings.Contains(tail, "\n") {
		*runOnLine = false
	}
	b.WriteString(tail)
	return b.String()
}

// inlineGlyph is the type marker that precedes an inline link. Garden marks
// every type; Business marks only the types that change how a sentence reads
// (plan, decision, escalation) since its bracket tags are heavy in prose.
func (r hydrateRenderer) inlineGlyph(bead *beadsapi.BeadDetail) string {
	kind := hydrateKind(bead)
	if r.theme.Name == Business.Name {
		switch kind {
		case "plan", "decision", "escalation":
		default:
			return ""
		}
	}
	return hydrateTypeGlyph(r.theme, bead)
}

// inlineJSON applies inline to every mrkdwn node of a Block Kit JSON string,
// skipping any node whose hydrated text would breach its Block Kit limit:
// 3000 for a section's own text, 2000 for context elements and section fields.
func (r hydrateRenderer) inlineJSON(j string) (string, bool) {
	var data any
	if err := json.Unmarshal([]byte(j), &data); err != nil {
		return j, false
	}
	changed := walkMrkdwnLimit(data, hydrateMaxContext, func(s string, limit int) string {
		nt := r.inline(s)
		if utf8.RuneCountInString(nt) > limit {
			return s
		}
		return nt
	})
	if !changed {
		return j, false
	}
	return encodeBlocksJSON(data, j)
}

// encodeBlocksJSON re-encodes decoded Block Kit JSON without HTML escaping
// (so `<url|label>` survives). Falls back to the original on error.
func encodeBlocksJSON(data any, orig string) (string, bool) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return orig, false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

// footer renders the References footer for ids, or "" when fewer than
// hydrateFooterMin resolved. Lines are sorted attention-first and capped.
func (r hydrateRenderer) footer(ids []string) string {
	var beads []*beadsapi.BeadDetail
	for _, id := range ids {
		if b := r.beads[id]; b != nil {
			beads = append(beads, b)
		}
	}
	if len(beads) < hydrateFooterMin {
		return ""
	}
	sort.SliceStable(beads, func(i, j int) bool {
		ki, kj := hydrateKindRank(beads[i]), hydrateKindRank(beads[j])
		if ki != kj {
			return ki < kj
		}
		return hydrateStatusRank(beads[i].Status) < hydrateStatusRank(beads[j].Status)
	})
	var lines []string
	for i, b := range beads {
		if i == hydrateFooterMax {
			lines = append(lines, fmt.Sprintf("+%d more", len(beads)-i))
			break
		}
		lines = append(lines, r.footerLine(b))
	}
	out := strings.Join(lines, "\n")
	if utf8.RuneCountInString(out) > hydrateMaxContext {
		return ""
	}
	return out
}

// footerLine is `<mark> <status glyph> <url|name> · <tail>`, the mark being
// the bead's emoji run when it bears arms (heraldry spec R9: the authored arms
// take the derived type glyph's place, one bead per line) and its type glyph
// otherwise. The tail is type-specific and omitted when it would carry
// nothing.
func (r hydrateRenderer) footerLine(b *beadsapi.BeadDetail) string {
	mark := armsRun(b.Heraldry)
	if mark == "" {
		mark = hydrateTypeGlyph(r.theme, b)
	}
	parts := []string{
		mark,
		beadStatusEmoji(r.theme, b.Status),
		fmt.Sprintf("<%s|%s>", r.link(b.ID), slackMrkdwnLabelEscaper.Replace(beadDisplayName(b))),
	}
	line := strings.Join(parts, " ")
	if tail := hydrateTail(b); tail != "" {
		line += " · " + tail
	}
	return line
}

// hydrateTail picks the one extra fact worth a footer column for this bead.
func hydrateTail(b *beadsapi.BeadDetail) string {
	if b.Status == "closed" {
		return ""
	}
	switch hydrateKind(b) {
	case "decision", "plan", "epic":
		return fmt.Sprintf("P%d", b.Priority)
	case "escalation":
		return "awaiting operator"
	case "agent":
		return ""
	default:
		if a := strings.TrimSpace(b.Assignee); a != "" {
			return "@" + strings.TrimPrefix(slackMrkdwnLabelEscaper.Replace(a), "@")
		}
		if b.Status == "blocked" {
			return ""
		}
		return "unassigned"
	}
}

// hydrateKind is the bead's rendering kind: its type, except that a plan
// epic renders as "plan".
func hydrateKind(b *beadsapi.BeadDetail) string {
	if beadIsPlan(b) {
		return "plan"
	}
	return b.Type
}

// hydrateTypeGlyph is the theme's badge for the bead's kind (Theme.BeadTypeGlyph).
// Unlike Theme.BeadTypeBadge it never pairs the glyph with the type word:
// the footer and prose are deliberately glyph-only (operator choice).
func hydrateTypeGlyph(th Theme, b *beadsapi.BeadDetail) string {
	return th.BeadTypeGlyph(hydrateKind(b))
}

// hydrateKindRank orders footer lines: things that need a human first.
func hydrateKindRank(b *beadsapi.BeadDetail) int {
	switch hydrateKind(b) {
	case "decision":
		return 0
	case "escalation":
		return 1
	case "plan", "epic":
		return 2
	case "agent":
		return 4
	default:
		return 3
	}
}

func hydrateStatusRank(status string) int {
	switch status {
	case "blocked":
		return 0
	case "in_progress":
		return 1
	case "open":
		return 2
	case "closed":
		return 3
	default:
		return 4
	}
}

// appendContextBlock appends one context block carrying footer to a Block
// Kit blocks JSON array.
func appendContextBlock(blocks, footer string) (string, bool) {
	var arr []any
	if err := json.Unmarshal([]byte(blocks), &arr); err != nil {
		return blocks, false
	}
	if len(arr) >= hydrateMaxBlocks {
		return blocks, false
	}
	arr = append(arr, map[string]any{
		"type": "context",
		"elements": []any{
			map[string]any{"type": "mrkdwn", "text": footer},
		},
	})
	return encodeBlocksJSON(arr, blocks)
}

// quoteLines renders footer as a Slack quote block for text-only messages.
func quoteLines(footer string) string {
	lines := strings.Split(footer, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}
