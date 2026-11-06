package commons

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/deathreport"
)

// Card faces (spec 2.5). A card bead carries only its subject; the face is
// rendered from the subject bead on the viewer's own bearer, so a card always
// shows the subject as it is now and a subject the viewer may not read shows
// no face. Agent cards (Commons II step 2) have two faces: the live face while
// the agent runs, and the wrap-up face once it has stopped. Decision and
// permission cards (step 3) are in decision_cards.go, escalation cards (step 4)
// in escalation_cards.go.

const (
	// cardReads bounds the parallel subject reads behind one fragment.
	cardReads = 8
	// cardMemoTTL bounds how long the hub trusts a subject's card list. A new
	// card drops its subject's entry at once (its own event), so the TTL only
	// heals a missed event.
	cardMemoTTL = 5 * time.Minute
	// kindSubject is the hub's internal invalidation for a changed subject;
	// it never reaches a client.
	kindSubject = "subject"
)

// cardSubjectTypes are the bead types a card renders a face for, whose
// changes re-render their cards.
var cardSubjectTypes = map[string]bool{"agent": true, "decision": true, "permission": true, "escalation": true}

// AgentCard is an agent card's face.
type AgentCard struct {
	ID    string // the agent bead
	Name  string
	Land  string
	Role  string
	State string // agent_state, or "done" / "failed" for a closed agent
	// Live is the live face; false is the wrap-up face.
	Live bool
	// Thread: the agent is bound to a Slack thread, so Restart (kill, then
	// spawn again under its name with its session) has a thread to restart in.
	Thread bool
	Task   string // the claimed task's title: the card leads with it (spec 2.5)
	// Stream is the live status line, from the coop state the status reporter
	// mirrors onto the agent bead (agent_state, and updated_at for activity).
	Stream    string
	Active    time.Time // last activity, for Stream's "ago"
	Cost      string
	Context   string
	Turns     string
	Owner     string // who it acts for: operated_by
	SpawnedBy string // the spawn vector: what spawned it
	// The wrap-up face (gb stop --wrapup).
	TLDR            string
	Accomplishments string
	Blockers        string
	Handoff         string
	Restart         string   // restart_when: what it waits on to come back
	PRs             []wrapPR // pull_requests
	// Cause is why an agent that died mid-work died (state "died"), from its
	// close reason and fields; its death report says the same in its thread.
	Cause string
}

// agentStates is the mark, the Glass state (GL-4) and the stream line of each
// agent_state. The card says the Glass's state machine, as the Glass header,
// the Wall and the gardener figure do (web/glass.js STATES): the bead's
// states that coop has no word for read as the coop state they ride on, and
// the stream line says the rest ("No progress lately"). A state not listed
// reads as its own name.
var agentStates = map[string]struct{ mark, glass, stream string }{
	"spawning":        {"pending", "starting", "Starting up"},
	"assigning":       {"pending", "starting", "Being handed its task"},
	"working":         {"working", "working", "Working"},
	"idle":            {"idle", "idle", "Idle, waiting for input"},
	"stuck":           {"stuck", "working", "No progress lately"},
	"rate_limited":    {"paused", "rate_limited", "Paused: the model provider is rate limiting"},
	"budget_exceeded": {"paused", "error", "Paused: out of budget"},
	"stopping":        {"pending", "working", "Wrapping up"},
	"done":            {"done", "exited", ""},
	"failed":          {"failed", "error", ""},
	"died":            {"failed", "error", ""},
}

// glassWords are the Glass's state words (web/glass.js STATES, internal/glass
// State): Starting, Working, Idle, Prompt, Error, Parked, Restarting, Exited,
// and Rate limited from coop's error_category.
var glassWords = map[string]string{
	"starting": "Starting", "working": "Working", "idle": "Idle", "prompt": "Prompt", "error": "Error",
	"parked": "Parked", "restarting": "Restarting", "exited": "Exited", "rate_limited": "Rate limited",
}

// stateMarks draws each state's mark as inline SVG in currentColor, not as a
// text glyph: the dingbats and geometric shapes the marks were (✓ ✕ ⏸ ● ○ ◍)
// are not in Noto Sans, so the browser fell back to whatever symbol font the
// machine had and the card looked different on every one (gd-icRpoTdPhD).
// The mark is aria-hidden; the state's label beside it is the words.
var stateMarks = map[string]template.HTML{
	// ◌ a dashed ring: starting or wrapping up
	"pending": markSVG(`<circle cx="8" cy="8" r="4.5" fill="none" stroke="currentColor" stroke-width="1.5" stroke-dasharray="2.36 2.36"/>`),
	// ● a dot
	"working": markSVG(`<circle cx="8" cy="8" r="5" fill="currentColor"/>`),
	// ○ a ring
	"idle": markSVG(`<circle cx="8" cy="8" r="4.5" fill="none" stroke="currentColor" stroke-width="1.5"/>`),
	// ◍ a ring with vertical fill
	"stuck": markSVG(`<circle cx="8" cy="8" r="4.5" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M6 4.5v7M8 3.5v9M10 4.5v7" stroke="currentColor" stroke-width="1"/>`),
	// ⏸ two bars
	"paused": markSVG(`<path d="M4.5 3.5H7v9H4.5zM9 3.5h2.5v9H9z" fill="currentColor"/>`),
	// ✓ a check
	"done": markSVG(`<path d="M3.5 8.5l3 3L12.5 5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>`),
	// ✕ a cross
	"failed": markSVG(`<path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`),
}

func markSVG(shape string) template.HTML {
	return template.HTML(`<svg viewBox="0 0 16 16" width="14" height="14" focusable="false">` + shape + `</svg>`)
}

// Facts are the card's small print: spend, context, turns, owner, spawn vector.
func (c AgentCard) Facts() []string {
	var out []string
	for _, f := range []string{c.Cost, c.Context, c.Turns} {
		if f != "" {
			out = append(out, f)
		}
	}
	if c.Owner != "" {
		out = append(out, "for "+c.Owner)
	}
	if c.SpawnedBy != "" {
		out = append(out, "spawned by "+c.SpawnedBy)
	}
	return out
}

// Headline is what the card leads with: the task (spec 2.5), else the
// wrap-up's tldr, else the agent's name.
func (c AgentCard) Headline() string {
	return firstNonEmpty(c.Task, c.HeadlineTLDR(), c.Name)
}

// HeadlineTLDR is the tldr when it is the headline (no task), so the wrap-up
// face does not say it twice.
func (c AgentCard) HeadlineTLDR() string {
	if c.Task == "" && !c.Live {
		return c.TLDR
	}
	return ""
}

// Mark is the state's mark, as inline SVG; Label its words.
func (c AgentCard) Mark() template.HTML {
	if s, ok := agentStates[c.State]; ok {
		return stateMarks[s.mark]
	}
	return stateMarks["working"]
}

// Glass is the state as the Glass's state machine has it (GL-4): the card's
// data-glass, whose Prompt and Error pulse on the live face.
func (c AgentCard) Glass() string {
	if s, ok := agentStates[c.State]; ok {
		return s.glass
	}
	if _, ok := glassWords[c.State]; ok {
		return c.State
	}
	return ""
}

func (c AgentCard) Label() string {
	if w, ok := glassWords[c.Glass()]; ok {
		return w
	}
	return strings.ReplaceAll(c.State, "_", " ")
}

// agentCardFrom reads the face of an agent card off its subject bead. ok is
// false when b is not an agent of land.
func agentCardFrom(b *beadsapi.BeadDetail, land string) (AgentCard, bool) {
	if b == nil || b.Type != "agent" {
		return AgentCard{}, false
	}
	// The subject is in the card's land: its project label, or (agent beads
	// written without one) its project field.
	if l := landOf(b.Labels); l != land && !(l == "" && strings.TrimSpace(b.Fields["project"]) == land) {
		return AgentCard{}, false
	}
	f := b.Fields
	c := AgentCard{
		ID: b.ID, Land: land, Role: f["role"], State: strings.TrimSpace(f["agent_state"]),
		Name: firstNonEmpty(f["agent"], b.Title, b.ID), Active: b.UpdatedAt,
		SpawnedBy: strings.TrimSpace(f["spawned_by"]), Task: strings.TrimSpace(f["task_title"]),
	}
	if owner := strings.TrimSpace(f["operated_by"]); owner != "" {
		c.Owner = authorName(owner)
	}
	c.Thread = strings.TrimSpace(f["slack_thread_channel"]) != "" && strings.TrimSpace(f["slack_thread_ts"]) != ""
	c.Live = b.Status != "closed" && c.State != "done" && c.State != "failed"
	if !c.Live && c.State != "failed" {
		c.State = "done"
	}
	// An involuntary death (the bridge's death report, by the same shared
	// classifier) is the died face, whatever agent_state the teardown left.
	died := !c.Live && deathreport.ClassifyWithFields(f["close_reason"], f).Involuntary
	if died {
		c.State = "died"
	}
	if c.Live {
		if s, ok := agentStates[c.State]; ok && s.stream != "" {
			c.Stream = s.stream
		} else if c.State != "" {
			c.Stream = c.Label()
		}
		c.Cost = dollars(firstNonEmpty(f["live_cost_usd"], f["total_cost_usd"]))
		c.Turns = count(firstNonEmpty(f["live_turns"], f["total_request_count"]), "turn", "turns")
	} else {
		c.Cost = dollars(firstNonEmpty(f["total_cost_usd"], f["live_cost_usd"]))
		c.Turns = count(firstNonEmpty(f["total_request_count"], f["live_turns"]), "turn", "turns")
		c.wrapUp(f["wrapup"], f["close_reason"])
		if died {
			c.Cause = deathCause(f)
			if c.TLDR == clipRunes(strings.TrimSpace(f["close_reason"]), 500) {
				// the close reason is a code, and the cause says it in words
				c.TLDR = ""
			}
		}
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(f["context_tokens"]), 10, 64); err == nil && n > 0 {
		c.Context = tokens(n) + " context"
	}
	return c, true
}

// wrapUp fills the wrap-up face from the agent's gb stop wrap-up; with none,
// the close reason is the tldr.
func (c *AgentCard) wrapUp(raw, closeReason string) {
	var w struct {
		TLDR            string   `json:"tldr"`
		Accomplishments string   `json:"accomplishments"`
		Blockers        string   `json:"blockers"`
		Handoff         string   `json:"handoff_notes"`
		Restart         string   `json:"restart_when"`
		PRs             []string `json:"pull_requests"`
	}
	if raw != "" && json.Unmarshal([]byte(raw), &w) == nil {
		c.TLDR = clipRunes(strings.TrimSpace(w.TLDR), 500)
		c.Accomplishments = clipRunes(strings.TrimSpace(w.Accomplishments), 2000)
		c.Blockers = clipRunes(strings.TrimSpace(w.Blockers), 2000)
		c.Handoff = clipRunes(strings.TrimSpace(w.Handoff), 2000)
		c.Restart = clipRunes(strings.TrimSpace(w.Restart), 500)
		for _, u := range w.PRs {
			if p, ok := wrapPRFrom(u); ok && len(c.PRs) < 6 {
				c.PRs = append(c.PRs, p)
			}
		}
	}
	if c.TLDR == "" {
		c.TLDR = clipRunes(strings.TrimSpace(closeReason), 500)
	}
}

// wrapPR is one of a wrap-up's pull requests: its link and its number.
type wrapPR struct{ URL, Num string }

// wrapPRFrom reads a pull or merge request URL; ok is false for anything that
// is not an http(s) link.
func wrapPRFrom(raw string) (wrapPR, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return wrapPR{}, false
	}
	n := path.Base(strings.TrimSuffix(u.Path, "/"))
	if _, err := strconv.Atoi(n); err != nil {
		n = ""
	}
	return wrapPR{URL: u.String(), Num: n}, true
}

// deathCause is a died agent's cause in words, as the bridge's death report
// words it (bridge deathCause): the reason the controller recorded when it
// deleted the pod, else the shared classifier's summary.
func deathCause(f map[string]string) string {
	if r := strings.TrimSpace(f["controller_restart_reason"]); r != "" {
		return clipRunes(r+" — the agent was mid-turn and did not `gb done`, so its claimed task was returned to the queue", 500)
	}
	return clipRunes(deathreport.ClassifyWithFields(f["close_reason"], f).Summary, 500)
}

// dollars formats a USD amount field; "" for none or zero.
func dollars(v string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 {
		return ""
	}
	if f < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", f)
}

func count(v, one, many string) string {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return ""
	}
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// tokens is a token count as the Slack card shows it: 812, 12k, 1.2M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt(n/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// ago is how long before now t was, in the largest whole unit.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

// withCards gives every card row its face, reading the subjects (and an agent's
// task) on the viewer's bearer, a few at a time, and says who each open ask
// waits on as viewer reads it. A subject that cannot be read leaves the row's
// plain subject link.
func (h *Handler) withCards(ctx context.Context, viewer string, rows []row) {
	var cards []*row
	for i := range rows {
		if r := &rows[i]; r.M.Kind == kindCard && r.M.SubjectID != "" && !r.M.Deleted {
			cards = append(cards, r)
		}
	}
	if len(cards) == 0 {
		return
	}
	now := h.now()
	sem := make(chan struct{}, cardReads)
	var memo cardOnce
	var wg sync.WaitGroup
	for _, r := range cards {
		wg.Add(1)
		sem <- struct{}{}
		go func(r *row) {
			defer func() { <-sem; wg.Done() }()
			b, err := h.Beads.GetBead(ctx, r.M.SubjectID)
			if err != nil || b == nil {
				return
			}
			switch b.Type {
			case "decision":
				if c, ok := decisionCardFrom(b, r.M.Land); ok {
					if c.Open {
						a := agentIn(ctx, h.Beads, b.Fields["requesting_agent_bead_id"], c.Asker, r.M.Land)
						if a != nil {
							c.AskerBead, c.For = a.ID, firstNonEmpty(c.For, operatorOf(a))
						}
						c.Waiting = waiting(c.For, viewer)
						c.Evidence = h.decisionEvidence(ctx, b, r.M.Land, "cm-"+r.M.ID)
					}
					r.Decision, r.Arms = &c, armsSVG(b.Heraldry)
				}
				return
			case "permission":
				if c, ok := permissionCardFrom(b, r.M.Land); ok {
					if c.Open {
						if a := agentIn(ctx, h.Beads, b.Fields["agent_bead_id"], c.Agent, r.M.Land); a != nil {
							c.AgentBead, c.For = a.ID, operatorOf(a)
						}
						c.Waiting = waiting(c.For, viewer)
					}
					r.Permission, r.Arms = &c, armsSVG(b.Heraldry)
				}
				return
			case "escalation":
				if c, ok := escalationCardFrom(b, r.M.Land); ok {
					c.Asker = askerName(ctx, h.Beads, b.Fields["requesting_agent"], r.M.Land)
					// ctx is the request's: it carries the viewer's bearer, so the
					// daemon answers for this viewer, never for the viewer process.
					c.CanApprove = EscalationActionsFor(ctx, h.Approvals, &c)
					c.Waiting = c.waiting()
					r.Escalation, r.Arms = &c, armsSVG(b.Heraldry)
				}
				return
			}
			c, ok := agentCardFrom(b, r.M.Land)
			if !ok {
				r.Arms = faceless(b, r.M.Land)
				return
			}
			r.Arms = armsSVG(b.Heraldry)
			if id := strings.TrimSpace(b.Fields["task_id"]); id != "" && (c.Task == "" || r.Arms == "") {
				if t := h.cardTask(ctx, &memo, id, r.M.Land); t != nil {
					if c.Task == "" {
						c.Task = clipRunes(strings.TrimSpace(t.Title), 200)
					}
					if r.Arms == "" {
						r.Arms = h.epicArms(ctx, &memo, t, r.M.Land)
					}
				}
			}
			if c.Live && c.Stream != "" {
				if a := ago(c.Active, now); a != "" {
					c.Stream += " · " + a
				}
			}
			r.Agent = &c
		}(r)
	}
	wg.Wait()
}

// cardOnce runs each read behind one withCards call once, however many cards
// ask for it: the wrap-ups of one plan share their tasks' epic, so its read.
type cardOnce struct {
	mu sync.Mutex
	by map[string]*cardOnceEntry
}

type cardOnceEntry struct {
	once sync.Once
	v    any
}

func (m *cardOnce) do(key string, f func() any) any {
	m.mu.Lock()
	e := m.by[key]
	if e == nil {
		if m.by == nil {
			m.by = map[string]*cardOnceEntry{}
		}
		e = &cardOnceEntry{}
		m.by[key] = e
	}
	m.mu.Unlock()
	e.once.Do(func() { e.v = f() })
	return e.v
}

// cardTask is the task an agent card's subject is on (its task_id) when that
// task is in land, else nil.
func (h *Handler) cardTask(ctx context.Context, m *cardOnce, id, land string) *beadsapi.BeadDetail {
	t, _ := m.do("task\x00"+land+"\x00"+id, func() any {
		t, err := h.Beads.GetBead(ctx, id)
		if err != nil || t == nil || t.ID != id || landOf(t.Labels) != land {
			return (*beadsapi.BeadDetail)(nil)
		}
		return t
	}).(*beadsapi.BeadDetail)
	return t
}

// epicArms is the picture of the epic task hangs from (its parent-child
// parent) when that epic is in land: agents rarely bear arms, epics do
// (heraldry spec R6), so an agent's card shows its work's. "" for none, and
// with no Nodes to read the edge.
func (h *Handler) epicArms(ctx context.Context, m *cardOnce, task *beadsapi.BeadDetail, land string) template.HTML {
	if h.Nodes == nil {
		return ""
	}
	up, _ := m.do("up\x00"+task.ID, func() any {
		deps, err := h.Nodes.BatchGetDependencies(ctx, []string{task.ID}, "forward")
		if err != nil {
			return ""
		}
		for _, d := range deps[task.ID] {
			if d.Type == "parent-child" && d.BeadID == task.ID && d.DependsOnID != "" {
				return d.DependsOnID
			}
		}
		return ""
	}).(string)
	if up == "" {
		return ""
	}
	arms, _ := m.do("arms\x00"+land+"\x00"+up, func() any {
		e, err := h.Beads.GetBead(ctx, up)
		if err != nil || e == nil || e.ID != up {
			return template.HTML("")
		}
		return faceless(e, land)
	}).(template.HTML)
	return arms
}

// agentIn is the bead of the agent an open decision or permission card names,
// so its See the screen opens that agent's Glass (Lands workbench-glass spec
// 3.3) and the card says whom it waits on: id, the bead the request recorded,
// when it is an agent of land; else the one open agent of land called name.
// nil when neither says, or the name is ambiguous.
func agentIn(ctx context.Context, rd Reader, id, name, land string) *beadsapi.BeadDetail {
	if id = strings.TrimSpace(id); id != "" {
		if a, err := rd.GetBead(ctx, id); err == nil && a != nil && a.ID == id && a.Type == "agent" && landOf(a.Labels) == land {
			return a
		}
	}
	if name = strings.TrimSpace(name); name == "" || land == "" {
		return nil
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"project:" + land},
		Fields:   map[string]string{"agent": name},
		Limit:    2,
	})
	if err != nil || res == nil || len(res.Beads) != 1 || res.Beads[0] == nil || landOf(res.Beads[0].Labels) != land {
		return nil
	}
	return res.Beads[0]
}

// operatorOf is the person an agent works for: its operated_by.
func operatorOf(a *beadsapi.BeadDetail) string {
	return firstNonEmpty(a.OperatedBy, a.Fields["operated_by"])
}

// cardLister lists beads; *beadsapi.Client satisfies it. The hub needs it to
// find a subject's cards; a BeadGetter without it gets no card refreshes.
type cardLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

type cardMemo struct {
	cards   []Invalidation
	expires time.Time
}

// subjectCards returns a "card" invalidation for every card bead whose subject
// is subject, read as the system and memoized. Each card's land is its own
// project label, from the daemon's answer; a card with none is dropped.
func (h *Hub) subjectCards(ctx context.Context, subject string) []Invalidation {
	now := h.now()
	h.memoMu.Lock()
	m, hit := h.cards[subject]
	h.memoMu.Unlock()
	if hit && now.Before(m.expires) {
		return m.cards
	}
	lister, ok := h.beads.(cardLister)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
	defer cancel()
	res, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{typeMessage},
		Fields: map[string]string{fieldKind: kindCard, fieldSubjectID: subject},
		Limit:  20,
	})
	if err != nil {
		return nil
	}
	var cards []Invalidation
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		land := landOf(b.Labels)
		ch, th := strings.TrimSpace(b.Fields[fieldChannelID]), strings.TrimSpace(b.Fields[fieldThreadID])
		if land == "" || (ch == "" && th == "") {
			continue
		}
		cards = append(cards, Invalidation{Kind: "card", ID: b.ID, Channel: ch, Thread: th, Land: land})
	}
	h.memoMu.Lock()
	if len(h.cards) >= landMemoCap {
		h.cards = map[string]cardMemo{}
	}
	h.cards[subject] = cardMemo{cards: cards, expires: now.Add(cardMemoTTL)}
	h.memoMu.Unlock()
	return cards
}

// forgetSubject drops subject's card memo, so a card just written is found on
// the subject's next change.
func (h *Hub) forgetSubject(subject string) {
	if subject == "" {
		return
	}
	h.memoMu.Lock()
	delete(h.cards, subject)
	h.memoMu.Unlock()
}
