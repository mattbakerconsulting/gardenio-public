package commons

import (
	"context"
	"html/template"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/kin"
)

// The steward node (Commons v2 steward view S1, gd-FsDEc7ZXGl; spec on
// gd-jNaTmCjity, option A): GET /garden/c/s/{land}, and /garden/c/s for the
// viewer's land, is the land's steward as a root of the workbench, on the node
// fragment and stream. Its trigger's own node, /garden/c/n/{trigger}, is the
// same. It shows:
//
//	head        what it is doing now (working with its run, resting at the
//	            daily cap, parked at the purse wall, idle), why, its runs and
//	            spend today, and Watch live on the run
//	face        its notes (Holds, Answers, Promises, In flight)
//	tree        the plans it tends, each with its gardener loop and whether
//	            the plan is stalled; then Wishes, the land's open thread heads
//	            (Slack and Commons), each with its replies, last line and the
//	            agent on it, flagged unanswered when a person spoke last and
//	            no agent is live (plan gd-gO1fTT7oGC, step gd-ak4P2qXMss)
//	transcript  the land's #general threads that address it, that it posted
//	            in or that are its wishes, newest last, each answer inside its
//	            thread; another agent's words (the home garden's @gardenio
//	            replies, a crew's squawks) are labelled
//	rails       Jobs (its run's), Tending (stalled plans, parked steps) and
//	            Asks for you (open decisions its runs filed, unanswered
//	            wishes, promises past due)
//
// Its conversation is the land's shared #general (D2 gd-R0hspYDBbq,
// no-room): the composer the fragment names posts there with an implicit
// @steward. Nothing new is stored: what the steward knows comes from Stewards
// (beads-viewer's, over steward.LoadLand), held per viewer and land for
// stewardCacheFor, so a stream's refetches do not reload the land.

// Stewards reads a land's steward for its node, on the request context.
// beads-viewer implements it over internal/steward, which this package does
// not import (the steward pulls in the bridge, which imports this package).
type Stewards interface {
	// Steward is the land's steward; land "" is the home garden's. nil and no
	// error when the land has none.
	Steward(ctx context.Context, land string) (*StewardLand, error)
}

// StewardLand is a land's steward as its node shows it.
type StewardLand struct {
	ID   string // its trigger bead
	Name string // <land>-steward
	Land string
	Home bool
	// General is the land's #general channel bead: its conversation.
	General string
	// State is StewardWorking, StewardResting, StewardParked or StewardIdle;
	// Reason the farm's line for why (FarmSteward.Reason).
	State, Reason string
	// Run is its live run's agent bead, "" between runs.
	Run                      string
	RunsToday, MaxRunsPerDay int
	// Spend is the day's spend as the land header says it, "" unknown.
	Spend string
	// NotesID and Notes are its memory, steward-notes:<land>.
	NotesID, Notes string
	Plans          []StewardPlan
	Parked         []StewardStep
}

// The steward's states.
const (
	StewardWorking = "working"
	StewardResting = "resting" // at its daily run cap
	StewardParked  = "parked"  // at the purse wall
	StewardIdle    = "idle"
)

// StewardPlan is one plan the steward tends. Stalled is a plan with no live
// gardener (the steward's stalledPlans), Why how its gardener stopped.
type StewardPlan struct {
	ID, Title, State string
	Stalled          bool
	Why              string
}

// StewardStep is a step parked at the purse wall, and its gardener.
type StewardStep struct {
	ID, Title, Agent string
}

const (
	// stewardCacheFor is how long one viewer's read of a land's steward is
	// kept; stewardCacheMax bounds the reads kept.
	stewardCacheFor = 30 * time.Second
	stewardCacheMax = 256
	// stewardThreadsMax and stewardRepliesMax bound the transcript: its
	// latest threads, each with its root and latest replies, each line cut
	// to stewardLineMax runes on screen (a cut line also carries its whole
	// text, as read: textMax). stewardPlansMax bounds the tree, and
	// stewardAsksScan the decisions read for the Asks rail. Together with
	// the notes (steward.NotesCapBytes) they keep the node near nodeBudget
	// at their worst.
	stewardThreadsMax = 4
	stewardRepliesMax = 3
	stewardLineMax    = 240
	stewardPlansMax   = 12
	stewardAsksScan   = 100
	// stewardNamesMax bounds the beads the transcript's words name that are
	// read for their titles; the rest link under their ids.
	stewardNamesMax = 12
	// stewardWishesScan is the open thread heads read for the Wishes tree and
	// the Asks rail, stewardWishesMax the wishes the tree draws, each title
	// and last line cut to stewardWishLineMax runes: at their worst about 6
	// KiB of the node.
	stewardWishesScan  = 40
	stewardWishesMax   = 8
	stewardWishLineMax = 120
)

// stewardHead is the steward node's own part of the doc.
type stewardHead struct {
	*StewardLand
	Base, Say, Watch string
	Tending          []nodeRow
	Transcript       []stewardThread
	Done             int // its runs' decisions answered
}

// stewardThread is one #general thread of the transcript.
type stewardThread struct {
	ID, Href string
	Root     stewardLine
	Replies  []stewardLine
	More     int // replies not sent
	last     time.Time
	ask      bool
}

// stewardLine is one message of the transcript: Voice is steward, agent or
// person, Via how it came (kin.Speaker). Base and Names link the ids and URLs
// in its words, as a timeline's. Whole is the message's whole text when Text
// was cut at stewardLineMax: the line carries it, and its Read it all opens it
// in place (web/cuts.js), so the cut has a way to the rest that never leaves
// the page (rubric C1, gd-r7JiBQdZNS; gd-Fmq3fgqFTm). Card is a card's subject: the line is its agent, linked to
// its panel, and State its state word, never the card's words.
type stewardLine struct {
	ID, Author, Voice, Via, Text string
	At                           time.Time
	Base, Whole                  string
	Names                        beadNames
	Card, State                  string
}

// stewardCache keeps each viewer's read of a land's steward.
type stewardCache struct {
	mu sync.Mutex
	at map[string]stewardCached
}

type stewardCached struct {
	sl    *StewardLand
	until time.Time
}

// steward serves GET /garden/c/s and /garden/c/s/{land}: the land's steward
// node. Without a land it is ?land=, else the viewer's first land, else (an
// unscoped viewer) the home garden.
func (h *Handler) steward(w http.ResponseWriter, r *http.Request) {
	if h.page(w, r) || !h.ready(w) {
		return
	}
	if h.Nodes == nil || h.Stewards == nil {
		http.Error(w, "the steward node is not enabled", http.StatusServiceUnavailable)
		return
	}
	sc := h.scope(r)
	land := strings.TrimSpace(firstNonEmpty(r.PathValue("land"), r.URL.Query().Get("land")))
	if land == "" && !sc.Admin && len(sc.Lands) > 0 {
		land = sc.Lands[0]
	}
	if land != "" && !sc.Allows(land) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	doc, err := h.readSteward(ctx, sc, land)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.write(w, "node", doc)
}

// stewardOf is the land's steward, from the cache when this viewer read it
// within stewardCacheFor.
func (h *Handler) stewardOf(ctx context.Context, sc Scope, land string) (*StewardLand, error) {
	key, now := sc.Principal+"\x00"+land, h.now()
	c := &h.stewardReads
	c.mu.Lock()
	got, ok := c.at[key]
	c.mu.Unlock()
	if ok && now.Before(got.until) {
		return got.sl, nil
	}
	sl, err := h.Stewards.Steward(ctx, land)
	if err != nil {
		return nil, readErr("steward of "+land, err)
	}
	c.mu.Lock()
	if c.at == nil || len(c.at) >= stewardCacheMax {
		c.at = map[string]stewardCached{}
	}
	c.at[key] = stewardCached{sl: sl, until: now.Add(stewardCacheFor)}
	c.mu.Unlock()
	return sl, nil
}

// readSteward is the land's steward node: the steward read, then one round of
// reads (its plans' gardener loops, #general's window, its runs' decisions).
func (h *Handler) readSteward(ctx context.Context, sc Scope, land string) (nodeDoc, error) {
	sl, err := h.stewardOf(ctx, sc, land)
	if err != nil {
		return nodeDoc{}, err
	}
	if sl == nil || !sc.Allows(sl.Land) {
		return nodeDoc{}, errNotFound
	}
	head := &stewardHead{StewardLand: sl, Base: h.BasePath}
	if sl.General != "" {
		head.Say = h.BasePath + "/garden/c/" + url.PathEscape(sl.Land) + "/" + url.PathEscape(sl.General) + "/say"
	}
	if sl.Run != "" {
		head.Watch = h.nodeHref(sl.Run)
	}
	doc := nodeDoc{Base: h.BasePath, Kind: nodeSteward, Land: sl.Land, Steward: head, Names: beadNames{},
		Root:     nodeRow{ID: sl.ID, Type: "trigger", Title: firstNonEmpty(sl.Name, sl.Land+"-steward"), Href: h.stewardHref(sl.Land), State: sl.State},
		Face:     strings.TrimSpace(sl.Notes),
		FaceFrom: sl.NotesID,
	}
	var loops, msgs, asks, heads, kinRows []*beadsapi.BeadDetail
	list := func(dst *[]*beadsapi.BeadDetail, what string, q beadsapi.ListBeadsQuery) func() error {
		return func() error {
			res, err := h.Beads.ListBeadsFiltered(ctx, q)
			if err != nil {
				return readErr(what+" of "+sl.Land, err)
			}
			*dst = res.Beads
			return nil
		}
	}
	plans := sl.Plans[:min(len(sl.Plans), stewardPlansMax)]
	epics := make([]string, len(plans))
	for i, p := range plans {
		epics[i] = GardenerEpicLabel + p.ID
	}
	in := []string{"project:" + sl.Land}
	round := []func() error{list(&asks, "decisions", beadsapi.ListBeadsQuery{Types: []string{"decision"}, Labels: in,
		Statuses: []string{"open", "closed"}, Sort: "-created_at", Limit: stewardAsksScan}),
		list(&heads, "wishes", beadsapi.ListBeadsQuery{Types: []string{"task"}, Labels: append(slices.Clone(in), "thread"),
			Statuses: []string{"open"}, Sort: "-updated_at", Limit: stewardWishesScan})}
	if len(epics) > 0 {
		round = append(round, list(&loops, "gardeners", beadsapi.ListBeadsQuery{Types: []string{"trigger", "schedule"}, Labels: in, LabelsAny: epics, Limit: 2 * len(epics)}))
	}
	if sl.General != "" {
		round = append(round, list(&msgs, "#general", beadsapi.ListBeadsQuery{Labels: in, Fields: map[string]string{fieldChannelID: sl.General}, Sort: "-created_at", Limit: timelineWindow}))
	}
	if err := parallel(round...); err != nil {
		return nodeDoc{}, err
	}
	heads = slices.DeleteFunc(heads, func(b *beadsapi.BeadDetail) bool {
		return b == nil || landOf(b.Labels) != sl.Land || !kin.IsWishHead(b)
	})
	if len(heads) > 0 {
		ids := make([]string, len(heads))
		for i, b := range heads {
			ids[i] = b.ID
		}
		if err := list(&kinRows, "wishes' kin", kin.SummaryQuery(ids))(); err != nil {
			return nodeDoc{}, err
		}
	}
	sums := kin.Summaries(heads, kinRows)
	for _, p := range plans {
		row := nodeRow{ID: p.ID, Type: "epic", Title: clipRunes(firstNonEmpty(p.Title, p.ID), nodeTitleMax), Href: h.nodeHref(p.ID), State: firstNonEmpty(p.State, "open")}
		if g := gardenerOf(loops, p.ID); g != nil && sc.Allows(landOf(g.Labels)) {
			row.Children = []nodeRow{{ID: g.ID, Type: g.Type, Title: rowTitle(g), Href: h.nodeHref(g.ID), State: loopState(g)}}
		}
		if p.Stalled {
			row.Flag = "stalled"
			head.Tending = append(head.Tending, nodeRow{ID: p.ID, Type: "epic", Title: row.Title, Href: row.Href, State: firstNonEmpty(p.Why, "stalled"), Flag: "stalled"})
		}
		doc.Children = append(doc.Children, row)
		doc.Names[p.ID] = row.Title
	}
	doc.More = len(sl.Plans) - len(plans)
	if w := h.stewardWishes(sl, heads, sums); w != nil {
		doc.Children = append(doc.Children, *w)
	}
	for _, s := range sl.Parked {
		head.Tending = append(head.Tending, nodeRow{ID: s.ID, Type: "task", Title: clipRunes(firstNonEmpty(s.Title, s.ID), nodeTitleMax), Href: h.nodeHref(s.ID), State: "parked", Flag: "parked"})
	}
	doc.Open, head.Done = stewardAsks(asks, heads, sums, sl, h.now())
	for _, o := range doc.Open {
		if o.ID != "" {
			doc.Names[o.ID] = o.Title // an ask reads as its title, not its id
		}
	}
	head.Transcript = h.stewardTranscript(wishTalk(msgs, heads, kinRows, sl), sl, sums, doc.Names)
	h.stewardNames(ctx, sc, doc.Face, head.Transcript, doc.Names)
	if sl.Run != "" {
		doc.Jobs, doc.JobCount = h.Jobs.Of(sl.Run)
		doc.Jobs = doc.Jobs[:min(len(doc.Jobs), nodeJobsMax)]
	}
	if len([]rune(doc.Face)) > nodeFaceMax {
		doc.Face, doc.FaceWhole = clipRunes(doc.Face, nodeFaceMax), clipRunes(doc.Face, nodeFaceWholeMax)
	}
	return doc, nil
}

// stewardRun reports whether a conversation author is one of the land's
// steward runs, <land>-steward-<suffix> (steward_sli.go).
func stewardRun(author, land string) bool {
	a := strings.TrimPrefix(strings.TrimPrefix(author, "agent:"), "oidc:")
	return strings.HasPrefix(a, land+"-"+stewardRole+"-")
}

// stewardAsks is the Asks rail: the open decisions the land's steward runs
// filed, newest first, then its unanswered wishes, latest first, then the
// promises in its notes whose date has passed; and how many of its decisions
// are answered.
func stewardAsks(decisions, heads []*beadsapi.BeadDetail, sums map[string]*kin.Summary, sl *StewardLand, now time.Time) (out []nodeOpen, done int) {
	for _, d := range decisions {
		if d == nil || landOf(d.Labels) != sl.Land || !stewardRun(d.CreatedBy, sl.Land) {
			continue
		}
		if d.Status == "closed" {
			done++
		} else if len(out) < nodeOpenMax {
			out = append(out, nodeOpen{ID: d.ID, Type: "decision", Title: rowTitle(d), State: d.Status})
		}
	}
	for _, b := range heads {
		if unanswered(sums[b.ID]) && len(out) < nodeOpenMax {
			out = append(out, nodeOpen{ID: b.ID, Type: "wish", Title: rowTitle(b), State: wishUnanswered})
		}
	}
	for _, p := range duePromises(sl.Notes, now) {
		if len(out) < nodeOpenMax {
			out = append(out, nodeOpen{Type: "promise", Title: clipRunes(p, nodeTitleMax), State: "due"})
		}
	}
	return out, done
}

// wishUnanswered flags a wish whose last word is a person's with no agent live
// on it: the land's ask nobody is answering.
const wishUnanswered = "unanswered"

func unanswered(s *kin.Summary) bool {
	return s != nil && s.Last.Voice == kin.VoicePerson && (s.Agent == nil || !s.Agent.Live)
}

// nodeWish is a wish row's conversation as the tree says it: its messages, its
// last line and who said it, and the agent on it.
type nodeWish struct {
	Replies       int
	More          bool // Replies is a floor
	Author, Voice string
	Text          string
	At            time.Time
	// Agent and AgentState are the agent on it, each left out when the row
	// already says it: its last line is the agent's, or its state is the row's.
	Agent, AgentState string
}

// stewardWishes is the Wishes tree under the plans: the land's open thread
// heads (latest first, the first stewardWishesMax), each linking to its bead
// view, under #general, where the land's wishes are asked; nil for none, or
// no #general to hang them from.
func (h *Handler) stewardWishes(sl *StewardLand, heads []*beadsapi.BeadDetail, sums map[string]*kin.Summary) *nodeRow {
	if len(heads) == 0 || sl.General == "" {
		return nil
	}
	g := &nodeRow{ID: sl.General, Type: "wishes", Title: "Wishes", Href: h.placeHref(sl.General), State: "open", More: max(0, len(heads)-stewardWishesMax)}
	for _, b := range heads[:min(len(heads), stewardWishesMax)] {
		s := sums[b.ID]
		w := &nodeWish{Replies: s.Replies, More: s.More, Author: s.Last.Author, Voice: s.Last.Voice, Text: clipRunes(s.Last.Text, stewardWishLineMax)}
		if s.Last.At > 0 {
			w.At = time.Unix(s.Last.At, 0)
		}
		row := nodeRow{ID: b.ID, Type: "wish", Title: clipRunes(rowTitle(b), stewardWishLineMax), Href: h.BasePath + "/garden?bead=" + url.QueryEscape(b.ID), State: b.Status, Thread: b.ID, Wish: w}
		if a := s.Agent; a != nil { // said once: its name unless it spoke last, its state unless the row's is it
			if s.Last.Author != "agent:"+a.Name {
				w.Agent = a.Name
			}
			if a.Live {
				row.State = firstNonEmpty(a.State, row.State)
			} else {
				w.AgentState = a.State
			}
		}
		if unanswered(s) {
			row.Flag = wishUnanswered
		}
		g.Children = append(g.Children, row)
	}
	return g
}

// wishTalk is the transcript's messages, newest first: #general's window, the
// land's wish heads and their kin's messages, each once. A wish's Slack reply
// or squawk carries no channel_id; it is in its head's thread, in #general.
func wishTalk(msgs, heads, kinRows []*beadsapi.BeadDetail, sl *StewardLand) []*beadsapi.BeadDetail {
	out, seen := make([]*beadsapi.BeadDetail, 0, len(msgs)+len(heads)+len(kinRows)), map[string]bool{}
	for _, rows := range [][]*beadsapi.BeadDetail{msgs, heads, kinRows} {
		for _, b := range rows {
			if b != nil && !seen[b.ID] && (b.Type == typeMessage || kin.IsWishHead(b)) {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b *beadsapi.BeadDetail) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// promiseDate is a date written in a Promises line.
var promiseDate = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)

// duePromises are the lines of the notes' Promises section that name a date
// before now's (UTC) day.
func duePromises(notes string, now time.Time) []string {
	var out []string
	in, today := false, now.UTC().Format(time.DateOnly)
	for l := range strings.SplitSeq(notes, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "## ") {
			in = strings.TrimSpace(strings.TrimPrefix(l, "## ")) == "Promises"
			continue
		}
		if m := promiseDate.FindStringSubmatch(l); in && m != nil && m[1] < today {
			out = append(out, strings.TrimSpace(strings.TrimLeft(l, "-* ")))
		}
	}
	return out
}

// stewardTranscript is the land's #general threads, from msgs (its window,
// newest first), that address the steward (an @steward in them), that it
// posted in or that are its wishes (sums' heads), by their last message, the
// latest stewardThreadsMax. Each line is who kin.Speaker says wrote it, so a
// Slack reply is the person's and a squawk the crew's.
func (h *Handler) stewardTranscript(msgs []*beadsapi.BeadDetail, sl *StewardLand, sums map[string]*kin.Summary, names beadNames) []stewardThread {
	byRoot := map[string]*stewardThread{}
	var threads []*stewardThread
	for _, b := range slices.Backward(msgs) {
		if b == nil {
			continue
		}
		m, ok := messageIn(b, sl.General)
		if !ok || m.Land != sl.Land || m.Channel != sl.General || m.Deleted {
			continue
		}
		root := firstNonEmpty(m.Thread, m.ID)
		t := byRoot[root]
		if t == nil {
			t = &stewardThread{ID: root, Href: h.placeHref(root), ask: sums[root] != nil}
			byRoot[root] = t
			threads = append(threads, t)
		}
		line := stewardLine{ID: m.ID, Text: clipRunes(m.Text, stewardLineMax), At: m.At, Base: h.BasePath, Names: names}
		if line.Text != m.Text {
			line.Whole = m.Text
		}
		line.Author, line.Voice, line.Via = m.Author, m.Voice, m.Via
		if m.Kind == kindCard && m.SubjectID != "" {
			line.Card, line.Text, line.Whole = m.SubjectID, "", ""
			if s := sums[root]; s != nil && s.Agent != nil && s.Agent.ID == m.SubjectID {
				line.State = agentWord(*s.Agent)
			}
		}
		if m.Thread == "" {
			t.Root = line
		} else {
			t.Replies = append(t.Replies, line)
		}
		t.last = m.At
		t.ask = t.ask || line.Voice == kin.VoiceSteward || stewardMention.MatchString(m.Text)
	}
	threads = slices.DeleteFunc(threads, func(t *stewardThread) bool { return !t.ask || t.Root.ID == "" })
	slices.SortStableFunc(threads, func(a, b *stewardThread) int { return a.last.Compare(b.last) })
	out := make([]stewardThread, 0, stewardThreadsMax)
	for _, t := range threads[max(0, len(threads)-stewardThreadsMax):] {
		if n := len(t.Replies); n > stewardRepliesMax {
			t.More, t.Replies = n-stewardRepliesMax, t.Replies[n-stewardRepliesMax:]
		}
		out = append(out, *t)
	}
	return out
}

// stewardNames reads into names the titles of the beads the transcript's and
// the notes' words name that it lacks, at most stewardNamesMax, so each links
// under its title as a timeline's does (readNames): one the daemon does not
// have, or the viewer may not see, is a "not found" chip that still links
// (rubric C12, F6; gd-r7JiBQdZNS).
func (h *Handler) stewardNames(ctx context.Context, sc Scope, notes string, threads []stewardThread, names beadNames) {
	var ids []string
	add := func(l stewardLine) {
		for _, id := range textBeadIDs(l.Text) {
			if _, ok := names[id]; !ok && !slices.Contains(ids, id) && len(ids) < stewardNamesMax {
				ids = append(ids, id)
			}
		}
	}
	for _, t := range threads {
		add(t.Root)
		for _, r := range t.Replies {
			add(r)
		}
	}
	add(stewardLine{Text: notes})
	maps.Copy(names, h.readNames(ctx, sc, ids))
}

// agentWord is an agent's state word, as the bead view's card line says it
// (web/beadtalk.js cardState): its agent_state, else at work or left.
func agentWord(a kin.Agent) string {
	switch {
	case a.State != "":
		return a.State
	case a.Live:
		return "at work"
	}
	return "left"
}

// stewardMention is an @steward in a message's words.
var stewardMention = regexp.MustCompile(`(?i)(^|[^\w@])@steward\b`)

// stewardNodeLand is the land a steward trigger is bound to, "" for any other
// bead: the steward marker and one gardener-land:<land> that matches its
// project label (gardenhub stewardLand).
func stewardNodeLand(b *beadsapi.BeadDetail) string {
	if b.Type != "trigger" || !hasLabel(b.Labels, stewardRole) {
		return ""
	}
	if land := landOf(b.Labels); land != "" && hasLabel(b.Labels, "gardener-land:"+land) {
		return land
	}
	return ""
}

func (h *Handler) stewardHref(land string) string {
	return h.BasePath + "/garden/c/s/" + url.PathEscape(land)
}

// The steward node's own parts of the node fragment (node_render.go): its
// head, the composer it names, and its Tending and transcript rails.
var _ = template.Must(fragments.Parse(`
{{define "stewardhead"}}<p class="cm-sw-now" data-state="{{.State}}" data-agent="{{.Name}}">{{if eq .State "working"}}Working{{else if eq .State "resting"}}Resting: its daily runs are used{{else if eq .State "parked"}}Parked at the purse wall{{else}}Idle{{end}}
{{- with .Reason}} <span class="cm-sw-why">{{linkify . $.Base nil}}</span>{{end}}</p>
<p class="cm-sw-today"><span data-slot="runs">{{plural .RunsToday "run" "runs"}} today{{if .MaxRunsPerDay}} of {{.MaxRunsPerDay}}{{end}}</span>{{with .Spend}} · <span data-slot="spend">{{.}}</span>{{end}}
{{- with .Watch}} · <a class="cm-sw-watch" href="{{.}}" data-node="{{$.Run}}">Watch live</a>{{end}}</p>
{{- with .Say}}<div class="cm-sw-compose" data-say="{{.}}" data-channel="{{$.General}}" data-mention="@steward" hidden></div>{{end}}{{end}}

{{define "stewardrails"}}<section class="cm-sw-tending" data-slot="tending"><h3>Tending</h3>
{{- with .Tending}}<ol>{{range .}}{{template "noderow" .}}{{end}}</ol>{{else}}<p class="cm-empty">Nothing stalled or parked.</p>{{end}}</section>
<section class="cm-sw-talk" data-slot="transcript" data-channel="{{.General}}"><h3>Conversation</h3>
{{- with .Transcript}}<ol>
{{- range .}}
<li class="cm-sw-thread" data-thread="{{.ID}}">{{template "stewardline" .Root}}
{{- if or .Replies .More}}<ol>{{if .More}}<li class="cm-tn-more"><a href="{{.Href}}" data-thread="{{.ID}}">{{plural .More "earlier reply" "earlier replies"}}</a></li>{{end}}{{range .Replies}}<li>{{template "stewardline" .}}</li>{{end}}</ol>{{end}}</li>
{{- end}}
</ol>{{else}}<p class="cm-empty">Nobody has asked the steward anything in #general yet.</p>{{end}}</section>{{end}}

{{define "wishmeta"}}<span class="cm-tn-wish" data-voice="{{.Voice}}">{{if .More}}at least {{end}}{{if .Replies}}{{plural .Replies "reply" "replies"}}{{else}}no replies{{end}}
{{- with .Text}} · <span class="cm-tn-last"><b>{{author $.Author}}</b>{{if not $.At.IsZero}} <time datetime="{{stamp $.At}}">{{date $.At}} {{hm $.At}}</time>{{end}} {{.}}</span>{{end}}
{{- if or .Agent .AgentState}} · <span class="cm-tn-agent" data-state="{{.AgentState}}">{{.Agent}}{{if and .Agent .AgentState}} {{end}}{{words .AgentState}}</span>{{end}}</span>{{end}}

{{define "stewardline"}}<div class="cm-sw-line cm-sw-line--{{.Voice}}" data-id="{{.ID}}" data-voice="{{.Voice}}"{{with .Via}} data-via="{{.}}"{{end}}>
{{- if .Card}}<a class="cm-sw-who cm-bead" href="{{.Base}}/garden?bead={{.Card}}" data-bead="{{.Card}}">{{author .Author}}</a> <span class="cm-sw-tag">card</span>{{with .State}} <span class="cm-sw-state">{{words .}}</span>{{end}} <time datetime="{{stamp .At}}">{{hm .At}}</time>
{{- else}}<span class="cm-sw-who">{{if eq .Voice "steward"}}Steward{{else}}{{author .Author}}{{if eq .Via "squawk"}} <span class="cm-sw-tag cm-via--squawk">squawk</span>{{else if eq .Voice "agent"}} <span class="cm-sw-tag">agent</span>{{end}}{{end}}</span> <time datetime="{{stamp .At}}">{{hm .At}}</time> {{if .Whole}}<span class="cm-cut"><span class="cm-sw-text cm-cut-head">{{linkify .Text .Base .Names}}</span><span class="cm-sw-text cm-cut-whole" hidden>{{linkify .Whole .Base .Names}}</span> <button type="button" class="cm-link cm-cut-go" aria-expanded="false">Read it all</button></span>{{else}}<span class="cm-sw-text">{{linkify .Text .Base .Names}}</span>{{end}}{{end}}</div>{{end}}
`))
