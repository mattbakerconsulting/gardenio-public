package main

import (
	"strings"
	"testing"
)

// mkMapsRow builds a gardenerRowView fixture for the pure buildMapsBoard tests.
// It carries just the fields the board reads; the rest stay zero.
func mkMapsRow(id, name string) gardenerRowView {
	return gardenerRowView{
		ID:      id,
		Name:    name,
		Title:   name,
		Href:    "/gardener/" + id,
		Status:  "active",
		Kind:    "trigger",
		Enabled: true,
	}
}

// TestBuildMapsBoard_MapsRowFields asserts the row→card field projection and
// that Href / initials / trigger-href are computed on the card.
func TestBuildMapsBoard_MapsRowFields(t *testing.T) {
	row := mkMapsRow("gd-cap1", "delivery gardener")
	row.TerminalGoal = "drain the ready queue"
	row.OwnerDisplay = "Matt Baker"
	row.RunsSpawned = 12
	row.RunsCompleted = 9
	row.Verdict = "making progress"
	row.VerdictKind = "progress"
	row.NextFire = "2026-06-01 13:00 UTC"
	row.NextFireRelative = "in 1h"

	board := buildMapsBoard([]gardenerRowView{row}, "/base")
	if len(board.Cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(board.Cards))
	}
	c := board.Cards[0]
	if c.ID != "gd-cap1" || c.Name != "delivery gardener" {
		t.Errorf("identity mismatch: %+v", c)
	}
	if c.Href != "/gardener/gd-cap1" {
		t.Errorf("Href = %q, want /gardener/gd-cap1", c.Href)
	}
	if c.TriggerHref != "/base/triggers" {
		t.Errorf("TriggerHref = %q, want /base/triggers", c.TriggerHref)
	}
	if c.Initials == "" {
		t.Errorf("Initials not computed")
	}
	if c.TerminalGoal != "drain the ready queue" || c.OwnerDisplay != "Matt Baker" {
		t.Errorf("goal/owner not copied: %+v", c)
	}
	if c.RunsSpawned != 12 || c.RunsCompleted != 9 {
		t.Errorf("run counters not copied: %+v", c)
	}
	if c.Verdict != "making progress" || c.VerdictKind != "progress" {
		t.Errorf("verdict not copied: %+v", c)
	}
	if board.Total != 1 {
		t.Errorf("Total = %d, want 1", board.Total)
	}
	if board.BasePath != "/base" {
		t.Errorf("BasePath = %q, want /base", board.BasePath)
	}
}

// TestBuildMapsBoard_OwnedEpic asserts the owned-epic rollup fields are copied
// onto the card (task ka-0000001266), and that a row with no owned epic carries
// no chip.
func TestBuildMapsBoard_OwnedEpic(t *testing.T) {
	withEpic := mkMapsRow("gd-cap1", "epic gardener")
	withEpic.HasOwnedEpic = true
	withEpic.OwnedEpicID = "gd-epicA"
	withEpic.OwnedEpicTotal = 40
	withEpic.OwnedEpicClosed = 12
	withEpic.OwnedEpicOpen = 28
	withEpic.OwnedEpicPct = 30

	without := mkMapsRow("gd-cap2", "no-epic gardener")

	board := buildMapsBoard([]gardenerRowView{withEpic, without}, "/base")
	c := board.Cards[0]
	if !c.HasOwnedEpic || c.OwnedEpicID != "gd-epicA" {
		t.Errorf("owned-epic id not copied: %+v", c)
	}
	if c.OwnedEpicTotal != 40 || c.OwnedEpicClosed != 12 || c.OwnedEpicOpen != 28 || c.OwnedEpicPct != 30 {
		t.Errorf("owned-epic rollup not copied: %+v", c)
	}
	if board.Cards[1].HasOwnedEpic {
		t.Errorf("no-epic card should not carry a rollup: %+v", board.Cards[1])
	}
}

// TestBuildMapsBoard_CostContext asserts the cost + context chip fields are
// copied verbatim onto the card (maps2 v2, ka-0000001668), and that a bare row
// carries neither chip.
func TestBuildMapsBoard_CostContext(t *testing.T) {
	rich := mkMapsRow("gd-cap1", "spendy gardener")
	rich.HasSpend = true
	rich.SpendLabel = "$12.34"
	rich.SpendStale = true
	rich.HasContext = true
	rich.ContextLabel = "128k"
	rich.ContextPct = 69
	rich.ContextTitle = "128,000 tokens (~69% of the ~184k auto-compact threshold on a 200k window)"
	rich.HasTurns = true
	rich.TurnsLabel = "47"

	bare := mkMapsRow("gd-cap2", "quiet gardener")

	board := buildMapsBoard([]gardenerRowView{rich, bare}, "/base")
	c := board.Cards[0]
	if !c.HasSpend || c.SpendLabel != "$12.34" || !c.SpendStale {
		t.Errorf("cost chip not copied: %+v", c)
	}
	if !c.HasContext || c.ContextLabel != "128k" || c.ContextPct != 69 ||
		c.ContextTitle != "128,000 tokens (~69% of the ~184k auto-compact threshold on a 200k window)" {
		t.Errorf("context chip not copied: %+v", c)
	}
	if !c.HasTurns || c.TurnsLabel != "47" {
		t.Errorf("turns chip not copied: %+v", c)
	}
	if b := board.Cards[1]; b.HasSpend || b.HasContext || b.HasTurns {
		t.Errorf("bare card should carry no cost/context/turns chip: %+v", b)
	}
}

// TestBuildMapsBoard_TriggerKind covers the trigger-node classification: the
// cron/bead_query/event sub-kind projected onto the row (ka-0000003781), the
// legacy schedule primitive, and the NextFire fallback for a pre-projection row.
func TestBuildMapsBoard_TriggerKind(t *testing.T) {
	cron := mkMapsRow("gd-cron", "cron cap")
	cron.TriggerSubKind = "cron"
	cron.NextFire = "2026-06-01 13:00 UTC"

	beadQuery := mkMapsRow("gd-bq", "bead-query cap")
	beadQuery.TriggerSubKind = "bead_query"

	event := mkMapsRow("gd-evt", "event cap")
	event.TriggerSubKind = "event"

	sched := mkMapsRow("gd-sch", "legacy schedule")
	sched.Kind = "schedule"

	// Pre-projection row (no sub-kind): a cron parse error still means a
	// cron-fired gardener via the NextFire-presence fallback.
	cronErr := mkMapsRow("gd-err", "bad cron")
	cronErr.NextFireError = "bad cron expr"

	// Pre-projection row with neither sub-kind nor cron → event fallback.
	bare := mkMapsRow("gd-bare", "bare cap")

	board := buildMapsBoard([]gardenerRowView{cron, beadQuery, event, sched, cronErr, bare}, "")
	gotKind := map[string]string{}
	gotLabel := map[string]string{}
	for _, c := range board.Cards {
		gotKind[c.ID] = c.TriggerKind
		gotLabel[c.ID] = c.TriggerKindLabel
	}
	wantKind := map[string]string{
		"gd-cron": "cron",
		"gd-bq":   "bead_query",
		"gd-evt":  "event",
		"gd-sch":  "schedule",
		"gd-err":  "cron",
		"gd-bare": "event",
	}
	for id, w := range wantKind {
		if gotKind[id] != w {
			t.Errorf("trigger kind for %s = %q, want %q", id, gotKind[id], w)
		}
	}
	// The CSS hook stays the raw kind; the rendered label humanizes bead_query.
	if gotLabel["gd-bq"] != "bead query" {
		t.Errorf("bead_query label = %q, want %q", gotLabel["gd-bq"], "bead query")
	}
	if gotLabel["gd-cron"] != "cron" {
		t.Errorf("cron label = %q, want %q", gotLabel["gd-cron"], "cron")
	}
}

// TestBuildMapsBoard_RecentRunsTailAndClasses asserts the strip is the last-N
// glyphs (oldest→newest) and that Outcome carries through for the pill class.
func TestBuildMapsBoard_RecentRunsTail(t *testing.T) {
	row := mkMapsRow("gd-runs", "runner")
	// 9 runs; the board keeps the last mapsRecentRunStrip (6), oldest→newest.
	for i := 1; i <= 9; i++ {
		outcome := "ok"
		if i%3 == 0 {
			outcome = "fail"
		}
		row.Runs = append(row.Runs, gardenerRunGlyph{RunNumber: i, Outcome: outcome, Height: i * 10})
	}

	board := buildMapsBoard([]gardenerRowView{row}, "")
	runs := board.Cards[0].Runs
	if len(runs) != mapsRecentRunStrip {
		t.Fatalf("strip len = %d, want %d", len(runs), mapsRecentRunStrip)
	}
	// Oldest kept run is RunNumber 4 (9 total, last 6), newest is 9.
	if runs[0].RunNumber != 4 {
		t.Errorf("oldest kept run = %d, want 4", runs[0].RunNumber)
	}
	if runs[len(runs)-1].RunNumber != 9 {
		t.Errorf("newest kept run = %d, want 9", runs[len(runs)-1].RunNumber)
	}
	// Run 9 (divisible by 3) is a fail; run 8 is ok — Outcome carries through.
	if runs[len(runs)-1].Outcome != "fail" {
		t.Errorf("run 9 outcome = %q, want fail", runs[len(runs)-1].Outcome)
	}
}

// TestBuildMapsBoard_EmptyRunsLegacyGardener: a gardener with no ProgressState
// (empty Runs) projects an empty strip — the card renders counters only.
func TestBuildMapsBoard_EmptyRunsLegacyGardener(t *testing.T) {
	row := mkMapsRow("gd-legacy", "legacy")
	row.RunsSpawned = 40
	row.RunsCompleted = 38
	row.RunsDerived = true

	board := buildMapsBoard([]gardenerRowView{row}, "")
	c := board.Cards[0]
	if len(c.Runs) != 0 {
		t.Errorf("expected empty run strip for legacy gardener, got %d", len(c.Runs))
	}
	if !c.RunsDerived {
		t.Errorf("RunsDerived not carried through")
	}
}

// TestBuildMapsBoard_SummaryCounts: NeedsAttention + Running summary tallies are
// computed over the rendered set.
func TestBuildMapsBoard_SummaryCounts(t *testing.T) {
	attn := mkMapsRow("gd-attn", "stuck cap")
	attn.NeedsAttention = true
	attn.VerdictKind = "stuck"

	running := mkMapsRow("gd-run", "running cap")
	running.IsRunning = true

	calm := mkMapsRow("gd-calm", "calm cap")

	board := buildMapsBoard([]gardenerRowView{attn, running, calm}, "")
	if board.Total != 3 {
		t.Errorf("Total = %d, want 3", board.Total)
	}
	if board.NeedsAttention != 1 {
		t.Errorf("NeedsAttention = %d, want 1", board.NeedsAttention)
	}
	if board.Running != 1 {
		t.Errorf("Running = %d, want 1", board.Running)
	}
}

// TestBuildMapsBoard_LiveAgentOverlay: the live-agent identity + liveness carry
// from the row to the card so the template can name and link the working agent
// (maps2 v2, ka-0000000595).
func TestBuildMapsBoard_LiveAgentOverlay(t *testing.T) {
	live := mkMapsRow("gd-live", "running cap")
	live.IsRunning = true
	live.LiveAgentName = "gardenio-delivery-drive-abc1"
	live.LiveAgentHref = "/gd-agent1"
	live.LiveAgentAlive = true

	// A running gardener whose last-agent shell is not alive (stale/closed):
	// the name links but the dot must stay off.
	stale := mkMapsRow("gd-stale", "stale cap")
	stale.IsRunning = true
	stale.LiveAgentName = "gardenio-wedged-xyz9"
	stale.LiveAgentHref = "/gd-agent2"
	stale.LiveAgentAlive = false

	board := buildMapsBoard([]gardenerRowView{live, stale}, "")
	l := board.Cards[0]
	if l.LiveAgentName != "gardenio-delivery-drive-abc1" || l.LiveAgentHref != "/gd-agent1" || !l.LiveAgentAlive {
		t.Errorf("live-agent fields not carried: %+v", l)
	}
	s := board.Cards[1]
	if s.LiveAgentName != "gardenio-wedged-xyz9" || s.LiveAgentAlive {
		t.Errorf("stale live-agent fields not carried: %+v", s)
	}
}

// TestBuildMapsBoard_AttentionAndDimming: attention signal + closed/disabled
// flags carry to the card so the template can ring / dim accordingly.
func TestBuildMapsBoard_AttentionAndDimming(t *testing.T) {
	attn := mkMapsRow("gd-attn", "stuck cap")
	attn.NeedsAttention = true
	attn.AttentionKind = "decision"
	attn.AttentionSignal = "pick a session strategy"
	attn.AttentionDecisionID = "gd-dec1"

	closed := mkMapsRow("gd-closed", "retired cap")
	closed.Status = "closed"
	closed.IsClosed = true
	closed.Enabled = false

	board := buildMapsBoard([]gardenerRowView{attn, closed}, "")
	a := board.Cards[0]
	if !a.NeedsAttention || a.AttentionSignal != "pick a session strategy" || a.AttentionDecisionID != "gd-dec1" {
		t.Errorf("attention fields not carried: %+v", a)
	}
	cl := board.Cards[1]
	if !cl.IsClosed || cl.Enabled {
		t.Errorf("closed/disabled flags not carried: %+v", cl)
	}
}

// TestMapsCardNameDivergesFromTitle mirrors the row reconciliation.
func TestMapsCardNameDivergesFromTitle(t *testing.T) {
	cases := []struct {
		name, title string
		want        bool
	}{
		{"delivery", "delivery", false},
		{"delivery", "Delivery Gardener bead", true},
		{"delivery", "", false},
		{"", "some title", false},
		{"same", "  same  ", false}, // whitespace-insensitive
	}
	for _, tc := range cases {
		c := mapsCardView{Name: tc.name, Title: tc.title}
		if got := c.NameDivergesFromTitle(); got != tc.want {
			t.Errorf("NameDivergesFromTitle(%q,%q) = %v, want %v", tc.name, tc.title, got, tc.want)
		}
	}
}

// TestMapsCardSearchText asserts the client-side search haystack folds the
// fields an operator types to find a gardener (name/title/goal/owner/id),
// lowercased and space-joined, skipping empty parts (maps2 v2, ka-0000006434).
func TestMapsCardSearchText(t *testing.T) {
	row := mkMapsRow("gd-cap1", "Delivery Gardener")
	row.Title = "delivery gardener bead"
	row.TerminalGoal = "Drain the READY queue"
	row.OwnerDisplay = "Matt Baker"
	row.OwnerSlug = "matthew.baker"

	got := buildMapsBoard([]gardenerRowView{row}, "").Cards[0].SearchText
	want := "delivery gardener delivery gardener bead drain the ready queue matt baker matthew.baker gd-cap1"
	if got != want {
		t.Errorf("SearchText = %q, want %q", got, want)
	}
	// Every typed-field token an operator might search should be present, lowercased.
	for _, tok := range []string{"delivery", "ready", "matt baker", "matthew.baker", "gd-cap1"} {
		if !strings.Contains(got, tok) {
			t.Errorf("SearchText %q missing token %q", got, tok)
		}
	}
}

// TestMapsCardSearchText_SkipsEmpty ensures absent fields leave no stray
// separators in the haystack.
func TestMapsCardSearchText_SkipsEmpty(t *testing.T) {
	row := gardenerRowView{ID: "gd-x", Name: "lonely"} // no title/goal/owner
	got := buildMapsBoard([]gardenerRowView{row}, "").Cards[0].SearchText
	if got != "lonely gd-x" {
		t.Errorf("SearchText = %q, want %q", got, "lonely gd-x")
	}
	if strings.Contains(got, "  ") {
		t.Errorf("SearchText has doubled separator: %q", got)
	}
}
