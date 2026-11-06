// Package gardenermode owns the timed⇄event firing-mode flip for gardener
// trigger beads, plus the deterministic cron stagger that spreads gardener
// fire minutes so a shared literal default ("7,37 * * * *") can never
// produce a synchronized spawn herd again. Spec:
// docs/superpowers/specs/2026-07-13-gardener-mode-flip-design.md
package gardenermode

import (
	"fmt"
	"hash/fnv"
	"time"
)

// StaggeredCron returns a deterministic cron expression whose fire minutes
// are seeded from an FNV-1a hash of seed (normally the gardener's epic or
// trigger bead id). Same seed → same cron forever, so re-flipping a gardener
// never churns its schedule.
//
//	period 30m → "M,M+30 * * * *"        (M in [0,30))
//	period 6h  → "M H,H+6,H+12,H+18 * * *" (M in [0,60), H in [0,6))
//	period 24h → "M H * * *"               (M in [0,60), H in [0,24))
//
// Any other period falls back to the 30m shape — the callers only pass these
// three values and a silent fallback beats a panic in a CLI path. The 24h
// shape is the daily watchdog plangardener.Bind gives a new event-mode plan
// gardener (gd-gsvlS6YDj3).
func StaggeredCron(seed string, period time.Duration) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	n := h.Sum32()
	if period == 24*time.Hour {
		return fmt.Sprintf("%d %d * * *", int(n%60), int((n/60)%24))
	}
	if period == 6*time.Hour {
		m := int(n % 60)
		hr := int((n / 60) % 6)
		return fmt.Sprintf("%d %d,%d,%d,%d * * *", m, hr, hr+6, hr+12, hr+18)
	}
	m := int(n % 30)
	return fmt.Sprintf("%d,%d * * * *", m, m+30)
}
