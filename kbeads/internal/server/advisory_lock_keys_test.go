package server

import "testing"

// Postgres advisory locks share one 64-bit keyspace per database, so two sweeps
// using the same key silently starve each other: whichever replica holds the
// lock makes the other sweep's TryAdvisoryLock return not-acquired and skip the
// tick (gd-cCSJlWd73X). Register every *AdvisoryLockKey constant here.
func TestAdvisoryLockKeysAreUnique(t *testing.T) {
	keys := map[string]int64{
		"decisionAgingAdvisoryLockKey":          decisionAgingAdvisoryLockKey,
		"auditRetentionAdvisoryLockKey":         auditRetentionAdvisoryLockKey,
		"slowToolCallsRetentionAdvisoryLockKey": slowToolCallsRetentionAdvisoryLockKey,
		"spawnPolicyAdvisoryLockKey":            spawnPolicyAdvisoryLockKey,
		"escalationRouteAdvisoryLockKey":        escalationRouteAdvisoryLockKey,
		"historyRetentionAdvisoryLockKey":       historyRetentionAdvisoryLockKey,
	}
	seen := make(map[int64]string, len(keys))
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("%s and %s share advisory lock key %#x", prev, name, key)
		}
		seen[key] = name
	}
}
