package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"gardenio/kbeads/internal/model"
)

// recent_beads.go implements the kd-side half of ADR docs/adr/taints.md §9.4's
// read-denial message: "kd prints 'not found or not visible to you' when the
// caller asked about a bead id it was just given by another call in the same
// session." Each kd invocation is its own process, so "same session" is
// approximated with a small local state file, overwritten (not accumulated)
// on every successful `kd show`/`list`/`search`, and read back within a short
// TTL. The ADR does not specify how far back "just given" extends — this
// implements only the immediate-prior-call case (one overwritten snapshot,
// not a history), noted on epic gd-6fmhiIEiLr per the task's explicit
// permission to leave that ambiguity for a follow-up.
const recentBeadsTTL = 10 * time.Minute

// recentBeadsState is the on-disk shape of the last-seen-beads cache file.
type recentBeadsState struct {
	IDs        []string  `json:"ids"`
	RecordedAt time.Time `json:"recorded_at"`
}

// recentBeadsStatePath mirrors remoteConfigPath's ~/.local/state/beads/
// convention (remote.go), creating the directory (mode 0o700) if needed.
func recentBeadsStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "state", "beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "last-seen-beads.json"), nil
}

// recordSeenBeadIDs best-effort overwrites the local "recently seen" cache
// with ids from the current call. It never returns an error: a broken HOME or
// an unwritable state dir just means wasRecentlySeen degrades to always-false,
// which is the same behavior kd had before this feature existed. A no-op ids
// slice leaves any existing cache untouched rather than clearing it, so a
// caller that fetched zero beads (e.g. an empty search) does not erase a
// still-fresh snapshot from a moment earlier in the same shell pipeline.
func recordSeenBeadIDs(ids []string) {
	if len(ids) == 0 {
		return
	}
	path, err := recentBeadsStatePath()
	if err != nil {
		return
	}
	data, err := json.Marshal(recentBeadsState{IDs: ids, RecordedAt: time.Now()})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// listedBeadIDs extracts bead IDs from a ListBeads response page for
// recordSeenBeadIDs. A nil bead in the slice (should not happen, but treated
// defensively the same way other list-handling code in this codebase does)
// is skipped rather than panicking.
func listedBeadIDs(beads []*model.Bead) []string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		if b != nil {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// wasRecentlySeen reports whether id appeared in the most recent
// recordSeenBeadIDs snapshot, written within the last recentBeadsTTL. Any
// failure to read/parse the cache (missing file, corrupt JSON, stale
// snapshot) reports false rather than erroring — this is a best-effort UX
// hint, never a source of truth.
func wasRecentlySeen(id string) bool {
	path, err := recentBeadsStatePath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state recentBeadsState
	if err := json.Unmarshal(data, &state); err != nil {
		return false
	}
	if time.Since(state.RecordedAt) > recentBeadsTTL {
		return false
	}
	for _, seen := range state.IDs {
		if seen == id {
			return true
		}
	}
	return false
}
