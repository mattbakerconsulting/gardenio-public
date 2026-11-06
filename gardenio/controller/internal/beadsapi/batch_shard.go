package beadsapi

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// batchShardConcurrency bounds how many sharded batch round-trips a single
// Client has in flight against the daemon at once.
//
// Why this exists (ka-0000000376): the batch helpers shard their ID list to
// stay under the daemon's per-request cap, and they used to walk those shards
// in a plain serial loop. That made wall-clock the SUM of every shard's
// latency, so cost grew linearly with the ID count even though each shard is
// individually fast (~25ms). On the /plans index that turned a 4522-child
// fetch into 91 sequential round-trips — 2.8s of the page's ~3.7s render, and
// the single dominant term in it.
//
// The limit is deliberately CLIENT-WIDE rather than per-call. Callers routinely
// fan several batch joins out concurrently (the /plans enrich runs four at
// once), so a per-call limit multiplies: 4 joins x 16 would put 64 requests on
// a shared daemon. Measured on the real page, that over-subscription was a net
// LOSS, so one shared semaphore makes the cap mean what it says no matter how
// many joins are in flight.
//
// Sized against measurement, not intuition — but the curve is FLAT, so do not
// read precision into this number. An interleaved A/B (variants rotated inside
// one load window, so drifting daemon load hits each equally) put the /plans
// median at 1.45s / 1.49s / 1.45s for a cap of 4 / 6 / 8 — indistinguishable.
// What matters is serial-vs-concurrent (3.7s -> ~1.5s), not the exact width. 6
// sits mid-range: enough to absorb a burst, well short of swamping a shared
// daemon.
//
// A caution for whoever re-tunes this: the daemon's own throughput drifts by
// ~2x with fleet load, so sequential A/B runs are NOT comparable. An earlier
// sequential sweep "showed" 6 clearly beating 4 and 8; interleaving erased the
// difference entirely. Always interleave.
const batchShardConcurrency = 6

// shardIDs splits ids into consecutive shards of at most maxPerShard entries.
// The shards alias ids (no copy); callers must not mutate them.
func shardIDs(ids []string, maxPerShard int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	if maxPerShard < 1 {
		maxPerShard = 1
	}
	shards := make([][]string, 0, (len(ids)+maxPerShard-1)/maxPerShard)
	for start := 0; start < len(ids); start += maxPerShard {
		end := min(start+maxPerShard, len(ids))
		shards = append(shards, ids[start:end])
	}
	return shards
}

// newBatchShardLimiter builds the per-Client shard semaphore. Every sharded
// batch helper on a Client shares this one instance, so batchShardConcurrency
// caps that client's total in-flight shard requests rather than each call's.
func newBatchShardLimiter() *semaphore.Weighted {
	return semaphore.NewWeighted(batchShardConcurrency)
}

// forEachShard splits ids into shards of at most maxPerShard and runs fn over
// them, bounded by the Client's shared shard limiter, returning the first error.
//
// Semantics deliberately match the serial loop this replaces: the first shard
// error aborts the batch and is returned, and the caller discards any partial
// result. The difference is that siblings still in flight are cancelled via the
// errgroup's derived context rather than never being started, so a failing
// batch stops paying for round-trips whose results would be thrown away.
//
// fn may be called concurrently from multiple goroutines and must therefore
// serialize its own writes to any shared accumulator.
func (c *Client) forEachShard(ctx context.Context, ids []string, maxPerShard int, fn func(context.Context, []string) error) error {
	shards := shardIDs(ids, maxPerShard)
	switch len(shards) {
	case 0:
		return nil
	case 1:
		// Fast path: the overwhelmingly common single-shard call skips the
		// errgroup, the goroutine, and the semaphore entirely, so it stays
		// exactly as cheap as it was before sharding went concurrent.
		return fn(ctx, shards[0])
	}
	lim := c.batchShardLimiter
	if lim == nil {
		// A Client built as a bare &Client{} (tests, embedders) has no limiter.
		// Fall back to a call-scoped one so the fan-out is still bounded rather
		// than unbounded or nil-panicking.
		lim = newBatchShardLimiter()
	}
	g, gctx := errgroup.WithContext(ctx)
	// acquireErr records why we stopped launching shards. Acquire only fails
	// when gctx is done — the caller cancelled, or a sibling already failed.
	// The sibling-failure case is reported by Wait, but a plain caller
	// cancellation is NOT: every shard that already ran returned nil, so Wait
	// would report success for a run that silently skipped the remaining
	// shards. Batch results are merged into one map, so that surfaces to the
	// caller as a partial map indistinguishable from "these beads do not
	// exist" — the exact shape of bug that makes a page render with rows
	// quietly missing. Capture it and fail loudly instead.
	var acquireErr error
	for _, shard := range shards {
		if err := lim.Acquire(gctx, 1); err != nil {
			acquireErr = err
			break
		}
		g.Go(func() error {
			defer lim.Release(1)
			return fn(gctx, shard)
		})
	}
	if err := g.Wait(); err != nil {
		// A real shard error (or the cancellation that caused one) wins — it is
		// the more specific cause.
		return err
	}
	if acquireErr != nil {
		return fmt.Errorf("batch shard fan-out aborted before all shards ran: %w", acquireErr)
	}
	return nil
}
