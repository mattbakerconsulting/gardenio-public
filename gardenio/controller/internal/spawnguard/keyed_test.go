package spawnguard

import (
	"sync"
	"testing"
	"time"
)

func TestKeyed_CapsEachKeyApart(t *testing.T) {
	k := NewKeyed(2, time.Hour)
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	k.SetNowFunc(func() time.Time { return clock })

	for i, id := range []string{"a1", "a2"} {
		if ok, _ := k.Allow("land-a", id); !ok {
			t.Fatalf("admission %d refused under the cap", i)
		}
	}
	if ok, reason := k.Allow("land-a", "a3"); ok || reason != ReasonRateLimit {
		t.Fatalf("third admission in the hour = %v %q, want refused rate_limit", ok, reason)
	}
	if ok, _ := k.Allow("land-b", "b1"); !ok {
		t.Fatal("another key must not share land-a's cap")
	}
	if got := k.Count("land-a"); got != 2 {
		t.Errorf("Count(land-a) = %d, want 2", got)
	}
}

func TestKeyed_SameIDCountsOnce(t *testing.T) {
	k := NewKeyed(1, time.Hour)
	if ok, _ := k.Allow("land-a", "kd-1"); !ok {
		t.Fatal("first admission refused")
	}
	// The same spawn decided at the second door is the same admission.
	if ok, _ := k.Allow("land-a", "kd-1"); !ok {
		t.Fatal("re-admitting the same id must not be refused")
	}
	if ok, _ := k.Allow("land-a", "kd-2"); ok {
		t.Fatal("a second id past the cap must be refused")
	}
	if got := k.Count("land-a"); got != 1 {
		t.Errorf("Count = %d, want 1 (the same id holds one slot)", got)
	}
}

func TestKeyed_EmptyIDAlwaysTakesASlot(t *testing.T) {
	k := NewKeyed(2, time.Minute)
	k.Allow("m", "")
	k.Allow("m", "")
	if ok, _ := k.Allow("m", ""); ok {
		t.Fatal("empty ids are not deduplicated: the third must be refused")
	}
}

func TestKeyed_WindowSlides(t *testing.T) {
	k := NewKeyed(1, time.Hour)
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	k.SetNowFunc(func() time.Time { return clock })
	k.Allow("land-a", "kd-1")

	clock = clock.Add(59 * time.Minute)
	if ok, _ := k.Allow("land-a", "kd-2"); ok {
		t.Fatal("admitted inside the window")
	}
	clock = clock.Add(time.Minute) // kd-1 is exactly an hour old: out
	if ok, _ := k.Allow("land-a", "kd-2"); !ok {
		t.Fatal("the slot must free once the oldest admission leaves the window")
	}
	clock = clock.Add(2 * time.Hour)
	if k.Count("land-a") != 0 || len(k.stamps) != 0 {
		t.Errorf("an idle key must be forgotten, stamps = %v", k.stamps)
	}
}

func TestKeyed_DisabledAndNilAdmit(t *testing.T) {
	var nilKeyed *Keyed
	for name, k := range map[string]*Keyed{"nil": nilKeyed, "max 0": NewKeyed(0, time.Hour), "window 0": NewKeyed(5, 0)} {
		for i := 0; i < 10; i++ {
			if ok, _ := k.Allow("x", ""); !ok {
				t.Fatalf("%s: admission %d refused", name, i)
			}
		}
		if k.Count("x") != 0 {
			t.Errorf("%s: Count = %d, want 0", name, k.Count("x"))
		}
	}
}

func TestKeyed_ConcurrentAdmissionsHoldTheCap(t *testing.T) {
	k := NewKeyed(10, time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := k.Allow("land-a", ""); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 10 {
		t.Errorf("admitted %d concurrently, want exactly 10", admitted)
	}
}

func TestKeyed_RetryAfterIsWhenTheOldestLeaves(t *testing.T) {
	k := NewKeyed(2, time.Hour)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	k.SetNowFunc(func() time.Time { return clock })

	k.Allow("land-a/ann", "m1")
	if got := k.RetryAfter("land-a/ann"); got != 0 {
		t.Fatalf("RetryAfter under the cap = %v, want 0", got)
	}
	clock = clock.Add(20 * time.Minute)
	k.Allow("land-a/ann", "m2")
	clock = clock.Add(5 * time.Minute)
	if got, want := k.RetryAfter("land-a/ann"), 35*time.Minute; got != want {
		t.Fatalf("RetryAfter at the cap = %v, want %v", got, want)
	}
	if got := NewKeyed(0, time.Hour).RetryAfter("x"); got != 0 {
		t.Fatalf("disabled RetryAfter = %v, want 0", got)
	}
}
