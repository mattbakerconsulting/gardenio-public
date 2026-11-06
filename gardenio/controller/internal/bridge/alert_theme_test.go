package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// businessIn themes only the named channels as Business and leaves every other
// channel Garden, mirroring a themeFor that resolves a different project per
// channel.
func businessIn(channels ...string) ThemeResolver {
	return func(_ context.Context, channel string) Theme {
		for _, c := range channels {
			if c == channel {
				return Business
			}
		}
		return Garden
	}
}

// wantText asserts every want substring is present and every legacy shortcode
// is gone.
func wantText(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	for _, legacy := range []string{":warning:", ":white_check_mark:", ":lock:", ":no_entry:"} {
		if strings.Contains(got, legacy) {
			t.Errorf("legacy shortcode %q survived in:\n%s", legacy, got)
		}
	}
}

func TestAlertTheme_NilResolverIsGarden(t *testing.T) {
	if got := themeOf(context.Background(), nil, "C1"); got.Name != Garden.Name {
		t.Errorf("themeOf(nil) = %q, want Garden", got.Name)
	}
	var b *Bot
	if b.ThemeResolver() != nil {
		t.Error("nil Bot must yield a nil resolver")
	}
}

func TestScifGateAlert_ThemedBothModes(t *testing.T) {
	a := newTestGateAlert(&fakeSlackAPI{})
	bead := &BeadEvent{ID: "gd-REQ1", Type: scifGateAlertBeadType, Fields: gate1PendingFields()}

	wantText(t, a.formatGate1Opening(Garden, bead), "🐞 *acme-scif Gate-1: an investigation-request is awaiting approval*")
	wantText(t, a.formatGate1Opening(Business, bead), "[REVIEW] *acme-scif Gate-1: an investigation-request is awaiting approval*")
	wantText(t, a.formatGate2(Garden, bead), "🐞 *acme-scif: a finished run is awaiting release*")
	wantText(t, a.formatGate2(Business, bead), "[REVIEW] *acme-scif: a finished run is awaiting release*")

	wantText(t, a.formatGate1Outcome(Garden, bead, scifApprovalApproved, false), "🌼 *Approved*")
	wantText(t, a.formatGate1Outcome(Business, bead, scifApprovalApproved, false), "[OK] *Approved*")
	wantText(t, a.formatGate1Outcome(Garden, bead, "denied", false), "🚧 *Denied*")
	wantText(t, a.formatGate1Outcome(Business, bead, "denied", false), "[DENIED] *Denied*")
}

func TestScifGateAlert_PostResolvesThemeForGateChannel(t *testing.T) {
	api := &capturingGate1Slack{}
	a := NewScifGateAlert(ScifGateAlertConfig{
		API:      api,
		Channel:  "C-gate",
		ThemeFor: businessIn("C-gate"),
		Logger:   slog.Default(),
	})
	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ1", scifGateAlertBeadType, nil, gate1PendingFields()))
	if len(api.texts) != 1 {
		t.Fatalf("posts = %d, want 1", len(api.texts))
	}
	wantText(t, api.texts[0], "[REVIEW] *acme-scif Gate-1")
}

func TestModelFallbackFlagger_ThemePerTargetChannel(t *testing.T) {
	api := &capturingGate1Slack{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{
		API:        api,
		OpsChannel: "C-ops",
		ThemeFor:   businessIn("C-thread"),
		Logger:     slog.Default(),
	})
	evt := &messageEvent{EventType: "usage", SessionID: "s", ExpectedModel: "claude-opus-5-oauth", Model: "claude-sonnet-4-6"}
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")
	if len(api.texts) != 2 {
		t.Fatalf("posts = %d, want 2 (thread + ops)", len(api.texts))
	}
	// The agent-thread channel is Business, the ops channel resolves Garden.
	wantText(t, api.texts[0], "[WARN] *Model fallback*")
	wantText(t, api.texts[1], "🌵 *Model fallback*")
}

func TestRefusalLogger_ThemePerTargetChannel(t *testing.T) {
	api := &capturingGate1Slack{}
	f := NewRefusalLogger(RefusalLogConfig{
		API:        api,
		OpsChannel: "C-ops",
		ThemeFor:   businessIn("C-thread"),
		Logger:     slog.Default(),
	})
	// A served != requested refusal also renders the silent-fallback bullet.
	evt := &messageEvent{EventType: "refusal", SessionID: "s", StopReason: "refusal", Model: "claude-opus-5", RequestedModel: "claude-sonnet-4-6", Text: "no"}
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")
	if len(api.texts) != 2 {
		t.Fatalf("posts = %d, want 2 (thread + ops)", len(api.texts))
	}
	wantText(t, api.texts[0], "[DENIED] *Model refusal*")
	wantText(t, api.texts[1], "🚧 *Model refusal*")
}

func TestRefusalLogger_FormatSilentFallbackBullet(t *testing.T) {
	f := NewRefusalLogger(RefusalLogConfig{API: &fakeSlackAPI{}, Logger: slog.Default()})
	evt := &messageEvent{Text: "no"}
	wantText(t, f.format(Garden, evt, "w", "", "a", "b", true), "• 🌵 served by a *silent fallback*")
	wantText(t, f.format(Business, evt, "w", "", "a", "b", true), "• [WARN] served by a *silent fallback*")
}
