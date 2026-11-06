package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

func seedKillAgent(d *mockDaemon, name string, a *beadsapi.BeadDetail) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a.Title, a.Type, a.Status = name, "agent", "open"
	if a.ID == "" {
		a.ID = "bd-" + name
	}
	if a.Fields == nil {
		a.Fields = map[string]string{}
	}
	a.Fields["agent"] = name
	d.beads[name] = a
}

// asOpener is the ctx handleInteraction stamps for the agent owner the
// restart and kill fixtures seed (OperatedBy oidc:opener@example.com).
func asOpener() context.Context {
	return beadsapi.WithOperator(context.Background(), "oidc:opener@example.com")
}

// waitClosed polls for the async kill to close a bead.
func waitClosed(t *testing.T, d *mockDaemon) []closeCall {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := d.getClosed(); len(c) > 0 {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	return d.getClosed()
}

func TestPersonOperator(t *testing.T) {
	for in, want := range map[string]string{
		"oidc:alice@example.com":   "oidc:alice@example.com",
		" alice@example.com ":      "oidc:alice@example.com",
		"slack:U123":               "",
		"fleet:slack-bridge":       "",
		"trigger:gd-abc":           "",
		"gardenio-plan-gardener-x": "",
		"oidc:":                    "",
		"":                         "",
	} {
		if got := personOperator(in); got != want {
			t.Errorf("personOperator(%q) = %q, want %q", in, got, want)
		}
	}
}

// gd-jpzPamBtO3: the daemon closes any agent for the bridge's fleet token, so
// a Slack user who does not own the agent must be refused here, before the
// close, for both /kill and /kill-thread. An unresolved user owns nothing, an
// agent with no person behind it is owned by nobody, and the identity
// handleSlashCommand stamped (the linked account) wins over the directory.
func TestKillCommands_RefuseNonOwner(t *testing.T) {
	owned := &beadsapi.BeadDetail{OperatedBy: "oidc:alice@example.com"}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		resolved string
		agent    *beadsapi.BeadDetail
	}{
		{"other person", context.Background(), "oidc:mallory@example.com", owned},
		{"unresolved slack user", context.Background(), "slack:U999", owned},
		{"bridge sentinel", context.Background(), "fleet:slack-bridge", owned},
		{"linked account is not the owner", beadsapi.WithOperator(context.Background(), "oidc:mallory@example.com"), "oidc:alice@example.com", owned},
		{"agent spawned by an agent, no person", context.Background(), "oidc:alice@example.com",
			&beadsapi.BeadDetail{OperatedBy: "slack:U1", Fields: map[string]string{"spawned_by": "gardenio-plan-gardener-x"}}},
		{"slack sentinel matches slack sentinel", context.Background(), "slack:U1",
			&beadsapi.BeadDetail{OperatedBy: "slack:U1"}},
	} {
		for _, command := range []string{"/kill", "/kill-thread"} {
			t.Run(tc.name+" "+command, func(t *testing.T) {
				daemon := newMockDaemon()
				a := *tc.agent
				a.Fields = map[string]string{}
				for k, v := range tc.agent.Fields {
					a.Fields[k] = v
				}
				seedKillAgent(daemon, "worker", &a)
				var captured slackAPICalls
				srv := newCapturingSlackServer(t, &captured)
				defer srv.Close()
				b := newTestBot(daemon, srv)
				b.operatorResolver = func(context.Context, string) string { return tc.resolved }

				cmd := slack.SlashCommand{Command: command, Text: "worker --force --restart", ChannelID: "C1", UserID: "U999"}
				if command == "/kill" {
					cmd.Text = "worker --force"
					b.handleKillCommand(tc.ctx, cmd)
				} else {
					b.handleKillThreadCommand(tc.ctx, cmd)
				}
				time.Sleep(50 * time.Millisecond)

				if c := daemon.getClosed(); len(c) != 0 {
					t.Fatalf("a non-owner must kill nothing, got %v", c)
				}
				eph := captured.bodyForPath("/chat.postEphemeral")
				if !strings.Contains(eph, "Only+an+agent%27s+owner+can+kill+it") || !strings.Contains(eph, "gb+agent+kill+worker+--override-owner") {
					t.Fatalf("refusal must name the owner rule and the admin path: %s", eph)
				}
				if strings.Contains(eph, "Killing") {
					t.Fatalf("a refused caller must not be told the kill started: %s", eph)
				}
				if command == "/kill-thread" && !b.reopenDebouncer.Allow("worker") {
					t.Fatal("a refused /kill-thread --restart must not open the owner's restart window")
				}
			})
		}
	}
}

// The owner's kill runs on their ctx (context.WithoutCancel), so the close
// reaches the daemon as the person, not the bridge. A bare-email spawned_by
// names an owner too.
func TestKillCommands_OwnerKillsAsThemselves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent *beadsapi.BeadDetail
	}{
		{"operated_by", &beadsapi.BeadDetail{OperatedBy: "oidc:alice@example.com"}},
		{"operated_by_email", &beadsapi.BeadDetail{OperatedBy: "slack:U1", OperatedByEmail: "alice@example.com"}},
		{"bare-email spawned_by", &beadsapi.BeadDetail{Fields: map[string]string{"spawned_by": "Alice@Example.com"}}},
	} {
		for _, command := range []string{"/kill", "/kill-thread"} {
			t.Run(tc.name+" "+command, func(t *testing.T) {
				daemon := newMockDaemon()
				a := *tc.agent
				a.Fields = map[string]string{}
				for k, v := range tc.agent.Fields {
					a.Fields[k] = v
				}
				seedKillAgent(daemon, "worker", &a)
				b := newTestBot(daemon, newCapturingSlackServer(t, &slackAPICalls{}))
				ctx := beadsapi.WithOperator(context.Background(), "oidc:alice@example.com")

				cmd := slack.SlashCommand{Command: command, Text: "worker --force", ChannelID: "C1", UserID: "U1"}
				if command == "/kill" {
					b.handleKillCommand(ctx, cmd)
				} else {
					b.handleKillThreadCommand(ctx, cmd)
				}

				closed := waitClosed(t, daemon)
				if len(closed) != 1 || closed[0].BeadID != "bd-worker" {
					t.Fatalf("the owner's kill must close the agent bead, got %v", closed)
				}
				daemon.mu.Lock()
				closeCtx := daemon.lastCloseBeadCtx
				daemon.mu.Unlock()
				if op := beadsapi.OperatorFromContext(closeCtx); op != "oidc:alice@example.com" {
					t.Fatalf("the close must carry the Slack actor, got operator %q", op)
				}
			})
		}
	}
}

// A local agent is owned by its runner's owner (spec D6), not by whatever its
// own operated_by says; a runner that cannot be read owns nothing.
func TestKillCommand_LocalAgentOwnedByRunnerOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caller    string
		runtimeID string
		wantKill  bool
	}{
		{"runner owner", "oidc:alice@example.com", "gd-runner", true},
		{"agent's own operated_by is not the owner", "oidc:mallory@example.com", "gd-runner", false},
		{"runner not a runner bead", "oidc:alice@example.com", "gd-missing", false},
		{"no runtime_id", "oidc:alice@example.com", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedRunner(daemon, "gd-runner", "laptop", "online")
			seedKillAgent(daemon, "local-worker", &beadsapi.BeadDetail{
				OperatedBy: "oidc:mallory@example.com",
				Fields:     map[string]string{"execution_target": "laptop", "runtime_id": tc.runtimeID},
			})
			var captured slackAPICalls
			srv := newCapturingSlackServer(t, &captured)
			defer srv.Close()
			b := newTestBot(daemon, srv)

			b.handleKillCommand(beadsapi.WithOperator(context.Background(), tc.caller), slack.SlashCommand{
				Command: "/kill", Text: "local-worker --force", ChannelID: "C1", UserID: "U1",
			})

			if tc.wantKill {
				if c := waitClosed(t, daemon); len(c) != 1 {
					t.Fatalf("the runner owner must kill the local agent, got %v", c)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
			if c := daemon.getClosed(); len(c) != 0 {
				t.Fatalf("expected a refusal, got closes %v", c)
			}
			if eph := captured.bodyForPath("/chat.postEphemeral"); !strings.Contains(eph, "Only+an+agent%27s+owner") {
				t.Fatalf("refusal missing: %s", eph)
			}
		})
	}
}

// gd-tWLlFTwonY: agentOwners read the runner owner only for
// execution_target "local", but gb agent start --local stamps "laptop"
// (podmanager.ExecutionTargetLaptop), so a laptop agent fell to the cluster
// branch and was owned by its own operated_by. A laptop agent, by field or by
// label, is owned by its runner's owner alone; "local" is an escalation-bead
// value, not an agent target, and names no runner.
func TestAgentOwners_LaptopTargetReadsRunner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  beadsapi.BeadDetail
		owners []string
	}{
		{"laptop field", beadsapi.BeadDetail{
			OperatedBy: "oidc:mallory@example.com",
			Fields:     map[string]string{"execution_target": podmanager.ExecutionTargetLaptop, "runtime_id": "gd-runner"},
		}, []string{"oidc:alice@example.com"}},
		{"laptop label only", beadsapi.BeadDetail{
			OperatedBy: "oidc:mallory@example.com",
			Labels:     []string{"execution_target:" + podmanager.ExecutionTargetLaptop},
			Fields:     map[string]string{"runtime_id": "gd-runner"},
		}, []string{"oidc:alice@example.com"}},
		{"laptop with unreadable runner owns nothing", beadsapi.BeadDetail{
			OperatedBy: "oidc:mallory@example.com",
			Fields:     map[string]string{"execution_target": podmanager.ExecutionTargetLaptop, "runtime_id": "gd-missing"},
		}, nil},
		{"cluster agent", beadsapi.BeadDetail{
			OperatedBy: "oidc:bob@example.com",
			Fields:     map[string]string{"execution_target": podmanager.ExecutionTargetKubernetes, "runtime_id": "gd-runner", "spawned_by": "some-agent"},
		}, []string{"oidc:bob@example.com"}},
		{"legacy local is not a laptop target", beadsapi.BeadDetail{
			OperatedBy: "oidc:bob@example.com",
			Fields:     map[string]string{"execution_target": "local", "runtime_id": "gd-runner"},
		}, []string{"oidc:bob@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedRunner(daemon, "gd-runner", "laptop", "online")
			srv := newCapturingSlackServer(t, &slackAPICalls{})
			defer srv.Close()
			b := newTestBot(daemon, srv)
			a := tc.agent
			a.ID = "bd-worker"
			got := b.agentOwners(context.Background(), &a)
			if strings.Join(got, ",") != strings.Join(tc.owners, ",") {
				t.Fatalf("agentOwners = %v, want %v", got, tc.owners)
			}
		})
	}
}

// slackKillSurfaces are the Slack kills gated besides the slash commands
// (gd-rkXElhEgNt): the card's Kill and Restart buttons and @gardenio kill/stop.
// Each presses on agent "worker", bound to thread C1/1.2, as Slack user U1.
// done is the form-encoded text of the last Slack post the surface's
// background goroutine makes, after its last state.json write.
var slackKillSurfaces = []struct {
	name string
	run  func(b *Bot, ctx context.Context)
	done string
}{
	{"kill button", func(b *Bot, ctx context.Context) { b.handleKillThreadAgent(ctx, "worker", ownerTestCallback()) }, "terminated"},
	{"restart button", func(b *Bot, ctx context.Context) { b.handleRestartThreadAgent(ctx, "worker", ownerTestCallback()) },
		"restarted+with+session+resume"},
	{"@gardenio kill", func(b *Bot, ctx context.Context) {
		b.handleMentionKill(ctx, &slackevents.AppMentionEvent{Channel: "C1", ThreadTimeStamp: "1.2", User: "U1"}, false)
	}, "terminated"},
	{"@gardenio stop", func(b *Bot, ctx context.Context) {
		b.handleMentionThreadCommand(ctx, &slackevents.AppMentionEvent{Channel: "C1", ThreadTimeStamp: "1.2", User: "U1"}, "worker", "stop")
	}, "terminated"},
}

func ownerTestCallback() slack.InteractionCallback {
	cb := slack.InteractionCallback{User: slack.User{ID: "U1"}}
	cb.Channel.GroupConversation.Conversation.ID = "C1"
	cb.Container.ThreadTs = "1.2"
	return cb
}

func newOwnerTestBot(t *testing.T, daemon *mockDaemon, captured *slackAPICalls) *Bot {
	t.Helper()
	srv := newCapturingSlackServer(t, captured)
	t.Cleanup(srv.Close)
	b := newTestBot(daemon, srv)
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state
	_ = state.SetThreadAgent("C1", "1.2", "worker")
	return b
}

// gd-rkXElhEgNt: the buttons and @mentions reached the close on the bridge's
// fleet token with no owner check, so any Slack member could stop any agent.
// A non-owner is now refused before the kill: nothing closes, nobody is told
// the kill started, and a refused restart does not open the owner's window.
func TestSlackKillSurfaces_RefuseNonOwner(t *testing.T) {
	for _, s := range slackKillSurfaces {
		t.Run(s.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedKillAgent(daemon, "worker", &beadsapi.BeadDetail{OperatedBy: "oidc:alice@example.com"})
			var captured slackAPICalls
			b := newOwnerTestBot(t, daemon, &captured)

			s.run(b, beadsapi.WithOperator(context.Background(), "oidc:mallory@example.com"))
			time.Sleep(50 * time.Millisecond)

			if c := daemon.getClosed(); len(c) != 0 {
				t.Fatalf("a non-owner must kill nothing, got %v", c)
			}
			eph := captured.bodyForPath("/chat.postEphemeral")
			if !strings.Contains(eph, "Only+an+agent%27s+owner+can+kill+it") || !strings.Contains(eph, "thread_ts=1.2") {
				t.Fatalf("refusal must name the owner rule, in the thread: %s", eph)
			}
			for _, c := range captured.calls {
				if strings.Contains(c.Body, "Killing") || strings.Contains(c.Body, "Restarting") {
					t.Fatalf("a refused caller must not be told the kill started: %s %s", c.Path, c.Body)
				}
			}
			if !b.reopenDebouncer.Allow("worker") {
				t.Fatal("a refused restart must not open the owner's restart window")
			}
		})
	}
}

// A laptop agent's own operated_by does not own it on the buttons and
// @mentions either: only its runner's owner may stop it (gd-tWLlFTwonY).
func TestSlackKillSurfaces_LaptopAgentRefusesOperatedBy(t *testing.T) {
	for _, s := range slackKillSurfaces {
		t.Run(s.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedRunner(daemon, "gd-runner", "laptop", "online")
			seedKillAgent(daemon, "worker", &beadsapi.BeadDetail{
				OperatedBy: "oidc:mallory@example.com",
				Fields:     map[string]string{"execution_target": podmanager.ExecutionTargetLaptop, "runtime_id": "gd-runner"},
			})
			var captured slackAPICalls
			b := newOwnerTestBot(t, daemon, &captured)

			s.run(b, beadsapi.WithOperator(context.Background(), "oidc:mallory@example.com"))
			time.Sleep(50 * time.Millisecond)

			if c := daemon.getClosed(); len(c) != 0 {
				t.Fatalf("the laptop agent's operated_by must kill nothing, got %v", c)
			}
			if eph := captured.bodyForPath("/chat.postEphemeral"); !strings.Contains(eph, "is+owned+by+oidc%3Aalice%40example.com") {
				t.Fatalf("refusal must name the runner owner: %s", eph)
			}
		})
	}
}

// The owner's kill closes the bead that was checked and reaches the daemon as
// the person (context.WithoutCancel of the actor ctx), not context.Background.
func TestSlackKillSurfaces_OwnerKillsAsThemselves(t *testing.T) {
	for _, s := range slackKillSurfaces {
		t.Run(s.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedKillAgent(daemon, "worker", &beadsapi.BeadDetail{
				OperatedBy: "oidc:alice@example.com",
				Fields:     map[string]string{"slack_thread_channel": "C1", "slack_thread_ts": "1.2"},
			})
			var captured slackAPICalls
			b := newOwnerTestBot(t, daemon, &captured)
			ctx, cancel := context.WithCancel(beadsapi.WithOperator(context.Background(), "oidc:alice@example.com"))
			s.run(b, ctx)
			cancel() // the Slack request ends; the kill must outlive it
			// gd-H6k8XrwBLH: the goroutine writes state.json in t.TempDir() after
			// the close (and a restart re-mints and remaps after that), so join it
			// by its last post before the TempDir cleanup removes the dir.
			captured.waitForBody(t, s.done)

			closed := waitClosed(t, daemon)
			if len(closed) == 0 || closed[0].BeadID != "bd-worker" {
				t.Fatalf("the owner's kill must close the agent bead, got %v", closed)
			}
			daemon.mu.Lock()
			closeCtx := daemon.lastCloseBeadCtx
			daemon.mu.Unlock()
			if op := beadsapi.OperatorFromContext(closeCtx); op != "oidc:alice@example.com" {
				t.Fatalf("the close must carry the Slack actor, got operator %q", op)
			}
		})
	}
}
