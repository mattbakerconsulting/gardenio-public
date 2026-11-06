package toollatency

import (
	"testing"
	"time"
)

func TestPairing_ObserveThenTake(t *testing.T) {
	p := NewPairing()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.Observe("abc", t0)

	dur, ok := p.Take("abc", t0.Add(2*time.Second))
	if !ok {
		t.Fatal("expected pairing to be found")
	}
	if dur != 2*time.Second {
		t.Errorf("duration = %v, want 2s", dur)
	}
	if p.Len() != 0 {
		t.Errorf("expected entry removed after Take, Len() = %d", p.Len())
	}
}

func TestPairing_TakeWithoutObserveMisses(t *testing.T) {
	p := NewPairing()
	_, ok := p.Take("never-seen", time.Now())
	if ok {
		t.Error("expected miss for unseen tool_use_id")
	}
}

func TestPairing_TakeIsOneShot(t *testing.T) {
	p := NewPairing()
	t0 := time.Now()
	p.Observe("abc", t0)
	if _, ok := p.Take("abc", t0.Add(time.Second)); !ok {
		t.Fatal("expected first Take to hit")
	}
	if _, ok := p.Take("abc", t0.Add(2*time.Second)); ok {
		t.Error("expected second Take for same id to miss (entry consumed)")
	}
}

func TestPairing_ExpiredEntryMisses(t *testing.T) {
	p := NewPairing()
	p.ttl = time.Minute
	t0 := time.Now()
	p.Observe("abc", t0)
	_, ok := p.Take("abc", t0.Add(2*time.Minute))
	if ok {
		t.Error("expected expired entry to miss even though still present until Take")
	}
}

func TestPairing_EvictsExpiredOnObserve(t *testing.T) {
	p := NewPairing()
	p.ttl = time.Minute
	t0 := time.Now()
	p.Observe("old", t0)
	p.Observe("new", t0.Add(2*time.Minute))
	if p.Len() != 1 {
		t.Errorf("expected stale entry evicted, Len() = %d", p.Len())
	}
	if _, ok := p.Take("new", t0.Add(2*time.Minute)); !ok {
		t.Error("expected fresh entry to survive eviction sweep")
	}
}

func TestPairing_MaxEntriesBounded(t *testing.T) {
	p := NewPairing()
	p.maxEntries = 3
	t0 := time.Now()
	p.Observe("a", t0)
	p.Observe("b", t0)
	p.Observe("c", t0)
	p.Observe("d", t0) // should evict "a"
	if p.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", p.Len())
	}
	if _, ok := p.Take("a", t0); ok {
		t.Error("expected oldest entry \"a\" to have been evicted to respect maxEntries")
	}
	if _, ok := p.Take("d", t0); !ok {
		t.Error("expected newest entry \"d\" to survive")
	}
}

func TestPairing_NilReceiverSafe(t *testing.T) {
	var p *Pairing
	p.Observe("x", time.Now())
	if _, ok := p.Take("x", time.Now()); ok {
		t.Error("nil Pairing should never produce a hit")
	}
	if p.Len() != 0 {
		t.Error("nil Pairing Len() should be 0")
	}
}
