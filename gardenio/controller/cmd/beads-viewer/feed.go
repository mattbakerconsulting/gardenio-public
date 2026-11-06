package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// feed.go — activity-feed page behind /feed, an opt-in route that leaves /
// untouched (task ka-0000003020, epic ka-0000003263). Design of record:
// gardenio/docs/beads-viewer-activity-feed-ux-proposals.md. The stream view and
// the Lanes view (?view=lanes, one column per bead status) share one handler and
// one SSE subscription; only the client layout differs.
//
// The page first-paints a backfill, then the client opens an EventSource on
// /feed/events, which subscribes to the bead-lifecycle stream (beads.bead.>)
// and writes one pre-rendered HTML card per event (the agent_events.go idiom).
//
// Every family the feed classifies (squawks, mail, agent, MR and pipeline) is
// a typed bead on that one stream, so the multi-family feed needs no new daemon
// endpoint, only a broader classifier (classifyFeedEvent / feedKindForBead). Phase 3: task ka-0000006994, epic ka-0000004907, inventory
// /bundle/ka-0000004916.

// feedEventTopics is the NATS topic glob the feed subscribes to; bead
// lifecycle carries every family the feed renders.
const feedEventTopics = "beads.bead.>"

// feedBackfillLimit caps the rows painted on first load before the live stream
// takes over (ka-0000003901). Keep it equal to the client DOM cap (feed.js
// maxRows) so the whole seed survives the trim.
const feedBackfillLimit = 200

// feedRailAgentsCap bounds the "Live agents" glance in the right rail.
const feedRailAgentsCap = 8

// feedEventSSE is the wrapper kbeads publishes on bead lifecycle SSE events.
// It mirrors agentEventSSE (agent_events.go), kept local so the feed can evolve.
type feedEventSSE struct {
	Bead     *feedEventBead `json:"bead"`
	ClosedBy string         `json:"closed_by,omitempty"`
	Changes  map[string]any `json:"changes,omitempty"`
}

// feedEventBead is the minimal subset of bead fields a feed card renders.
type feedEventBead struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Status    string          `json:"status"`
	Assignee  string          `json:"assignee"`
	CreatedBy string          `json:"created_by"`
	Labels    []string        `json:"labels"`
	Priority  int             `json:"priority"`
	Fields    json.RawMessage `json:"fields"`

	// Operator identity, for the client's "mine" scope. OperatedByEmail is the
	// resolved email matched against the viewer's OIDC email; OperatedBy is the
	// raw operator id (e.g. "oidc:matt@…", "slack:U…"), a fallback chip.
	OperatedBy      string `json:"operated_by"`
	OperatedByEmail string `json:"operated_by_email"`
}

// feedKind is one entry in the color-coded event taxonomy from the UX
// proposal. The client (feed.js) buckets cards into the status lanes by reading
// data-lane, which is derived from the kind here.
type feedKind struct {
	Name string // machine name, used as the SSE event: name and CSS modifier
	Lane string // Lanes-view bucket — a bead status: "open" | "in_progress" | "blocked" | "done"
}

// Each kind's Lane is the bead status its lifecycle event lands the bead in
// (open / in_progress / blocked / done). feed.js reads the
// .feed-lane-body[data-lane] elements in feed.html, so the lane set is defined
// entirely by these Lane values plus the template.
var (
	feedKindDecision = feedKind{"decision", "in_progress"}
	feedKindClosed   = feedKind{"closed", "done"}
	feedKindMRMerged = feedKind{"mr-merged", "done"}
	feedKindClaimed  = feedKind{"claimed", "in_progress"}
	feedKindBlocked  = feedKind{"blocked", "blocked"}
	feedKindCreated  = feedKind{"created", "open"}
	// feedWorkKind routes in_progress/blocked updates elsewhere, so an
	// "updated" bead is still open.
	feedKindUpdated = feedKind{"updated", "open"}

	// Phase 3 multi-family kinds (task ka-0000006994). Colors follow
	// docs/beads-viewer-activity-feed-ux-proposals.md §"Event taxonomy".
	feedKindMROpened = feedKind{"mr-opened", "open"}       // merge-request bead, opened (purple, lighter)
	feedKindPipeline = feedKind{"pipeline", "in_progress"} // pipeline status on an MR bead (green ok / red failed)
	feedKindSquawk   = feedKind{"squawk", "in_progress"}   // message bead carrying squawk text (teal)
	feedKindMail     = feedKind{"mail", "in_progress"}     // mail bead (teal, "needs you"-adjacent)
	feedKindSpawned  = feedKind{"spawned", "in_progress"}  // agent bead spawn (grey, noise-by-default)
	feedKindStopped  = feedKind{"stopped", "done"}         // agent bead stop/close (grey)
	feedKindGate     = feedKind{"gate", "in_progress"}     // agent gate transition (amber, decision-adjacent)
)

// feedScopeDefault is the default scope value when neither a URL param nor a
// stored preference selects one. Why "everyone": most events are agent-driven
// and carry no operator email, so "mine" left the feed nearly empty
// (ka-0000003901 / ka-0000002242).
const feedScopeDefault = "everyone"

// handleFeed renders the activity-feed page shell: chrome, view toggle, scope,
// project and user filters, the "needs you" rail, and a first-paint backfill
// the client augments live from /feed/events. Filters (ka-0000003403):
//   - scope:   ?scope= → "mine" | "everyone" (default: feedScopeDefault).
//   - project: ?project= → a project name | "" (all). Default is the viewer's
//     `default_spawn_project` preference.
//   - user:    ?user= → one operator's activity | "" (all).
func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	ctx := r.Context()

	view := r.URL.Query().Get("view")
	if view != "lanes" {
		view = "stream" // Concept A default
	}

	// Best-effort: anonymous or bead-less viewers get no default.
	defaultProject := s.feedDefaultProjectForViewer(ctx, email)

	// Explicit ?scope= wins; an anonymous viewer always gets "everyone", since
	// "mine" with no identity is empty (ka-0000003901).
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	switch scope {
	case "mine", "everyone":
		// honor explicit choice
	default:
		if email != "" {
			scope = feedScopeDefault
		} else {
			scope = "everyone"
		}
	}

	// Has() lets an explicit empty "?project=" mean "all projects" even when a
	// default is set.
	var project string
	if r.URL.Query().Has("project") {
		project = strings.TrimSpace(r.URL.Query().Get("project"))
	} else {
		project = defaultProject
	}

	// First-paint seed from the durable event log (GET /v1/events, Phase 2,
	// ka-0000000643), so the warm paint matches what arrives live. The same scan
	// yields the user dropdown (operators, most recently active first).
	// Best-effort: a daemon hiccup yields an empty seed.
	backfill, users := s.feedSeedCardsAndUsers(ctx, feedBackfillLimit)
	rail := s.computeIndexRail(ctx, email)

	// Lower-cased to match the cards' data-operator[-email] attributes and the
	// harvested option keys.
	user := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("user")))

	if len(rail.Agents) > feedRailAgentsCap {
		rail.Agents = rail.Agents[:feedRailAgentsCap]
	}

	data := map[string]any{
		"Title":          "Activity feed",
		"ViewerEmail":    email,
		"InboxCount":     s.inboxCountForViewer(ctx, email),
		"View":           view,
		"Backfill":       backfill,
		"Rail":           rail,
		"Scope":          scope,
		"Project":        project,
		"DefaultProject": defaultProject,
		"Projects":       s.feedProjectNames(ctx),
		// SelectedUser keeps the dropdown choice across reloads.
		"Users":        users,
		"SelectedUser": user,
		// HasViewer gates the "Mine" control.
		"HasViewer": email != "",
		// R5: the feed is moving into the garden's project panel (garden_retire.go).
		"GardenHref": s.gardenBanner(r, retireActivity, gardenActivityTarget),
	}
	s.render(w, r, "feed.html", data)
}

// feedDefaultProjectForViewer returns the viewer's user-bead
// `default_spawn_project` preference, or "" (all projects) when the viewer is
// anonymous, has no user bead, or set no default.
func (s *Server) feedDefaultProjectForViewer(ctx context.Context, email string) string {
	if email == "" {
		return ""
	}
	bead, err := s.findUserBeadByEmail(ctx, email)
	if err != nil || bead == nil {
		if err != nil {
			s.logger.Debug("feed: looking up user bead for default project", "email", email, "error", err)
		}
		return ""
	}
	return strings.TrimSpace(parsePreferences(bead.Fields).DefaultSpawnProject)
}

// feedProjectNames returns the sorted project names for the filter dropdown,
// or nil on a daemon error.
func (s *Server) feedProjectNames(ctx context.Context) []string {
	projectsMap, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		s.logger.Debug("feed: listing projects for filter", "error", err)
		return nil
	}
	names := make([]string, 0, len(projectsMap))
	for name := range projectsMap {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// feedBackfillCard is the template view model for one first-paint card. HTML
// comes from the live stream's renderer, so the initial paint matches what
// arrives live. Lane is the status bucket, so the handler can SSR the same
// cards into the lanes view (ka-0000001008).
type feedBackfillCard struct {
	HTML template.HTML
	Lane string
}

// feedSeedWindow bounds how far back the durable first-paint seed reaches; the
// paging loop and the reconnect-replay cap share it.
const feedSeedWindow = 72 * time.Hour

// feedSeedMaxPages caps the durable-seed paging loop so first paint can't
// become an unbounded scan. Alone it still allowed a ~69s cold render under a
// comment flood, hence feedSeedMaxRawEventsScanned (gd-QnY7HUw55v,
// gd-4q76kkHR3V).
const feedSeedMaxPages = 20

// feedSeedMaxRawEventsScanned stops the paging loop once this many raw events
// have been scanned even if `distinct` never reaches `limit`: a comment/squawk
// flood on a few beads never grows `distinct` (gd-4q76kkHR3V).
const feedSeedMaxRawEventsScanned = 4000

// feedUser is one entry in the user-filter dropdown: a distinct operator seen
// in the seed window. Key is the lower-cased value the client matches against a
// card's data-operator-email / data-operator; Label is the option text;
// LastActive drives the sort (most recent first).
type feedUser struct {
	Key        string
	Label      string
	LastActive time.Time
}

// feedSeedCacheTTL bounds how stale a served first-paint seed can be, so a
// render burst shares one scan. The live stream takes over after paint, so the
// staleness is invisible (ka-0000002634).
const feedSeedCacheTTL = 5 * time.Second

// feedSeedSnapshot is one cached first-paint seed: the rendered backfill cards
// (newest-first) plus the operator dropdown.
type feedSeedSnapshot struct {
	cards []feedBackfillCard
	users []feedUser
}

// feedSeedCache memoizes the viewer-independent /feed first-paint seed for a
// short TTL. Process-local; mu guards all fields. Keyed on the seed limit so a
// different depth never gets a wrong-sized snapshot.
type feedSeedCache struct {
	mu        sync.Mutex
	limit     int
	snapshot  feedSeedSnapshot
	fetchedAt time.Time
}

// get returns the cached snapshot when it was built for the same limit and is
// within ttl of fetchedAt. The boolean is true on a fresh hit.
func (c *feedSeedCache) get(now time.Time, limit int, ttl time.Duration) (feedSeedSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() || c.limit != limit || now.Sub(c.fetchedAt) > ttl {
		return feedSeedSnapshot{}, false
	}
	return c.snapshot, true
}

// set replaces the cached snapshot and stamps the limit it was built for.
func (c *feedSeedCache) set(snap feedSeedSnapshot, limit int, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = snap
	c.limit = limit
	c.fetchedAt = now
}

// feedSeedCardsAndUsers returns the first-paint cards (newest-first, capped at
// limit) and the operator dropdown, fronted by feedSeedCache (ka-0000002634).
// Only a non-empty build is cached, so an empty seed after a daemon error is
// retried on the next load.
func (s *Server) feedSeedCardsAndUsers(ctx context.Context, limit int) ([]feedBackfillCard, []feedUser) {
	// Nil-safe for bare test Servers. A scoped principal's seed is its own
	// project's events, so it neither reads nor fills the shared snapshot.
	if s.feedSeedCache == nil || isScopedCtx(ctx) {
		return s.buildFeedSeedCardsAndUsers(ctx, limit)
	}
	now := time.Now()
	if snap, ok := s.feedSeedCache.get(now, limit, feedSeedCacheTTL); ok {
		return snap.cards, snap.users
	}
	cards, users := s.buildFeedSeedCardsAndUsers(ctx, limit)
	if len(cards) > 0 {
		s.feedSeedCache.set(feedSeedSnapshot{cards: cards, users: users}, limit, now)
	}
	return cards, users
}

// buildFeedSeedCardsAndUsers pages the durable event log once and returns the
// first-paint cards (newest-first, capped at limit) and the operators seen in
// the window (most recently active first). Best-effort: a daemon error returns
// whatever was harvested so far.
func (s *Server) buildFeedSeedCardsAndUsers(ctx context.Context, limit int) ([]feedBackfillCard, []feedUser) {
	now := time.Now()
	cards, users := s.collectFeedRenderedCardsAndUsers(ctx, now.Add(-feedSeedWindow), now, limit)
	// Reverse the oldest-first durable order for the newest-first paint.
	out := make([]feedBackfillCard, 0, len(cards))
	for i := len(cards) - 1; i >= 0; i-- {
		out = append(out, feedBackfillCard{HTML: template.HTML(cards[i].HTML), Lane: cards[i].Lane})
	}
	return out, users
}

// feedUserFromBead derives the dropdown key+label for an event bead's operator:
// the resolved email if set, else the raw operator id (e.g. "slack:U123"). ok
// is false when the bead has no operator identity (still rendered as a card).
func feedUserFromBead(b *feedEventBead) (key, label string, ok bool) {
	if email := strings.TrimSpace(b.OperatedByEmail); email != "" {
		return strings.ToLower(email), email, true
	}
	if op := strings.TrimSpace(b.OperatedBy); op != "" {
		return strings.ToLower(op), op, true
	}
	return "", "", false
}

// feedUsersSorted returns the harvested operators sorted by LastActive
// descending, with a Key tiebreak for determinism.
func feedUsersSorted(seen map[string]feedUser) []feedUser {
	users := make([]feedUser, 0, len(seen))
	for _, u := range seen {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].LastActive.Equal(users[j].LastActive) {
			return users[i].Key < users[j].Key
		}
		return users[i].LastActive.After(users[j].LastActive)
	})
	return users
}

// collectFeedCards pages the durable /v1/events log over [since, until] and
// renders each renderable lifecycle event into feed-card HTML, oldest-first,
// keeping only the newest `limit` cards.
func (s *Server) collectFeedCards(ctx context.Context, since, until time.Time, limit int) []string {
	cards, _ := s.collectFeedCardsAndUsers(ctx, since, until, limit)
	return cards
}

// feedDedupWindow bounds how close in time two identical adjacent events (same
// kind + bead) must be to fold into one "×N" card (Phase A3, ka-0000000291).
// It sits on top of the Phase A2 server-side suppression, and only /feed
// collapses.
const feedDedupWindow = 5 * time.Minute

// feedCardItem is one not-yet-rendered feed event plus count, the number of
// identical events it stands in for (the "×N" badge).
type feedCardItem struct {
	kind     feedKind
	bead     *feedEventBead
	project  string
	closedBy string
	ts       time.Time
	count    int
}

// feedItemsAdjacentDuplicate reports whether the event (kind, b, ts) duplicates
// the preceding card prev: same kind, same bead id, and within feedDedupWindow
// of prev (Phase A3, ka-0000000291).
func feedItemsAdjacentDuplicate(prev feedCardItem, kind feedKind, b *feedEventBead, ts time.Time) bool {
	if prev.bead == nil || b == nil {
		return false
	}
	if prev.kind.Name != kind.Name || prev.bead.ID != b.ID {
		return false
	}
	gap := ts.Sub(prev.ts)
	if gap < 0 {
		gap = -gap
	}
	return gap <= feedDedupWindow
}

// appendCollapsingAdjacent appends one classified event to items, or folds an
// adjacent duplicate into the trailing card (count++, newer ts and closedBy).
// Events arrive oldest-first.
func appendCollapsingAdjacent(items []feedCardItem, kind feedKind, b *feedEventBead, project, closedBy string, ts time.Time) []feedCardItem {
	if n := len(items); n > 0 && feedItemsAdjacentDuplicate(items[n-1], kind, b, ts) {
		items[n-1].count++
		items[n-1].ts = ts
		items[n-1].closedBy = closedBy
		return items
	}
	return append(items, feedCardItem{kind: kind, bead: b, project: project, closedBy: closedBy, ts: ts, count: 1})
}

// collapseGlobalDuplicates folds non-adjacent repeats of the same (kind,
// bead-id) into one card with the summed count, placed at the run's last
// occurrence (it is that item, so it keeps the newest ts/closedBy). Why: an
// interleaved flood otherwise renders as many cards. Input and output are
// oldest-first; nil-bead items pass through unmerged. Pure.
func collapseGlobalDuplicates(items []feedCardItem) []feedCardItem {
	if len(items) < 2 {
		return items
	}
	type dupKey struct {
		kind string
		bead string
	}
	// idx maps a (kind, bead) key to its representative's position in out.
	idx := make(map[dupKey]int, len(items))
	out := make([]feedCardItem, 0, len(items))
	for _, it := range items {
		if it.bead == nil {
			out = append(out, it)
			continue
		}
		k := dupKey{kind: it.kind.Name, bead: it.bead.ID}
		if pos, seen := idx[k]; seen {
			// Fold the earlier occurrence into this newer one.
			it.count += out[pos].count
			out[pos].count = 0 // tombstone; compacted below
		}
		out = append(out, it)
		idx[k] = len(out) - 1
	}
	// Compact out the tombstoned (folded-away) earlier occurrences.
	compacted := out[:0]
	for _, it := range out {
		if it.bead != nil && it.count == 0 {
			continue
		}
		compacted = append(compacted, it)
	}
	return compacted
}

// collapseBeadLifecycle folds every card for one bead id, regardless of kind,
// into its newest occurrence, i.e. the bead's latest state (ka-0000002530): an
// MR otherwise shows opened, pipeline and merged cards.
//
// Runs after collapseGlobalDuplicates. The representative keeps its own count
// (not summed across kinds), so "×N" still means N identical events. Input and
// output are oldest-first; nil-bead items pass through. Pure.
func collapseBeadLifecycle(items []feedCardItem) []feedCardItem {
	if len(items) < 2 {
		return items
	}
	// idx maps a bead id to its representative's position in out.
	idx := make(map[string]int, len(items))
	out := make([]feedCardItem, 0, len(items))
	for _, it := range items {
		if it.bead == nil {
			out = append(out, it)
			continue
		}
		if pos, seen := idx[it.bead.ID]; seen {
			// The newer event is the bead's representative.
			out[pos].count = 0 // tombstone the earlier state; compacted below
		}
		out = append(out, it)
		idx[it.bead.ID] = len(out) - 1
	}
	// Compact out the tombstoned (folded-away) earlier-state occurrences.
	compacted := out[:0]
	for _, it := range out {
		if it.bead != nil && it.count == 0 {
			continue
		}
		compacted = append(compacted, it)
	}
	return compacted
}

// feedAgentPodSuffixRE matches the 4-char random pod suffix coop appends to an
// agent name on each (re)spawn (e.g. -6zxh), so respawns of one agent share an
// author identity (ka-0000005857). A tag like "-v3" is not matched.
var feedAgentPodSuffixRE = regexp.MustCompile(`-[a-z0-9]{4}$`)

// feedNormalizeAgentAuthor returns the lowercased author with the pod suffix
// stripped from "gardenio-" agent ids; other authors pass through.
func feedNormalizeAgentAuthor(author string) string {
	a := strings.ToLower(strings.TrimSpace(author))
	if a == "" {
		return ""
	}
	// Human authors like "matthew.baker@acme.example" are left untouched.
	if strings.HasPrefix(a, "gardenio-") {
		a = feedAgentPodSuffixRE.ReplaceAllString(a, "")
	}
	return a
}

// feedSquawkTopicRE extracts the topic a status squawk is about: the first id
// after "epic", "decision", "gardener" or "program" (e.g. "epic ka-0000007498",
// "decision ka-0000003998", "Gardener ka-0000000398"). It is the cluster key for
// a gardener's repeated status squawks (ka-0000005857, ka-0000004629). The
// leading keyword is required so a bare "(ka-abc123)" self-reference does not
// over-fold.
var feedSquawkTopicRE = regexp.MustCompile(`(?i)\b(?:epic|decision|gardener|program)\s+((?:gd|ka)-[a-z0-9]+)`)

// feedSquawkTopicRef returns the lowercased topic id a squawk title references
// (see feedSquawkTopicRE), or "" when it names none.
func feedSquawkTopicRef(title string) string {
	m := feedSquawkTopicRE.FindStringSubmatch(title)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// feedSquawkNamesDelta reports whether a squawk title names a concrete delta
// (feedSquawkConcreteOutcome, or a non-zero "spawned N"/"closed N" count). Such
// a squawk is substantive and keeps keying on its author in the cross-author
// fold; it errs toward retain (ka-0000004629).
func feedSquawkNamesDelta(title string) bool {
	t := strings.ToLower(title)
	return feedSquawkConcreteOutcome(t) || feedSquawkLoopHeartbeatActionRE.MatchString(t)
}

// feedSquawkZeroDeltaClass returns a coarse noise class (one per family) for a
// zero-delta status ping, so topicless heartbeats that slipped past the drop
// classifiers still fold to one card (ka-0000006458). It returns "" for a
// squawk naming a concrete delta (feedSquawkNamesDelta) or no known class.
func feedSquawkZeroDeltaClass(title string) string {
	if feedSquawkNamesDelta(title) {
		return ""
	}
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "thread agent") || strings.Contains(t, "role=thread"):
		// "👋 <name> thread agent on project gardenio here" — pure presence pings.
		return "thread-agent-ping"
	case strings.Contains(t, "starting sweep") || strings.Contains(t, "waking"):
		// triage-intake "gardener waking / starting sweep" — no outcome attached.
		return "sweep-start"
	case feedSquawkLoopHeartbeatRE.MatchString(t):
		// run-HH:MMZ "loop OPERATOR-GATED, no agent work left" gated heartbeats.
		return "loop-heartbeat"
	case feedSquawkZeroDeltaRE.MatchString(t):
		// "merged 0 / spawned 0 / 0 closed / NO CUT / all green" no-op cycle ticks.
		return "noop-tick"
	default:
		return ""
	}
}

// collapseSquawkClusters folds squawks into one card per cluster (newest
// squawk, summed count). The cluster key is:
//
//   - substantive (feedSquawkNamesDelta): (normalized author, topic), so
//     different gardeners' real updates on one topic each render
//     (ka-0000004629, ka-0000005857).
//   - redundant framing (topic, no delta): topic alone, cross-author
//     (ka-0000000398).
//   - no topic: feedSquawkZeroDeltaClass alone, cross-author; a topicless
//     squawk with no class passes through.
//
// Input and output are oldest-first; non-squawks pass through. Pure.
func collapseSquawkClusters(items []feedCardItem) []feedCardItem {
	if len(items) < 2 {
		return items
	}
	type clusterKey struct {
		author string // empty for a cross-author (redundant-framing) fold
		topic  string
		class  string // zero-delta noise class for a topicless status ping
	}
	// idx maps a cluster key to its representative's position in out.
	idx := make(map[clusterKey]int, len(items))
	out := make([]feedCardItem, 0, len(items))
	for _, it := range items {
		if it.bead == nil || it.kind.Name != feedKindSquawk.Name {
			out = append(out, it)
			continue
		}
		topic := feedSquawkTopicRef(it.bead.Title)
		if topic == "" {
			// Zero-delta pings fold cross-author by class: presence pings come
			// from different thread agents (ka-0000006458).
			class := feedSquawkZeroDeltaClass(it.bead.Title)
			if class == "" {
				out = append(out, it)
				continue
			}
			k := clusterKey{class: class}
			if pos, seen := idx[k]; seen {
				it.count += out[pos].count
				out[pos].count = 0 // tombstone; compacted below
			}
			out = append(out, it)
			idx[k] = len(out) - 1
			continue
		}
		var k clusterKey
		if feedSquawkNamesDelta(it.bead.Title) {
			// Substantive: fold only same-author respawns.
			author := feedNormalizeAgentAuthor(it.bead.CreatedBy)
			if author == "" {
				out = append(out, it)
				continue
			}
			k = clusterKey{author: author, topic: topic}
		} else {
			// Redundant framing: fold cross-author by topic alone.
			k = clusterKey{topic: topic}
		}
		if pos, seen := idx[k]; seen {
			// Fold the earlier squawk into this newer one.
			it.count += out[pos].count
			out[pos].count = 0 // tombstone; compacted below
		}
		out = append(out, it)
		idx[k] = len(out) - 1
	}
	// Compact out the tombstoned (folded-away) earlier squawks.
	compacted := out[:0]
	for _, it := range out {
		if it.bead != nil && it.kind.Name == feedKindSquawk.Name && it.count == 0 {
			continue
		}
		compacted = append(compacted, it)
	}
	return compacted
}

// feedExternalTicketRE matches an upstream-tracker ticket token, PROJECT-NNN
// (e.g. DEVOPS-775, PE-1422), anywhere in a title. Case-sensitive, so lowercase
// prose and gd-… bead ids don't match; word boundaries keep out "v2-786"-style
// suffixes (ka-0000001658).
var feedExternalTicketRE = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-[0-9]+)\b`)

// feedExternalTicketRef returns the first external-ticket token in title (e.g.
// "DEVOPS-775"), or "" when it names none.
func feedExternalTicketRef(title string) string {
	m := feedExternalTicketRE.FindStringSubmatch(title)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// collapseExternalTicketClusters folds cards naming the same upstream ticket,
// regardless of bead, author or kind, into the newest one with the summed count
// (ka-0000001658). Why: several gardeners file near-identical beads for one
// ticket, which the same-bead and squawk passes (run before this) miss. Input
// and output are oldest-first; cards naming no ticket pass through. Pure.
func collapseExternalTicketClusters(items []feedCardItem) []feedCardItem {
	if len(items) < 2 {
		return items
	}
	// idx maps a ticket token to its representative's position in out.
	idx := make(map[string]int, len(items))
	out := make([]feedCardItem, 0, len(items))
	for _, it := range items {
		if it.bead == nil {
			out = append(out, it)
			continue
		}
		ticket := feedExternalTicketRef(it.bead.Title)
		if ticket == "" {
			out = append(out, it)
			continue
		}
		if pos, seen := idx[ticket]; seen {
			// Fold the earlier card into this newer one.
			it.count += out[pos].count
			out[pos].count = 0 // tombstone; compacted below
			out = append(out, it)
			idx[ticket] = len(out) - 1
			continue
		}
		out = append(out, it)
		idx[ticket] = len(out) - 1
	}
	// Compact out the tombstoned (folded-away) earlier cards.
	compacted := out[:0]
	for i := range out {
		if out[i].bead != nil && out[i].count == 0 {
			continue
		}
		compacted = append(compacted, out[i])
	}
	return compacted
}

// feedDailyBundlePublicationWindow bounds how far in time a daily-bundle
// sub-artifact may sit from its bundle anchor and still fold into it. Why: one
// publication lands in a minutes-long burst while editions are hours apart, so
// the window never bridges two editions.
const feedDailyBundlePublicationWindow = 30 * time.Minute

// feedDailyBundleBundleRE extracts the bundle bead id a title names ("bundle
// ka-XXXX" or "/bundle/ka-XXXX"), so the "published and merged" decision folds
// into that bundle's cluster. Case-insensitive.
var feedDailyBundleBundleRE = regexp.MustCompile(`(?i)/?bundle[/ ]+((?:gd|ka)-[a-z0-9]+)`)

// feedDailyBundleBundleRef returns the bundle id a title names, or "" when it
// names none.
func feedDailyBundleBundleRef(title string) string {
	m := feedDailyBundleBundleRE.FindStringSubmatch(title)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// feedDailyBundleSectionPhrases is the allowlist of daily-bundle section/index
// titles and the announcement marker. A match folds only with the same author
// and within feedDailyBundlePublicationWindow of an anchor (ka-0000005042).
var feedDailyBundleSectionPhrases = []string{
	"front index",              // "The News Archive — Front Index"
	"the wire",                 // "🔌 The Wire"
	"inside this issue",        // "📖 Inside This Issue"
	"infrastructure & markets", // "📈 Infrastructure & Markets"
	"page one",                 // "📰 Page One"
	"daily bundle",             // ":newspaper: *The Daily Bundle — …*" announcement
}

// feedIsDailyBundleAnchor reports whether a card is a daily bundle's bundle
// bead (type bundle with a "daily bundle" title), the cluster anchor.
func feedIsDailyBundleAnchor(b *feedEventBead) bool {
	if b == nil || b.Type != "bundle" {
		return false
	}
	return strings.Contains(strings.ToLower(b.Title), "daily bundle")
}

// feedIsDailyBundleSection reports whether a card's title matches
// feedDailyBundleSectionPhrases. The caller also gates on author and window.
func feedIsDailyBundleSection(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	for _, p := range feedDailyBundleSectionPhrases {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

// collapseDailyBundleClusters folds one Daily Bundle publication (bundle bead,
// section docs, announcement, "published" decision) into a single card that
// shows the bundle's bead and kind, with count = folded artifacts
// (ka-0000005042). Why: these share no bead id, parent or type, so the other
// passes miss them. Cluster membership:
//
//   - a daily-bundle bundle bead is an anchor, keyed by its own id;
//   - a card whose title names a known anchor's bundle id folds into it;
//   - an allowlisted section/announcement by the same (normalized) author
//     within feedDailyBundlePublicationWindow folds into the nearest anchor.
//
// The card sits at the cluster's newest position. With no anchor in the window,
// nothing folds. Runs last; input and output are oldest-first. Pure.
func collapseDailyBundleClusters(items []feedCardItem) []feedCardItem {
	if len(items) < 2 {
		return items
	}
	// Phase 1: index the bundle anchors by id and by normalized author.
	type anchor struct {
		id     string
		author string
		ts     time.Time
		pos    int // position of the anchor card in items
	}
	// Lower-cased ids, matching feedDailyBundleBundleRef.
	anchorByID := make(map[string]*anchor)
	anchorsByAuthor := make(map[string][]*anchor)
	for i := range items {
		b := items[i].bead
		if b == nil || !feedIsDailyBundleAnchor(b) {
			continue
		}
		id := strings.ToLower(b.ID)
		a := &anchor{id: id, author: feedNormalizeAgentAuthor(b.CreatedBy), ts: items[i].ts, pos: i}
		anchorByID[id] = a
		anchorsByAuthor[a.author] = append(anchorsByAuthor[a.author], a)
	}
	if len(anchorByID) == 0 {
		return items // nothing to anchor on; leave the window untouched.
	}

	// clusterFor returns the anchor id a card folds into, or "" when it is not a
	// daily-bundle sub-artifact.
	clusterFor := func(it feedCardItem) string {
		b := it.bead
		if b == nil {
			return ""
		}
		if feedIsDailyBundleAnchor(b) {
			return strings.ToLower(b.ID)
		}
		// The card names a known bundle id (the decision).
		if ref := feedDailyBundleBundleRef(b.Title); ref != "" {
			if _, ok := anchorByID[ref]; ok {
				return ref
			}
		}
		// Allowlisted section/announcement: fold into the nearest same-author
		// anchor within the publication window.
		if !feedIsDailyBundleSection(b) {
			return ""
		}
		author := feedNormalizeAgentAuthor(b.CreatedBy)
		if author == "" {
			return ""
		}
		var best *anchor
		var bestGap time.Duration
		for _, a := range anchorsByAuthor[author] {
			gap := it.ts.Sub(a.ts)
			if gap < 0 {
				gap = -gap
			}
			if gap > feedDailyBundlePublicationWindow {
				continue
			}
			if best == nil || gap < bestGap {
				best, bestGap = a, gap
			}
		}
		if best == nil {
			return ""
		}
		return best.id
	}

	// Phase 2: fold. idx maps cluster id to its representative's position in out.
	idx := make(map[string]int, len(anchorByID))
	out := make([]feedCardItem, 0, len(items))
	for _, it := range items {
		cid := clusterFor(it)
		if cid == "" {
			out = append(out, it)
			continue
		}
		folded := max(it.count, 1)
		if pos, seen := idx[cid]; seen {
			// Fold the earlier representative into this newer slot.
			folded += out[pos].count
			out[pos].count = 0 // tombstone; compacted below
		}
		// The representative always shows the anchor, whichever member holds
		// the slot.
		ac := anchorByID[cid]
		rep := items[ac.pos]
		rep.count = folded
		rep.ts = it.ts // advance to the newest member's time
		out = append(out, rep)
		idx[cid] = len(out) - 1
	}
	// Compact out the tombstoned (folded-away) representatives.
	compacted := out[:0]
	for i := range out {
		if out[i].bead != nil && out[i].count == 0 {
			continue
		}
		compacted = append(compacted, out[i])
	}
	return compacted
}

// feedRenderedCard is one rendered feed card plus its status lane, so the page
// can SSR the same cards into the lanes view (ka-0000001008).
type feedRenderedCard struct {
	HTML string
	Lane string
}

// collectFeedCardsAndUsers is collectFeedRenderedCardsAndUsers without the
// per-card lane, for the SSE replay path and tests.
func (s *Server) collectFeedCardsAndUsers(ctx context.Context, since, until time.Time, limit int) ([]string, []feedUser) {
	rich, users := s.collectFeedRenderedCardsAndUsers(ctx, since, until, limit)
	out := make([]string, len(rich))
	for i, c := range rich {
		out[i] = c.HTML
	}
	return out, users
}

// collectFeedRenderedCardsAndUsers pages the durable event log over [since,
// until] and returns the rendered cards (oldest-first, the newest `limit`) and
// the distinct operators seen across the whole scan (not just the kept cards),
// most recently active first.
func (s *Server) collectFeedRenderedCardsAndUsers(ctx context.Context, since, until time.Time, limit int) ([]feedRenderedCard, []feedUser) {
	// One resolver per page memoizes liveness lookups per assignee
	// (ka-0000003918); a nil daemon degrades to plain-text actors.
	resolver := newAgentResolver(ctx, s.feedAgentFinder())
	// Page newest-first (order=desc) back from `until` so the scan cap is spent
	// on recent events (bug ka-0000004135). The collapse helpers expect
	// oldest-first, so the buffer is folded in reverse afterward.
	type classifiedEvent struct {
		kind     feedKind
		bead     *feedEventBead
		project  string
		closedBy string
		ts       time.Time
	}
	var desc []classifiedEvent // newest-first across pages
	seenUsers := map[string]feedUser{}
	distinct := map[string]struct{}{}
	var cursor int64
	var rawScanned int
	for range feedSeedMaxPages {
		resp, err := s.daemon.ListGlobalEvents(ctx, beadsapi.ListGlobalEventsRequest{
			Since:  since,
			Until:  until,
			Cursor: cursor,
			Limit:  maxEventsPerSeedPage,
			Desc:   true,
			// Server-side drop of bookkeeping beads so the raw-scan budget below
			// is spent on displayable events (gd-Mfr8MRXbYG).
			ExcludeTypes: feedSeedExcludeTypes,
		})
		if err != nil {
			s.logger.Debug("feed: seeding from durable events", "error", err)
			break
		}
		rawScanned += len(resp.Events)
		for _, e := range resp.Events {
			kind, b, closedBy, ok := classifyFeedEvent(e.Topic, e.Payload)
			if !ok {
				continue
			}
			// Keep each operator's max activity time (defensive; pages are
			// newest-first).
			if key, label, ok := feedUserFromBead(b); ok {
				if prev, exists := seenUsers[key]; !exists || e.CreatedAt.After(prev.LastActive) {
					seenUsers[key] = feedUser{Key: key, Label: label, LastActive: e.CreatedAt}
				}
			}
			desc = append(desc, classifiedEvent{kind: kind, bead: b, project: feedProjectFromBead(b), closedBy: closedBy, ts: e.CreatedAt})
			if b != nil {
				distinct[b.ID] = struct{}{}
			}
		}
		// `limit` distinct beads already fill the tail, so older events would
		// only be trimmed (bug ka-0000004135).
		if len(distinct) >= limit {
			break
		}
		// Flood bound: a few busy beads keep `distinct` low; trade a shorter
		// backfill for a bounded render time (gd-4q76kkHR3V).
		if rawScanned >= feedSeedMaxRawEventsScanned {
			break
		}
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		next, err := strconv.ParseInt(resp.NextCursor, 10, 64)
		if err != nil {
			break
		}
		cursor = next
	}
	// Fold oldest-first: walk the newest-first buffer in reverse.
	var items []feedCardItem
	for i := len(desc) - 1; i >= 0; i-- {
		c := desc[i]
		items = appendCollapsingAdjacent(items, c.kind, c.bead, c.project, c.closedBy, c.ts)
	}
	// Collapse passes run before the tail trim so the tail keeps distinct
	// cards. First, interleaved same kind+bead floods.
	items = collapseGlobalDuplicates(items)
	// Each bead's lifecycle to its latest state (ka-0000002530).
	items = collapseBeadLifecycle(items)
	// Repeated status squawks on one topic (ka-0000005857).
	items = collapseSquawkClusters(items)
	// Fan-out of one external ticket, e.g. DEVOPS-775 (ka-0000001658).
	items = collapseExternalTicketClusters(items)
	// One Daily Bundle publication (ka-0000005042).
	items = collapseDailyBundleClusters(items)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	tail := make([]feedRenderedCard, 0, len(items))
	for _, it := range items {
		tail = append(tail, feedRenderedCard{
			HTML: buildFeedCard(s.basePath, it.kind, it.bead, it.project, it.closedBy, formatAgo(it.ts), it.ts, resolver, it.count),
			Lane: it.kind.Lane,
		})
	}
	return tail, feedUsersSorted(seenUsers)
}

// maxEventsPerSeedPage is the page size for the durable-seed paging loop: the
// server's max.
const maxEventsPerSeedPage = 1000

// handleFeedBackfill serves reconnect gap-fill as SSE: ?since=<RFC3339> (the
// client's last-seen event time, clamped to feedSeedWindow) yields the
// renderable events since then, oldest-first, as "backfill" card events
// (Phase 2, ka-0000000643). Why created_at, not Last-Event-ID: live SSE ids
// are an in-process counter that resets on restart.
func (s *Server) handleFeedBackfill(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	now := time.Now()
	since := now.Add(-feedSeedWindow)
	if v := strings.TrimSpace(r.URL.Query().Get("since")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			// Clamp to the seed window to bound the scan.
			if t.After(since) {
				since = t
			}
		}
	}

	// Oldest-first, so the client prepends them in chronological order.
	cards := s.collectFeedCards(r.Context(), since, now, feedBackfillReplayCap)
	for _, card := range cards {
		card = strings.ReplaceAll(card, "\r", "")
		card = strings.ReplaceAll(card, "\n", " ")
		// The kind is in the .feed-card-<name> class, so one event name serves.
		fmt.Fprintf(w, "event: backfill\ndata: %s\n\n", card)
	}
	flusher.Flush()
}

// feedBackfillReplayCap bounds how many cards a single reconnect replay emits.
// Matches the client-side DOM cap (maxRows in feed.js).
const feedBackfillReplayCap = 200

// handleFeedEvents serves the activity feed as Server-Sent Events, one
// pre-rendered HTML card per event. It reads the shared BeadFeedHub (one
// upstream stream for all viewers, with a warm-paint ring; Phase 2,
// ka-0000000643), or a per-connection upstream stream when no hub is wired.
func (s *Server) handleFeedEvents(w http.ResponseWriter, r *http.Request) {
	r, cancelStream := sseRequestUntilTokenExpiry(r)
	defer cancelStream()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ctx := r.Context()

	var events <-chan beadsapi.SSEEvent
	if s.beadFeed != nil {
		// Subscribe replays the recent ring (warm paint) before going live.
		var cancel func()
		events, cancel = s.beadFeed.SubscribeFor(ctx, viewerScopeFromContext(ctx))
		defer cancel()
	} else {
		// Fallback: a dedicated upstream stream for this connection.
		ch, err := s.daemon.EventStream(ctx, feedEventTopics)
		if err != nil {
			s.logger.Error("opening feed event stream", "error", err)
			// Headers already sent — emit a comment so the browser EventSource
			// retries via its reconnect logic instead of treating empty as success.
			fmt.Fprintf(w, ": stream open failed\n\n")
			flusher.Flush()
			return
		}
		events = ch
	}

	// Comment line so intermediate proxies (Traefik) flush the header chunk
	// and the browser EventSource fires onopen.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			s.writeFeedEvent(ctx, w, flusher, evt)
		}
	}
}

// writeFeedEvent classifies one SSE event, renders its card, and writes it as
// an SSE message named by the kind; unclassifiable events are skipped. It uses
// a per-event agent resolver where the seed memoizes per page (ka-0000003918).
func (s *Server) writeFeedEvent(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, evt beadsapi.SSEEvent) {
	// Stamp the receive time, so the client's ?since= tracker advances on
	// every live event.
	now := time.Now()
	kind, b, closedBy, ok := classifyFeedEvent(evt.Event, evt.Data)
	if !ok {
		return
	}
	resolver := newAgentResolver(ctx, s.feedAgentFinder())
	row := renderFeedCardFull(s.basePath, kind, b, feedProjectFromBead(b), closedBy, formatFeedAgo(now), now, resolver)
	writeFeedSSERow(w, flusher, kind.Name, row)
}

// feedAgentFinder returns the daemon as an agentBeadFinder for card liveness
// decoration, or nil when no daemon is wired; a nil finder degrades to
// plain-text actors.
func (s *Server) feedAgentFinder() agentBeadFinder {
	if s == nil || s.daemon == nil {
		return nil
	}
	return s.daemon
}

// writeFeedSSERow flattens a rendered card to a single line and emits it as one
// SSE message named eventName, then flushes.
func writeFeedSSERow(w http.ResponseWriter, flusher http.Flusher, eventName, row string) {
	row = strings.ReplaceAll(row, "\r", "")
	row = strings.ReplaceAll(row, "\n", " ")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, row)
	flusher.Flush()
}

// feedSurfacedMetaTypes re-admits bead types that the shared
// recentActivityNonActionableTypes filter (/index) drops but the feed surfaces
// (task ka-0000006994).
var feedSurfacedMetaTypes = map[string]struct{}{
	"decision": {},
	"mail":     {},
	"agent":    {},
}

// feedNonActionableTypes are bead types /feed drops as bookkeeping churn on top
// of the shared recentActivityNonActionableTypes, which /index keeps using
// unchanged (task ka-0000000782, epic ka-0000004907; audit in
// docs/research/feed-promotion-real-sample-audit.md).
var feedNonActionableTypes = map[string]struct{}{
	"account": {},
	"trigger": {},
	"event":   {},
	"session": {},
	"audit":   {},
	"report":  {},
}

// feedTypeSurfaced reports whether a bead type produces feed cards:
// feedSurfacedMetaTypes wins, then feedNonActionableTypes and
// recentActivityNonActionableTypes drop.
func feedTypeSurfaced(t string) bool {
	if _, surfaced := feedSurfacedMetaTypes[t]; surfaced {
		return true
	}
	if _, skip := feedNonActionableTypes[t]; skip {
		return false
	}
	_, skip := recentActivityNonActionableTypes[t]
	return !skip
}

// feedSeedExcludeTypes is the sorted set of bead types classifyFeedEvent always
// drops, sent as GET /v1/events exclude_types so they don't burn the
// feedSeedMaxRawEventsScanned budget (gd-Mfr8MRXbYG). It includes "agent",
// which feedKindForBead never renders, and is derived from the same maps as
// feedTypeSurfaced.
var feedSeedExcludeTypes = func() []string {
	set := map[string]struct{}{"agent": {}}
	for t := range feedNonActionableTypes {
		set[t] = struct{}{}
	}
	for t := range recentActivityNonActionableTypes {
		if _, surfaced := feedSurfacedMetaTypes[t]; !surfaced {
			set[t] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}()

// feedSchedulerBookkeepingLabel marks a schedule fire record (a gardener tick's
// wrapper task; see internal/scheduler), not operator work.
const feedSchedulerBookkeepingLabel = "source:multi"

// feedSquawkOutcomeRE matches a concrete completed outcome on a specific object
// (a numbered merge, a closed bead, a cut tag, a deploy). It must not match a
// KPI label ("KPI: MRs merged"), a bare self-reference, or "merged 0". It is the
// retain-guard of the squawk-noise classifiers (ka-0000004501).
var feedSquawkOutcomeRE = regexp.MustCompile(`(?i)(` +
	`merged\s+(mr\s+)?!?[1-9]\d*` + // merged MR 3286 / merged !4575 / merged MR !4575 / merged 26
	`|(mr\s+)?!?[1-9]\d*\s+merged` + // MR !4575 merged / MR 26 merged / !4575 merged
	`|(closed|fixed|resolved|reverted)\s+(?:gd|ka)-[a-z0-9]+` + // closed ka-x / fixed ka-abc
	`|(?:gd|ka)-[a-z0-9]+\s+(closed|fixed|resolved|merged|shipped|reverted)` + // ka-x closed
	`|\bcut\s+\d{4}\.\d+` + // CUT 2026.169.2
	`|\b(deployed|shipped|released)\b` + // deployed / shipped / released <thing>
	`|\bbump\s+done` + // BUMP done: MR !4575 merged
	`|\bmolecule\b[^.]{0,80}\bcomplete\b` + // molecule ka-X COMPLETE: … merged (ka-0000001954)
	`)`)

// feedSquawkParenRE matches a parenthetical aside in a squawk title. Why:
// gardeners recap prior-cycle outcomes in parentheses ("(merged !4625)"), which
// must not count as this cycle's action (ka-0000001954).
var feedSquawkParenRE = regexp.MustCompile(`\([^)]*\)`)

// feedSquawkPrimaryClause returns lowerTitle with parenthetical asides blanked
// out.
func feedSquawkPrimaryClause(lowerTitle string) string {
	return feedSquawkParenRE.ReplaceAllString(lowerTitle, " ")
}

// feedSquawkLeadingClause returns the first sentence of the primary clause,
// cut at the first ". " so release tags like "cut 2026.170.1" survive. Why: a
// later sentence is recap, not this cycle's headline (ka-0000001954).
func feedSquawkLeadingClause(lowerTitle string) string {
	s := feedSquawkPrimaryClause(lowerTitle)
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i]
	}
	return s
}

// feedSquawkConcreteOutcome reports whether a lowercased squawk title names a
// completed action (feedSquawkOutcomeRE) in its leading clause; the retain side
// of the heartbeat / no-op-tick classifiers (ka-0000001954).
func feedSquawkConcreteOutcome(lowerTitle string) bool {
	return feedSquawkOutcomeRE.MatchString(feedSquawkLeadingClause(lowerTitle))
}

// feedSquawkZeroDeltaRE matches an explicit zero-delta status line ("merged 0",
// "cuts_today=0 … NO CUT", "no eligible MRs", "all green") (ka-0000004501).
var feedSquawkZeroDeltaRE = regexp.MustCompile(`(?i)(` +
	`=\s*0\b` + // cuts_today=0 / swept=0 / abandoned-reopened=0
	`|\b(merged|spawned|swept|cut|closed|opened|filed|scored)\s+0\b` + // merged 0, spawned 0
	`|\b0\s+(self-labelled|dups?|done-closed|crew|escalations?|candidates?|molecules?|cuts?|closes|drift)\b` + // 0 self-labelled, 0 dups closed, 0 done-closed (count-BEFORE-noun absence)
	`|\b0\s+(closed|spawned|merged|opened|filed|swept|scored|in-flight)\b` + // 0 closed, 0 spawned (count-BEFORE-verb zero-delta — ka-0000001954 Escape 2, ka-0000007218)
	`|\bno cut\b` + // NO CUT
	`|\bno eligible\b` + // no eligible MRs
	`|\bno actionable\b` + // NO actionable drift
	`|\bno (drift|new pass|new-pass|change|spawnable|stale)\b` + // NO drift / NO new pass — Class-A live phrasings
	`|\bnothing (to do|actionable|eligible|ready)\b` +
	`|\ball green\b` + // resume-latency watchdog: all green
	`|\blane empty\b` +
	`)`)

// feedSquawkCycleShapeRE matches a scheduled-gardener cycle line's cadence
// marker: "cycle", "tick", or a "run N/M" counter. Real outcome squawks don't
// carry these words; feedSquawkCycleActionRE retains cycle lines that did act
// (ka-0000001954).
var feedSquawkCycleShapeRE = regexp.MustCompile(`(?i)(\b(cycle|tick)\b|\brun\s+\d+/\d+)`)

// feedSquawkCycleActionRE is the tight retain-guard for cycle/tick lines: a
// numbered merge, a cut release, a finished molecule, or a non-zero count
// ("closed 5"). Why not feedSquawkConcreteOutcome: its "closed gd-<id>" and
// bare deployed/shipped arms match prior-cycle history in a recap.
var feedSquawkCycleActionRE = regexp.MustCompile(`(?i)(` +
	`merged\s+(mr\s+)?!?[1-9]\d*` + // merged MR !4598 / merged 3
	`|(mr\s+)?!?[1-9]\d*\s+merged` + // !4598 merged / MR 26 merged
	`|\bcut\s+\d{4}\.\d+` + // cut 2026.169.11
	`|\bbump\s+done` + // BUMP done
	`|\bmolecule\b[^.]{0,80}\bcomplete\b` + // molecule ka-X COMPLETE
	`|\b(closed|merged|spawned|swept|fixed|resolved|opened|filed|landed|cut)\s+[1-9]\d*\b` + // closed 5 / spawned 1 crew / swept 3 (non-zero count)
	`)`)

// Note: a real deploy/cut/merge this cycle is always numbered or counted
// (ka-0000001954).

// feedSquawkGardenerCycleTick reports whether a squawk is a gardener cycle
// status recap: cycle shape (feedSquawkCycleShapeRE) and no this-cycle action
// (feedSquawkCycleActionRE) in the primary clause (ka-0000001954). Real work
// surfaces under its own beads, so the recap carries no signal.
func feedSquawkGardenerCycleTick(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	if !feedSquawkCycleShapeRE.MatchString(t) {
		return false
	}
	// Parentheticals hold prior-cycle tokens (ka-0000001954).
	return !feedSquawkCycleActionRE.MatchString(feedSquawkPrimaryClause(t))
}

// feedSquawkHealthRecapRE matches a periodic fleet-liveness health recap
// ("fleet-liveness: all watched gardeners healthy …", ka-0000007055). It needs
// both the liveness vocabulary and a standing-state token, so "healthy" in prose
// does not match.
var feedSquawkHealthRecapRE = regexp.MustCompile(`(?i)(fleet-liveness|liveness guard|fleet liveness).*(healthy|all (watched|green)|status=open|enabled=true)`)

// feedSquawkStandingState reports whether a squawk is a standing-state recap
// with no concrete outcome; today, the fleet-liveness health recap
// (ka-0000001954).
func feedSquawkStandingState(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	if feedSquawkConcreteOutcome(t) {
		return false
	}
	return feedSquawkHealthRecapRE.MatchString(t)
}

// feedSquawkNoopTick reports whether a squawk is a zero-delta gardener report:
// it matches feedSquawkZeroDeltaRE and names no concrete outcome
// (ka-0000004501).
func feedSquawkNoopTick(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	if !feedSquawkZeroDeltaRE.MatchString(t) {
		return false
	}
	// Retain-guard.
	return !feedSquawkConcreteOutcome(t)
}

// feedSquawkLoopHeartbeatRE matches a respawning gardener's "nothing for an
// agent to do, blocked on the operator" line ("run HH:MMZ — loop
// OPERATOR-GATED, no agent work left") and sweep-start ticks. These carry
// neither a cycle marker nor a zero count, so the other classifiers miss them
// (ka-0000004629).
var feedSquawkLoopHeartbeatRE = regexp.MustCompile(`(?i)(` +
	`\bno agent work left\b` + // run HH:MMZ — no agent work left
	`|\ball? agent[- ]?(doable )?work (is )?done\b` + // all agent work DONE / ALL agent-doable work done
	`|\boperator-gated\b` + // loop OPERATOR-GATED / loop is OPERATOR-GATED
	`|\boperator-only gated\b` + // SOLE BLOCKER = operator-only gated cutover
	`|\bgated to operator\b` + // live apply gated to operator
	`|\bstarting sweep\b` + // triage-intake: starting sweep
	`|\bgardener\b.{0,40}\bwaking\b` + // <gardener> (project) waking for/up (also greeting-covered)
	`)`)

// feedSquawkLoopHeartbeatActionRE is the retain-guard for a loop heartbeat: a
// numbered merge/close, a cut release, or a non-zero count. It is checked
// against the full title, deliberately broad: false-dropping a real update is
// worse than leaving a heartbeat (ka-0000004629).
var feedSquawkLoopHeartbeatActionRE = regexp.MustCompile(`(?i)(` +
	`merged\s+(mr\s+)?!?[1-9]\d*` + // merged MR !4625 / merged 3
	`|(mr\s+)?!?[1-9]\d*\s+merged` + // !4625 merged / MR 26 merged
	`|(closed|fixed|resolved|reverted)\s+(?:gd|ka)-[a-z0-9]+` + // closed ka-x
	`|(?:gd|ka)-[a-z0-9]+\s+(closed|fixed|resolved|merged|shipped|reverted)` + // ka-x closed
	`|\bcut\s+\d{4}\.\d+` + // cut 2026.169.2
	`|\b(spawned|respawned|closed|merged|swept|opened|filed)\s+[1-9]\d*\b` + // spawned 1 crew / closed 5
	`)`)

// feedSquawkLoopHeartbeat reports whether a squawk matches
// feedSquawkLoopHeartbeatRE and not feedSquawkLoopHeartbeatActionRE. The
// blocker is already an open escalation/decision card (ka-0000004629).
func feedSquawkLoopHeartbeat(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	if !feedSquawkLoopHeartbeatRE.MatchString(t) {
		return false
	}
	// Retain-guard, against the full title.
	return !feedSquawkLoopHeartbeatActionRE.MatchString(t)
}

// feedResumeSweepTitleRE matches the resumer-watchdog's timestamped "Resume
// sweep <date> <time> UTC" lead, shared by its squawk tick and its scorecard
// task (ka-0000008239). The timestamp keeps prose like "Resume sweep is too
// noisy" out.
var feedResumeSweepTitleRE = regexp.MustCompile(`(?i)^resume sweep \d{4}-\d{2}-\d{2} \d{2}:\d{2} utc`)

// feedResumeSweepScorecardTitleRE matches the bare scorecard header; the end
// anchor excludes the squawk recap's ": …" body (ka-0000008239).
var feedResumeSweepScorecardTitleRE = regexp.MustCompile(`(?i)^resume sweep \d{4}-\d{2}-\d{2} \d{2}:\d{2} utc$`)

// feedResumeSweepActionRE is the retain-guard for a resume sweep that re-armed,
// rescued, respawned, reopened or escalated something. Each arm needs a
// past-tense verb or a non-zero count, so a dormant sweep's "No re-arm" and
// "0 stuck" don't match (ka-0000008239).
var feedResumeSweepActionRE = regexp.MustCompile(`(?i)(` +
	`\bre-?armed\b` + // re-armed ka-X
	`|\brescued\b` + // rescued crew
	`|\brespawned\b` + // respawned gardener ka-X
	`|\b(reopened|escalated)\s+((?:gd|ka)-[a-z0-9]+|[1-9]\d*)` + // reopened 3 / escalated ka-X
	`|\b[1-9]\d*\s+(stuck|re-?armed|escalated|rescued|respawned)\b` + // 1 stuck re-armed / 3 escalated
	`)`)

// feedSquawkResumeSweepTick reports whether a squawk is a resume-sweep recap
// with no action (feedResumeSweepActionRE) (ka-0000008239).
func feedSquawkResumeSweepTick(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	if !feedResumeSweepTitleRE.MatchString(strings.TrimSpace(b.Title)) {
		return false
	}
	return !feedResumeSweepActionRE.MatchString(b.Title)
}

// feedResumeSweepScorecard reports whether a bead is the resumer-watchdog's
// per-sweep scorecard: a task titled exactly "Resume sweep <date> <time> UTC".
// It is machinery, dropped like feedFormulaStepBead; a real sweep still shows
// via its squawk (ka-0000008239).
func feedResumeSweepScorecard(b *feedEventBead) bool {
	if b == nil || b.Type != "task" {
		return false
	}
	return feedResumeSweepScorecardTitleRE.MatchString(strings.TrimSpace(b.Title))
}

// feedSquawkHeartbeat reports whether a squawk is an agent greeting
// (feedSquawkGreeting) with no concrete outcome (feedSquawkConcreteOutcome).
// Why: a wake-up announcement says an agent woke, not what it did
// (ka-0000004805, ka-0000003549, ka-0000004501).
func feedSquawkHeartbeat(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	t := strings.ToLower(b.Title)
	// Outcome guard first.
	if feedSquawkConcreteOutcome(t) {
		return false
	}
	return feedSquawkGreeting(t)
}

// feedSquawkGardenerOnlineRE matches "gardener … online —" with a short clause
// in between ("<role> gardener (project:gardenio) online — sweeping …"), which
// a literal "gardener online" check misses.
var feedSquawkGardenerOnlineRE = regexp.MustCompile(`(?i)gardener\b.{0,40}\bonline\b.{0,6}[—-]`)

// feedSquawkGreeting reports whether a lowercased squawk title is an agent
// greeting / start-of-cycle announcement. It applies no outcome guard.
func feedSquawkGreeting(t string) bool {
	// Form 1: "<agent> online — <start-of-cycle verb>" (ka-0000001954).
	if strings.Contains(t, "online") {
		for _, v := range []string{"starting", "checking", "assessing", "picking up", "cycle", "kicking off", "resuming", "pouring", "beginning", "sweeping", "driving"} {
			if strings.Contains(t, v) {
				return true
			}
		}
		// "<role> gardener (project:…) online — <verb> …".
		if feedSquawkGardenerOnlineRE.MatchString(t) {
			return true
		}
		// "<name> gardener online (epic …). <standing-state recap>"
		// (ka-0000002345 / ka-0000002577).
		if strings.Contains(t, "gardener online") {
			return true
		}
	}
	// Form 4: wake-up greetings without "online" ("<gardener> waking up",
	// "First-mate gardener spinning up", "waking for <sweep>") (ka-0000001954,
	// ka-0000003096).
	if strings.Contains(t, "waking up") || strings.Contains(t, "wakeup") || strings.Contains(t, "waking for") ||
		strings.Contains(t, "spinning up") || strings.Contains(t, "spinning-up") {
		return true
	}
	// Form 5: "<thread>… (gardener …) on it — <investigating>", gated on
	// "(gardener" (ka-0000006665).
	if strings.Contains(t, "on it —") || strings.Contains(t, "on it -") {
		if strings.Contains(t, "(gardener") {
			return true
		}
	}
	// Form 6: thread-agent startup presence pings ("👋 <name> thread agent on
	// project gardenio here"), gated on "thread agent" / "role=thread"
	// (ka-0000004629).
	if strings.Contains(t, "thread agent") || strings.Contains(t, "role=thread") {
		for _, v := range []string{"here", "online", "reading thread", "picking up", "on project"} {
			if strings.Contains(t, v) {
				return true
			}
		}
	}
	// Form 2: "reporting in" / "<role> reporting (project:…)" / "reporting for duty".
	if strings.Contains(t, "reporting in") ||
		strings.Contains(t, "reporting (project") ||
		strings.Contains(t, "reporting for duty") {
		return true
	}
	// Form 3: "crew on <proj>: claiming/starting <thing>" (and "crew/<proj>").
	if strings.Contains(t, "crew on ") || strings.Contains(t, "crew/") {
		for _, v := range []string{"claiming", "starting"} {
			if strings.Contains(t, v) {
				return true
			}
		}
	}
	return false
}

// feedSchedulerBookkeeping reports whether a work bead is a scheduler-fire
// wrapper ("Multi[cron]: …"): it carries feedSchedulerBookkeepingLabel and a
// multi_fire_key field. Why: these cycle every tick, and the real work shows
// under its own beads.
func feedSchedulerBookkeeping(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	if !slices.Contains(b.Labels, feedSchedulerBookkeepingLabel) {
		return false
	}
	// A hand-applied source:multi label alone does not silence a bead.
	return strings.TrimSpace(fmtFeedField(feedBeadFields(b)["multi_fire_key"])) != ""
}

// feedScheduleFireTitleRE matches a schedule fire bead's title, "[<ts>]
// Schedule: <gardener name>" (ka-0000000435).
var feedScheduleFireTitleRE = regexp.MustCompile(`^\[[^\]]+\]\s*Schedule:\s`)

// feedScheduleFireBead reports whether a bead is a schedule fire record:
// type=schedule, or a task wrapper with feedScheduleFireTitleRE's title or a
// schedule_fire_key field (ka-0000000435).
func feedScheduleFireBead(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	if b.Type == "schedule" {
		return true
	}
	if feedScheduleFireTitleRE.MatchString(strings.TrimSpace(b.Title)) {
		return true
	}
	return strings.TrimSpace(fmtFeedField(feedBeadFields(b)["schedule_fire_key"])) != ""
}

// feedAuditRubricSuffixes are the kind-label suffixes of scheduled rubric-audit
// lanes, whose formulas pour multi-step molecules; one suffix covers every page
// a rubric audits (kind:gardener-cdqr, kind:users-cdqr, …). Add new audit
// lanes here.
var feedAuditRubricSuffixes = []string{
	"-cdqr",  // Correctness/Density/Quality/Relevance — gardener, people, users
	"-camp",  // Coverage/Accuracy/Management/Polish — gardeners page, user surfaces
	"-flat",  // Fidelity/Legibility/All-time-series/Tidiness — retired users lane
	"-grade", // Grafana GRADE audits
	"-club",  // Maps beauty CLUB audit
}

// feedAuditRubricLabel reports whether a label marks a bead as belonging to a
// scheduled rubric-audit lane (see feedAuditRubricSuffixes).
func feedAuditRubricLabel(label string) bool {
	if !strings.HasPrefix(label, "kind:") {
		return false
	}
	return slices.ContainsFunc(feedAuditRubricSuffixes, func(suffix string) bool {
		return strings.HasSuffix(label, suffix)
	})
}

// feedFormulaStepBead reports whether a bead is a scheduled-audit
// formula/molecule step: audit machinery whose real output is the roll-up and
// filed bugs (ka-0000004173). Only task/molecule/formula beads qualify, so the
// bugs an audit files (sharing its kind label) survive. A vessel is a step when
// it has:
//
//	(a) an audit-rubric kind label (feedAuditRubricLabel);
//	(b) an unexpanded "{{" template token in the title;
//	(c) or is a molecule/formula head with created_by=static (the engine stamps
//	    it on every formula-spawned bead).
func feedFormulaStepBead(b *feedEventBead) bool {
	if b == nil {
		return false
	}
	switch b.Type {
	case "task", "molecule", "formula":
		// machinery vessel — eligible for the structural signals below
	default:
		// Artifacts an audit produces are kept even with the lane's kind label.
		return false
	}
	// (b) literal unexpanded template token — the formula never interpolated.
	if strings.Contains(b.Title, "{{") {
		return true
	}
	// (a) audit-rubric kind label on any step of a rubric audit molecule.
	if slices.ContainsFunc(b.Labels, feedAuditRubricLabel) {
		return true
	}
	// (c) the formula/molecule head — static-authored vessel, no token/label.
	if b.CreatedBy == "static" && (b.Type == "molecule" || b.Type == "formula") {
		return true
	}
	return false
}

// classifyFeedEvent parses a bead-lifecycle event wrapper and classifies it
// into a feedKind (task ka-0000006994). ok is false for malformed payloads,
// events with no bead, filtered types, machinery beads, and routine
// per-family noise. The live stream, ?since= replay and first-paint seed all
// use it, so they classify identically.
func classifyFeedEvent(topic string, data json.RawMessage) (kind feedKind, b *feedEventBead, closedBy string, ok bool) {
	var wrapper feedEventSSE
	if err := json.Unmarshal(data, &wrapper); err != nil || wrapper.Bead == nil {
		return feedKind{}, nil, "", false
	}
	b = wrapper.Bead
	if !feedTypeSurfaced(b.Type) {
		return feedKind{}, nil, "", false
	}
	if feedSchedulerBookkeeping(b) {
		return feedKind{}, nil, "", false
	}
	if feedScheduleFireBead(b) {
		return feedKind{}, nil, "", false
	}
	if feedFormulaStepBead(b) {
		return feedKind{}, nil, "", false
	}
	if feedResumeSweepScorecard(b) {
		return feedKind{}, nil, "", false
	}
	base := feedBaseKind(topic)
	if base.Name == "" {
		return feedKind{}, nil, "", false
	}
	kind = feedKindForBead(base, b, b.Status, wrapper.Changes)
	if kind.Name == "" {
		// A surfaced type whose specific event is routine noise.
		return feedKind{}, nil, "", false
	}
	return kind, b, wrapper.ClosedBy, true
}

// feedBaseKind maps a kbeads SSE topic onto the coarse lifecycle kind, or the
// zero kind; feedKindForBead refines it.
func feedBaseKind(topic string) feedKind {
	switch {
	case strings.Contains(topic, "created"):
		return feedKindCreated
	case strings.Contains(topic, "closed"):
		return feedKindClosed
	case strings.Contains(topic, "updated"):
		return feedKindUpdated
	default:
		return feedKind{}
	}
}

// feedKindForBead refines the coarse lifecycle kind by bead family, then
// status and fields (Phase 3, task ka-0000006994):
//
//	decision bead                          -> decision (amber, "needs you")
//	merge-request bead, merged             -> mr-merged (purple)
//	merge-request bead, pipeline changed   -> pipeline (green ok / red failed)
//	merge-request bead, opened             -> mr-opened (purple, lighter)
//	message bead with squawk label          -> squawk (teal)
//	mail bead                               -> mail (teal)
//	agent bead (spawn / stop / gate)         -> dropped (orchestration churn)
//	work bead -> in_progress                -> claimed (blue)
//	work bead -> blocked                    -> blocked (red)
//	work bead closed / created / updated    -> closed / created / updated
//
// A zero feedKind means "routine noise for this family, drop it".
//
// changes is the event's BeadUpdated.changes map (may be nil); currently
// unused.
func feedKindForBead(base feedKind, b *feedEventBead, status string, changes map[string]any) feedKind {
	_ = changes
	switch b.Type {
	case "decision":
		return feedKindDecision
	case "merge-request":
		return feedMRKind(base, b)
	case "message":
		// Squawks (`squawk` label) count only on creation.
		if base.Name == feedKindCreated.Name && slices.Contains(b.Labels, "squawk") {
			// Drop greetings ("crew on gardenio: claiming ka-y") and zero-delta
			// status recaps (ka-0000004805, ka-0000003549, ka-0000004501).
			if feedSquawkHeartbeat(b) || feedSquawkNoopTick(b) || feedSquawkGardenerCycleTick(b) || feedSquawkStandingState(b) || feedSquawkResumeSweepTick(b) || feedSquawkLoopHeartbeat(b) {
				return feedKind{}
			}
			return feedKindSquawk
		}
		return feedKind{}
	case "mail":
		// Mail counts only on creation.
		if base.Name == feedKindCreated.Name {
			return feedKindMail
		}
		return feedKind{}
	case "agent":
		// Agent spawn/stop/gate is orchestration churn; agents show in the
		// LIVE AGENTS rail (ka-0000003549).
		return feedKind{}
	default:
		return feedWorkKind(base, status)
	}
}

// feedMRKind classifies a merge-request bead: merged, closed, terminal
// pipeline, or opened; other updates are dropped. Merge wins so the
// accompanying "success" pipeline never masks it.
func feedMRKind(base feedKind, b *feedEventBead) feedKind {
	if feedMRMerged(b) {
		return feedKindMRMerged
	}
	if base.Name == feedKindClosed.Name {
		// Closed without merging.
		return feedKindClosed
	}
	if feedMRPipelineTerminal(b) {
		return feedKindPipeline
	}
	if base.Name == feedKindCreated.Name {
		return feedKindMROpened
	}
	// Routine churn.
	return feedKind{}
}

// feedWorkKind classifies a plain work bead (task/bug/feature/…) by its
// lifecycle event and resulting status.
func feedWorkKind(base feedKind, status string) feedKind {
	switch base.Name {
	case feedKindClosed.Name:
		return feedKindClosed
	case feedKindCreated.Name:
		return feedKindCreated
	default: // updated — disambiguate by the new status
		switch status {
		case "in_progress":
			return feedKindClaimed
		case "blocked":
			return feedKindBlocked
		default:
			return feedKindUpdated
		}
	}
}

// feedBeadFields decodes a bead's Fields blob into a map; a nil or malformed
// blob yields an empty (non-nil) map.
func feedBeadFields(b *feedEventBead) map[string]any {
	if len(b.Fields) == 0 {
		return map[string]any{}
	}
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		return map[string]any{}
	}
	return f
}

// fmtFeedField renders a fields value as a string ("" for nil).
func fmtFeedField(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// feedMRPipelineTerminal reports whether a merge-request bead's
// mr_pipeline_status (written by gitlab_sync.go) is terminal.
func feedMRPipelineTerminal(b *feedEventBead) bool {
	_, ok := feedPipelineTerminalStatuses[feedMRPipelineStatus(b)]
	return ok
}

// feedMRPipelineStatus extracts the lower-cased mr_pipeline_status field.
func feedMRPipelineStatus(b *feedEventBead) string {
	return strings.ToLower(strings.TrimSpace(fmtFeedField(feedBeadFields(b)["mr_pipeline_status"])))
}

// feedPipelineTerminalStatuses are GitLab pipeline statuses that read as a
// finished run; success renders green, the rest red.
var feedPipelineTerminalStatuses = map[string]struct{}{
	"success":  {},
	"failed":   {},
	"canceled": {},
}

// feedPipelineOK reports whether a pipeline card represents a passing run.
func feedPipelineOK(b *feedEventBead) bool {
	return feedMRPipelineStatus(b) == "success"
}

// feedMRMerged reports whether a merge-request bead's fields show it merged.
func feedMRMerged(b *feedEventBead) bool {
	if len(b.Fields) == 0 {
		return false
	}
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		return false
	}
	state, _ := f["mr_state"].(string)
	return strings.EqualFold(strings.TrimSpace(state), "merged")
}

// feedProjectFromBead resolves a bead's project label for the card chip.
func feedProjectFromBead(b *feedEventBead) string {
	return projectFromLabels(b.Labels)
}

// renderFeedCardFull renders one activity-feed card as an HTML string (built
// inline, not templated, so the SSE writer can put it straight into a `data:`
// line). ts is written as data-ts (RFC3339), which the client sends back as
// ?since= on reconnect (Phase 2, ka-0000000643); a zero ts omits it.
//
// resolver decorates the claimed/spawned actor with a live-agent dot and link
// when the assignee is a live agent (ka-0000003918); nil falls back to a
// plain-text @assignee.
func renderFeedCardFull(basePath string, kind feedKind, b *feedEventBead, project, closedBy, ago string, ts time.Time, resolver *agentResolver) string {
	return buildFeedCard(basePath, kind, b, project, closedBy, ago, ts, resolver, 1)
}

// buildFeedCard renders one card. count is the number of events the card
// stands in for after the collapse passes (Phase A3, ka-0000000291); count>1
// adds an "×N" badge whose tooltip names the cluster type.
func buildFeedCard(basePath string, kind feedKind, b *feedEventBead, project, closedBy, ago string, ts time.Time, resolver *agentResolver, count int) string {
	title := b.Title
	if title == "" {
		title = b.ID
	}

	var sb strings.Builder
	sb.WriteString(`<article class="feed-card feed-card-`)
	sb.WriteString(html.EscapeString(kind.Name))
	// Pipeline cards carry an ok/failed modifier (green/red).
	if kind.Name == feedKindPipeline.Name {
		if feedPipelineOK(b) {
			sb.WriteString(` feed-card-pipeline-ok`)
		} else {
			sb.WriteString(` feed-card-pipeline-failed`)
		}
	}
	sb.WriteString(`" data-bead-id="`)
	sb.WriteString(html.EscapeString(b.ID))
	sb.WriteString(`" data-kind="`)
	sb.WriteString(html.EscapeString(kind.Name))
	if !ts.IsZero() {
		sb.WriteString(`" data-ts="`)
		sb.WriteString(html.EscapeString(ts.UTC().Format(time.RFC3339)))
	}
	sb.WriteString(`" data-lane="`)
	sb.WriteString(html.EscapeString(kind.Lane))
	sb.WriteString(`" data-project="`)
	sb.WriteString(html.EscapeString(project))
	// Operator identity for client-side "mine" scoping (ka-0000003403).
	sb.WriteString(`" data-operator="`)
	sb.WriteString(html.EscapeString(strings.ToLower(b.OperatedBy)))
	sb.WriteString(`" data-operator-email="`)
	sb.WriteString(html.EscapeString(strings.ToLower(b.OperatedByEmail)))
	sb.WriteString(`">`)

	// Kind glyph + label.
	sb.WriteString(`<span class="feed-card-kind" title="`)
	sb.WriteString(html.EscapeString(feedKindLabel(kind)))
	sb.WriteString(`">`)
	sb.WriteString(html.EscapeString(feedKindGlyph(kind)))
	sb.WriteString(` `)
	sb.WriteString(html.EscapeString(feedKindLabel(kind)))
	sb.WriteString(`</span>`)

	// Collapsed-run badge (ka-0000000291).
	if count > 1 {
		// The tooltip names the cluster type; squawk topic: ka-0000005857.
		badgeTitle := fmt.Sprintf("%d consecutive identical events", count)
		switch {
		case feedIsDailyBundleAnchor(b):
			// Daily-bundle cluster (ka-0000005042).
			badgeTitle = fmt.Sprintf("%d artifacts in this Daily Bundle publication", count)
		case feedExternalTicketRef(b.Title) != "":
			// External-ticket cluster (ka-0000001658).
			badgeTitle = fmt.Sprintf("%d cards on ticket %s", count, feedExternalTicketRef(b.Title))
		case kind.Name == feedKindSquawk.Name:
			badgeTitle = fmt.Sprintf("%d related status updates on this topic", count)
		}
		sb.WriteString(`<span class="feed-card-count" title="`)
		sb.WriteString(html.EscapeString(badgeTitle))
		sb.WriteString(`">×`)
		sb.WriteString(strconv.Itoa(count))
		sb.WriteString(`</span>`)
	}

	// Bead type chip.
	if b.Type != "" {
		sb.WriteString(`<span class="feed-card-type">`)
		sb.WriteString(html.EscapeString(b.Type))
		sb.WriteString(`</span>`)
	}

	// Title links to the bead.
	sb.WriteString(`<a class="feed-card-title" href="`)
	sb.WriteString(html.EscapeString(basePath + "/" + b.ID))
	sb.WriteString(`">`)
	sb.WriteString(html.EscapeString(title))
	sb.WriteString(`</a>`)

	// Meta row: bead id, project, actor, relative time.
	sb.WriteString(`<span class="feed-card-meta">`)
	sb.WriteString(`<code class="feed-card-id">`)
	sb.WriteString(html.EscapeString(b.ID))
	sb.WriteString(`</code>`)
	if project != "" {
		sb.WriteString(`<span class="feed-card-project">`)
		sb.WriteString(html.EscapeString(project))
		sb.WriteString(`</span>`)
	}
	if actor := feedActor(kind, b, closedBy); actor != "" {
		sb.WriteString(`<span class="feed-card-actor">`)
		// A live assignee agent gets a dot + agent link (ka-0000003918).
		if icon, link := feedActorAgentDecoration(basePath, kind, b, resolver); icon != "" || link != "" {
			sb.WriteString(string(icon))
			sb.WriteString(string(link))
		} else {
			sb.WriteString(html.EscapeString(actor))
		}
		sb.WriteString(`</span>`)
	}
	if ago != "" {
		sb.WriteString(`<time class="feed-card-ago">`)
		sb.WriteString(html.EscapeString(ago))
		sb.WriteString(`</time>`)
	}
	sb.WriteString(`</span>`)

	sb.WriteString(`</article>`)
	return sb.String()
}

// feedActor returns the most relevant actor string for a card kind.
func feedActor(kind feedKind, b *feedEventBead, closedBy string) string {
	switch kind.Name {
	case feedKindClosed.Name, feedKindMRMerged.Name, feedKindStopped.Name:
		if closedBy != "" {
			return "by " + closedBy
		}
		if kind.Name == feedKindStopped.Name && b.Assignee != "" {
			return "@" + b.Assignee
		}
	case feedKindClaimed.Name, feedKindSpawned.Name:
		if b.Assignee != "" {
			return "@" + b.Assignee
		}
	case feedKindCreated.Name, feedKindMROpened.Name, feedKindSquawk.Name, feedKindMail.Name:
		if b.CreatedBy != "" {
			return "by " + b.CreatedBy
		}
	case feedKindGate.Name:
		if by := strings.TrimSpace(fmtFeedField(feedBeadFields(b)["gate_satisfied_by"])); by != "" {
			return "via " + by
		}
	case feedKindPipeline.Name:
		if st := feedMRPipelineStatus(b); st != "" {
			return st
		}
	}
	return ""
}

// feedActorAgentDecoration returns the live-agent dot and "@<agent>" link that
// replace the plain @assignee on a claimed/spawned card while the assignee
// agent is alive (per renderAgentLiveIcon). Otherwise, including for a nil
// resolver, it returns two empty strings (ka-0000003918).
func feedActorAgentDecoration(basePath string, kind feedKind, b *feedEventBead, resolver *agentResolver) (icon, link template.HTML) {
	switch kind.Name {
	case feedKindClaimed.Name, feedKindSpawned.Name:
		// these key their actor off Assignee
	default:
		return "", ""
	}
	if b.Assignee == "" || resolver == nil {
		return "", ""
	}
	icon = renderAgentLiveIcon(basePath, b.Assignee, resolver)
	if icon == "" {
		// Not alive (or unresolvable): keep the plain @assignee text.
		return "", ""
	}
	link = template.HTML("@") + renderAgentLink(basePath, b.Assignee, resolver)
	return icon, link
}

// feedKindGlyph / feedKindLabel give each taxonomy entry a stable glyph +
// label. Colors live in feed.html (CSS keyed off .feed-card-<name>).
func feedKindGlyph(kind feedKind) string {
	switch kind.Name {
	case feedKindDecision.Name:
		return "◆"
	case feedKindClosed.Name:
		return "✓"
	case feedKindMRMerged.Name:
		return "⌥"
	case feedKindMROpened.Name:
		return "⎇"
	case feedKindClaimed.Name:
		return "▸"
	case feedKindBlocked.Name:
		return "■"
	case feedKindCreated.Name:
		return "+"
	case feedKindPipeline.Name:
		return "⚙"
	case feedKindSquawk.Name:
		return "❝"
	case feedKindMail.Name:
		return "✉"
	case feedKindSpawned.Name:
		return "✦"
	case feedKindStopped.Name:
		return "⏹"
	case feedKindGate.Name:
		return "⛬"
	default:
		return "·"
	}
}

func feedKindLabel(kind feedKind) string {
	switch kind.Name {
	case feedKindDecision.Name:
		return "decision · needs you"
	case feedKindClosed.Name:
		return "closed"
	case feedKindMRMerged.Name:
		return "MR merged"
	case feedKindMROpened.Name:
		return "MR opened"
	case feedKindClaimed.Name:
		return "claimed"
	case feedKindBlocked.Name:
		return "blocked"
	case feedKindCreated.Name:
		return "created"
	case feedKindPipeline.Name:
		return "pipeline"
	case feedKindSquawk.Name:
		return "squawk"
	case feedKindMail.Name:
		return "mail"
	case feedKindSpawned.Name:
		return "spawned"
	case feedKindStopped.Name:
		return "stopped"
	case feedKindGate.Name:
		return "gate"
	default:
		return "updated"
	}
}

// formatFeedAgo renders the UTC clock time shown on live cards, where a
// relative "just now" would say nothing.
func formatFeedAgo(now time.Time) string {
	return now.UTC().Format("15:04:05")
}
