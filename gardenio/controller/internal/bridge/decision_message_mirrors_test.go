package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mirrorSlackFixture is a fake Slack that records every chat.* call with the
// channel + ts it targeted, so a test can prove a mirror (not just the primary)
// was updated or deleted.
type mirrorSlackFixture struct {
	srv *httptest.Server

	mu      sync.Mutex
	updates []mirrorSlackCall
	deletes []mirrorSlackCall
}

type mirrorSlackCall struct {
	channel string
	ts      string
	text    string
	blocks  string
}

func newMirrorSlackFixture(t *testing.T) *mirrorSlackFixture {
	t.Helper()
	f := &mirrorSlackFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		call := mirrorSlackCall{
			channel: r.FormValue("channel"),
			ts:      r.FormValue("ts"),
			text:    r.FormValue("text"),
			blocks:  r.FormValue("blocks"),
		}
		f.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.update"):
			f.updates = append(f.updates, call)
		case strings.HasSuffix(r.URL.Path, "chat.delete"):
			f.deletes = append(f.deletes, call)
		}
		f.mu.Unlock()

		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": call.channel, "ts": call.ts, "message_ts": call.ts,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *mirrorSlackFixture) snapshot() ([]mirrorSlackCall, []mirrorSlackCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mirrorSlackCall(nil), f.updates...),
		append([]mirrorSlackCall(nil), f.deletes...)
}

// primaryRef registers a primary decision message directly (what NotifyDecision
// would have tracked) so the resolve/supersede paths have a primary to edit.
func primaryRef(bot *Bot, beadID string, ref MessageRef) {
	bot.mu.Lock()
	bot.messages[beadID] = ref
	bot.mu.Unlock()
}

// TestAddAndLookupMirror asserts a registered mirror is retrievable, de-duped,
// and additive to (never overwriting) the primary ref.
func TestAddAndLookupMirror(t *testing.T) {
	f := newMirrorSlackFixture(t)
	bot := newTestBot(newMockDaemon(), f.srv)

	primaryRef(bot, "dec-1", MessageRef{ChannelID: "D-DM", Timestamp: "1.0"})
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})
	// A duplicate must not double-register.
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})
	// A ref missing a channel or ts is ignored.
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "", Timestamp: "3.0"})

	mirrors := bot.lookupMessageMirrors("dec-1")
	if len(mirrors) != 1 {
		t.Fatalf("expected exactly 1 mirror after dedup, got %d: %+v", len(mirrors), mirrors)
	}
	if mirrors[0].ChannelID != "C-CHAN" {
		t.Errorf("mirror channel = %q, want C-CHAN", mirrors[0].ChannelID)
	}
	// The primary is untouched by mirror registration.
	if ref, ok := bot.lookupMessage("dec-1"); !ok || ref.ChannelID != "D-DM" {
		t.Errorf("primary ref should still be the DM, got %+v (ok=%v)", ref, ok)
	}
}

// TestMirrorUpdatedOnResolve is the reason C1 exists: a second copy of a
// decision must be rewritten to Resolved, not left advertising an open ask.
func TestMirrorUpdatedOnResolve(t *testing.T) {
	f := newMirrorSlackFixture(t)
	bot := newTestBot(newMockDaemon(), f.srv)

	primaryRef(bot, "dec-1", MessageRef{ChannelID: "D-DM", Timestamp: "1.0"})
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})

	if err := bot.UpdateDecision(context.Background(), "dec-1", "push", "ship it"); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	updates, _ := f.snapshot()
	var primaryUpdated, mirrorUpdated bool
	for _, u := range updates {
		if !strings.Contains(u.blocks, "Resolved") {
			continue
		}
		switch {
		case u.channel == "D-DM" && u.ts == "1.0":
			primaryUpdated = true
		case u.channel == "C-CHAN" && u.ts == "2.0":
			mirrorUpdated = true
		}
	}
	if !primaryUpdated {
		t.Errorf("primary copy was not updated to resolved; updates=%+v", updates)
	}
	if !mirrorUpdated {
		t.Errorf("MIRROR was not updated to resolved — it would advertise an open decision forever; updates=%+v", updates)
	}
}

// TestMirrorDeletedOnDismiss asserts a dismissed decision leaves no live-looking
// mirror behind and clears its tracking.
func TestMirrorDeletedOnDismiss(t *testing.T) {
	f := newMirrorSlackFixture(t)
	bot := newTestBot(newMockDaemon(), f.srv)

	primaryRef(bot, "dec-1", MessageRef{ChannelID: "D-DM", Timestamp: "1.0"})
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})

	if err := bot.DismissDecision(context.Background(), "dec-1"); err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}

	_, deletes := f.snapshot()
	var mirrorDeleted bool
	for _, d := range deletes {
		if d.channel == "C-CHAN" && d.ts == "2.0" {
			mirrorDeleted = true
		}
	}
	if !mirrorDeleted {
		t.Errorf("mirror was not deleted on dismiss; deletes=%+v", deletes)
	}
	if got := len(bot.lookupMessageMirrors("dec-1")); got != 0 {
		t.Errorf("mirror tracking should be cleared after dismiss, got %d", got)
	}
}

// TestMirrorDeletedWhenPrimaryAlreadyGone covers the path where the primary ref
// was already dropped: a dismiss must still reap orphaned mirrors.
func TestMirrorDeletedWhenPrimaryAlreadyGone(t *testing.T) {
	f := newMirrorSlackFixture(t)
	bot := newTestBot(newMockDaemon(), f.srv)

	// No primary registered — only a mirror.
	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})

	if err := bot.DismissDecision(context.Background(), "dec-1"); err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}

	_, deletes := f.snapshot()
	var mirrorDeleted bool
	for _, d := range deletes {
		if d.channel == "C-CHAN" && d.ts == "2.0" {
			mirrorDeleted = true
		}
	}
	if !mirrorDeleted {
		t.Errorf("orphaned mirror was not deleted when primary was already gone; deletes=%+v", deletes)
	}
	if got := len(bot.lookupMessageMirrors("dec-1")); got != 0 {
		t.Errorf("mirror tracking should be cleared, got %d", got)
	}
}

// TestMirrorUpdatedOnSuperseded asserts the supersede fanout rewrites the mirror
// to point at the successor decision.
func TestMirrorUpdatedOnSuperseded(t *testing.T) {
	f := newMirrorSlackFixture(t)
	bot := newTestBot(newMockDaemon(), f.srv)

	bot.addMessageMirror("dec-1", MessageRef{ChannelID: "C-CHAN", Timestamp: "2.0"})

	bot.markDecisionSuperseded(context.Background(), "dec-1", "dec-2", "D-DM", "1.0")

	updates, _ := f.snapshot()
	var mirrorSuperseded bool
	for _, u := range updates {
		if u.channel == "C-CHAN" && u.ts == "2.0" && strings.Contains(u.blocks, "Superseded") {
			mirrorSuperseded = true
		}
	}
	if !mirrorSuperseded {
		t.Errorf("mirror was not rewritten to Superseded; updates=%+v", updates)
	}
}

// TestMirrorPersistenceRoundTrip pins that mirrors survive a bridge restart:
// SetDecisionMessageMirrors → a fresh StateManager over the same file → Get.
func TestMirrorPersistenceRoundTrip(t *testing.T) {
	path := t.TempDir() + "/state.json"
	sm, err := NewStateManager(path)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	refs := []MessageRef{{ChannelID: "C-CHAN", Timestamp: "2.0"}}
	if err := sm.SetDecisionMessageMirrors("dec-1", refs); err != nil {
		t.Fatalf("SetDecisionMessageMirrors: %v", err)
	}

	sm2, err := NewStateManager(path)
	if err != nil {
		t.Fatalf("reopen StateManager: %v", err)
	}
	got := sm2.GetDecisionMessageMirrors("dec-1")
	if len(got) != 1 || got[0].ChannelID != "C-CHAN" || got[0].Timestamp != "2.0" {
		t.Fatalf("mirror did not survive restart, got %+v", got)
	}

	// An empty set removes the entry (and persists the removal).
	if err := sm2.SetDecisionMessageMirrors("dec-1", nil); err != nil {
		t.Fatalf("clear mirrors: %v", err)
	}
	sm3, err := NewStateManager(path)
	if err != nil {
		t.Fatalf("reopen StateManager (2): %v", err)
	}
	if got := sm3.GetDecisionMessageMirrors("dec-1"); len(got) != 0 {
		t.Errorf("cleared mirrors should not survive restart, got %+v", got)
	}
}
