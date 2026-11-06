package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

func TestWrapUpOutcome(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		wrapup    string
		wantGlyph string
		wantLabel string
	}{
		{"clean done", "done", `{"accomplishments":"shipped"}`, Garden.Glyph(LifecycleDone), "done"},
		{"blocked", "done", `{"accomplishments":"x","blockers":"waiting on key"}`, Garden.Glyph(LifecycleBlocked), "blocked"},
		{"handed off", "done", `{"accomplishments":"x","handoff_notes":"finish step 3"}`, Garden.Glyph(LifecycleHandedOff), "handed-off"},
		{"blockers win over handoff", "done", `{"blockers":"b","handoff_notes":"h"}`, Garden.Glyph(LifecycleBlocked), "blocked"},
		{"failed aborts regardless of wrapup", "failed", `{"accomplishments":"x"}`, Garden.Glyph(LifecycleAborted), "aborted"},
		{"failed aborts even with blockers", "failed", `{"blockers":"b"}`, Garden.Glyph(LifecycleAborted), "aborted"},
		{"nil wrapup done", "done", ``, Garden.Glyph(LifecycleDone), "done"},
		// Filler sentinels in blockers/handoff must not drive the outcome.
		{"blockers none is not blocked", "done", `{"accomplishments":"x","blockers":"none"}`, Garden.Glyph(LifecycleDone), "done"},
		{"blockers none with handoff is handed-off", "done", `{"blockers":"none","handoff_notes":"finish step 3"}`, Garden.Glyph(LifecycleHandedOff), "handed-off"},
		{"blockers N/A is not blocked", "done", `{"blockers":"N/A"}`, Garden.Glyph(LifecycleDone), "done"},
		{"blockers dash is not blocked", "done", `{"blockers":"-"}`, Garden.Glyph(LifecycleDone), "done"},
		{"blockers none. with trailing period", "done", `{"blockers":"None."}`, Garden.Glyph(LifecycleDone), "done"},
		{"handoff none is clean done", "done", `{"accomplishments":"x","handoff_notes":"none"}`, Garden.Glyph(LifecycleDone), "done"},
		{"real blocker still blocks", "done", `{"blockers":"waiting on key"}`, Garden.Glyph(LifecycleBlocked), "blocked"},
		// Progressing: respawn-armed runs that will resume (bead ka-0000000754).
		{"goal not satisfied is progressing", "done", `{"accomplishments":"x","goal_satisfied":"no"}`, Garden.Glyph(LifecycleRestarted), "progressing"},
		{"goal satisfied yes is done", "done", `{"accomplishments":"x","goal_satisfied":"yes"}`, Garden.Glyph(LifecycleDone), "done"},
		{"respawn_after is progressing", "done", `{"accomplishments":"x","respawn_after":"10m"}`, Garden.Glyph(LifecycleRestarted), "progressing"},
		{"restart_when is progressing", "done", `{"accomplishments":"x","restart_when":"when gd-X merges"}`, Garden.Glyph(LifecycleRestarted), "progressing"},
		{"progressing outranks handed-off", "done", `{"handoff_notes":"finish step 3","goal_satisfied":"no"}`, Garden.Glyph(LifecycleRestarted), "progressing"},
		{"blocked outranks progressing", "done", `{"blockers":"waiting on key","goal_satisfied":"no"}`, Garden.Glyph(LifecycleBlocked), "blocked"},
		{"failed outranks progressing", "failed", `{"goal_satisfied":"no"}`, Garden.Glyph(LifecycleAborted), "aborted"},
		// goal_unachievable is terminal — it must suppress the progressing glyph
		// even when goal_satisfied:"no" or respawn_after would otherwise trigger it
		// (P3, ka-0000005843). The run declared its goal deterministically
		// impossible; the card must not promise a respawn that will never come.
		{"goal_unachievable is terminal done", "done", `{"accomplishments":"x","goal_unachievable":true}`, Garden.Glyph(LifecycleDone), "done"},
		{"goal_unachievable overrides goal_satisfied no", "done", `{"goal_satisfied":"no","goal_unachievable":true}`, Garden.Glyph(LifecycleDone), "done"},
		{"goal_unachievable overrides respawn_after", "done", `{"respawn_after":"10m","goal_unachievable":true}`, Garden.Glyph(LifecycleDone), "done"},
		{"goal_unachievable still blocks if blockers present", "done", `{"blockers":"waiting on key","goal_unachievable":true}`, Garden.Glyph(LifecycleBlocked), "blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := parseWrapUp(tt.wrapup)
			glyph, label := wrapUpOutcome(Garden, tt.state, w)
			if glyph != tt.wantGlyph || label != tt.wantLabel {
				t.Errorf("wrapUpOutcome(Garden, %q, %q) = (%q,%q), want (%q,%q)",
					tt.state, tt.wrapup, glyph, label, tt.wantGlyph, tt.wantLabel)
			}
		})
	}
}

func TestRestartConditionLine(t *testing.T) {
	tests := []struct {
		name        string
		wrapup      string
		fields      map[string]string
		wantEmpty   bool
		wantSubstrs []string
	}{
		{
			name:      "no respawn armed -> empty",
			wrapup:    `{"accomplishments":"x"}`,
			fields:    nil,
			wantEmpty: true,
		},
		{
			name:        "goal not satisfied, no fields -> generic resume",
			wrapup:      `{"goal_satisfied":"no"}`,
			fields:      nil,
			wantSubstrs: []string{"Restart", "Resumes"},
		},
		{
			name:        "respawn_after delay",
			wrapup:      `{"respawn_after":"10m"}`,
			fields:      nil,
			wantSubstrs: []string{"Resumes after 10m"},
		},
		{
			name:        "respawn_after immediate",
			wrapup:      `{"respawn_after":"immediate"}`,
			fields:      nil,
			wantSubstrs: []string{"Resumes immediately"},
		},
		{
			name:        "next_respawn_at field drives line even without wrapup signal",
			wrapup:      `{"accomplishments":"x"}`,
			fields:      map[string]string{"next_respawn_at": "2026-06-01T20:30:00Z"},
			wantSubstrs: []string{"Resumes ~20:30"},
		},
		{
			name:        "budget counters render N/M",
			wrapup:      `{"goal_satisfied":"no"}`,
			fields:      map[string]string{"respawn_count": "2", "max_respawns": "5"},
			wantSubstrs: []string{"(respawn 2/5)"},
		},
		{
			name:        "full: timestamp + budget",
			wrapup:      `{"goal_satisfied":"no"}`,
			fields:      map[string]string{"next_respawn_at": "2026-06-01T20:30:00Z", "respawn_count": "1", "max_respawns": "5"},
			wantSubstrs: []string{"Resumes ~20:30", "(respawn 1/5)"},
		},
		{
			name:      "goal satisfied yes, no fields -> empty",
			wrapup:    `{"goal_satisfied":"yes"}`,
			fields:    nil,
			wantEmpty: true,
		},
		// Wake-trigger arm (restart_when, bead ka-0000007398).
		{
			name:        "restart_when bare phrase gets Wakes lead-in",
			wrapup:      `{"restart_when":"when gd-X merges"}`,
			fields:      nil,
			wantSubstrs: []string{"Restart", "Wakes when gd-X merges"},
		},
		{
			name:        "restart_when already a Wakes clause is used verbatim",
			wrapup:      `{"restart_when":"Wakes when the operator resolves gd-Y"}`,
			fields:      nil,
			wantSubstrs: []string{"Wakes when the operator resolves gd-Y"},
		},
		{
			name:        "restart_when already a Waiting clause is used verbatim",
			wrapup:      `{"restart_when":"Waiting on decision gd-Z"}`,
			fields:      nil,
			wantSubstrs: []string{"Waiting on decision gd-Z"},
		},
		{
			name:        "respawn outranks wake-trigger when both present",
			wrapup:      `{"respawn_after":"10m","restart_when":"when gd-X merges"}`,
			fields:      nil,
			wantSubstrs: []string{"Resumes after 10m"},
		},
		// Gardener-refire arm (schedule_* fields).
		{
			name:        "gardener with runs left renders run N/M",
			wrapup:      `{"accomplishments":"x"}`,
			fields:      map[string]string{"schedule_runs_spawned": "3", "schedule_max_runs": "8"},
			wantSubstrs: []string{"Gardener refires", "(run 3/8)"},
		},
		{
			name:        "standing gardener (no cap) renders run N",
			wrapup:      `{"accomplishments":"x"}`,
			fields:      map[string]string{"schedule_runs_spawned": "1339"},
			wantSubstrs: []string{"Gardener refires", "(run 1339)"},
		},
		{
			name:      "gardener on its last run -> empty (no refire)",
			wrapup:    `{"accomplishments":"x"}`,
			fields:    map[string]string{"schedule_runs_spawned": "8", "schedule_max_runs": "8"},
			wantEmpty: true,
		},
		{
			name:      "non-gardener agent (no schedule fields) -> empty",
			wrapup:    `{"accomplishments":"x"}`,
			fields:    map[string]string{"role": "crew"},
			wantEmpty: true,
		},
		{
			name:        "respawn outranks gardener refire when both present",
			wrapup:      `{"goal_satisfied":"no"}`,
			fields:      map[string]string{"next_respawn_at": "2026-06-01T20:30:00Z", "schedule_runs_spawned": "3", "schedule_max_runs": "8"},
			wantSubstrs: []string{"Resumes ~20:30"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := parseWrapUp(tt.wrapup)
			got := restartConditionLine(Garden, w, tt.fields)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("restartConditionLine = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("restartConditionLine = empty, want non-empty")
			}
			for _, sub := range tt.wantSubstrs {
				if !strings.Contains(got, sub) {
					t.Errorf("restartConditionLine = %q, want substring %q", got, sub)
				}
			}
		})
	}
}

// TestBuildWrapUpAgentCardBlocks_GardenerRefireLine proves the gardener-refire
// restart line reaches the actual wrap-up card when the agent bead carries the
// schedule_* loop-context fields (bead ka-0000007398). A non-gardener wrap-up
// with the same shape renders no restart line.
func TestBuildWrapUpAgentCardBlocks_GardenerRefireLine(t *testing.T) {
	wrapupJSON := `{"accomplishments":"dispatched 2 crew"}`
	b := &Bot{}
	fields := map[string]string{
		"schedule_runs_spawned": "3",
		"schedule_max_runs":     "8",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "gardener-bot", "done", wrapupJSON, "", 0, fields)
	if !cardBlocksContain(blocks, "Gardener refires") || !cardBlocksContain(blocks, "(run 3/8)") {
		t.Errorf("expected gardener-refire restart line on card, blocks: %s", renderCardBlocks(blocks))
	}

	// No schedule fields → no restart line.
	plain := b.buildWrapUpAgentCardBlocks(Garden, "crew-bot", "done", wrapupJSON, "", 0, map[string]string{"role": "crew"})
	if cardBlocksContain(plain, "Restart:") {
		t.Errorf("did not expect a restart line for a non-gardener wrap-up, blocks: %s", renderCardBlocks(plain))
	}
}

// cardBlocksContain reports whether any section/context/header block's text
// contains substr. A small helper for the card-level restart-line assertions.
func cardBlocksContain(blocks []slack.Block, substr string) bool {
	return strings.Contains(renderCardBlocks(blocks), substr)
}

// renderCardBlocks flattens the text of every section/context/header block into
// one string for substring assertions + error messages.
func renderCardBlocks(blocks []slack.Block) string {
	var sb strings.Builder
	for _, blk := range blocks {
		switch b := blk.(type) {
		case *slack.SectionBlock:
			if b.Text != nil {
				sb.WriteString(b.Text.Text)
				sb.WriteString("\n")
			}
		case *slack.HeaderBlock:
			if b.Text != nil {
				sb.WriteString(b.Text.Text)
				sb.WriteString("\n")
			}
		case *slack.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok {
					sb.WriteString(t.Text)
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}

func TestParseWrapUp(t *testing.T) {
	if _, ok := parseWrapUp(""); ok {
		t.Error("empty string should not parse")
	}
	if _, ok := parseWrapUp("   "); ok {
		t.Error("whitespace should not parse")
	}
	if _, ok := parseWrapUp("not json"); ok {
		t.Error("invalid json should not parse")
	}
	w, ok := parseWrapUp(`{"accomplishments":"hi"}`)
	if !ok || w.Accomplishments != "hi" {
		t.Errorf("valid wrapup not parsed: ok=%v w=%+v", ok, w)
	}
}

// TestResolveAgentCardRef_FromHotCache verifies the in-memory cache is the
// first lookup path.
func TestResolveAgentCardRef_FromHotCache(t *testing.T) {
	b := &Bot{
		logger:     slog.Default(),
		agentCards: map[string]MessageRef{"a1": {ChannelID: "C1", Timestamp: "111.1"}},
	}
	ref, ok := b.resolveAgentCardRef(context.Background(), "a1")
	if !ok || ref.ChannelID != "C1" || ref.Timestamp != "111.1" {
		t.Fatalf("expected hot-cache ref, got ok=%v ref=%+v", ok, ref)
	}
}

// TestResolveAgentCardRef_FromBeadCardFields verifies fallback to the agent
// bead's slack_card_* fields when nothing is cached.
func TestResolveAgentCardRef_FromBeadCardFields(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["a2"] = &beadsapi.BeadDetail{
		ID:     "bead-a2",
		Type:   "agent",
		Fields: map[string]string{"slack_card_channel": "C2", "slack_card_ts": "222.2"},
	}
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	ref, ok := b.resolveAgentCardRef(context.Background(), "a2")
	if !ok || ref.ChannelID != "C2" || ref.Timestamp != "222.2" {
		t.Fatalf("expected card-field ref, got ok=%v ref=%+v", ok, ref)
	}
}

// TestResolveAgentCardRef_ThreadFallback verifies fallback to slack_thread_*
// when slack_card_* is absent.
func TestResolveAgentCardRef_ThreadFallback(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["a3"] = &beadsapi.BeadDetail{
		ID:     "bead-a3",
		Type:   "agent",
		Fields: map[string]string{"slack_thread_channel": "C3", "slack_thread_ts": "333.3"},
	}
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	ref, ok := b.resolveAgentCardRef(context.Background(), "a3")
	if !ok || ref.ChannelID != "C3" || ref.Timestamp != "333.3" {
		t.Fatalf("expected thread-field ref, got ok=%v ref=%+v", ok, ref)
	}
}

// TestResolveAgentCardRef_None verifies no ref resolves when neither cache nor
// bead fields carry a card location.
func TestResolveAgentCardRef_None(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["a4"] = &beadsapi.BeadDetail{ID: "bead-a4", Type: "agent", Fields: map[string]string{}}
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	if _, ok := b.resolveAgentCardRef(context.Background(), "a4"); ok {
		t.Fatal("expected no ref to resolve")
	}
}

// TestPersistCardRef writes the card location to the agent bead and is
// idempotent on a second identical write.
func TestPersistCardRef(t *testing.T) {
	daemon := newMockDaemon()
	bead := &beadsapi.BeadDetail{ID: "bead-a5", Type: "agent", Fields: map[string]string{}}
	// FindAgentBead resolves by agent name; UpdateBeadFields resolves by ID.
	// Register the same bead under both keys to mirror the real daemon.
	daemon.beads["a5"] = bead
	daemon.beads["bead-a5"] = bead
	b := &Bot{logger: slog.Default(), daemon: daemon}

	b.persistCardRef(context.Background(), "a5", "C5", "555.5")
	got := daemon.beads["a5"]
	if got.Fields["slack_card_channel"] != "C5" || got.Fields["slack_card_ts"] != "555.5" {
		t.Fatalf("card fields not persisted: %+v", got.Fields)
	}

	// Now that the fields match, resolution should round-trip.
	ref, ok := b.resolveAgentCardRef(context.Background(), "a5")
	if !ok || ref.ChannelID != "C5" || ref.Timestamp != "555.5" {
		t.Fatalf("round-trip resolve failed: ok=%v ref=%+v", ok, ref)
	}
}

// TestWrapUpSuccessor normalizes the succession intent to the {none,same,new}
// enum, folding empty/whitespace/case/garbage to "none".
func TestWrapUpSuccessor(t *testing.T) {
	tests := []struct {
		name   string
		wrapup string
		want   string
	}{
		{"nil wrapup", ``, successorNone},
		{"absent field", `{"accomplishments":"x"}`, successorNone},
		{"explicit none", `{"successor":"none"}`, successorNone},
		{"same", `{"successor":"same"}`, successorSame},
		{"new", `{"successor":"new"}`, successorNew},
		{"uppercase new normalized", `{"successor":"New"}`, successorNew},
		{"padded new normalized", `{"successor":"  new  "}`, successorNew},
		{"garbage folds to none", `{"successor":"maybe"}`, successorNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := parseWrapUp(tt.wrapup)
			if got := wrapUpSuccessor(w); got != tt.want {
				t.Errorf("wrapUpSuccessor(%q) = %q, want %q", tt.wrapup, got, tt.want)
			}
			wantNew := tt.want == successorNew
			if got := wrapUpSuccessorNew(w); got != wantNew {
				t.Errorf("wrapUpSuccessorNew(%q) = %v, want %v", tt.wrapup, got, wantNew)
			}
		})
	}
}

// TestSuccessorIDFromCloseReason parses the successor bead id back out of the
// predecessor's "superseded: continued as <id>" close_reason.
func TestSuccessorIDFromCloseReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"empty", "", ""},
		{"unrelated reason", "stop_gate_cooldown", ""},
		{"prefix only, no id", "superseded: continued as ", ""},
		{"canonical", "superseded: continued as gd-ABC123", "gd-ABC123"},
		{"case-insensitive prefix", "Superseded: Continued As gd-ABC123", "gd-ABC123"},
		{"trailing whitespace trimmed", "superseded: continued as gd-ABC123  ", "gd-ABC123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := successorIDFromCloseReason(tt.reason); got != tt.want {
				t.Errorf("successorIDFromCloseReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// TestWrapUpOutcome_SuccessorNew verifies successor=new maps to the continued glyph
// "continued" outcome, and that it sits at the right precedence.
func TestWrapUpOutcome_SuccessorNew(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		wrapup    string
		wantGlyph string
		wantLabel string
	}{
		{"successor new is continued", "done", `{"accomplishments":"x","successor":"new"}`, Garden.Glyph(LifecycleContinued), "continued"},
		{"successor new outranks handed-off", "done", `{"handoff_notes":"finish it","successor":"new"}`, Garden.Glyph(LifecycleContinued), "continued"},
		{"blockers outrank continued", "done", `{"blockers":"waiting","successor":"new"}`, Garden.Glyph(LifecycleBlocked), "blocked"},
		{"progressing outranks continued", "done", `{"goal_satisfied":"no","successor":"new"}`, Garden.Glyph(LifecycleRestarted), "progressing"},
		{"failed outranks continued", "failed", `{"successor":"new"}`, Garden.Glyph(LifecycleAborted), "aborted"},
		{"successor same is not continued", "done", `{"accomplishments":"x","successor":"same"}`, Garden.Glyph(LifecycleDone), "done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := parseWrapUp(tt.wrapup)
			glyph, label := wrapUpOutcome(Garden, tt.state, w)
			if glyph != tt.wantGlyph || label != tt.wantLabel {
				t.Errorf("wrapUpOutcome(Garden, %q,%q) = (%q,%q), want (%q,%q)",
					tt.state, tt.wrapup, glyph, label, tt.wantGlyph, tt.wantLabel)
			}
		})
	}
}

// TestSuccessorContinuationLine renders the forward "Continued as <id>" link
// only for successor=new, resolving the id from close_reason.
func TestSuccessorContinuationLine(t *testing.T) {
	b := &Bot{}
	tests := []struct {
		name        string
		wrapup      string
		closeReason string
		wantEmpty   bool
		wantSubstrs []string
	}{
		{
			name:      "no successor -> empty",
			wrapup:    `{"accomplishments":"x"}`,
			wantEmpty: true,
		},
		{
			name:      "successor same -> empty (inert until phase 2)",
			wrapup:    `{"successor":"same"}`,
			wantEmpty: true,
		},
		{
			name:        "successor new with id -> linked line",
			wrapup:      `{"successor":"new"}`,
			closeReason: "superseded: continued as gd-ABC123",
			wantSubstrs: []string{Garden.Glyph(LifecycleContinued), "Continued as", "gd-ABC123"},
		},
		{
			name:        "successor new without id -> linkless line",
			wrapup:      `{"successor":"new"}`,
			closeReason: "",
			wantSubstrs: []string{Garden.Glyph(LifecycleContinued), "Continued", "new card"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := parseWrapUp(tt.wrapup)
			got := b.successorContinuationLine(Garden, w, tt.closeReason)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("successorContinuationLine = %q, want empty", got)
				}
				return
			}
			for _, sub := range tt.wantSubstrs {
				if !strings.Contains(got, sub) {
					t.Errorf("successorContinuationLine = %q, want substring %q", got, sub)
				}
			}
		})
	}
}

// TestBuildWrapUpAgentCardBlocks_SuccessorNew is the card-integration test:
// a successor=new wrap-up renders the 🌿 continued header and a "Continued as
// <id>" section resolved from the bead's close_reason.
func TestBuildWrapUpAgentCardBlocks_SuccessorNew(t *testing.T) {
	b := &Bot{}
	// A wrap-up tldr promotes the headline to a Slack *header* block; the outcome
	// glyph/label live in the context line beneath it. Provide a tldr so the
	// continued outcome is observable in the header glyph.
	wrapupJSON := `{"tldr":"Passed the epic on","accomplishments":"Handed the epic to a fresh agent","successor":"new"}`
	fields := map[string]string{"close_reason": "superseded: continued as gd-NEW999"}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "gardenio/crew/old-bot", "done", wrapupJSON, "", 0, fields)

	// Header block carries the continued glyph.
	header := blocks[0].(*slack.HeaderBlock)
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleContinued)) {
		t.Errorf("expected continued glyph in header, got: %s", header.Text.Text)
	}
	// Context line beneath carries the "continued" outcome label.
	meta := blocks[1].(*slack.ContextBlock)
	metaText := meta.ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if !strings.Contains(metaText, "continued") {
		t.Errorf("expected 'continued' outcome in meta line, got: %s", metaText)
	}

	// One of the section blocks must carry the forward continuation link.
	var found bool
	for _, blk := range blocks {
		sec, ok := blk.(*slack.SectionBlock)
		if !ok || sec.Text == nil {
			continue
		}
		if strings.Contains(sec.Text.Text, "Continued as") && strings.Contains(sec.Text.Text, "gd-NEW999") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'Continued as gd-NEW999' section block, got blocks: %+v", blocks)
	}
}

// TestCloseReasonTLDR_Superseded verifies the successor-retirement close_reason
// renders a deliberate-handoff headline rather than an idle-out phrasing.
func TestCloseReasonTLDR_Superseded(t *testing.T) {
	got := closeReasonTLDR("superseded: continued as gd-NEW999")
	if !strings.Contains(strings.ToLower(got), "continued") {
		t.Errorf("closeReasonTLDR(superseded) = %q, want a 'continued' headline", got)
	}
}

// TestFetchAgentDuration computes created_at→now and truncates to seconds.
func TestFetchAgentDuration(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["a6"] = &beadsapi.BeadDetail{
		ID:        "bead-a6",
		Type:      "agent",
		CreatedAt: time.Now().Add(-90 * time.Second),
	}
	b := &Bot{logger: slog.Default(), daemon: daemon}
	d := b.fetchAgentDuration(context.Background(), "a6")
	if d < 89*time.Second || d > 92*time.Second {
		t.Errorf("expected ~90s duration, got %s", d)
	}

	// Zero CreatedAt → zero duration.
	daemon.beads["a7"] = &beadsapi.BeadDetail{ID: "bead-a7", Type: "agent"}
	if d := b.fetchAgentDuration(context.Background(), "a7"); d != 0 {
		t.Errorf("expected 0 duration for zero CreatedAt, got %s", d)
	}
}
