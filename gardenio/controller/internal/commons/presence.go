package commons

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Presence is who is in a land and in a channel (Commons III, spec sections
// 2.6 and 7.3 step 4.3). A person is present while they hold an event stream
// open on a land, and on a channel when the stream names one
// (GET /garden/c/events?land=<land>&channel=<name or id>). An agent is present
// while the kbeads presence tracker has heard from it lately, in its land and
// in the channel its card is placed in. It is in-memory state, never a bead.
//
// A change sends an id-only "presence" event through the hub, cut per
// subscriber by land like every other invalidation:
//
//	event: presence
//	data: {"id":"<channel id>","channel":"<channel id>","land":"<land>"}
//
// id and channel are empty when only the land changed. The client refetches
// GET /garden/c/{land}/{channel}/presence for the channel it shows.
//
// Typing (step 4.4, typing.go) lives here too: a principal may signal typing
// only where it is Present, and the typing event rides the same publish.
//
// A nil *Presence is safe: nobody is present and Join records nothing.
type Presence struct {
	publish func(Invalidation)
	agents  AgentSource
	logger  *slog.Logger
	// Linger keeps a person present this long after their last stream on a
	// spot ends, so the reconnect at bearer expiry does not flicker. Set
	// before the first Join; tests set 0.
	Linger time.Duration
	// TypingFor is how long one typing signal keeps its sender typing, and
	// TypingGap the shortest gap between two signals let through from one
	// principal's composer on one spot: the channel's, or any thread's
	// (typing.go). Zero means the defaults; set before the first Type.
	TypingFor, TypingGap time.Duration
	now                  func() time.Time

	mu     sync.Mutex
	people map[presenceKey]int      // open streams per principal and spot
	bots   map[presenceKey]struct{} // agent spots from the last poll
	typing map[typingKey]*typist    // who is typing where (typing.go)

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Spot is where a principal is: a land, and a channel bead id in it ("" when
// only the land).
type Spot struct {
	Land    string
	Channel string
}

type presenceKey struct {
	Who string
	Spot
}

// AgentSource returns where each live agent is, by principal (agent:<name>).
// An agent's spots name its land; one with a channel puts it in that channel
// too. Agents.Spots is the production source.
type AgentSource func(ctx context.Context) (map[string][]Spot, error)

const (
	// presenceLinger is the default Linger.
	presenceLinger = 20 * time.Second
	// agentPollEvery is how often the agent spots are re-read.
	agentPollEvery = 15 * time.Second
	// agentPollTimeout bounds one re-read.
	agentPollTimeout = 10 * time.Second
	// presenceShown caps the avatars one presence fragment draws.
	presenceShown = 12
	// presenceListed caps the names its list under the avatars gives.
	presenceListed = 50
)

// NewPresence returns presence that publishes through hub (nil publishes
// nothing) and reads agents from agents (nil: people only).
func NewPresence(hub *Hub, agents AgentSource, logger *slog.Logger) *Presence {
	if logger == nil {
		logger = slog.Default()
	}
	return &Presence{
		publish: hub.Publish, agents: agents, logger: logger, Linger: presenceLinger, now: time.Now,
		people: map[presenceKey]int{}, bots: map[presenceKey]struct{}{}, typing: map[typingKey]*typist{},
	}
}

// Start polls the agent source until Close. Nil-safe; a no-op without one.
func (p *Presence) Start() {
	if p == nil || p.agents == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(agentPollEvery)
		defer tick.Stop()
		for {
			p.PollAgents(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// Close stops the agent poll. Nil-safe.
func (p *Presence) Close() {
	if p == nil || p.cancel == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}

// Join records one open stream of who at the spot and returns the func that
// ends it. The first stream of a principal on a spot, and the end of its last
// one (after Linger), publish the spot. An empty who or land records nothing.
func (p *Presence) Join(who string, at Spot) (leave func()) {
	if p == nil || who == "" || at.Land == "" {
		return func() {}
	}
	k := presenceKey{Who: who, Spot: at}
	p.mu.Lock()
	p.people[k]++
	first := p.people[k] == 1
	p.mu.Unlock()
	if first {
		p.Publish(at)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if p.Linger <= 0 {
				p.drop(k)
				return
			}
			time.AfterFunc(p.Linger, func() { p.drop(k) })
		})
	}
}

func (p *Presence) drop(k presenceKey) {
	p.mu.Lock()
	p.people[k]--
	last := p.people[k] <= 0
	if last {
		delete(p.people, k)
	}
	p.mu.Unlock()
	if last {
		p.Publish(k.Spot)
	}
}

// Publish sends a presence event for the spot to the subscribers whose scope
// admits its land. Nil-safe.
func (p *Presence) Publish(at Spot) {
	if p == nil || p.publish == nil {
		return
	}
	p.publish(Invalidation{Kind: "presence", ID: at.Channel, Channel: at.Channel, Land: at.Land, At: p.now()})
}

// Present reports whether who is at the spot: a person holding a stream on it,
// or an agent placed there. A land-only spot admits who anywhere in the land.
func (p *Presence) Present(who string, at Spot) bool {
	for _, w := range p.Here(at) {
		if w == who {
			return true
		}
	}
	return false
}

// Here returns the principals at the spot, people first, then agents, each
// sorted by name. A land-only spot returns everyone anywhere in the land.
func (p *Presence) Here(at Spot) []string {
	if p == nil || at.Land == "" {
		return nil
	}
	seen := map[string]bool{}
	match := func(k presenceKey) {
		if k.Land == at.Land && (at.Channel == "" || k.Channel == at.Channel) {
			seen[k.Who] = true
		}
	}
	p.mu.Lock()
	for k := range p.people {
		match(k)
	}
	for k := range p.bots {
		match(k)
	}
	p.mu.Unlock()
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := authorParty(out[i]), authorParty(out[j]); a != b {
			return a == "human"
		}
		return authorName(out[i]) < authorName(out[j])
	})
	return out
}

// PollAgents re-reads the agent spots and publishes every spot whose agents
// changed. A failed read keeps the last answer, so a daemon hiccup does not
// empty every channel.
func (p *Presence) PollAgents(ctx context.Context) {
	if p == nil || p.agents == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, agentPollTimeout)
	defer cancel()
	spots, err := p.agents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.logger.Warn("commons presence: agent read failed, keeping the last roster", "error", err)
		}
		return
	}
	next := map[presenceKey]struct{}{}
	for who, ss := range spots {
		for _, s := range ss {
			if who != "" && s.Land != "" {
				next[presenceKey{Who: who, Spot: s}] = struct{}{}
			}
		}
	}
	changed := map[Spot]bool{}
	p.mu.Lock()
	for k := range next {
		if _, ok := p.bots[k]; !ok {
			changed[k.Spot] = true
		}
	}
	for k := range p.bots {
		if _, ok := next[k]; !ok {
			changed[k.Spot] = true
		}
	}
	p.bots = next
	p.mu.Unlock()
	for s := range changed {
		p.Publish(s)
	}
}

// joinStream records the stream's viewer at the spot its query names
// (streamSpot), and returns the func that ends it. Nothing is recorded for an
// anonymous viewer or a spot with no land.
func (h *Handler) joinStream(r *http.Request, at Spot) func() {
	if h.Presence == nil || h.Viewer == nil || at.Land == "" {
		return func() {}
	}
	who := h.Viewer(r)
	if who == "" {
		return func() {}
	}
	return h.Presence.Join(who, at)
}

// streamSpot is where a stream's query (?land=&channel=) puts its viewer, and
// the channel when it names one: nothing for a land outside the scope, and
// nothing for a channel the viewer cannot read (resolved on the viewer's own
// bearer, so a members-only channel admits only its members). A lookup that
// failed for any other reason (a slow or down daemon) is an error: the stream
// ends on it, rather than carry ids only for a channel the pane paints from
// content (gd-RvJ6t2Nagr), and the client reconnects from its last id.
func (h *Handler) streamSpot(ctx context.Context, r *http.Request, sc Scope) (Spot, *Channel, error) {
	q := r.URL.Query()
	land, ref := strings.TrimSpace(q.Get("land")), strings.TrimSpace(q.Get("channel"))
	if !sc.Allows(land) {
		return Spot{}, nil, nil
	}
	at := Spot{Land: land}
	if ref == "" || h.Beads == nil {
		return at, nil, nil
	}
	rctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	ch, err := readChannel(rctx, h.Beads, sc, land, ref)
	cancel()
	if errors.Is(err, errNotFound) {
		return Spot{}, nil, nil
	}
	if err != nil {
		return Spot{}, nil, err
	}
	at.Channel = ch.ID
	return at, &ch, nil
}

// presence serves GET /garden/c/{land}/{channel}/presence: who is in the
// channel, for its header, and how many more are elsewhere in the land.
func (h *Handler) presence(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.write(w, "presence", h.presenceOf(ch))
}

// presenceOf is the presence fragment's data for ch: the route's answer, and
// what the stream's presence event carries.
func (h *Handler) presenceOf(ch Channel) any {
	here := h.Presence.Here(Spot{Land: ch.Land, Channel: ch.ID})
	land := h.Presence.Here(Spot{Land: ch.Land})
	shown, more := here, 0
	if len(shown) > presenceShown {
		shown, more = shown[:presenceShown], len(shown)-presenceShown
	}
	listed, unlisted := here, 0
	if len(listed) > presenceListed {
		listed, unlisted = listed[:presenceListed], len(listed)-presenceListed
	}
	return struct {
		Channel   Channel
		Here      []string
		More      int
		Listed    []string
		Unlisted  int
		Count     int
		Elsewhere int
	}{ch, shown, more, listed, unlisted, len(here), len(land) - len(here)}
}

// avatarInitial is a principal's avatar letter: the first letter or digit of its
// display name, upper-cased.
func avatarInitial(p string) string {
	for _, r := range authorName(p) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	if r, _ := utf8.DecodeRuneInString(authorName(p)); r != utf8.RuneError {
		return string(r)
	}
	return "?"
}

// The presence fragment lives beside the others in the one template set. The
// avatar row is the summary of a disclosure whose body lists the names, in the
// same order and past the avatar cap, so a touch user, who has no hover title,
// can tap a letter to learn who it is.
var _ = template.Must(fragments.New("presence").Parse(
	`<div class="cm-presence" data-commons="presence" data-land="{{.Channel.Land}}" data-channel="{{.Channel.ID}}">
<details class="cm-who">
<summary class="cm-avatars" aria-label="{{plural .Count "person" "people"}} in #{{.Channel.Name}}">
{{- range .Here}}
<span class="cm-avatar cm-avatar--{{party .}}" title="{{author .}}{{if eq (party .) "agent"}} (agent){{end}}" data-who="{{.}}" aria-hidden="true">{{avatar .}}</span>
{{- end}}
{{- if .More}}
<span class="cm-avatar cm-avatar--more" title="{{.More}} more" aria-hidden="true">+{{.More}}</span>
{{- end}}
</summary>
<ul class="cm-who-list">
{{- range .Listed}}
<li class="cm-who-name"><span class="cm-who-ini cm-who-ini--{{party .}}" aria-hidden="true">{{avatar .}}</span><span class="cm-who-text">{{author .}}{{if eq (party .) "agent"}} <span class="cm-who-kind">(agent)</span>{{end}}</span></li>
{{- end}}
{{- if .Unlisted}}
<li class="cm-who-name cm-who-name--more">and {{.Unlisted}} more</li>
{{- end}}
</ul>
</details>
{{- if .Elsewhere}}
<p class="cm-presence-land">{{.Elsewhere}} more in {{.Channel.Land}}</p>
{{- end}}
</div>
`))
