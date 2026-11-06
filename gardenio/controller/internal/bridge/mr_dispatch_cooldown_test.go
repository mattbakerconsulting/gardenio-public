package bridge

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestEvaluateDispatchCooldown(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const sha = "8f3fb5cedeadbeefcafefeedfacebeefdeadfeed"
	const other = "1111111111111111111111111111111111111111"

	t.Run("no current head SHA → dispatch (fail-open)", func(t *testing.T) {
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   "",
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-1 * time.Minute),
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true when head SHA unresolved; reason=%q", got.Reason)
		}
	})

	t.Run("no prior stamp → dispatch (first dispatch)", func(t *testing.T) {
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA: sha,
			Now:            base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true with no prior stamp; reason=%q", got.Reason)
		}
	})

	t.Run("prior stamp but zero timestamp → dispatch", func(t *testing.T) {
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: time.Time{},
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true when last dispatch time is zero; reason=%q", got.Reason)
		}
	})

	t.Run("same SHA, within window → SUPPRESS", func(t *testing.T) {
		// Dispatched 20m ago (one cycle), same head — nothing changed to re-review.
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-20 * time.Minute),
			Now:              base,
		})
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false (same SHA, 20m < 50m window); reason=%q", got.Reason)
		}
	})

	t.Run("same SHA, at window edge just inside → SUPPRESS", func(t *testing.T) {
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-DispatchCooldownWindow + time.Second),
			Now:              base,
		})
		if got.MayDispatch {
			t.Fatalf("expected MayDispatch=false just inside the window; reason=%q", got.Reason)
		}
	})

	t.Run("same SHA, window expired → dispatch (periodic re-look)", func(t *testing.T) {
		// Dispatched 60m ago (three cycles) — the cooldown has lapsed, so a stale
		// green MR gets a periodic re-look.
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-60 * time.Minute),
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (window expired); reason=%q", got.Reason)
		}
	})

	t.Run("same SHA, exactly at window boundary → dispatch", func(t *testing.T) {
		// elapsed == window is NOT < window, so it dispatches (boundary is exclusive).
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-DispatchCooldownWindow),
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true exactly at the window boundary; reason=%q", got.Reason)
		}
	})

	t.Run("head SHA moved within window → dispatch (new code)", func(t *testing.T) {
		// The whole point: a real push always re-crews immediately, even 1m after
		// the last dispatch.
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   other,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-1 * time.Minute),
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true when head SHA moved; reason=%q", got.Reason)
		}
	})

	t.Run("clock skew: stamp in the future → dispatch (never a stuck suppress)", func(t *testing.T) {
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(5 * time.Minute), // future
			Now:              base,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true on negative elapsed (clock skew); reason=%q", got.Reason)
		}
	})

	t.Run("custom window is honored", func(t *testing.T) {
		// A 10m window: a 15m-old same-SHA dispatch is outside it → dispatch.
		got := EvaluateDispatchCooldown(DispatchCooldownInput{
			CurrentHeadSHA:   sha,
			LastHeadSHA:      sha,
			LastDispatchedAt: base.Add(-15 * time.Minute),
			Now:              base,
			Window:           10 * time.Minute,
		})
		if !got.MayDispatch {
			t.Fatalf("expected MayDispatch=true (15m > 10m custom window); reason=%q", got.Reason)
		}
	})
}

// TestCooldownStampRoundTrip confirms FormatCooldownStamp → ParseCooldownStamp
// recovers the exact head SHA and (second-truncated) timestamp, and that the
// marker gates the parse.
func TestCooldownStampRoundTrip(t *testing.T) {
	const sha = "8f3fb5cedeadbeefcafefeedfacebeefdeadfeed"
	at := time.Date(2026, 8, 4, 12, 34, 56, 0, time.UTC)

	body := FormatCooldownStamp(sha, at)
	gotSHA, gotAt, ok := ParseCooldownStamp(body)
	if !ok {
		t.Fatalf("ParseCooldownStamp(%q) ok=false, want true", body)
	}
	if gotSHA != sha {
		t.Errorf("head SHA = %q, want %q", gotSHA, sha)
	}
	if !gotAt.Equal(at) {
		t.Errorf("dispatched_at = %v, want %v", gotAt, at)
	}
}

func TestParseCooldownStamp_Rejects(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"no marker", "gardener-delivery: review-merge crew: merged"},
		{"marker but no sha", GardenerDeliveryCooldownMarker + "\ndispatched_at=2026-08-04T12:00:00Z"},
		{"marker but no time", GardenerDeliveryCooldownMarker + "\nhead_sha=abc123"},
		{"marker but unparseable time", GardenerDeliveryCooldownMarker + "\nhead_sha=abc123 dispatched_at=not-a-time"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseCooldownStamp(tc.text); ok {
				t.Errorf("ParseCooldownStamp(%q) ok=true, want false", tc.text)
			}
		})
	}
}

// TestLatestCooldownStamp confirms the most-recent stamp wins across a mixed
// comment stream (foreign comments ignored, latest dispatched_at chosen).
func TestLatestCooldownStamp(t *testing.T) {
	const shaOld = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const shaNew = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tOld := time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC)
	tNew := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	comments := []beadsapi.Comment{
		{ID: 1, Text: "review-merge crew: held for operator"}, // foreign
		{ID: 2, Text: FormatCooldownStamp(shaOld, tOld)},
		{ID: 3, Text: "gitlab-bridge: pipeline succeeded"}, // foreign
		{ID: 4, Text: FormatCooldownStamp(shaNew, tNew)},
	}
	gotSHA, gotAt, ok := LatestCooldownStamp(comments)
	if !ok {
		t.Fatal("LatestCooldownStamp ok=false, want true")
	}
	if gotSHA != shaNew {
		t.Errorf("head SHA = %q, want the newest %q", gotSHA, shaNew)
	}
	if !gotAt.Equal(tNew) {
		t.Errorf("dispatched_at = %v, want newest %v", gotAt, tNew)
	}
}

func TestLatestCooldownStamp_None(t *testing.T) {
	comments := []beadsapi.Comment{
		{ID: 1, Text: "review-merge crew: merged"},
		{ID: 2, Text: "gitlab-bridge: coverage passing"},
	}
	if _, _, ok := LatestCooldownStamp(comments); ok {
		t.Error("LatestCooldownStamp ok=true on a stream with no stamp, want false")
	}
}

// TestLatestCooldownStamp_TieBreakByID confirms that when two stamps share a
// dispatched_at (same-second collision), the higher comment ID (later write) wins.
func TestLatestCooldownStamp_TieBreakByID(t *testing.T) {
	const shaA = "cccccccccccccccccccccccccccccccccccccccc"
	const shaB = "dddddddddddddddddddddddddddddddddddddddd"
	ts := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	comments := []beadsapi.Comment{
		{ID: 10, Text: FormatCooldownStamp(shaA, ts)},
		{ID: 11, Text: FormatCooldownStamp(shaB, ts)},
	}
	gotSHA, _, ok := LatestCooldownStamp(comments)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if gotSHA != shaB {
		t.Errorf("head SHA = %q, want the later-ID %q", gotSHA, shaB)
	}
}
