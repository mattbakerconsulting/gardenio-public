package commons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Mentions (Commons II step 7, slice 4d). A message that @-mentions
// @gardenio, or an agent of its land, is a mention: once it is written, the
// Commons hands it to the bridge as {"kind":"compose","message":<id>}, and the
// bridge starts, wakes, nudges or stops the thread's agent
// (internal/bridge/inbound_compose.go). The bridge checks the person's land
// role itself ("spawn", and "agent_control" for kill, stop, restart and
// clear); the Gate here only decides whether to ask it, and the composer's
// @ list (GET /garden/c/{land}/{channel}/mentions) offers what the viewer may
// mention.
//
// People (Commons III step 6, gd-4fmDZb5C52). The @ list also offers the
// people of the channel's land, read from kbeads on the viewer's bearer
// (GET /v1/lands/{land}/members, which answers only the viewer's own lands),
// to anyone who may post there, guests included. A message that @-mentions
// one of them writes one type=notification bead per person, on the viewer's
// bearer, in the message's land; kbeads refuses one to anyone who is not a
// member of that land. Notifications are best-effort: the message stands
// whether or not they are written. Delivering them is step 7's.

// actionSpawn is the action-table entry a mention needs
// (conversation.ActionSpawn).
const actionSpawn = "spawn"

// BotMention is the bot's name in a mention (the bridge's commonsBotName).
const BotMention = "gardenio"

// StewardMention is the land's steward's name in a mention (steward.Mention
// without its @): a person's @steward wakes it in any channel of the land.
const StewardMention = "steward"

// mentionsMax caps the agents the @ list offers, and the people.
const mentionsMax = 40

// Notification bead fields and values (kbeads notification.go).
const (
	typeNotification = "notification"
	fieldTo          = "to"
	fieldReason      = "reason"
	fieldMessageID   = "message_id"
	reasonMention    = "mention"
	// notifyMax caps the people one message notifies.
	notifyMax = 20
	// notifyTitleMax caps a notification's title.
	notifyTitleMax = 200
)

// People lists a land's people on the viewer's bearer. *beadsapi.Client
// satisfies it.
type People interface {
	LandMembers(ctx context.Context, land string) ([]beadsapi.LandMember, error)
}

// person is one member of a land as a mention names them.
type person struct {
	Handle    string // what follows the @, lowercased
	Name      string
	Principal string // oidc:<email>
}

// landPeople reads land's people and gives each a handle: the email's local
// part, lowercased, as the timeline names an author, or the whole email with
// its @ as a dot when two people share a local part (ann.example.com), so a
// handle always names one person. A person with no usable handle is left out.
func landPeople(ctx context.Context, pp People, land string) ([]person, error) {
	members, err := pp.LandMembers(ctx, land)
	if err != nil {
		return nil, readErr("people of "+land, err)
	}
	type entry struct{ email, local, name string }
	var list []entry
	locals := map[string]int{}
	seen := map[string]bool{}
	for _, m := range members {
		email, ok := strings.CutPrefix(strings.TrimSpace(m.Principal), "oidc:")
		email = strings.ToLower(email)
		local, _, found := strings.Cut(email, "@")
		if !ok || !found || local == "" || seen[email] {
			continue
		}
		seen[email] = true
		locals[local]++
		list = append(list, entry{email: email, local: local, name: strings.TrimSpace(m.Name)})
	}
	out := make([]person, 0, len(list))
	for _, e := range list {
		handle := e.local
		if locals[e.local] > 1 {
			handle = strings.Replace(e.email, "@", ".", 1)
		}
		if !mentionableName.MatchString(handle) {
			continue
		}
		out = append(out, person{Handle: handle, Name: firstNonEmpty(e.name, e.local), Principal: "oidc:" + e.email})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// notifyMentioned writes a notification for each person of ch's land that m
// @-mentions, once each, never to its author. It reads the land's people on
// the viewer's bearer and writes on it too, so kbeads decides both.
func (h *Handler) notifyMentioned(ctx context.Context, ch Channel, m Message) {
	names := mentionNames(m.Text)
	if len(names) == 0 || h.People == nil || h.Writes == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	people, err := landPeople(ctx, h.People, ch.Land)
	if err != nil {
		slog.Default().Warn("commons: mention: people not read", "land", ch.Land, "message", m.ID, "error", err)
		return
	}
	byHandle := make(map[string]person, len(people))
	for _, p := range people {
		byHandle[p.Handle] = p
	}
	title := clipBytes(authorName(m.Author)+" mentioned you in "+channelLabel(ch), notifyTitleMax)
	sent := 0
	for _, n := range names {
		p, ok := byHandle[n]
		if !ok || strings.EqualFold(p.Principal, m.Author) {
			continue
		}
		if sent == notifyMax {
			slog.Default().Info("commons: mention: notified the first people only", "message", m.ID, "max", notifyMax)
			return
		}
		fields := map[string]any{fieldTo: p.Principal, fieldReason: reasonMention, fieldMessageID: m.ID, fieldChannelID: ch.ID}
		if m.Thread != "" {
			fields[fieldThreadID] = m.Thread
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			continue
		}
		if _, err := h.Writes.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:          title,
			Type:           typeNotification,
			Labels:         []string{"project:" + ch.Land},
			Fields:         raw,
			CreatedBy:      m.Author,
			IdempotencyKey: "mention:" + m.ID + ":" + p.Principal,
		}); err != nil {
			slog.Default().Warn("commons: mention: notification not written", "message", m.ID, "to", p.Principal, "error", err)
			continue
		}
		sent++
	}
}

// mentionToken is an @name at the start of the text or after a space. An
// agent's name is ASCII, as the bridge reads it; a person's handle is their
// email's local part, which may be in any script, so letters and digits here
// are any script's (the bridge matches an agent only on its ASCII names).
var mentionToken = regexp.MustCompile(`(^|\s)@([\p{L}\p{N}][\p{L}\p{M}\p{N}._-]{0,127})`)

// agentToken is an @name as the bridge reads one (its mentionToken): only a
// message that has one goes to the bridge, so a mention of a person by a
// handle in another script stays words in the channel there, as before.
var agentToken = regexp.MustCompile(`(^|\s)@[A-Za-z0-9][A-Za-z0-9._-]{0,127}`)

// mentionableName is a name the composer can @-mention whole.
var mentionableName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{M}\p{N}._-]{0,127}$`)

// mentionNames returns the names text @-mentions, lowercased, once each.
func mentionNames(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionToken.FindAllStringSubmatch(text, -1) {
		name := strings.ToLower(strings.TrimRight(m[2], "."))
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Composer carries a mention to the bridge. NewBridgeComposer is the HTTP one
// beads-viewer builds.
type Composer interface {
	Compose(ctx context.Context, req ComposeRequest) (CardAnswer, error)
}

// ComposeRequest is one mention for the bridge's POST
// /api/conversation/inbound.
type ComposeRequest struct {
	Kind    string `json:"kind"` // "compose"
	Message string `json:"message"`
}

// NewBridgeComposer returns a Composer that calls the bridge at base (its
// service URL); nil when base is empty.
func NewBridgeComposer(base string, client *http.Client) Composer {
	in, ok := NewBridgeInbound(base, client).(*bridgeInbound)
	if !ok {
		return nil
	}
	return in
}

func (b *bridgeInbound) Compose(ctx context.Context, req ComposeRequest) (CardAnswer, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CardAnswer{}, err
	}
	return b.send(ctx, bytes.NewReader(body))
}

// mentionNote hands a message the viewer just wrote to the bridge when it
// mentions, and says what came of it in X-Commons-Note (the pane toasts it).
// The message stands whatever the bridge says: a mention that does not go
// through is still words in the channel. A viewer whose role may not spawn (a
// guest) is handed over too: the bridge delivers their mention in a thread as
// a reply, which the SSE watchers leave to it, and refuses the rest.
func (h *Handler) mentionNote(w http.ResponseWriter, r *http.Request, m Message) {
	names := mentionNames(m.Text)
	if len(names) == 0 || h.Compose == nil || !agentToken.MatchString(m.Text) {
		return
	}
	bot := false
	for _, n := range names {
		bot = bot || n == BotMention
	}
	note := ""
	switch {
	case h.Gate.MayAct == nil:
		if bot {
			note = "Your role in " + m.Land + " does not let you start agents; your message is posted."
		}
	default:
		ctx, cancel := context.WithTimeout(r.Context(), cardTimeout)
		defer cancel()
		a, err := h.Compose.Compose(ctx, ComposeRequest{Kind: "compose", Message: m.ID})
		var refusal *CardRefusal
		switch {
		case err == nil:
			note = cardNote(a.Notes)
		case errors.As(err, &refusal) && refusal.Status == http.StatusUnprocessableEntity && !bot:
			// Only people were named: no agent to tell.
		case errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500:
			note = refusal.Message
		default:
			slog.Default().Warn("commons: mention did not reach the bridge", "message", m.ID, "error", err)
			note = "Your message is posted, but the mention did not reach the agents. Mention again to retry."
		}
	}
	if note != "" {
		w.Header().Set("X-Commons-Note", url.PathEscape(clipBytes(note, 300)))
	}
}

// mentionable is one entry of the composer's @ list.
type mentionable struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Kind  string `json:"kind"` // "bot" | "person" | "agent"
}

// mentions serves GET /garden/c/{land}/{channel}/mentions: what the viewer may
// @-mention in the channel, as JSON. @gardenio first, when their land role
// lets them start agents; then @steward, when steward nodes are served (the
// steward wakes on any person's mention, spawn or not); then the land's
// people but the viewer, by handle;
// then, with @gardenio, the land's agents, the running ones first. A viewer
// who may not post there gets an empty list.
func (h *Handler) mentions(w http.ResponseWriter, r *http.Request) {
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
	out := []mentionable{}
	if h.may(r, ch, ActionSay) || h.may(r, ch, ActionReply) {
		spawn := h.Compose != nil && h.Gate.MayAct != nil && h.Gate.MayAct(r, ch.Land, actionSpawn)
		if spawn {
			out = append(out, mentionable{Name: BotMention, Label: "Start or wake an agent", Kind: "bot"})
		}
		if h.Stewards != nil {
			out = append(out, mentionable{Name: StewardMention, Label: "Ask the land's steward", Kind: "steward"})
		}
		if h.People != nil {
			people, err := landPeople(ctx, h.People, ch.Land)
			if err != nil {
				h.fail(w, err)
				return
			}
			self, n := h.self(r), 0
			for _, p := range people {
				if strings.EqualFold(p.Principal, self) || n == mentionsMax {
					continue
				}
				label := "person"
				if !strings.EqualFold(p.Name, p.Handle) {
					label += " · " + p.Name
				}
				out = append(out, mentionable{Name: p.Handle, Label: label, Kind: "person"})
				n++
			}
		}
		if spawn {
			agents, err := landAgents(ctx, h.Beads, ch.Land)
			if err != nil {
				h.fail(w, err)
				return
			}
			out = append(out, agents...)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=30")
	writeJSON(w, out)
}

// landAgents lists the agents of land on the viewer's bearer: the running
// ones first, then by name.
func landAgents(ctx context.Context, rd Reader, land string) ([]mentionable, error) {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Labels:   []string{"project:" + land},
		Statuses: []string{"open", "in_progress", "blocked"},
		Sort:     "-updated_at",
		Limit:    mentionsMax * 2,
	})
	if err != nil {
		return nil, readErr("agents of "+land, err)
	}
	type agent struct {
		m    mentionable
		live bool
	}
	var list []agent
	seen := map[string]bool{}
	for _, b := range res.Beads {
		if b == nil || landOf(b.Labels) != land {
			continue
		}
		name := strings.TrimSpace(firstNonEmpty(b.Fields["agent"], b.Title))
		if name == "" || seen[name] || !mentionableName.MatchString(name) {
			continue
		}
		seen[name] = true
		state := strings.TrimSpace(b.Fields["agent_state"])
		label := strings.TrimSpace(b.Fields["role"])
		if state != "" {
			label = strings.TrimSpace(label + " · " + state)
		}
		switch state {
		case "done", "failed", "stopping", "budget_exceeded":
			list = append(list, agent{m: mentionable{Name: name, Label: label, Kind: "agent"}})
		default:
			list = append(list, agent{m: mentionable{Name: name, Label: label, Kind: "agent"}, live: true})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].live != list[j].live {
			return list[i].live
		}
		return list[i].m.Name < list[j].m.Name
	})
	if len(list) > mentionsMax {
		list = list[:mentionsMax]
	}
	out := make([]mentionable, len(list))
	for i, a := range list {
		out[i] = a.m
	}
	return out, nil
}
