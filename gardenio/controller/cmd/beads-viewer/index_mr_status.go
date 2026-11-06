package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gardenio/controller/internal/bridge"
)

// newIndexMRLister builds a live GitLab client for the /index MR-status panel.
// Returns nil when either input is empty so callers can fall back to the
// "not configured" empty state without a separate guard.
func newIndexMRLister(logger *slog.Logger, baseURL, token string) mrLister {
	if baseURL == "" || token == "" {
		return nil
	}
	return bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: baseURL,
		Token:   token,
		Logger:  logger,
	})
}

// mrLister is the subset of bridge.GitLabClient that the /index MR-status
// panel needs. Defined as an interface so tests can substitute a fake without
// dragging the full GitLab REST surface area.
type mrLister interface {
	ListMergeRequests(ctx context.Context, projectPath string, opts bridge.MRListOptions) ([]bridge.GitLabMR, error)
}

// indexMRStatus is the /index "Merge requests" rail panel — live GitLab counts
// for the configured gardenio project. Computed from two REST calls (opened
// MRs once, merged-since-midnight once) cached for mrStatusCacheTTL so a busy
// landing page doesn't hammer GitLab on every render.
type indexMRStatus struct {
	// Available is false when no mrLister is wired (production deploys
	// without a token, or tests). The template renders an explanatory empty
	// state in that case so the column doesn't go blank.
	Available bool
	// ProjectPath is the gardenio project path (e.g. "example-org/gardenio").
	// Surfaced in the panel header so the operator knows which project the
	// counts speak for.
	ProjectPath string
	// ProjectURL is the GitLab MR list URL — header "Merge requests" links here.
	ProjectURL string
	// Open is opened MRs (any draft state).
	Open int
	// Mergeable is opened, non-draft MRs with merge_status=can_be_merged.
	// Best proxy for "approved" we can compute without per-MR approval calls.
	Mergeable int
	// Drafts is opened, draft=true MRs — work-in-progress, not ready for review.
	Drafts int
	// MergedToday is merged MRs with updated_at on or after today's UTC midnight.
	MergedToday int
	// Err carries the fetch error message when the panel failed to load.
	// Set only on hard failure — partial success (e.g. opened succeeded but
	// merged failed) still renders the available numbers.
	Err string
}

// mrStatusCacheTTL bounds how often /index hits the GitLab API. A landing-page
// burst (operator hits / on a fresh tab) folds into one fetch; the next
// refresh outside the TTL pulls a new snapshot.
const mrStatusCacheTTL = 60 * time.Second

// mrStatusCache holds the last-fetched indexMRStatus and when it was fetched.
// Process-local and goroutine-safe — multiple /index handlers fetch through
// the same gate via mu so a thundering herd collapses into one upstream call.
type mrStatusCache struct {
	mu        sync.Mutex
	value     indexMRStatus
	fetchedAt time.Time
}

// get returns the cached indexMRStatus when within ttl of fetchedAt. The
// boolean second return is true on a fresh hit (caller can skip refetch).
func (c *mrStatusCache) get(now time.Time, ttl time.Duration) (indexMRStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) > ttl {
		return indexMRStatus{}, false
	}
	return c.value, true
}

// set replaces the cached snapshot.
func (c *mrStatusCache) set(v indexMRStatus, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = v
	c.fetchedAt = now
}

// computeIndexMRStatus fetches MR counts from GitLab with a short-TTL cache.
// Returns Available=false when no lister is configured (the panel renders an
// explanatory empty state). Hard fetch errors set Err but keep Available=true
// so the template can show a "couldn't reach GitLab" hint instead of going
// silent.
func (s *Server) computeIndexMRStatus(ctx context.Context) indexMRStatus {
	if s.mrLister == nil || s.mrProjectPath == "" {
		return indexMRStatus{Available: false}
	}
	now := s.now()
	if cached, ok := s.mrCache.get(now, mrStatusCacheTTL); ok {
		return cached
	}

	out := indexMRStatus{
		Available:   true,
		ProjectPath: s.mrProjectPath,
		ProjectURL:  s.mrProjectURL,
	}

	var (
		wg          sync.WaitGroup
		opened      []bridge.GitLabMR
		openedErr   error
		merged      []bridge.GitLabMR
		mergedErr   error
		midnightUTC = time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		opened, openedErr = s.mrLister.ListMergeRequests(ctx, s.mrProjectPath, bridge.MRListOptions{
			State:   "opened",
			PerPage: 100,
		})
	}()
	go func() {
		defer wg.Done()
		merged, mergedErr = s.mrLister.ListMergeRequests(ctx, s.mrProjectPath, bridge.MRListOptions{
			State:        "merged",
			UpdatedAfter: midnightUTC,
			PerPage:      100,
		})
	}()
	wg.Wait()

	if openedErr != nil {
		s.logger.Warn("listing opened MRs for index", "error", openedErr, "project", s.mrProjectPath)
		out.Err = "Couldn't reach GitLab."
	} else {
		for _, mr := range opened {
			out.Open++
			if mr.Draft {
				out.Drafts++
				continue
			}
			if mr.MergeStatus == "can_be_merged" && !mr.HasConflicts {
				out.Mergeable++
			}
		}
	}

	if mergedErr != nil {
		s.logger.Warn("listing merged-today MRs for index", "error", mergedErr, "project", s.mrProjectPath)
		if out.Err == "" {
			out.Err = "Couldn't reach GitLab."
		}
	} else {
		for _, mr := range merged {
			// Defensive: GitLab's updated_after on the "merged" scope already
			// constrains the window, but a merge can be amended/rebased after
			// the fact. Cross-check the merged_at field when present so a
			// stale-updated MR from yesterday doesn't tick today's counter.
			if mr.MergedAt == nil {
				out.MergedToday++
				continue
			}
			t, err := time.Parse(time.RFC3339, *mr.MergedAt)
			if err != nil || !t.Before(midnightUTC) {
				out.MergedToday++
			}
		}
	}

	s.mrCache.set(out, now)
	return out
}
