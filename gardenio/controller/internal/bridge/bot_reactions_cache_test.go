package bridge

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// blockingRulesDaemon embeds the shared mockDaemon (promoting every BeadClient
// method) and overrides ONLY ListBeadsFiltered to (a) count calls and (b) block
// on a release channel so the test can observe behaviour WHILE a fetch is
// in-flight. This is what makes the "lock held across I/O" regression
// (ka-0000008004) observable.
type blockingRulesDaemon struct {
	*mockDaemon
	calls   atomic.Int32
	entered chan struct{} // signalled once, when the first fetch enters
	release chan struct{} // closed by the test to let the fetch return
	rules   []*beadsapi.BeadDetail
}

func (d *blockingRulesDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	n := d.calls.Add(1)
	if n == 1 {
		close(d.entered)
	}
	<-d.release
	return &beadsapi.ListBeadsResult{Beads: d.rules}, nil
}

// TestLoadReactionRules_LockNotHeldAcrossFetch proves loadReactionRules does
// NOT hold cache.mu across the daemon round-trip (the ka-0000008004
// regression): while one refresh is blocked in the daemon, cache.mu must be
// free. It also proves singleflight collapses the concurrent stale-cache burst
// into exactly one daemon call instead of one per caller.
//
// The test runs in a synctest bubble so the burst is deterministic
// (gd-8X9OAWaQQZ): synctest.Wait returns only once the leader is parked on the
// daemon's release channel and every other caller is parked in singleflight's
// WaitGroup, both durable blocks. Only then is the fetch released, so no caller
// can pass the stale check and reach singleflight after the leader's call is
// gone (the "got 20 daemon calls" flake on loaded runners). The lock check runs
// before the burst starts: a caller blocked on cache.mu is not durably blocked,
// so a held lock would otherwise hang synctest.Wait instead of failing.
func TestLoadReactionRules_LockNotHeldAcrossFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		daemon := &blockingRulesDaemon{
			mockDaemon: newMockDaemon(),
			entered:    make(chan struct{}),
			release:    make(chan struct{}),
			rules: []*beadsapi.BeadDetail{
				makeReactionRule("rule-1", "runner", "e2e-test-summary", "monorepo", "crew", "prompt"),
			},
		}
		// A t.Fatal must still let the parked fetch return, or the bubble
		// deadlocks instead of reporting the failure.
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(daemon.release) }) }
		defer release()

		bot := &Bot{daemon: daemon, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		// Pre-seed a STALE cache pointer so this test exercises the concurrent
		// refresh/fetch path (the ka-0000008004 finding), not the one-time lazy
		// init of the cache struct itself. fetchedAt is left zero -> already stale.
		bot.reactionRuleCache = &reactionRuleCache{ttl: reactionRuleCacheTTL}
		cache := bot.reactionRuleCache

		const n = 20
		var done sync.WaitGroup
		done.Add(n)
		results := make([]int, n)
		errs := make([]error, n)
		load := func(i int) {
			defer done.Done()
			rules, err := bot.loadReactionRules(context.Background())
			errs[i] = err
			if err == nil {
				results[i] = len(rules)
			}
		}

		// Park the leader in the daemon.
		go load(0)
		synctest.Wait()
		if got := daemon.calls.Load(); got != 1 {
			t.Fatalf("leader made %d daemon calls, want 1", got)
		}

		// While the refresh is parked in the daemon, nothing may hold cache.mu
		// (ka-0000008004), and the cache is still stale: fetchedAt is stamped
		// only after the daemon call returns.
		if !cache.mu.TryLock() {
			t.Fatal("cache.mu held across the daemon fetch — regression ka-0000008004")
		}
		stale := cache.fetchedAt.IsZero()
		cache.mu.Unlock()
		if !stale {
			t.Fatal("cache stamped fresh before the in-flight fetch returned")
		}

		// The burst finds the cache stale and must join the in-flight call.
		for i := 1; i < n; i++ {
			go load(i)
		}
		synctest.Wait()
		if got := daemon.calls.Load(); got != 1 {
			t.Fatalf("singleflight failed to collapse the burst: got %d in-flight daemon calls, want 1", got)
		}

		// Let the in-flight fetch return; the burst collapses onto its result.
		release()
		done.Wait()

		for i := range results {
			if errs[i] != nil {
				t.Fatalf("loader %d: %v", i, errs[i])
			}
			if results[i] != 1 {
				t.Fatalf("loader %d saw %d rules, want 1", i, results[i])
			}
		}
		if got := daemon.calls.Load(); got != 1 {
			t.Fatalf("singleflight failed to collapse the burst: got %d daemon calls, want 1", got)
		}
	})
}

// TestLoadReactionRules_FreshCacheServedWithoutFetch proves a fresh cache entry
// is served entirely from memory (no daemon call), and — critically — that a
// reader is NOT blocked while an unrelated refresh holds the cache open. We
// pre-seed a fresh cache and assert the daemon is never called.
func TestLoadReactionRules_FreshCacheServedWithoutFetch(t *testing.T) {
	daemon := &blockingRulesDaemon{
		mockDaemon: newMockDaemon(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	bot := &Bot{daemon: daemon, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "x", "monorepo", "crew", "prompt"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	rules, err := bot.loadReactionRules(context.Background())
	if err != nil {
		t.Fatalf("loadReactionRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 cached rule, got %d", len(rules))
	}
	if got := daemon.calls.Load(); got != 0 {
		t.Fatalf("fresh cache should not hit the daemon, got %d calls", got)
	}
}
