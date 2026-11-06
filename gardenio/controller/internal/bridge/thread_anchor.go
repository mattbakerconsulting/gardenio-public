package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
)

// threadAt names the thread a spawn or a respawn works in (design spec
// 2026-10-19-lands-commons-design.md, section 2.3, slice 4d). A Slack thread
// has its channel and ts, and its head once one is minted; a Commons thread
// with no Slack copy has only its head. The spawn and respawn cores
// (handleThreadSpawnAt, respawnThreadAgentAt) post to Slack, stream to it and
// set its status bar only when the thread has a Slack copy, and they key the
// thread's binding on the head; what they would have said in Slack goes to Say.
type threadAt struct {
	Channel string // the Slack thread's channel, when it has a Slack copy
	TS      string // the Slack thread's ts
	Head    string // the thread's wish head, when known

	// A Commons thread's, read off its head by the bridge.
	Land      string // the head's one land
	ChannelID string // the channel bead the head was said in
	Actor     string // oidc:<email> of the person who asked
	// Say takes what a Slack thread would have been told (a refusal, a warning)
	// for the person who asked; nil drops it.
	Say func(string)
}

// slack reports whether the thread has a Slack copy to post in.
func (a threadAt) slack() bool { return a.Channel != "" && a.TS != "" }

// commons reports whether the spawn was asked for in the Commons, with or
// without a Slack copy: only a Commons anchor carries the land.
func (a threadAt) commons() bool { return a.Land != "" }

// key is the thread's spawn-in-flight key: the Slack tuple, as it always was,
// else the head.
func (a threadAt) key() string {
	if a.slack() {
		return a.Channel + ":" + a.TS
	}
	return "head:" + a.Head
}

// slackThreadAt is the anchor of a Slack thread, with its head when the
// thread is aliased to one already.
func (b *Bot) slackThreadAt(channel, threadTS string) threadAt {
	at := threadAt{Channel: channel, TS: threadTS}
	if b.state != nil {
		at.Head, _ = b.state.ThreadHead(channel, threadTS)
	}
	return at
}

// threadNote tells the thread text: a reply in its Slack thread, exactly as
// the cores always posted it, else to the person who asked.
func (b *Bot) threadNote(at threadAt, text string) {
	if at.slack() {
		if b.api != nil {
			_, _, _ = b.api.PostMessage(at.Channel,
				slack.MsgOptionText(text, false),
				slack.MsgOptionTS(at.TS),
			)
		}
		return
	}
	if at.Say != nil {
		at.Say(plainNote(text))
	}
}

// plainNote drops the Slack emoji shortcodes a note opens with: the Commons
// shows its words as a toast.
func plainNote(text string) string {
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, ":") {
		end := strings.Index(text[1:], ":")
		if end < 0 || strings.ContainsAny(text[1:end+1], " \n") {
			break
		}
		text = strings.TrimSpace(text[end+2:])
	}
	return text
}

// bindThreadAgent records agent as the thread's agent: keyed on the head, with
// the Slack thread aliased to it.
func (b *Bot) bindThreadAgent(at threadAt, agent string) {
	if b.state == nil {
		return
	}
	if !at.slack() {
		if at.Head != "" {
			_ = b.state.SetThreadAgentByHead(at.Head, agent)
		}
		return
	}
	_ = b.state.BindThreadHead(at.Channel, at.TS, at.Head)
	_ = b.state.SetThreadAgent(at.Channel, at.TS, agent)
}

// threadAgentAt returns the agent bound to the thread.
func (b *Bot) threadAgentAt(at threadAt) (string, bool) {
	if b.state == nil {
		return "", false
	}
	if at.slack() {
		return b.state.GetThreadAgent(at.Channel, at.TS)
	}
	return b.state.GetThreadAgentByHead(at.Head)
}

// setThreadStatusAt sets the Slack thread's assistant status; a Commons thread
// has none (the agent card is the agent's status).
func (b *Bot) setThreadStatusAt(ctx context.Context, at threadAt, status string) {
	if at.slack() {
		b.SetThreadStatus(ctx, at.Channel, at.TS, status)
	}
}

// commonsThreadAgentName is the agent name of a Commons thread with no Slack
// copy: stable per head, so a respawn finds the same name (and with it the
// same session) as the first spawn, and label- and DNS-safe whatever the head
// id's case.
func commonsThreadAgentName(head string) string {
	sum := sha256.Sum256([]byte(head))
	return "thread-c" + hex.EncodeToString(sum[:])[:10]
}

// commonsChannelSettings reads the spawn settings of the Commons channel a
// thread was said in: the role a plain mention spawns there (default_role)
// and its RuntimeClass (runtime_class). Empty when unset or unreadable.
func (b *Bot) commonsChannelSettings(ctx context.Context, channelID string) (role, runtimeClass string) {
	if b.daemon == nil || channelID == "" {
		return "", ""
	}
	ch, err := b.daemon.GetBead(ctx, channelID)
	if err != nil || ch == nil || ch.Type != "channel" {
		return "", ""
	}
	return strings.TrimSpace(ch.Fields["default_role"]), strings.TrimSpace(ch.Fields["runtime_class"])
}

// landsGardenerRole is the role a Commons mention runs in a tenant land: it
// holds the lands-tenant tier, which role thread does not (gd-PtbbUxKWds).
const landsGardenerRole = "lands-gardener"

// commonsThreadRole is the role a Commons mention in land runs when no channel
// setting or --role names one: lands-gardener in a tenant land, else thread.
// A land that cannot be read gets thread; the daemon holds an agent bound to a
// tenant land to the tier whatever its role.
func (b *Bot) commonsThreadRole(ctx context.Context, land string) string {
	if kind, err := b.tenantKind(ctx, land); err == nil && kind != "" {
		return landsGardenerRole
	}
	return "thread"
}

// landSpawnRoles is the set of roles a Commons mention may spawn in land: the
// default (thread, or lands-gardener in a tenant land, as commonsThreadRole),
// and every role the land's project bead sends a channel to (channel_roles).
// Only an admin writes a project bead, while a land member may write a channel
// bead's default_role, so the project bead alone says which roles the land
// runs. A project that cannot be read allows only the thread default.
func (b *Bot) landSpawnRoles(ctx context.Context, land string) map[string]bool {
	roles := map[string]bool{"thread": true}
	if b.daemon == nil || land == "" {
		return roles
	}
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		return roles
	}
	if info, ok := projects[land]; ok {
		if !info.IsOperatorLand() {
			roles = map[string]bool{landsGardenerRole: true}
		}
		for _, rs := range info.ChannelRoles {
			for _, r := range rs {
				if r = strings.ReplaceAll(strings.TrimSpace(r), ",", "-"); r != "" {
					roles[r] = true
				}
			}
		}
	}
	return roles
}

// sortedRoles lists roles for a refusal.
func sortedRoles(roles map[string]bool) string {
	out := make([]string, 0, len(roles))
	for r := range roles {
		out = append(out, r)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// commonsSpawnRefusal is why a Commons message's spawn flags may not run in
// land, or "". --harness, --ultracode and --backend=microvm reach past what a
// land runs (another agent CLI, the workflow fleet, a Lambda MicroVM), so they
// stay with Slack's operators; --project must be the land, and --role a role
// of the land (landSpawnRoles). The flags are read as the spawn and respawn cores read
// them, in the same order.
func (b *Bot) commonsSpawnRefusal(ctx context.Context, land, text string) string {
	_, text = parseListenFlag(text)
	_, text = parseFreshFlag(text)
	project, text := parseProjectOverride(text)
	if project != "" && project != land {
		return fmt.Sprintf("An agent here works in %s; --project %s is not allowed in the Commons.", land, project)
	}
	role, text := parseRoleOverride(text)
	_, rawHarness, text := parseModelHarnessFlags(text)
	if rawHarness != "" {
		return "--harness is not offered in the Commons. Ask without it."
	}
	if v, bad, _ := parseUltracodeFlag(text); v != "" || bad != "" {
		return "--ultracode is not offered in the Commons. Ask without it."
	}
	if t, _, _ := parseBackendFlag(text); t == podmanager.ExecutionTargetLambdaMicroVM {
		return "--backend=microvm is not offered in the Commons. Ask without it."
	}
	if role != "" {
		role = strings.ReplaceAll(role, ",", "-")
		if roles := b.landSpawnRoles(ctx, land); !roles[role] {
			return fmt.Sprintf("%s does not run the %s role. Ask for one of: %s.", land, role, sortedRoles(roles))
		}
	}
	return ""
}

// commonsThreadLabel marks an agent bead spawned for a Commons thread with no
// Slack copy, as "slack-thread" marks a Slack thread's.
const commonsThreadLabel = "commons-thread"

// commonsSpawnFields fits a spawn's agent fields to a Commons thread with no
// Slack copy: no Slack binding, spawn_source source, and the person who asked
// as the operator when no Slack user stands in for them. A no-op for a Slack
// thread.
func (b *Bot) commonsSpawnFields(ctx context.Context, at threadAt, userID, source string, fields map[string]string) {
	if at.slack() {
		return
	}
	for _, k := range []string{"slack_thread_channel", "slack_thread_ts", "slack_spawn_channel"} {
		delete(fields, k)
	}
	fields["spawn_source"] = source
	if userID != "" || at.Actor == "" {
		return
	}
	fields["operated_by"] = at.Actor
	if email, ok := strings.CutPrefix(at.Actor, "oidc:"); ok {
		if u := b.UserDirectory().LookupByEmail(ctx, email); u != nil && u.BeadID != "" {
			fields["user_bead_id"] = u.BeadID
		}
	}
}

// askerUser is the user bead of the person who asked for a spawn: by their
// Slack user, else, in the Commons, by their email.
func (b *Bot) askerUser(ctx context.Context, at threadAt, slackUser string) *UserIdentity {
	if slackUser != "" {
		return b.UserDirectory().LookupBySlackID(ctx, slackUser)
	}
	if email, ok := strings.CutPrefix(at.Actor, "oidc:"); ok && email != "" {
		return b.UserDirectory().LookupByEmail(ctx, email)
	}
	return nil
}

// commonsThreadMax bounds how many of a Commons thread's messages a spawned
// agent's description carries, newest last, as fetchThreadContext bounds a
// Slack thread's.
const commonsThreadMax = 50

// commonsThreadContext is a Commons thread's conversation for a spawned
// agent's description, read from its beads: the head's words, then every
// message of the thread (thread_id the head, or the chat root it was promoted
// from), each as "author: text". Agents' own messages stay in: the thread is
// the agent's to read, as gb thread reads it.
//
// Only land's messages are read (the head must be land's too): a message's
// thread_id is its writer's, so a message of another land could name this
// thread, and its words must not reach this land's agent.
func (b *Bot) commonsThreadContext(ctx context.Context, headID, land string) string {
	if b.daemon == nil || headID == "" || land == "" {
		return "(no thread context)"
	}
	head, err := b.daemon.GetBead(ctx, headID)
	if err != nil || head == nil {
		return "(no thread context)"
	}
	if hl, ok := onlyLand(head.Labels); !ok || hl != land {
		return "(no thread context)"
	}
	var lines []string
	if t := strings.TrimSpace(firstNonEmptyField(head.Fields, conversation.BeadText, "slack_root_text")); t != "" {
		lines = append(lines, t)
	}
	roots := []string{head.ID}
	if from := strings.TrimSpace(head.Fields[fieldPromotedFrom]); from != "" {
		roots = append(roots, from)
	}
	var msgs []*beadsapi.BeadDetail
	for _, root := range roots {
		res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"message"},
			Labels: []string{"project:" + land},
			Fields: map[string]string{conversation.BeadThreadID: root},
			Sort:   "created_at",
			Limit:  commonsThreadMax,
		})
		if err != nil || res == nil {
			continue
		}
		msgs = append(msgs, res.Beads...)
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) })
	if len(msgs) > commonsThreadMax {
		msgs = msgs[len(msgs)-commonsThreadMax:]
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if ml, ok := onlyLand(m.Labels); !ok || ml != land {
			continue
		}
		text := strings.TrimSpace(firstNonEmptyField(m.Fields, conversation.BeadText))
		if text == "" {
			text = strings.TrimSpace(m.Title)
		}
		if text == "" {
			continue
		}
		who := commonsMessageFromDetail(m).author()
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(who, "oidc:"), "agent:")+": "+text)
	}
	if len(lines) == 0 {
		return "(no thread context)"
	}
	return strings.Join(lines, "\n")
}
