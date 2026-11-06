package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

var updateEscalationCardsGolden = flag.Bool("update-escalation-cards", false,
	"rewrite testdata/escalation_cards_calls.golden.json from the escalation card and operator DM handlers' recorded calls")

// newEscalationCardsBot is newDecisionCardsBot plus crew-r, whose cards go to
// C0REJECT, where the twin refuses blocks, and the live operator alias feed,
// so an escalation addressed to a person is DMed to them.
func newEscalationCardsBot(t *testing.T, twin *surfacetest.SlackTwin) (*Bot, *mockDaemon) {
	t.Helper()
	b, daemon := newDecisionCardsBot(t, twin)
	daemon.slackAliases = liveAliasFeed()
	daemon.beads["crew-r"] = &beadsapi.BeadDetail{ID: "gd-agent-r", Type: "agent", Status: "open", Title: "crew-r", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-r", "project": "gardenio", "role": "crew", "agent_state": "working"}}
	daemon.beads["gd-agent-r"] = daemon.beads["crew-r"]
	b.agentProject["crew-r"] = "gardenio"
	b.agentRole["crew-r"] = "crew"
	b.agentSpawnChannel["crew-r"] = "C0REJECT"
	twin.RejectBlocks("C0REJECT")
	// #crew has a Commons channel bead bound to C0CREW, so a secret
	// escalation's card has a Commons copy to write.
	daemon.beads["gd-ch-crew"] = &beadsapi.BeadDetail{ID: "gd-ch-crew", Type: "channel", Status: "open", Title: "Crew", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "crew", "channel_kind": "crew", "slack_channel_id": "C0CREW", "mirror": "both"}}
	return b, daemon
}

// escalationCardEvent is an escalation of the gardenio land filed by agent and
// addressed to operatedBy ("" for none).
func escalationCardEvent(daemon *mockDaemon, id, agent, operatedBy string, priority int, extra map[string]string) BeadEvent {
	f := map[string]string{
		"operation_type":          "gitlab_mr_merge",
		"execution_target":        "local",
		"requesting_agent":        agent,
		"requesting_task":         "gd-task-1",
		"context":                 "PR #42 is green and reviewed; merge it so the chart fix ships.",
		"credential_requirements": `[{"type":"gitlab_token","identifier":"merge"}]`,
	}
	for k, v := range extra {
		f[k] = v
	}
	ev := BeadEvent{ID: id, Type: "escalation", Title: "Merge PR #42", Status: "open", Priority: priority,
		Labels: []string{"project:gardenio"}, CreatedBy: agent, Fields: f}
	d := &beadsapi.BeadDetail{ID: id, Type: "escalation", Status: "open", Title: ev.Title, Priority: priority,
		Labels: ev.Labels, CreatedBy: agent, OperatedBy: operatedBy, Fields: f}
	if operatedBy != "" {
		d.OperatedByID = i64(370)
	}
	daemon.beads[id] = d
	return ev
}

// escalationCardsScenario drives every escalation card and operator DM handler
// through b once: a card in the agent's channel and its operator DM, one in
// the agent's Slack thread, a sensitive one, one whose blocks Slack refuses
// (the plain-text retry), then the approved edit and the result of the first,
// a pingback of the second, and the rejection of the sensitive one.
func escalationCardsScenario(t *testing.T, b *Bot, daemon *mockDaemon) {
	t.Helper()
	ctx := context.Background()
	dm := b.EscalationOperatorDM(daemon, b.logger)

	evX := escalationCardEvent(daemon, "gd-esc-x", "crew-x", "oidc:alex.novak@contractor-b.example", 1, nil)
	evT := escalationCardEvent(daemon, "gd-esc-t", "crew-t", "", 0, map[string]string{"execution_script": "git merge"})
	evS := escalationCardEvent(daemon, "gd-esc-s", "crew-x", "oidc:alex.novak@contractor-b.example", 1, map[string]string{"sensitivity": "secret"})
	evR := escalationCardEvent(daemon, "gd-esc-r", "crew-r", "", 2, nil)
	for _, ev := range []BeadEvent{evX, evT, evS, evR} {
		if err := b.NotifyNewEscalation(ctx, ev); err != nil {
			t.Fatalf("NotifyNewEscalation %s: %v", ev.ID, err)
		}
		dm.EscalationCreated(ctx, ev)
	}

	evX.Fields["approved_by"], evX.Fields["approved_by_email"], evX.Fields["approval_method"] = "gd-user-1", "matt@example.com", "web_ui"
	if err := b.UpdateEscalationApproved(ctx, evX); err != nil {
		t.Fatal(err)
	}
	evX.Status, evX.Fields["result_status"], evX.Fields["executor"] = "closed", "success", "gardenio-merge-desk"
	if err := b.NotifyEscalationResult(ctx, evX); err != nil {
		t.Fatal(err)
	}
	// No notified_at: the pingback would print its age relative to now.
	evT.Fields["pingback_count"], evT.Fields["pingback_source"], evT.Fields["pingback_reason"] = "2", "reflag", "still blocks the release"
	evT.Fields["notified_at"] = ""
	if err := b.NotifyEscalationPingback(ctx, evT); err != nil {
		t.Fatal(err)
	}
	if err := b.DismissEscalation(ctx, "gd-esc-s"); err != nil {
		t.Fatal(err)
	}
}

// TestEscalationCardCallsGolden pins the Slack Web API calls of the slice-3
// escalation handlers (design spec 2026-10-19-lands-commons-design.md, section
// 5.2): the escalation card in a channel and in an agent's thread, a secret
// one, the plain-text retry when Slack refuses the blocks, the operator DMs,
// the approved and result edits, the pingback and the rejection. The golden was
// recorded from the direct-call code before those handlers moved behind the
// conversation Router; the routed code must send identical calls (no
// Slack-visible change).
// Regenerate with: go test ./internal/bridge -run TestEscalationCardCallsGolden -update-escalation-cards
func TestEscalationCardCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newEscalationCardsBot(t, twin)
	escalationCardsScenario(t, b, daemon)

	got, err := json.MarshalIndent(twin.Calls(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "escalation_cards_calls.golden.json")
	if *updateEscalationCardsGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("ESCALATION_CARDS_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-escalation-cards)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("escalation card Slack calls drifted from %s (rerun with -update-escalation-cards only if the change is intended):\n%s", path, got)
	}
}

// TestEscalationCardBeads checks the dual-write behind the golden: every
// escalation card is a card bead of its escalation in the gardenio land with
// the Slack copy's ref, the operator DM is a second card, and a secret
// escalation's card is in the Commons without a Slack ref and its DM is not
// recorded.
func TestEscalationCardBeads(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newEscalationCardsBot(t, twin)
	escalationCardsScenario(t, b, daemon)

	type card struct{ channelID, slack, ts, thread string }
	cards := map[string][]card{}
	for _, bd := range daemon.beads {
		if bd.Type != "message" || bd.Fields["kind"] != "card" {
			continue
		}
		if got := projectFromLabels(bd.Labels); got != "gardenio" {
			t.Errorf("card %s has land %q, want gardenio", bd.ID, got)
		}
		f := bd.Fields
		cards[f["subject_id"]] = append(cards[f["subject_id"]], card{f["channel_id"], f["slack_channel"], f["slack_ts"], f["thread_id"]})
	}
	byChannel := func(subject string) map[string]card {
		out := map[string]card{}
		for _, c := range cards[subject] {
			out[c.slack] = c
		}
		return out
	}

	x := byChannel("gd-esc-x")
	if len(cards["gd-esc-x"]) != 2 || x["C0CREW"].ts == "" || x["C0CREW"].channelID != "gd-ch-crew" || x["DU0PERSON003"].ts == "" {
		t.Errorf("gd-esc-x cards = %+v, want the #crew card and the operator DM, both with a Slack ref", cards["gd-esc-x"])
	}
	if got := cards["gd-esc-t"]; len(got) != 1 || got[0].slack != "C0THREAD" || got[0].ts == "" || got[0].thread != "gd-head-t" {
		t.Errorf("gd-esc-t cards = %+v, want one in C0THREAD under the wish head gd-head-t", got)
	}
	if got := cards["gd-esc-s"]; len(got) != 1 || got[0].channelID != "gd-ch-crew" || got[0].slack != "" || got[0].ts != "" {
		t.Errorf("gd-esc-s cards = %+v, want one Commons card in #crew with no Slack ref and no DM card", got)
	}
	// Slack refused gd-esc-r's blocks: its card bead records the text copy.
	if got := cards["gd-esc-r"]; len(got) != 1 || got[0].slack != "C0REJECT" || got[0].ts == "" {
		t.Errorf("gd-esc-r cards = %+v, want one recording the plain-text copy in C0REJECT", got)
	}
	if s := daemon.beads["gd-esc-s"].Fields; s["slack_channel"] != "" || s["operator_dm_channel"] != "" {
		t.Errorf("secret escalation got its Slack refs stamped: %v", s)
	}
}
