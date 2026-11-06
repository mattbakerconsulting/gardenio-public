package glass

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func controlOf(t *testing.T, evs []Event) (Control, bool) {
	t.Helper()
	var c Control
	found := false
	for _, e := range evs {
		if e.Kind == EventControl {
			c = Control{}
			if err := json.Unmarshal(e.Data, &c); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	return c, found
}

// A claim is announced to every watcher, a watcher who joins later hears of it with its first events, and a
// claim by another person is refused unless they take it.
func TestControlClaimIsAnnouncedToEveryWatcher(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	a := r.Watch("s1", alice, -1)
	defer a.Close()
	u := mux.upstream(t, 0)
	u.replay("hi", 2)
	collect(t, a, func(e []Event) bool { return bytesOf(t, e) == "hi" })

	c, err := r.Claim("s1", "alice@example.com", false)
	if err != nil || c.Holder != "alice@example.com" || c.Reason != ControlClaimed || !c.Until.After(c.Since) {
		t.Fatalf("claim = %+v, %v", c, err)
	}
	got, _ := controlOf(t, collect(t, a, func(e []Event) bool { return hasKind(e, EventControl) }))
	if got.Holder != "alice@example.com" {
		t.Fatalf("the watcher was told %+v", got)
	}

	b := r.Watch("s1", alice, -1)
	defer b.Close()
	if got, ok := controlOf(t, collect(t, b, func(e []Event) bool { return hasKind(e, EventControl) })); !ok || got.Holder != "alice@example.com" {
		t.Fatalf("a later watcher was told %+v", got)
	}

	if held, err := r.Claim("s1", "bob@example.com", false); !errors.Is(err, ErrHeld) || held.Holder != "alice@example.com" {
		t.Fatalf("bob's claim over alice's = %+v, %v; want ErrHeld naming alice", held, err)
	}
	if _, ok := r.Renew("s1", "bob@example.com"); ok {
		t.Fatal("bob renewed a claim he does not hold")
	}
	if r.Release("s1", "bob@example.com") {
		t.Fatal("bob let go of a claim he does not hold")
	}
	taken, err := r.Claim("s1", "bob@example.com", true)
	if err != nil || taken.Holder != "bob@example.com" || taken.Reason != ControlTaken || taken.From != "alice@example.com" {
		t.Fatalf("take = %+v, %v", taken, err)
	}
	got, _ = controlOf(t, collect(t, a, func(e []Event) bool { c, _ := controlOf(t, e); return c.Reason == ControlTaken }))
	if got.Holder != "bob@example.com" || got.From != "alice@example.com" {
		t.Fatalf("alice was told %+v", got)
	}
	if !r.Release("s1", "bob@example.com") {
		t.Fatal("bob could not let go")
	}
	got, _ = controlOf(t, collect(t, a, func(e []Event) bool { c, _ := controlOf(t, e); return c.Reason == ControlReleased }))
	if got.Holder != "" || got.From != "bob@example.com" {
		t.Fatalf("a let-go keyboard was announced as %+v", got)
	}
	if _, ok := r.Holder("s1"); ok {
		t.Fatal("the keyboard is still held")
	}
	// A watcher who joins now hears of no claim.
	c2 := r.Watch("s1", alice, -1)
	defer c2.Close()
	evs := collect(t, c2, func(e []Event) bool { return hasKind(e, EventReplay) })
	if hasKind(evs, EventControl) {
		t.Fatalf("a free keyboard was announced to a new watcher: %s", kinds(evs))
	}
}

// A claim nobody renews lapses, and the watchers are told; a renewed one does not.
func TestControlLeaseLapses(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	// Renewals span longer than the lease, so the claim outlives it only by
	// being renewed; each gap leaves 400ms of slack for a loaded runner (gd-H2pNjlQThU).
	r.Lease = 500 * time.Millisecond
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	if _, err := r.Claim("s1", "alice@example.com", false); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		time.Sleep(100 * time.Millisecond)
		if _, ok := r.Renew("s1", "alice@example.com"); !ok {
			t.Fatal("a renewed claim lapsed")
		}
	}
	got, _ := controlOf(t, collect(t, w, func(e []Event) bool { c, _ := controlOf(t, e); return c.Reason == ControlExpired }))
	if got.Holder != "" || got.From != "alice@example.com" {
		t.Fatalf("a lapsed claim was announced as %+v", got)
	}
	if _, ok := r.Holder("s1"); ok {
		t.Fatal("a lapsed claim is still held")
	}
	// Claiming with no watcher and no upstream is fine: the next watcher hears of it.
	if _, err := r.Claim("s2", "alice@example.com", false); err != nil {
		t.Fatal(err)
	}
	if mux.dials.Load() != 1 {
		t.Fatal("a claim dialled coopmux")
	}
}

func TestKeys(t *testing.T) {
	for _, k := range []string{"Enter", "esc", "Escape", "Tab", "Up", "PageDown", "F12", "Ctrl-C", "ctrl-a", "Backspace", "Space"} {
		if !IsKeyName(k) {
			t.Errorf("%q is a key coop types", k)
		}
	}
	for _, k := range []string{"", "ctrl-", "ctrl-1", `ctrl-\`, "ctrl-ab", "alt-x", "F13", "x", "shift-tab"} {
		if IsKeyName(k) {
			t.Errorf("%q is not a key coop types", k)
		}
	}
	if !IsDestructiveKey([]string{"Up", " CTRL-C "}) || !IsDestructiveKey([]string{"Ctrl-Z"}) || !IsDestructiveKey([]string{"Ctrl-D"}) {
		t.Error("one destructive key holds the batch")
	}
	if !IsDestructiveKey([]string{"Tab", "Esc"}) || !IsDestructiveKey([]string{"ESCAPE"}) {
		t.Error("Esc interrupts Claude Code, so it asks first")
	}
	if IsDestructiveKey([]string{"Tab", "Ctrl-A", "Enter"}) {
		t.Error("Tab, Ctrl-A and Enter do not ask first")
	}
	// A key that asks first is one coop can type, or the confirm guards a key that never arrives (gd-HCcFZAc9Rj).
	for k := range destructiveKeys {
		if !IsKeyName(k) {
			t.Errorf("%q asks first but coop cannot type it", k)
		}
	}
}
