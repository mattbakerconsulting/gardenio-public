package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

func TestScanBareBeadIDs(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"empty", "", nil},
		{"no id", "just some chatter about the release", nil},
		{"single bare id", "look at gd-abc123 please", []string{"gd-abc123"}},
		{
			"multiple distinct ids",
			"gd-aaa depends on gd-bbb and gd-ccc",
			[]string{"gd-aaa", "gd-bbb", "gd-ccc"},
		},
		{
			"dedupes repeats, first-seen order",
			"gd-bbb then gd-aaa then gd-bbb again",
			[]string{"gd-bbb", "gd-aaa"},
		},
		{
			"punctuation boundaries",
			"(gd-aaa), [gd-bbb]; gd-ccc.",
			[]string{"gd-aaa", "gd-bbb", "gd-ccc"},
		},
		{
			// A pasted viewer URL is handled by the Phase 1 URL unfurler, so
			// the id inside its path must NOT be re-scanned here.
			"id inside a bare url is skipped",
			"see https://prod.gardenio.acme.example/beads/gd-inurl for details",
			nil,
		},
		{
			"id inside a slack mrkdwn link span is skipped",
			"see <https://prod.gardenio.acme.example/beads/gd-inspan|the bead>",
			nil,
		},
		{
			// The bare id outside the URL is previewed; the one inside is not.
			"bare id kept, url id skipped",
			"gd-bare and https://prod.gardenio.acme.example/beads/gd-inurl",
			[]string{"gd-bare"},
		},
		{
			// URL first: the protected span is skipped before the bare id is
			// scanned, so span ordering does not matter.
			"url first then bare id",
			"https://prod.gardenio.acme.example/beads/gd-inurl then gd-bare",
			[]string{"gd-bare"},
		},
		{
			// Same id both inside a URL and bare: the URL occurrence is in a
			// protected span (never seen), so the bare occurrence still cards.
			"same id bare and in url -> bare wins",
			"https://prod.gardenio.acme.example/beads/gd-dup and bare gd-dup",
			[]string{"gd-dup"},
		},
		{
			"not matched mid-token",
			"prefixgd-aaa and agd-bbb",
			nil,
		},
		{
			// `_` is a word char, so `\bgd-[A-Za-z0-9]+\b` finds no trailing
			// boundary inside `gd-abc_def` and matches nothing — the same
			// false-positive-avoiding behavior the linkify rule already uses.
			"no match when trailing word char blocks the boundary",
			"gd-abc_def",
			nil,
		},
		{
			// A non-word char (here `.`) provides the trailing boundary.
			"matches up to a non-word boundary",
			"gd-abc.",
			[]string{"gd-abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanBareBeadIDs(tt.text)
			if !equalStrings(got, tt.want) {
				t.Errorf("scanBareBeadIDs(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestScanBareBeadIDsCap(t *testing.T) {
	var sb strings.Builder
	for i := range maxBarePreviewIDs + 5 {
		sb.WriteString("gd-id")
		sb.WriteByte(byte('a' + i))
		sb.WriteByte(' ')
	}
	got := scanBareBeadIDs(sb.String())
	if len(got) != maxBarePreviewIDs {
		t.Fatalf("len = %d, want cap %d", len(got), maxBarePreviewIDs)
	}
}

func TestBareIDPreviewThrottle(t *testing.T) {
	base := time.Unix(1_784_000_000, 0)
	th := newBareIDPreviewThrottle(10 * time.Minute)

	if !th.allow("C1", "gd-a", base) {
		t.Fatal("first post should be allowed")
	}
	if th.allow("C1", "gd-a", base.Add(time.Minute)) {
		t.Fatal("repeat within window should be throttled")
	}
	// Different bead in the same channel is independent.
	if !th.allow("C1", "gd-b", base.Add(time.Minute)) {
		t.Fatal("different bead should be allowed")
	}
	// Same bead in a different channel is independent.
	if !th.allow("C2", "gd-a", base.Add(time.Minute)) {
		t.Fatal("same bead in a different channel should be allowed")
	}
	// After the window elapses, the same (channel,bead) is allowed again.
	if !th.allow("C1", "gd-a", base.Add(11*time.Minute)) {
		t.Fatal("post after the window should be allowed again")
	}

	// A nil throttle allows everything.
	var nilTh *bareIDPreviewThrottle
	if !nilTh.allow("C1", "gd-a", base) {
		t.Fatal("nil throttle should allow")
	}
}

// newPreviewBot builds a Bot wired for bareBeadPreviewAttachments tests: a
// stub batch daemon, a configured viewer host, and a fresh throttle.
func newPreviewBot(enabled bool, beads map[string]*beadsapi.BeadDetail) *Bot {
	return &Bot{
		daemon:               &fakeBatchDaemon{beads: beads},
		beadsViewerURL:       "https://prod.gardenio.acme.example/beads",
		botUserID:            "UBOT",
		logger:               slog.Default(),
		bareIDPreviewEnabled: enabled,
		bareIDPreview:        newBareIDPreviewThrottle(bareIDPreviewWindow),
	}
}

func msg(text string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		Channel:     "C123",
		User:        "UHUMAN",
		TimeStamp:   "1784055212.286079",
		ChannelType: "channel",
		Text:        text,
	}
}

func TestBareBeadPreviewAttachments(t *testing.T) {
	openBeads := map[string]*beadsapi.BeadDetail{
		"gd-open":   {ID: "gd-open", Title: "Open task", Type: "task", Status: "open"},
		"gd-prog":   {ID: "gd-prog", Title: "Working", Type: "epic", Status: "in_progress"},
		"gd-closed": {ID: "gd-closed", Title: "Done", Type: "bug", Status: "closed"},
	}

	t.Run("toggle off -> no cards, no daemon fetch", func(t *testing.T) {
		b := newPreviewBot(false, openBeads)
		fake := b.daemon.(*fakeBatchDaemon)
		atts := b.bareBeadPreviewAttachments(context.Background(), msg("look at gd-open"))
		if atts != nil {
			t.Fatalf("atts = %v, want nil when toggle off", atts)
		}
		if fake.batchCalls != 0 {
			t.Fatalf("batchCalls = %d, want 0 when toggle off", fake.batchCalls)
		}
	})

	t.Run("open bead -> one card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		atts := b.bareBeadPreviewAttachments(context.Background(), msg("please see gd-open"))
		if len(atts) != 1 {
			t.Fatalf("len(atts) = %d, want 1", len(atts))
		}
		text := renderBlocksText(atts[0])
		if !strings.Contains(text, "Open task") {
			t.Errorf("card missing title: %s", text)
		}
		// Card title links to the viewer deep link (no pasted URL to link back to).
		if !strings.Contains(text, "https://prod.gardenio.acme.example/beads/gd-open|") {
			t.Errorf("card does not link to viewer deep link: %s", text)
		}
	})

	t.Run("unknown id -> no card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		atts := b.bareBeadPreviewAttachments(context.Background(), msg("what is gd-nope"))
		if atts != nil {
			t.Fatalf("atts = %v, want nil for unknown id", atts)
		}
	})

	t.Run("closed id -> no card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		atts := b.bareBeadPreviewAttachments(context.Background(), msg("ref gd-closed"))
		if atts != nil {
			t.Fatalf("atts = %v, want nil for closed id", atts)
		}
	})

	t.Run("mixed: open + closed + unknown -> only open cards", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		atts := b.bareBeadPreviewAttachments(context.Background(),
			msg("gd-open and gd-prog and gd-closed and gd-nope"))
		if len(atts) != 2 {
			t.Fatalf("len(atts) = %d, want 2 (open + in_progress; closed/unknown skipped)", len(atts))
		}
	})

	t.Run("id inside a pasted viewer url -> no card (Phase 1 owns it)", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		atts := b.bareBeadPreviewAttachments(context.Background(),
			msg("https://prod.gardenio.acme.example/beads/gd-open"))
		if atts != nil {
			t.Fatalf("atts = %v, want nil — url id must not be re-carded", atts)
		}
	})

	t.Run("bot's own message -> no card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		m := msg("gd-open")
		m.User = "UBOT"
		if atts := b.bareBeadPreviewAttachments(context.Background(), m); atts != nil {
			t.Fatalf("atts = %v, want nil for bot's own message", atts)
		}
	})

	t.Run("other app/bot message -> no card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		m := msg("gd-open")
		m.BotID = "B999"
		if atts := b.bareBeadPreviewAttachments(context.Background(), m); atts != nil {
			t.Fatalf("atts = %v, want nil for another bot's message", atts)
		}
	})

	t.Run("dm (im) message -> no card", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		m := msg("gd-open")
		m.ChannelType = "im"
		if atts := b.bareBeadPreviewAttachments(context.Background(), m); atts != nil {
			t.Fatalf("atts = %v, want nil for a DM", atts)
		}
	})

	t.Run("throttle suppresses a repeat in the same channel", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		first := b.bareBeadPreviewAttachments(context.Background(), msg("gd-open"))
		if len(first) != 1 {
			t.Fatalf("first: len = %d, want 1", len(first))
		}
		second := b.bareBeadPreviewAttachments(context.Background(), msg("gd-open again"))
		if second != nil {
			t.Fatalf("second: atts = %v, want nil (throttled)", second)
		}
	})

	t.Run("nil event / nil receiver are safe", func(t *testing.T) {
		b := newPreviewBot(true, openBeads)
		if atts := b.bareBeadPreviewAttachments(context.Background(), nil); atts != nil {
			t.Fatalf("atts = %v, want nil for nil event", atts)
		}
		var nilBot *Bot
		if atts := nilBot.bareBeadPreviewAttachments(context.Background(), msg("gd-open")); atts != nil {
			t.Fatalf("atts = %v, want nil for nil receiver", atts)
		}
	})
}

// TestMaybePreviewBareBeadIDsGate covers the async entry point: toggle-off is a
// no-op that never touches the daemon, and toggle-on runs the detached
// goroutine (joined via handlerWg) without panicking even when the Slack api is
// nil. The card-decision logic itself is asserted in
// TestBareBeadPreviewAttachments; this just guards the gate + goroutine plumbing.
func TestMaybePreviewBareBeadIDsGate(t *testing.T) {
	beads := map[string]*beadsapi.BeadDetail{
		"gd-open": {ID: "gd-open", Title: "Open", Type: "task", Status: "open"},
	}

	t.Run("toggle off: no goroutine, no daemon fetch", func(t *testing.T) {
		b := newPreviewBot(false, beads)
		fake := b.daemon.(*fakeBatchDaemon)
		b.maybePreviewBareBeadIDs(context.Background(), msg("gd-open"))
		b.handlerWg.Wait()
		if fake.batchCalls != 0 {
			t.Fatalf("batchCalls = %d, want 0 when toggle off", fake.batchCalls)
		}
	})

	t.Run("toggle on: goroutine fetches; nil api is a safe no-op", func(t *testing.T) {
		b := newPreviewBot(true, beads) // api is nil (no live *slack.Client)
		fake := b.daemon.(*fakeBatchDaemon)
		b.maybePreviewBareBeadIDs(context.Background(), msg("gd-open"))
		b.handlerWg.Wait() // join the detached goroutine
		if fake.batchCalls != 1 {
			t.Fatalf("batchCalls = %d, want 1 (goroutine ran the scan+fetch)", fake.batchCalls)
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var nilBot *Bot
		nilBot.maybePreviewBareBeadIDs(context.Background(), msg("gd-open"))
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
