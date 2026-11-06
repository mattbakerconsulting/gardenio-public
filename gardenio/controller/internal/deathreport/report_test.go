package deathreport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestBuild_RCAEndToEnd reconstructs the ka-0000002026 death end-to-end from a
// transcript: 4 stop-gate blocks with the RCA deltas, a cooldown-family
// close_reason, and the agent's parked "Holding for decision" last words. The
// assembled report must (1) classify the family as stop_gate_cooldown +
// involuntary, (2) quote the close_reason and last words, (3) pinpoint block #4
// as the cooldown-timing kill.
func TestBuild_RCAEndToEnd(t *testing.T) {
	lines := []string{
		assistantLine("m1", "2026-07-24T01:00:05Z", "I must not gb stop; the gate is keeping me alive."),
		userBlockLine("2026-07-24T01:00:05Z"), // block #1 t0
		assistantLine("m2", "2026-07-24T01:00:44Z", "Holding for decision gd-Bc."),
		userBlockLine("2026-07-24T01:00:45Z"), // block #2 +40s
		assistantLine("m3", "2026-07-24T01:03:41Z", "Holding for decision gd-Bc."),
		userBlockLine("2026-07-24T01:03:42Z"), // block #3 +177s
		assistantLine("m4", "2026-07-24T01:04:48Z", "Holding for decision gd-Bc."),
		userBlockLine("2026-07-24T01:04:49Z"), // block #4 +67s → IN COOLDOWN
	}
	transcript := strings.Join(lines, "\n")

	rep, err := Build(strings.NewReader(transcript), Options{
		AgentBeadID: "gd-4MIX5fM3w5",
		AgentName:   "thread-1784851806-182809",
		CloseReason: "stop_gate_cooldown_involuntary: claude exited mid-work without gb done; reopened gd-pkxih8brbw",
		LastN:       10,
	})
	if err != nil {
		t.Fatal(err)
	}

	// (1) classification
	if rep.Classification.Family != FamilyStopGateCooldown {
		t.Errorf("Family = %q, want %q", rep.Classification.Family, FamilyStopGateCooldown)
	}
	if !rep.Classification.Involuntary {
		t.Error("expected Involuntary = true")
	}

	// (2) last words captured
	if rep.Transcript == nil || !strings.Contains(rep.Transcript.LastWords, "Holding for decision") {
		t.Errorf("LastWords not captured: %+v", rep.Transcript)
	}

	// (3) cadence pinpoints block #4
	if rep.Cadence == nil {
		t.Fatal("Cadence is nil")
	}
	if rep.Cadence.KillBlock != 4 {
		t.Errorf("KillBlock = %d, want 4", rep.Cadence.KillBlock)
	}

	// Headline names the agent + cause.
	if !strings.Contains(rep.Headline, "thread-1784851806-182809") {
		t.Errorf("Headline missing agent name: %q", rep.Headline)
	}

	// Text render includes the quoted close_reason and last words.
	var buf bytes.Buffer
	rep.RenderText(&buf)
	out := buf.String()
	if !strings.Contains(out, "stop_gate_cooldown_involuntary") {
		t.Error("render missing quoted close_reason")
	}
	if !strings.Contains(out, "IN COOLDOWN") {
		t.Error("render missing cooldown anomaly marker")
	}

	// JSON round-trips.
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Cadence.KillBlock != 4 {
		t.Errorf("JSON round-trip KillBlock = %d, want 4", back.Cadence.KillBlock)
	}
}

// TestBuild_TranscriptURLRendered: the full-recording link (acceptance §3) is
// carried from Options through the Report and rendered in the text view, so an
// operator can pull the complete recording behind the last-N summary.
func TestBuild_TranscriptURLRendered(t *testing.T) {
	const url = "s3://gardenio-sessions-prod/coop/sessions/gd-abc/spawn-1/transcripts/3.jsonl"
	lines := []string{
		assistantLine("m1", "2026-07-24T01:00:00Z", "Doing work.", "Bash"),
	}
	rep, err := Build(strings.NewReader(strings.Join(lines, "\n")), Options{
		AgentName:     "some-agent",
		CloseReason:   "stop_gate_cooldown_involuntary: reopened gd-x",
		TranscriptURL: url,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TranscriptURL != url {
		t.Errorf("TranscriptURL = %q, want %q", rep.TranscriptURL, url)
	}

	var buf bytes.Buffer
	rep.RenderText(&buf)
	if !strings.Contains(buf.String(), url) {
		t.Errorf("render missing full-transcript link %q:\n%s", url, buf.String())
	}

	// Absent TranscriptURL: no "Full transcript:" line.
	rep2, err := Build(strings.NewReader(strings.Join(lines, "\n")), Options{CloseReason: "idle_ttl_expired"})
	if err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	rep2.RenderText(&buf2)
	if strings.Contains(buf2.String(), "Full transcript:") {
		t.Errorf("empty TranscriptURL should not render a link line:\n%s", buf2.String())
	}
}

// TestBuild_CloseReasonOnly: no transcript, classify from close_reason alone.
func TestBuild_CloseReasonOnly(t *testing.T) {
	rep, err := Build(nil, Options{
		AgentName:   "some-agent",
		CloseReason: "idle_ttl_expired",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Transcript != nil || rep.Cadence != nil {
		t.Error("close-reason-only report should have nil Transcript/Cadence")
	}
	if rep.Classification.Family != FamilyIdleOut {
		t.Errorf("Family = %q, want %q", rep.Classification.Family, FamilyIdleOut)
	}
	if rep.Classification.Involuntary {
		t.Error("idle-out should not be involuntary")
	}
}

// TestBuild_NoBlocksStatesNoTimingKill: a context-cancelled death has no
// stop-gate blocks; the cadence diff should positively state that.
func TestBuild_NoBlocksStatesNoTimingKill(t *testing.T) {
	lines := []string{
		assistantLine("m1", "2026-07-24T01:00:00Z", "Doing work.", "Bash"),
		assistantLine("m2", "2026-07-24T01:00:10Z", "More work.", "Edit"),
	}
	rep, err := Build(strings.NewReader(strings.Join(lines, "\n")), Options{
		CloseReason: "context_cancelled_involuntary: reopened gd-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cadence.KillBlock != 0 {
		t.Errorf("KillBlock = %d, want 0 (no blocks)", rep.Cadence.KillBlock)
	}
	if !strings.Contains(rep.Cadence.Anomaly, "not a stop-gate") {
		t.Errorf("Anomaly should state no timing kill, got %q", rep.Cadence.Anomaly)
	}
	if rep.Classification.Family != FamilyContextCancelled {
		t.Errorf("Family = %q, want %q", rep.Classification.Family, FamilyContextCancelled)
	}
}

// TestBuild_WipReapFieldsRendered: Tier-3 reap-scratch preservation
// (gd-PF42yiyH1d) surfaces wip_reap_ref / wip_reap_comment from the dead
// agent bead's Fields onto the Report and into the rendered text under
// "Uncommitted work preserved on reap". Absent fields must render nothing.
func TestBuild_WipReapFieldsRendered(t *testing.T) {
	rep, err := Build(nil, Options{
		AgentBeadID: "gd-agent1",
		CloseReason: "involuntary_termination: OOMKilled",
		Fields: map[string]string{
			"wip_reap_ref":     "refs/gardenio/wip/gd-agent1/work",
			"wip_reap_comment": "worktrees/gd-other",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WipReapRef != "refs/gardenio/wip/gd-agent1/work" {
		t.Errorf("WipReapRef = %q, want the scratch ref", rep.WipReapRef)
	}
	if rep.WipReapComment != "worktrees/gd-other" {
		t.Errorf("WipReapComment = %q, want the commented target", rep.WipReapComment)
	}

	var buf bytes.Buffer
	rep.RenderText(&buf)
	out := buf.String()
	if !strings.Contains(out, "Uncommitted work preserved on reap") {
		t.Error("render missing the reap-preservation section")
	}
	if !strings.Contains(out, "refs/gardenio/wip/gd-agent1/work") {
		t.Error("render missing the scratch ref")
	}
	if !strings.Contains(out, "worktrees/gd-other") {
		t.Error("render missing the commented target")
	}
}

// TestBuild_WipReapFieldsAbsentOmitsSection: a clean-tree death (or a report
// built with no bead Fields at all) must set neither Report field and must
// not render the "Uncommitted work preserved on reap" section.
func TestBuild_WipReapFieldsAbsentOmitsSection(t *testing.T) {
	rep, err := Build(nil, Options{
		AgentBeadID: "gd-agent1",
		CloseReason: "idle_ttl_expired",
		Fields:      map[string]string{"some_other_field": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WipReapRef != "" || rep.WipReapComment != "" {
		t.Errorf("expected both wip-reap fields empty, got ref=%q comment=%q", rep.WipReapRef, rep.WipReapComment)
	}

	var buf bytes.Buffer
	rep.RenderText(&buf)
	if strings.Contains(buf.String(), "Uncommitted work preserved on reap") {
		t.Error("clean-tree report must not render the reap-preservation section")
	}
}
