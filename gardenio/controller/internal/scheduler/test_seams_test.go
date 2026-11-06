package scheduler

import "time"

// GardenerWallClockExpiry is GardenerWallClockExpirySince with no
// wall_clock_since override.
func GardenerWallClockExpiry(beadID string, createdAt time.Time, window time.Duration) (expiry time.Time, ok bool) {
	return GardenerWallClockExpirySince(beadID, createdAt, time.Time{}, window)
}
