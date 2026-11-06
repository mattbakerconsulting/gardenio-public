package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedAgentBeadFields adds a minimal type=agent bead to the mock, keyed by agent
// name (so FindAgentBeadAnyStatus resolves it by the map fast-path), with an
// arbitrary starting fields map.
func seedAgentBeadFields(d *mockDaemon, agent string, fields map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fields == nil {
		fields = map[string]string{}
	}
	// Key the map by the bead ID (== agent name here) so both the
	// FindAgentBeadAnyStatus name fast-path AND the mock's UpdateBeadFields
	// (which looks up by detail.ID) resolve the same bead.
	d.beads[agent] = &beadsapi.BeadDetail{
		ID:     agent,
		Title:  agent,
		Type:   "agent",
		Status: "open",
		Fields: fields,
	}
}

// TestRecordTTFT_StampsBeadField verifies RecordTTFT writes ttft_ms onto the
// agent bead so it is queryable via the API.
func TestRecordTTFT_StampsBeadField(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBeadFields(daemon, "worker-1", nil)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.RecordTTFT(context.Background(), "worker-1", 1234)

	got := daemon.beads["worker-1"].Fields["ttft_ms"]
	if got != "1234" {
		t.Errorf("ttft_ms = %q, want %q", got, "1234")
	}
}

// TestRecordTTFT_Idempotent verifies an already-stamped ttft_ms is never
// overwritten — the first (real boot) reading wins across a restart/respawn.
func TestRecordTTFT_Idempotent(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBeadFields(daemon, "worker-1", map[string]string{"ttft_ms": "500"})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.RecordTTFT(context.Background(), "worker-1", 9999)

	if got := daemon.beads["worker-1"].Fields["ttft_ms"]; got != "500" {
		t.Errorf("ttft_ms = %q, want unchanged 500", got)
	}
}

// TestRecordTTFT_NonPositiveNoOp verifies a non-positive latency never stamps a
// bogus field.
func TestRecordTTFT_NonPositiveNoOp(t *testing.T) {
	daemon := newMockDaemon()
	seedAgentBeadFields(daemon, "worker-1", nil)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.RecordTTFT(context.Background(), "worker-1", 0)
	bot.RecordTTFT(context.Background(), "worker-1", -5)

	if _, ok := daemon.beads["worker-1"].Fields["ttft_ms"]; ok {
		t.Errorf("non-positive ttft must not stamp the field, got %v", daemon.beads["worker-1"].Fields)
	}
}

// TestRecordTTFT_MissingBeadNoPanic verifies a missing agent bead is a safe
// no-op (agent closed/despawned before the recorder ran).
func TestRecordTTFT_MissingBeadNoPanic(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	// Must not panic; nothing to assert beyond no crash / no stamp.
	bot.RecordTTFT(context.Background(), "ghost", 1000)
}

// TestSetStreamManagerWiresTTFTRecorder verifies SetStreamManager wires the bot
// as the TTFT recorder so the boot→first-token path can persist ttft_ms.
func TestSetStreamManagerWiresTTFTRecorder(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	bot.SetStreamManager(mgr)
	if mgr.ttftRecorder == nil {
		t.Fatal("SetStreamManager should wire the bot as the TTFT recorder")
	}
}

// TestTTFTChipRendersOnFullAgentCard is the end-to-end display assertion: a bead
// carrying ttft_ms flows through the usage rollup into the full agent card's
// context line, so the operator sees the "🐇 TTFT" chip on the card.
func TestTTFTChipRendersOnFullAgentCard(t *testing.T) {
	fields := map[string]string{
		"context_tokens": "100000",
		"ttft_ms":        "1800",
		"agent_state":    "working",
	}
	// buildAgentRollup(live=true) is what flushAgentCard feeds the full card's
	// contextLine (rollup.Context). Assert the chip is present there.
	rollup := buildAgentRollup(Garden, fields, true)
	blocks := buildAgentCardBlocks(Garden, "crew-gardenio-x", "", 0, "working", "", time.Time{},
		"", "", "crew", rollup.Cost, rollup.Context, nil, "", false, triggerCardInfo{}, nil)
	text := blocksToText(blocks)
	if !strings.Contains(text, "🐇 TTFT 1.8s") {
		t.Errorf("full agent card missing TTFT chip, rendered: %q", text)
	}
}
