package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// End-to-end pin of the Slack channel ladder (gd-HL1aukSMkQ, epic
// gd-3cFHKIHCen). TestResolveChannel_Ladder covers resolveChannel's rungs on
// hand-set caches; this file drives the bridge the way production does
// instead: a bot built by NewBot with its Slack API URL pointed at the
// slackmock twin (as SLACK_API_URL points the deployed bridge at slackmock),
// agent beads in the daemon, and the four posts an agent makes reaching it
// through their real entrypoints:
//
//   - agent card:       NotifyAgentSpawn (the agent bead's created event)
//   - decision card:    NotifyDecision
//   - escalation card:  NotifyNewEscalation (gb escalation create)
//   - squawk:           Squawk.handleClosed (a closed squawk message bead)
//
// Where each post landed is read back from the twin's per-channel message
// store, never from bridge state. Every case wires up the rungs below the one
// it expects to win (a role channel, a project primary, a bridge default) so
// moving any rung up or down the ladder sends at least one case elsewhere.
//
// Each case runs twice: warm, where the spawn event filled the routing
// caches first, and cold, where the bridge never saw the spawn (a restart)
// and the decision has to refresh the agent from its bead
// (gd-35LkF76wAr).

const (
	rtDefaultCh  = "C-RT-DEFAULT"  // the bridge's own channel
	rtPrimaryCh  = "C-RT-PRIMARY"  // gardenio's primary slack_channel
	rtCrewCh     = "C-RT-CREW"     // channel_roles: crew
	rtGardenerCh = "C-RT-GARDENER" // channel_roles: gardener
	rtPinCh      = "C-RT-PIN"      // an explicit slack_spawn_channel
	rtSchedCh    = "C-RT-SCHED"    // a schedule's slack channel
	rtThreadCh   = "C-RT-THREAD"   // a Slack thread the agent is bound to
	rtThreadTS   = "1699999999.000042"

	rtDecisionMark   = "rt-probe-decision"
	rtEscalationMark = "rt-probe-escalation"
	rtSquawkMark     = "rt-probe-squawk"
)

type routingCase struct {
	name   string
	role   string
	fields map[string]string // agent bead fields beyond agent/project/role
	noProj bool
	want   string
	thread bool // posts must be replies in rtThreadCh under rtThreadTS
}

var routingCases = []routingCase{
	{
		name: "explicit spawn channel pin beats role channel",
		role: "crew",
		fields: map[string]string{
			"slack_spawn_channel": rtPinCh,
		},
		want: rtPinCh,
	},
	{
		// The schedule channel also beats a spawn channel inherited from a
		// spawner whose role differs: it is a pin, not an inheritance.
		name: "schedule channel beats inherited spawn channel and role channel",
		role: "crew",
		fields: map[string]string{
			"schedule_slack_channel":                  rtSchedCh,
			"slack_spawn_channel":                     rtGardenerCh,
			"slack_spawn_channel_inherited_from_role": "gardener",
		},
		want: rtSchedCh,
	},
	{
		name: "schedule channel beats explicit spawn channel pin",
		role: "crew",
		fields: map[string]string{
			"schedule_slack_channel": rtSchedCh,
			"slack_spawn_channel":    rtPinCh,
		},
		want: rtSchedCh,
	},
	{
		name: "spawn channel inherited from a same-role spawner is kept",
		role: "crew",
		fields: map[string]string{
			"slack_spawn_channel":                     rtPinCh,
			"slack_spawn_channel_inherited_from_role": "crew",
		},
		want: rtPinCh,
	},
	{
		// The crew child of a gardener (gd-3JHijlgZrE).
		name: "inherited spawn channel from a different role yields to channel_roles",
		role: "crew",
		fields: map[string]string{
			"slack_spawn_channel":                     rtGardenerCh,
			"slack_spawn_channel_inherited_from_role": "gardener",
		},
		want: rtCrewCh,
	},
	{
		name: "inherited spawn channel with no role channel beats project primary",
		role: "researcher",
		fields: map[string]string{
			"slack_spawn_channel":                     rtGardenerCh,
			"slack_spawn_channel_inherited_from_role": "gardener",
		},
		want: rtGardenerCh,
	},
	{
		name: "role channel only",
		role: "crew",
		want: rtCrewCh,
	},
	{
		name: "project primary channel only",
		role: "researcher",
		want: rtPrimaryCh,
	},
	{
		name:   "no project falls to the bridge default",
		role:   "crew",
		noProj: true,
		want:   rtDefaultCh,
	},
	{
		// A thread-bound agent stays in its thread whatever else it carries.
		name: "thread-bound agent stays in its thread",
		role: "crew",
		fields: map[string]string{
			"slack_thread_channel": rtThreadCh,
			"slack_thread_ts":      rtThreadTS,
			"spawn_source":         "slack-thread",
			"slack_spawn_channel":  rtPinCh,
		},
		want:   rtThreadCh,
		thread: true,
	},
}

func TestE2EChannelRouting_Ladder(t *testing.T) {
	for _, warm := range []bool{true, false} {
		mode := "cold"
		if warm {
			mode = "warm"
		}
		for i, tc := range routingCases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				runRoutingCase(t, tc, warm, i)
			})
		}
	}
}

// newRoutingE2EBot builds the bot through the production constructor with
// agent threading on (the deployed default) and its Slack client on twin.
func newRoutingE2EBot(t *testing.T, daemon BeadClient, twin *surfacetest.SlackTwin) *Bot {
	t.Helper()
	return NewBot(BotConfig{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		APIURL:        twin.Serve(t),
		Channel:       rtDefaultCh,
		ThreadingMode: "agent",
		Daemon:        daemon,
		Logger:        slog.Default(),
	})
}

func runRoutingCase(t *testing.T, tc routingCase, warm bool, i int) {
	ctx := context.Background()
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", rtPrimaryCh, map[string]string{
		rtCrewCh:     "crew",
		rtGardenerCh: "gardener",
	})
	twin := surfacetest.NewSlackTwin()
	bot := newRoutingE2EBot(t, daemon, twin)

	agent := "rt-agent-" + string(rune('a'+i))
	agentBeadID := "gd-rtagent" + string(rune('a'+i))
	fields := map[string]string{"agent": agent, "role": tc.role}
	if !tc.noProj {
		fields["project"] = "gardenio"
	}
	for k, v := range tc.fields {
		fields[k] = v
	}
	// Keyed by name for FindAgentBead; the escalation names it by bead id.
	detail := &beadsapi.BeadDetail{ID: agentBeadID, Title: agent, Type: "agent", Status: "open", Fields: fields}
	daemon.mu.Lock()
	daemon.beads[agent] = detail
	daemon.beads[agentBeadID] = detail
	daemon.mu.Unlock()

	if warm {
		bot.NotifyAgentSpawn(ctx, BeadEvent{ID: agentBeadID, Type: "agent", Title: agent, Status: "open", Fields: fields})
	}

	if err := bot.NotifyDecision(ctx, BeadEvent{
		ID:       "gd-rtdec" + agentBeadID[len("gd-rtagent"):],
		Type:     "decision",
		Assignee: agent,
		Fields: map[string]string{
			"prompt":                   rtDecisionMark + ": ship it?",
			"options":                  `["yes","no"]`,
			"requesting_agent_bead_id": agentBeadID,
		},
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	if err := bot.NotifyNewEscalation(ctx, BeadEvent{
		ID:    "gd-rtesc" + agentBeadID[len("gd-rtagent"):],
		Type:  "escalation",
		Title: rtEscalationMark + ": merge the PR",
		Fields: map[string]string{
			"operation_type":   "gitlab_mr_merge",
			"requesting_agent": agentBeadID,
		},
	}); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}

	squawk := NewSquawk(SquawkConfig{Daemon: daemon, Bot: bot, Logger: slog.Default()})
	ev, _ := json.Marshal(map[string]any{"bead": map[string]any{
		"id": "gd-rtsqk" + agentBeadID[len("gd-rtagent"):], "type": "message", "status": "closed",
		"title":  rtSquawkMark,
		"labels": []string{"squawk", "from:" + agent},
		"fields": map[string]string{"text": rtSquawkMark + ": tests green", "source_agent": agent},
	}})
	squawk.handleClosed(ctx, ev)

	assertRoutedTo(t, twin, tc)
}

// assertRoutedTo reads every channel the twin holds messages in and requires
// that all of them are tc.want, that the decision, escalation and squawk each
// landed there, and that they are threaded where the case says: under the
// agent's card (the only top-level post) or, for a thread-bound agent, in its
// Slack thread with no card of its own.
func assertRoutedTo(t *testing.T, twin *surfacetest.SlackTwin, tc routingCase) {
	t.Helper()
	chans := twin.Channels()
	for _, ch := range chans {
		if ch != tc.want {
			for _, m := range twin.Messages(ch) {
				t.Errorf("post landed in %s, want every post in %s: %q", ch, tc.want, routedSnippet(m))
			}
		}
	}

	var tops []string
	found := map[string]string{} // mark -> thread_ts it was posted under
	for _, m := range twin.Messages(tc.want) {
		shown, ok := twin.Lookup(conversation.Ref{Surface: conversation.SurfaceSlack, Channel: tc.want, TS: m[0]})
		if !ok {
			t.Fatalf("twin lost message %s in %s", m[0], tc.want)
		}
		if shown.ThreadTS == "" {
			tops = append(tops, m[0])
		}
		for _, mark := range []string{rtDecisionMark, rtEscalationMark, rtSquawkMark} {
			if strings.Contains(m[1], mark) || strings.Contains(m[2], mark) {
				if _, dup := found[mark]; dup {
					t.Errorf("%s posted more than once in %s", mark, tc.want)
				}
				found[mark] = shown.ThreadTS
			}
		}
	}

	parent := rtThreadTS
	if tc.thread {
		if len(tops) != 0 {
			t.Errorf("thread-bound agent got %d top-level post(s) in %s, want none", len(tops), tc.want)
		}
	} else {
		if len(tops) != 1 {
			t.Fatalf("want exactly one top-level post (the agent card) in %s, got %d (channels with posts: %v)",
				tc.want, len(tops), chans)
		}
		parent = tops[0]
	}
	for _, mark := range []string{rtDecisionMark, rtEscalationMark, rtSquawkMark} {
		ts, ok := found[mark]
		if !ok {
			t.Errorf("%s never reached %s (channels with posts: %v)", mark, tc.want, chans)
			continue
		}
		if ts != parent {
			t.Errorf("%s posted under thread %q in %s, want under %q", mark, ts, tc.want, parent)
		}
	}
}

func routedSnippet(m [3]string) string {
	s := m[1]
	if s == "" {
		s = m[2]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
