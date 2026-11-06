package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// RebuildState scans account beads and reconstructs the per-project runtime
// failover map. Called once on controller startup so switchback continues to
// work after restarts. A project is considered in failover when its primary
// account has status=exhausted with a cooldown_until in the future; the
// current account is the first name in the pool that is not currently in
// cooldown.
func (f *authFailover) RebuildState(ctx context.Context) error {
	result, err := f.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return fmt.Errorf("listing account beads: %w", err)
	}

	byProject := map[string][]*beadsapi.BeadDetail{}
	for _, b := range result.Beads {
		for _, l := range b.Labels {
			if p, ok := strings.CutPrefix(l, "project:"); ok {
				byProject[p] = append(byProject[p], b)
			}
		}
	}

	now := time.Now()
	var rebuilt int
	for project, accts := range byProject {
		primaryName := projectPrimary(accts)
		if primaryName == "" {
			continue
		}
		primary := findByTitle(accts, primaryName)
		if primary == nil {
			continue
		}

		if !inActiveCooldown(primary, now) {
			continue // primary is healthy — no failover state to rebuild
		}

		// Pick the least-utilized account in the pool that is not in active
		// cooldown. Falls back to walking the fallback chain if the scan
		// finds nothing.
		current := firstHealthyByUtilization(accts, now, primaryName)
		if current == "" {
			current = walkChainForHealthy(accts, primary, now)
		}
		if current == "" || current == primaryName {
			continue
		}

		failedOverAt := now
		if t, err := time.Parse(time.RFC3339, primary.Fields["exhausted_at"]); err == nil && !t.IsZero() {
			failedOverAt = t
		}

		f.runtime.Set(project, failoverState{
			Primary:        primaryName,
			Current:        current,
			FailedOverAt:   failedOverAt,
			FailoverReason: "rebuilt_on_restart",
		})
		rebuilt++
		f.logger.Info("rebuilt failover state",
			"project", project, "primary", primaryName, "current", current,
			"failed_over_at", failedOverAt.Format(time.RFC3339))
	}

	if rebuilt > 0 {
		f.logger.Info("failover runtime rebuilt", "projects", rebuilt)
	}
	return nil
}

// primaryStuck returns true when a project's primary account cannot currently
// serve traffic and so should be considered for stuck-failover retry. This
// covers two states:
//
//  1. Active cooldown — status=exhausted with a future cooldown_until. Set
//     when authFailover.Handle successfully shifted to a fallback; the
//     project then re-enters "stuck" if the chain later breaks.
//  2. Live throttle — throttle_status=rejected. Set by the rate-limit probe
//     (or directly by an upstream signal). When Handle fires for a project
//     whose chain has no healthy fallback, it returns without marking the
//     primary exhausted, so the bead stays status=active even though the
//     account is unusable. The retry path must still recognize this state.
func primaryStuck(bead *beadsapi.BeadDetail, now time.Time) bool {
	if bead == nil {
		return false
	}
	if inActiveCooldown(bead, now) {
		return true
	}
	return bead.Fields["throttle_status"] == "rejected"
}

// inActiveCooldown returns true when the account is marked exhausted with a
// cooldown_until in the future.
func inActiveCooldown(bead *beadsapi.BeadDetail, now time.Time) bool {
	if bead.Fields["status"] != "exhausted" {
		return false
	}
	until, err := time.Parse(time.RFC3339, bead.Fields["cooldown_until"])
	if err != nil {
		return false
	}
	return until.After(now)
}

// cooldownUntilElapsed reports whether the account carries a parseable
// cooldown_until timestamp that has already passed (until <= now). Unlike
// inActiveCooldown it does NOT inspect status — the caller is expected to have
// already established that the account is exhausted.
//
// This is the primary-free timer-based recovery backstop (Phase 2,
// ka-0000008240): it replaces the switchback maxFailoverDur auto-reset. Once an
// exhausted account's cooldown window elapses it becomes eligible to rejoin the
// spread pool even without a fresh rate-limit probe, so a Rust-marked or
// otherwise unprobed account cannot leak capacity forever. An account with no
// cooldown_until (empty/unparseable) returns false — there is no timer to have
// elapsed, so it can only recover via the probe path.
func cooldownUntilElapsed(bead *beadsapi.BeadDetail, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, bead.Fields["cooldown_until"])
	if err != nil {
		return false
	}
	return !until.After(now)
}

// firstHealthyByUtilization returns the healthy (not-in-cooldown) account that
// is best to serve traffic, preferring the LEAST-utilized account so healthy
// low-utilization subscriptions are picked first and near-limit / spend-capped
// accounts sort LAST (ka-0000003305). It replaces the former alphabetical pick,
// which landed new pods on whichever account sorted first by name regardless of
// how close it was to its budget (e.g. gardenio3 at u7d 0.98 winning over
// matthewcbaker0122 at 0.04 purely on the leading letter).
//
// Ordering keys, in priority:
//  1. deprioritized_until in the future (429-with-headroom accounts) sort last.
//  2. Higher effective utilization (max of 5h/7d) sorts later.
//  3. Title, as a stable deterministic tiebreak.
func firstHealthyByUtilization(accounts []*beadsapi.BeadDetail, now time.Time, skip string) string {
	candidates := make([]*beadsapi.BeadDetail, 0, len(accounts))
	for _, a := range accounts {
		if a.Title == skip {
			continue
		}
		if inActiveCooldown(a, now) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return accountLess(candidates[i], candidates[j], now)
	})
	return candidates[0].Title
}

// accountLess orders account beads from most-preferred to least-preferred for
// placement: non-deprioritized before deprioritized, then ascending by
// effective utilization, then by title for determinism.
func accountLess(a, b *beadsapi.BeadDetail, now time.Time) bool {
	ad, bd := isDeprioritized(a, now), isDeprioritized(b, now)
	if ad != bd {
		return !ad // a preferred when it is NOT deprioritized
	}
	au, bu := accountUtilization(a), accountUtilization(b)
	if au != bu {
		return au < bu
	}
	return a.Title < b.Title
}

// accountUtilization returns the account's effective utilization — the higher
// of its rolling 5h and 7d windows — used to rank near-limit accounts last.
// Missing/unparseable fields read as 0 (treated as least-utilized).
func accountUtilization(bead *beadsapi.BeadDetail) float64 {
	u5 := parseFloat(bead.Fields["utilization_5h"])
	u7 := parseFloat(bead.Fields["utilization_7d"])
	if u5 > u7 {
		return u5
	}
	return u7
}

// isDeprioritized reports whether the account carries a deprioritized_until
// timestamp still in the future — set by auth_failover.Handle when a 429 was
// attributed to a spend-cap/transient throttle rather than budget exhaustion.
// Such accounts remain assignable but rank last during the window.
func isDeprioritized(bead *beadsapi.BeadDetail, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, bead.Fields["deprioritized_until"])
	if err != nil {
		return false
	}
	return until.After(now)
}

func walkChainForHealthy(accounts []*beadsapi.BeadDetail, start *beadsapi.BeadDetail, now time.Time) string {
	visited := map[string]bool{start.Title: true}
	candidate := start.Fields["fallback"]
	for candidate != "" {
		if visited[candidate] {
			return ""
		}
		visited[candidate] = true
		bead := findByTitle(accounts, candidate)
		if bead == nil {
			return ""
		}
		if !inActiveCooldown(bead, now) {
			return candidate
		}
		candidate = bead.Fields["fallback"]
	}
	return ""
}

// earliestResetAt walks the fallback chain starting from the given account and
// returns the earliest throttle_reset_at timestamp found (RFC3339). Returns
// "" if no reset times are available.
func earliestResetAt(accounts []*beadsapi.BeadDetail, start *beadsapi.BeadDetail) string {
	var earliest string
	visited := map[string]bool{}

	for cursor := start; cursor != nil; {
		if visited[cursor.Title] {
			break
		}
		visited[cursor.Title] = true

		if ra := cursor.Fields["throttle_reset_at"]; ra != "" {
			if earliest == "" || ra < earliest {
				earliest = ra
			}
		}

		next := cursor.Fields["fallback"]
		if next == "" {
			break
		}
		cursor = findByTitle(accounts, next)
	}

	return earliest
}

// findByTitle returns the first bead with the given title, or nil.
func findByTitle(beads []*beadsapi.BeadDetail, title string) *beadsapi.BeadDetail {
	for _, b := range beads {
		if b.Title == title {
			return b
		}
	}
	return nil
}
