package bridge

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

func TestParseRunnerInvite(t *testing.T) {
	uid, projects, err := parseRunnerInvite("invite <@U123|alice> pet-a pet-b")
	if err != nil || uid != "U123" || len(projects) != 2 || projects[1] != "pet-b" {
		t.Fatalf("got %q %v %v", uid, projects, err)
	}
	if uid, _, err := parseRunnerInvite("invite <@U123> pet-a"); err != nil || uid != "U123" {
		t.Fatalf("unlabelled mention: got %q %v", uid, err)
	}
	if _, _, err := parseRunnerInvite("invite <@U123>"); err == nil {
		t.Fatal("an invite without projects must be rejected (D8)")
	}
	if _, _, err := parseRunnerInvite("invite alice pet"); err == nil {
		t.Fatal("a bare name is not a Slack mention")
	}
}

func TestNewInviteCode_Shape(t *testing.T) {
	code, err := newInviteCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "GRI-") || len(code) != 13 || code[8] != '-' {
		t.Fatalf("code %q should look like GRI-XXXX-XXXX", code)
	}
	if strings.ContainsAny(code[4:], "01IO") {
		t.Fatalf("ambiguous characters must be excluded: %q", code)
	}
}

func TestRunnerInvite_CreatesInviteBeadWithHashOnly(t *testing.T) {
	daemon := newMockDaemon()
	b := newTestBot(daemon, newCapturingSlackServer(t, &slackAPICalls{}))
	b.operatorResolver = func(context.Context, string) string { return "oidc:alice@example.com" }
	code, id, err := b.createRunnerInvite(context.Background(), "U123", []string{"pet"})
	if err != nil {
		t.Fatal(err)
	}
	bead := daemon.beads[id]
	if bead == nil || bead.Type != "runner_invite" {
		t.Fatalf("expected runner_invite bead, got %+v", bead)
	}
	raw := string(daemon.lastCreateBeadRawFields)
	if strings.Contains(raw, code) {
		t.Fatal("the invite code must be stored only as a hash")
	}
	if !strings.Contains(raw, `"code_hash":"`+inviteCodeHash(code)+`"`) {
		t.Fatalf("code_hash missing: %s", raw)
	}
	if !strings.Contains(raw, `"invitee":"oidc:alice@example.com"`) {
		t.Fatalf("invitee missing: %s", raw)
	}
	if !strings.Contains(raw, `"projects":["pet"]`) {
		t.Fatalf("projects must be a JSON list: %s", raw)
	}
}

func TestRunnerInvite_RefusesUnresolvedInvitee(t *testing.T) {
	daemon := newMockDaemon()
	b := newTestBot(daemon, newCapturingSlackServer(t, &slackAPICalls{}))
	b.operatorResolver = func(_ context.Context, uid string) string { return "slack:" + uid }
	if _, _, err := b.createRunnerInvite(context.Background(), "U123", []string{"pet"}); err == nil {
		t.Fatal("a slack: sentinel must not become a runner owner")
	}
	if len(daemon.beads) != 0 {
		t.Fatalf("no invite bead may be written, got %d", len(daemon.beads))
	}
}

func TestRunnerCommand_InviteDMsCode(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(daemon, srv)
	b.runnerPublicURL = "https://runner.example"
	b.operatorResolver = func(context.Context, string) string { return "oidc:alice@example.com" }

	b.handleRunnerCommand(context.Background(), slack.SlashCommand{
		Command: "/runner", Text: "invite <@U123|alice> pet-a pet-b", ChannelID: "C1", UserID: "UADMIN",
	})

	dm := captured.bodyForPath("/chat.postMessage")
	if !strings.Contains(dm, "channel=U123") || !strings.Contains(dm, "GRI-") ||
		!strings.Contains(dm, "https%3A%2F%2Frunner.example") || !strings.Contains(dm, "pet-a%2C+pet-b") {
		t.Fatalf("DM to the invitee missing code/URL/projects: %s", dm)
	}
	if eph := captured.bodyForPath("/chat.postEphemeral"); !strings.Contains(eph, "Invite+sent") {
		t.Fatalf("inviter confirmation missing: %s", eph)
	}
}

func seedRunner(d *mockDaemon, id, hostname, state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads[id] = &beadsapi.BeadDetail{
		ID: id, Title: hostname, Type: "runner", Status: "open",
		Fields: map[string]string{
			"hostname": hostname, "owner": "oidc:alice@example.com",
			"state": state, "projects": `["pet"]`,
		},
	}
}

func TestRunnerCommand_Revoke(t *testing.T) {
	daemon := newMockDaemon()
	seedRunner(daemon, "gd-old", "laptop", runnerStateRevoked)
	seedRunner(daemon, "gd-live", "laptop", "online")
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(daemon, srv)
	b.operatorResolver = func(context.Context, string) string { return "oidc:Alice@example.com" }

	b.handleRunnerCommand(context.Background(), slack.SlashCommand{Command: "/runner", Text: "revoke laptop"})

	if len(daemon.revokedRunners) != 1 || daemon.revokedRunners[0] != "gd-live" {
		t.Fatalf("expected only the live runner revoked, got %v", daemon.revokedRunners)
	}
	if eph := captured.bodyForPath("/chat.postEphemeral"); !strings.Contains(eph, "Revoked+laptop+%28gd-live%29%3A+1+tokens%2C+0+agents+told+to+stop.") {
		t.Fatalf("unexpected reply: %s", eph)
	}
}

// gd-FebppYflAV: the daemon admits the bridge's fleet token for any runner,
// so a Slack user who is not the runner's owner must be refused here, before
// the call. An unresolved user (slack:/fleet: sentinel) owns nothing, and the
// identity handleSlashCommand stamped (the linked account) wins over the
// directory lookup.
func TestRunnerCommand_RevokeRefusesNonOwner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		resolved string
	}{
		{"other person", context.Background(), "oidc:mallory@example.com"},
		{"unresolved slack user", context.Background(), "slack:U999"},
		{"bridge sentinel", context.Background(), "fleet:slack-bridge"},
		{"linked account is not the owner", beadsapi.WithOperator(context.Background(), "oidc:mallory@example.com"), "oidc:alice@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedRunner(daemon, "gd-live", "laptop", "online")
			var captured slackAPICalls
			srv := newCapturingSlackServer(t, &captured)
			defer srv.Close()
			b := newTestBot(daemon, srv)
			b.operatorResolver = func(context.Context, string) string { return tc.resolved }

			b.handleRunnerCommand(tc.ctx, slack.SlashCommand{Command: "/runner", Text: "revoke laptop", UserID: "U999"})

			if len(daemon.revokedRunners) != 0 {
				t.Fatalf("a non-owner must revoke nothing, got %v", daemon.revokedRunners)
			}
			if eph := captured.bodyForPath("/chat.postEphemeral"); !strings.Contains(eph, "Only+the+owner") {
				t.Fatalf("refusal must say only the owner may revoke: %s", eph)
			}
		})
	}
}

func TestRunnerCommand_RevokeAmbiguous(t *testing.T) {
	daemon := newMockDaemon()
	seedRunner(daemon, "gd-a", "laptop", "online")
	seedRunner(daemon, "gd-b", "laptop", "offline")
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(daemon, srv)

	b.handleRunnerCommand(context.Background(), slack.SlashCommand{Command: "/runner", Text: "revoke laptop"})

	if len(daemon.revokedRunners) != 0 {
		t.Fatalf("an ambiguous hostname must revoke nothing, got %v", daemon.revokedRunners)
	}
	eph := captured.bodyForPath("/chat.postEphemeral")
	if !strings.Contains(eph, "gd-a") || !strings.Contains(eph, "gd-b") {
		t.Fatalf("ambiguity reply must list the ids: %s", eph)
	}
}

func TestRunnerCommand_Status(t *testing.T) {
	daemon := newMockDaemon()
	seedRunner(daemon, "gd-live", "laptop", "online")
	daemon.beads["gd-live"].Fields["last_seen_at"] = time.Now().UTC().Format(time.RFC3339)
	daemon.beads["gd-a1"] = laptopAgentBead("gd-a1", "local-laptop", "gd-live")
	seedRunner(daemon, "gd-gone", "desk", "online") // never sent a heartbeat
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(daemon, srv)

	b.handleRunnerCommand(context.Background(), slack.SlashCommand{Command: "/runner", Text: "status"})

	eph, _ := url.QueryUnescape(captured.bodyForPath("/chat.postEphemeral"))
	for _, want := range []string{"*laptop*", "online (last seen just now)", "local-laptop", "*desk*", "offline", "pet", "alice@example.com"} {
		if !strings.Contains(eph, want) {
			t.Errorf("runner cards missing %q: %s", want, eph)
		}
	}
}

func TestBuildRunnerCardBlocks(t *testing.T) {
	blocks := buildRunnerCardBlocks(Garden, RunnerInfo{ID: "gd-r1", Hostname: "laptop", Owner: "oidc:alice@example.com", State: "online", Tags: []string{"browser"}},
		time.Now().Add(-5*time.Minute), []string{"pet"}, []string{"local-laptop"})
	text := localCardText(blocks)
	for _, want := range []string{"🧑‍💻 *laptop*", "alice@example.com", "offline (last seen 5m ago)", "browser", "pet", "local-laptop", "/runner revoke laptop"} {
		if !strings.Contains(text, want) {
			t.Errorf("runner card missing %q: %s", want, text)
		}
	}

	text = localCardText(buildRunnerCardBlocks(Business, RunnerInfo{ID: "gd-r2", Hostname: "desk", State: "revoked"}, time.Time{}, nil, nil))
	for _, want := range []string{"[LOCAL] *desk*", "*Owner*\nunknown", "*State*\nrevoked", "*Tags*\nnone", "*Active agents*\nnone"} {
		if !strings.Contains(text, want) {
			t.Errorf("empty runner card missing %q: %s", want, text)
		}
	}
}

// TestBuildRunnerStatusBlocks_UnderSlackBlockLimit: Slack drops a message
// over 50 blocks without posting it, so cards, dividers and the overflow
// line together must fit for any number of runners past the cap.
func TestBuildRunnerStatusBlocks_UnderSlackBlockLimit(t *testing.T) {
	for _, n := range []int{runnerStatusMaxCards, runnerStatusMaxCards + 1, 40} {
		var runners []*beadsapi.BeadDetail
		for i := 0; i < n; i++ {
			runners = append(runners, &beadsapi.BeadDetail{ID: fmt.Sprintf("gd-r%d", i), Title: fmt.Sprintf("host-%d", i),
				Fields: map[string]string{"state": "online", "last_seen_at": time.Now().Format(time.RFC3339)}})
		}
		blocks := buildRunnerStatusBlocks(Garden, runners, nil)
		if len(blocks) > 50 {
			t.Errorf("%d runners: %d blocks, Slack allows 50", n, len(blocks))
		}
		text := localCardText(blocks)
		if more := n - runnerStatusMaxCards; more > 0 && !strings.Contains(text, fmt.Sprintf("...and %d more", more)) {
			t.Errorf("%d runners: overflow line missing: %s", n, text)
		}
	}
}

func TestRunnerCommand_IsGated(t *testing.T) {
	if !gatedSlashCommands["/runner"] {
		t.Fatal("/runner mutates server state and must be capability-gated")
	}
}
