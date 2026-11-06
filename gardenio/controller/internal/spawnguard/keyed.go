package spawnguard

import (
	"sync"
	"time"
)

// Keyed is a sliding-window rate cap per key: at most Max admissions per key
// in any Window. The controller keys it by land for the Lands per-land spawn
// rate (Track B spec §5.4, step 10.4), and beads-viewer keys it by land and
// member for the Commons message rate. Unlike SpawnGuard it has no breaker:
// a key at its cap is refused until its oldest admission leaves the window.
//
// Each admission may carry an id. The same id admitted again inside the
// window is let through without taking a second slot, so a spawn decided at
// two doors (the controller's AgentSpawn event and the reconciler's
// create-missing pass) counts once. An empty id always takes a slot.
//
// State is in memory: a restart forgets it, so the cap is a brake on a storm,
// not a ledger. Safe for concurrent use.
type Keyed struct {
	max    int
	window time.Duration

	mu      sync.Mutex
	stamps  map[string][]admission
	nowFunc func() time.Time
}

type admission struct {
	at time.Time
	id string
}

// NewKeyed creates a Keyed cap of max admissions per key per window. A max
// or window of zero or less disables it: Allow always admits.
func NewKeyed(max int, window time.Duration) *Keyed {
	return &Keyed{max: max, window: window, stamps: map[string][]admission{}, nowFunc: time.Now}
}

// Max is the cap's admissions per window, for refusal messages.
func (k *Keyed) Max() int {
	if k == nil {
		return 0
	}
	return k.max
}

// Window is the cap's window, for refusal messages.
func (k *Keyed) Window() time.Duration {
	if k == nil {
		return 0
	}
	return k.window
}

func (k *Keyed) disabled() bool { return k == nil || k.max <= 0 || k.window <= 0 }

// Allow reports whether key may admit one more, reserving a slot for id if
// so. A nil or disabled Keyed always admits.
func (k *Keyed) Allow(key, id string) (bool, Reason) {
	if k.disabled() {
		return true, ReasonAllowed
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.nowFunc()
	ts := k.evict(key, now)
	if id != "" {
		for _, a := range ts {
			if a.id == id {
				return true, ReasonAllowed
			}
		}
	}
	if len(ts) >= k.max {
		return false, ReasonRateLimit
	}
	k.stamps[key] = append(ts, admission{at: now, id: id})
	return true, ReasonAllowed
}

// Count is the number of admissions key holds in the window now.
func (k *Keyed) Count(key string) int {
	if k.disabled() {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.evict(key, k.nowFunc()))
}

// RetryAfter is how long until key may admit one more: zero when it is under
// its cap now, else until its oldest admission leaves the window.
func (k *Keyed) RetryAfter(key string) time.Duration {
	if k.disabled() {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.nowFunc()
	ts := k.evict(key, now)
	if len(ts) < k.max {
		return 0
	}
	return ts[0].at.Add(k.window).Sub(now)
}

// SetNowFunc overrides the clock. Test-only.
func (k *Keyed) SetNowFunc(f func() time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.nowFunc = f
}

// evict drops key's admissions older than the window and returns the rest,
// forgetting a key that has none so idle keys do not accumulate. The caller
// holds mu.
func (k *Keyed) evict(key string, now time.Time) []admission {
	ts := k.stamps[key]
	cutoff := now.Add(-k.window)
	i := 0
	for i < len(ts) && !ts[i].at.After(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(k.stamps, key)
		return nil
	}
	k.stamps[key] = ts
	return ts
}
