package main

// Harness-aware session usage (ADR 0004 §9, gd-MolQ67L3xX).
//
// parseSessionUsage reads a Claude Code JSONL log. A Codex or Gemini pod has no
// such log (its session log is a rollout / a chat-session JSON file), and the ADR rules out a Go rollout parser: the
// usage a Codex session produced is already normalised by coop, which serves it
// for every driver at GET /api/v1/session/usage. loadSessionUsage picks the
// source from the pod's harness; the Claude branch is exactly parseSessionUsage.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// sessionUsageFromCoop reports whether this pod's usage comes from coop rather
// than a Claude session log. GARDENIO_HARNESS is set by the controller only for
// non-default harnesses, so an unset or unknown value keeps the Claude path.
func sessionUsageFromCoop() bool {
	switch strings.TrimSpace(os.Getenv("GARDENIO_HARNESS")) {
	case beadsapi.HarnessCodex, beadsapi.HarnessGemini:
		return true
	}
	return false
}

// canLoadSessionUsage is the guard the callers used to write as logPath != "":
// a coop-sourced harness has no log path but can still report usage.
func canLoadSessionUsage(logPath string) bool {
	return logPath != "" || sessionUsageFromCoop()
}

// coopUsageSnapshot is the last usage coop answered with for the current
// session. closeSessionBead / runSessionClose run after coop has exited, when
// GET /api/v1/session/usage is refused, and coop persists no usage file — so the
// close-time record falls back to this snapshot (merge-desk send-back on #415).
var coopUsageSnapshot struct {
	mu    sync.Mutex
	usage *beadsapi.SessionUsage
}

func storeCoopUsageSnapshot(u *beadsapi.SessionUsage) {
	coopUsageSnapshot.mu.Lock()
	defer coopUsageSnapshot.mu.Unlock()
	cp := *u
	coopUsageSnapshot.usage = &cp
}

func cachedCoopUsageSnapshot() *beadsapi.SessionUsage {
	coopUsageSnapshot.mu.Lock()
	defer coopUsageSnapshot.mu.Unlock()
	if coopUsageSnapshot.usage == nil {
		return nil
	}
	cp := *coopUsageSnapshot.usage
	return &cp
}

// resetCoopUsageSnapshot drops the cached snapshot so a restarted coop session
// never inherits the previous session's usage.
func resetCoopUsageSnapshot() {
	coopUsageSnapshot.mu.Lock()
	defer coopUsageSnapshot.mu.Unlock()
	coopUsageSnapshot.usage = nil
}

// coopUsageSnapshotInterval is how often the snapshot poller asks coop for
// usage; the close-time record is at most this stale.
var coopUsageSnapshotInterval = 15 * time.Second

// startCoopUsageSnapshot begins a new session's usage tracking and returns the
// function that stops it. It clears the previous session's snapshot, points
// COOP_URL at the pod's own coop when gb's env does not carry it (only the
// coop/agent env is given COOP_URL, so the localhost:8080 default would break
// on a non-default port), and — for coop-sourced harnesses — polls coop so a
// snapshot exists when coop exits. The poller is independent of VMAGENT_URL.
func startCoopUsageSnapshot(ctx context.Context, coopPort int) (stop func()) {
	resetCoopUsageSnapshot()
	if !sessionUsageFromCoop() {
		return func() {}
	}
	if os.Getenv("COOP_URL") == "" && coopPort > 0 {
		_ = os.Setenv("COOP_URL", fmt.Sprintf("http://localhost:%d", coopPort))
	}
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(coopUsageSnapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				_, _ = fetchCoopSessionUsage(pollCtx) // refreshes the snapshot
			}
		}
	}()
	return func() { cancel(); <-done }
}

// fetchCoopSessionUsage reads usage from the live coop and refreshes the
// snapshot on success.
func fetchCoopSessionUsage(parent context.Context) (*beadsapi.SessionUsage, error) {
	coopURL := strings.TrimRight(os.Getenv("COOP_URL"), "/")
	if coopURL == "" {
		coopURL = "http://localhost:8080"
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	u, err := coopapi.New(coopURL).GetSessionUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading usage from coop: %w", err)
	}
	usage := &beadsapi.SessionUsage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		TotalCostUSD:     u.TotalCostUSD,
		RequestCount:     u.RequestCount,
		ContextTokens:    u.ContextTokens,
	}
	storeCoopUsageSnapshot(usage)
	return usage, nil
}

// loadSessionUsage returns the session's usage from the right source for the
// pod's harness. For coop-sourced harnesses logPath is ignored, ThinkingTokens,
// ToolCallCount, ErrorCount and CompactionCount stay zero (coop does not report
// them), and TotalCostUSD is whatever coop reports (0 on a subscription). When
// coop is unreachable (it has exited — the close path) the last successful
// snapshot is returned; an error only when coop never answered.
func loadSessionUsage(logPath string) (*beadsapi.SessionUsage, error) {
	if !sessionUsageFromCoop() {
		return parseSessionUsage(logPath)
	}
	usage, err := fetchCoopSessionUsage(context.Background())
	if err != nil {
		if snap := cachedCoopUsageSnapshot(); snap != nil {
			return snap, nil
		}
		return nil, err
	}
	return usage, nil
}
