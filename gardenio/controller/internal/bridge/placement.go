package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
)

// Placement (local-agents spec §7, D4) decides whether a spawn lands on a
// local runner. A runner is chosen only explicitly: by the task's own
// placement fields, by a --runner flag, or by its owner mentioning the bot in
// a thread they opened. Pool pressure, cluster capacity and taints never
// choose one, and nothing here reads them.

// runnerOnlineWindow is how long after its last heartbeat a runner still
// counts as online (three missed 30 s beats).
const runnerOnlineWindow = 90 * time.Second

// placementLocal is the task bead's fields.placement value that pins it to a
// local runner (kbeads placementFields).
const placementLocal = "local"

// RunnerInfo is the part of a type=runner bead placement reasons about.
type RunnerInfo struct {
	ID, Hostname, Owner, State string
	Tags                       []string
}

// Placement is where a spawn goes.
type Placement struct {
	RunnerID  string       // chosen runner, "" = cluster
	AgentName string       // active local agent on that runner, "" if none
	Ask       []RunnerInfo // >1 online runners: ask in-thread which
	Reason    string       // human-readable, posted in-thread
}

func runnerInfo(r *beadsapi.BeadDetail) RunnerInfo {
	var tags []string
	_ = json.Unmarshal([]byte(r.Fields["tags"]), &tags)
	return RunnerInfo{ID: r.ID, Hostname: r.Fields["hostname"], Owner: r.Fields["owner"], State: r.Fields["state"], Tags: tags}
}

// runnerOnline reports whether r is online: state=online and a heartbeat
// within runnerOnlineWindow of now.
func runnerOnline(r *beadsapi.BeadDetail, now time.Time) bool {
	if r.Fields["state"] != "online" {
		return false
	}
	seen, err := time.Parse(time.RFC3339, r.Fields["last_seen_at"])
	return err == nil && now.Sub(seen) <= runnerOnlineWindow
}

// activeLocalAgentOnRunner returns the name of the live laptop agent (`gb
// runner join`, execution_target=laptop) whose runtime_id is runnerID, or ""
// when none has joined. An agent stopping or in a terminal state does not
// count: mail to it would never be read.
func (b *Bot) activeLocalAgentOnRunner(ctx context.Context, runnerID string) (string, error) {
	byRunner, err := b.activeLocalAgentsByRunner(ctx)
	if err != nil || len(byRunner[runnerID]) == 0 {
		return "", err
	}
	return byRunner[runnerID][0], nil
}

// activeLocalAgentsByRunner maps each runner id to the names of its live
// laptop agents, in listing order, from one listing.
func (b *Bot) activeLocalAgentsByRunner(ctx context.Context) (map[string][]string, error) {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"execution_target:" + podmanager.ExecutionTargetLaptop},
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	byRunner := map[string][]string{}
	for _, a := range res.Beads {
		if deadAgentStates[a.Fields["agent_state"]] || a.Fields["stop_requested"] == "true" {
			continue
		}
		name := a.Fields["agent"]
		if name == "" {
			name = a.Title
		}
		byRunner[a.Fields["runtime_id"]] = append(byRunner[a.Fields["runtime_id"]], name)
	}
	return byRunner, nil
}

// runnerHostnameTTL is how long runnerHostnames reuses its last listing.
const runnerHostnameTTL = 60 * time.Second

// runnerHostCache holds the last runner id -> hostname listing.
type runnerHostCache struct {
	mu    sync.Mutex
	at    time.Time
	hosts map[string]string
}

// runnerHostnames maps runner bead id to hostname, for the laptop badge on
// agent cards and /roster. The listing is cached for runnerHostnameTTL so a
// busy card flusher does not list runners on every edit; a failed listing
// keeps the previous map (or returns an empty one) and the badge falls back
// to "unknown host".
func (b *Bot) runnerHostnames(ctx context.Context) map[string]string {
	c := &b.runnerHosts
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts != nil && time.Since(c.at) < runnerHostnameTTL {
		return c.hosts
	}
	runners, err := b.listRunners(ctx)
	if err != nil {
		b.logger.Warn("could not list runners for the laptop badge", "error", err)
		if c.hosts == nil {
			return map[string]string{}
		}
		return c.hosts
	}
	hosts := make(map[string]string, len(runners))
	for _, r := range runners {
		hosts[r.ID] = runnerHostname(r)
	}
	c.hosts, c.at = hosts, time.Now()
	return hosts
}

// resolvePlacement decides where a spawn goes. In order: (1) the task's
// placement=local with a runner_id or runner_tag matching an online runner;
// (3) runnerFlag, the hostname of one of the mentioner's runners; (2) mentionUser == threadOpener, resolved to an
// owner with online runners (one: use it; several: Ask). Anything else is the
// cluster. A chosen runner with no joined agent comes back with RunnerID set,
// AgentName empty and a Reason: the caller says so and stops, it never falls
// back to the cluster for an explicit choice.
func (b *Bot) resolvePlacement(ctx context.Context, taskFields map[string]string, mentionUser, threadOpener, runnerFlag string) (Placement, error) {
	all, err := b.listRunners(ctx)
	if err != nil {
		return Placement{}, err
	}
	now := time.Now()
	pick := func(r *beadsapi.BeadDetail) (Placement, error) {
		agent, err := b.activeLocalAgentOnRunner(ctx, r.ID)
		if err != nil {
			return Placement{}, err
		}
		p := Placement{RunnerID: r.ID, AgentName: agent}
		if agent == "" {
			p.Reason = fmt.Sprintf("%s is online but no agent has joined; run `gb runner join` there.", runnerHostname(r))
		}
		return p, nil
	}

	// (1) explicit placement on the task bead.
	if taskFields["placement"] == placementLocal {
		for _, r := range all {
			if !runnerOnline(r, now) {
				continue
			}
			if id := taskFields["runner_id"]; id != "" && id == r.ID {
				return pick(r)
			}
			if tag := taskFields["runner_tag"]; tag != "" {
				for _, t := range runnerInfo(r).Tags {
					if t == tag {
						return pick(r)
					}
				}
			}
		}
		return Placement{Reason: "the task is placed on a local runner but none matching is online; it waits for one."}, nil
	}

	resolve := b.operatorFromSlackUser
	if b.operatorResolver != nil {
		resolve = b.operatorResolver
	}
	ownerOf := func(uid string) string {
		if uid == "" {
			return ""
		}
		if o := resolve(ctx, uid); strings.HasPrefix(o, "oidc:") {
			return o
		}
		return ""
	}

	// (3) --runner <hostname>. Only the mentioner's own runner: a local agent
	// works as its owner (spec §6), so nobody else may hand it work.
	if runnerFlag != "" {
		owner := ownerOf(mentionUser)
		foreign := false
		for _, r := range all {
			if r.Fields["state"] == runnerStateRevoked || r.Fields["hostname"] != runnerFlag {
				continue
			}
			if owner == "" || r.Fields["owner"] != owner {
				foreign = true
				continue
			}
			if runnerOnline(r, now) {
				return pick(r)
			}
			return Placement{Reason: fmt.Sprintf("runner %s is offline; running on the cluster.", runnerFlag)}, nil
		}
		if foreign {
			return Placement{Reason: fmt.Sprintf("runner %s is not one of yours; running on the cluster.", runnerFlag)}, nil
		}
		return Placement{Reason: fmt.Sprintf("no runner named %s; running on the cluster.", runnerFlag)}, nil
	}

	// (2) the owner, in a thread they opened.
	if mentionUser == "" || mentionUser != threadOpener {
		return Placement{}, nil
	}
	owner := ownerOf(mentionUser)
	if owner == "" {
		return Placement{}, nil
	}
	var online []*beadsapi.BeadDetail
	registered := false
	for _, r := range all {
		if r.Fields["owner"] != owner || r.Fields["state"] == runnerStateRevoked {
			continue
		}
		registered = true
		if runnerOnline(r, now) {
			online = append(online, r)
		}
	}
	switch len(online) {
	case 0:
		// Someone with no runner at all hears nothing: every thread they open
		// would otherwise carry the note.
		if !registered {
			return Placement{}, nil
		}
		return Placement{Reason: "none of your laptop runners is online; running on the cluster."}, nil
	case 1:
		return pick(online[0])
	default:
		p := Placement{}
		for _, r := range online {
			p.Ask = append(p.Ask, runnerInfo(r))
		}
		return p, nil
	}
}

// askRunnersText is the in-thread question when the owner has several
// runners online.
func askRunnersText(ask []RunnerInfo) string {
	names := make([]string, 0, len(ask))
	for _, r := range ask {
		names = append(names, r.Hostname)
	}
	return fmt.Sprintf("You have %d laptops online: %s. Reply with `--runner <hostname>` to pick one.", len(ask), strings.Join(names, ", "))
}

// parseRunnerFlag extracts a "--runner <hostname>" or "--runner=<hostname>"
// flag from mention text, returning the hostname and the text without it.
func parseRunnerFlag(text string) (string, string) {
	words := strings.Fields(text)
	for i, w := range words {
		host, rest := "", 0
		switch {
		case w == "--runner" && i+1 < len(words):
			host, rest = words[i+1], i+2
		case strings.HasPrefix(w, "--runner="):
			host, rest = strings.TrimPrefix(w, "--runner="), i+1
		default:
			continue
		}
		if host == "" {
			continue
		}
		remaining := append(append([]string{}, words[:i]...), words[rest:]...)
		return host, strings.Join(remaining, " ")
	}
	return "", text
}

// placeThreadOnRunner resolves placement for a first mention in a thread and,
// when a runner is chosen, binds the thread to its laptop agent and mails it
// the mention. It reports whether the mention was handled here; false means
// spawn on the cluster as before. An explicit choice that cannot be honoured
// (several runners, none joined, a failed lookup for --runner) is said in the
// thread and handled: it never falls through to a pod.
func (b *Bot) placeThreadOnRunner(ctx context.Context, at threadAt, mentionUser, threadOpener, runnerFlag, text string) bool {
	p, err := b.resolvePlacement(ctx, nil, mentionUser, threadOpener, runnerFlag)
	if err != nil {
		b.logger.Warn("thread-spawn: placement lookup failed", "runner", runnerFlag, "error", err)
		if runnerFlag != "" {
			b.threadNote(at, fmt.Sprintf("Could not look up runner %s (%v). Not spawning.", runnerFlag, err))
			return true
		}
		return false
	}
	switch {
	case len(p.Ask) > 0:
		b.threadNote(at, askRunnersText(p.Ask))
		return true
	case p.RunnerID != "" && p.AgentName == "":
		b.threadNote(at, p.Reason)
		return true
	case p.RunnerID == "":
		if p.Reason != "" {
			b.threadNote(at, p.Reason)
		}
		return false
	}

	// Point the laptop agent's home thread here, as a spawn does for a pod
	// agent, so its gb slack reply lands in this thread.
	if agent, ferr := b.daemon.FindAgentBead(ctx, p.AgentName); ferr == nil && agent != nil {
		fields := map[string]string{}
		if at.slack() {
			fields["slack_thread_channel"], fields["slack_thread_ts"] = at.Channel, at.TS
			if mentionUser != "" {
				fields["slack_user_id"] = mentionUser
			}
			if threadOpener != "" {
				fields["slack_opener_user_id"] = threadOpener
			}
		}
		beadsapi.SetWishPointer(fields, at.Head)
		if len(fields) > 0 {
			if uerr := b.daemon.UpdateBeadFields(ctx, agent.ID, fields); uerr != nil {
				b.logger.Warn("thread-spawn: could not stamp the thread on the laptop agent", "agent", p.AgentName, "error", uerr)
			}
		}
	}
	msg := fmt.Sprintf("Slack mention (thread %s in %s): %s", at.TS, at.Channel, text)
	if !at.slack() {
		msg = fmt.Sprintf("Commons mention (thread %s): %s", at.Head, text)
	}
	if err := NudgeAgent(ctx, b.daemon, b.logger, p.AgentName, msg); err != nil {
		b.logger.Warn("thread-spawn: mail to laptop agent failed", "agent", p.AgentName, "runner", p.RunnerID, "error", err)
		b.threadNote(at, fmt.Sprintf("Could not reach %s on its runner (%v). Not spawning.", p.AgentName, err))
		return true
	}
	b.bindThreadAgent(at, p.AgentName)
	b.logger.Info("thread-spawn: placed on local runner",
		"channel", at.Channel, "thread_ts", at.TS, "runner", p.RunnerID, "agent", p.AgentName)
	b.threadNote(at, fmt.Sprintf("Sent to *%s* on your laptop runner.", p.AgentName))
	return true
}

// spawnPlacement resolves /spawn --runner. It reports false when it has
// answered the command itself: a failed lookup, or a runner that is online
// with no agent joined. An offline or unknown runner is said and the spawn
// goes on to the cluster, the same as a mention.
func (b *Bot) spawnPlacement(ctx context.Context, cmd slack.SlashCommand, runnerFlag string) (Placement, bool) {
	th := b.themeFor(ctx, cmd.ChannelID)
	p, err := b.resolvePlacement(ctx, nil, cmd.UserID, "", runnerFlag)
	if err != nil {
		b.ephemeral(ctx, cmd, th.Semantic(SemFail)+fmt.Sprintf(" Could not look up runner %s (%v). Not spawning.", runnerFlag, err))
		return Placement{}, false
	}
	if p.RunnerID != "" && p.AgentName == "" {
		b.ephemeral(ctx, cmd, th.Semantic(SemFail)+" "+p.Reason)
		return Placement{}, false
	}
	if p.RunnerID == "" && p.Reason != "" {
		b.ephemeral(ctx, cmd, th.Semantic(SemWarn)+" "+p.Reason)
	}
	return p, true
}

// placeSpawnOnRunner stamps placement=local and runner_id on taskID and mails
// the runner's agent to pick it up, instead of spawning a pod.
func (b *Bot) placeSpawnOnRunner(ctx context.Context, cmd slack.SlashCommand, p Placement, hostname, taskID, description string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	if err := b.daemon.UpdateBeadFields(ctx, taskID, map[string]string{"placement": placementLocal, "runner_id": p.RunnerID}); err != nil {
		b.ephemeral(ctx, cmd, th.Semantic(SemFail)+fmt.Sprintf(" Could not place %s on %s: %v", taskID, hostname, err))
		return
	}
	msg := fmt.Sprintf("Task %s was placed on this runner by /spawn from <@%s>. Run `kd claim %s` and work it.", taskID, cmd.UserID, taskID)
	if description != "" {
		msg += "\n\n" + description
	}
	if err := NudgeAgent(ctx, b.daemon, b.logger, p.AgentName, msg); err != nil {
		b.logger.Warn("spawn: mail to laptop agent failed", "agent", p.AgentName, "task", taskID, "error", err)
		b.ephemeral(ctx, cmd, th.Semantic(SemWarn)+fmt.Sprintf(" Placed %s on %s, but could not mail %s (%v); it shows in gb ready there.", taskID, hostname, p.AgentName, err))
		return
	}
	b.logger.Info("spawn: placed task on local runner", "task", taskID, "runner", p.RunnerID, "agent", p.AgentName)
	b.ephemeral(ctx, cmd, th.Semantic(SemOK)+fmt.Sprintf(" Placed %s on %s and sent it to *%s*.", b.beadLink(taskID, taskID), hostname, p.AgentName))
}
