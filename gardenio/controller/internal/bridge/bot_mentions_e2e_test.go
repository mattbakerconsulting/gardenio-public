package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// TestE2E_SlackMention_TrackingTaskHasDepToThreadBead drives the full
// app_mention → handleAppMention → CreateBead + getOrCreateThreadBead +
// AddDependency path and asserts that the spawned mention-tracking task
// carries a `triggered-by` dep to the canonical Slack-thread bead.
//
// Plan gd-T5vMf08jDN §2.1 (epic ka-0000000933 / M2d).
//
// Distinct from TestHandleAppMention_BoundThreadTracking_WritesTriggeredByDep
// (M2d unit test): this e2e variant lives in the dedicated *_e2e_test.go
// file alongside the gitlab/webhook/jira/formula e2e suites so the
// ingestion-path-coverage matrix is discoverable in one place.
func TestE2E_SlackMention_TrackingTaskHasDepToThreadBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agent-hq",
		},
	})

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.router = router
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	const (
		channel  = "C-agent-hq"
		threadTS = "1779.999"
	)

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U0OPERATOR1",
		Text:            "<@U-BOT> please fix the flaky deploy",
		TimeStamp:       "1779.9999",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	})

	// Locate the mention tracking bead and the canonical thread bead.
	var trackingID, threadID string
	threadLabel := threadBeadLabel(channel, threadTS)
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "hq" && hasLabel(bead.Labels, "slack-mention") {
			trackingID = bead.ID
		}
		if hasLabel(bead.Labels, threadLabel) {
			threadID = bead.ID
		}
	}
	if trackingID == "" {
		t.Fatal("expected mention tracking bead to be created")
	}
	if threadID == "" {
		t.Fatal("expected canonical thread bead to be created")
	}

	var sawTrackingDep bool
	for _, d := range daemon.getDeps() {
		if d.BeadID != trackingID {
			continue
		}
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q for tracking bead (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == threadID {
			sawTrackingDep = true
			if d.CreatedBy != "slack-bridge" {
				t.Errorf("triggered-by dep CreatedBy = %q, want slack-bridge", d.CreatedBy)
			}
		}
	}
	if !sawTrackingDep {
		t.Errorf("missing triggered-by dep from tracking task %s to thread bead %s (deps=%+v)",
			trackingID, threadID, daemon.getDeps())
	}
}
