package beadsapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
)

// batchIDCapFloor is the batch-read cap every daemon has accepted since the
// endpoint was introduced (393a5263e). It is the safe landing spot when a
// daemon rejects our optimistic cap but we cannot read its real limit out of
// the rejection.
const batchIDCapFloor = 50

// capRejectionRe matches the daemon's over-cap rejection for the two batch-ID
// surfaces, capturing the limit it advertises:
//
//	{"error":"maximum 500 IDs per batch request"}
//
// Deliberately anchored on "IDs per batch request" so it cannot be confused
// with the neighbouring caps that are NOT this one and move independently:
// children-by-status says "maximum 100 parent IDs per request", and the
// name-lookup endpoint says "maximum 200 names per batch request".
var capRejectionRe = regexp.MustCompile(`maximum (\d+) IDs per batch request`)

// effectiveBatchIDCap is the number of IDs this client will put in one
// /v1/batch/beads or /v1/batch/dependencies request. It starts at the
// compiled-in optimum and only ever ratchets DOWN, when a daemon tells us it
// accepts less (see noteBatchCapRejection).
//
// One value covers both surfaces because the daemon defines them in lockstep
// (batchGetDependenciesMaxIDs = batchGetMaxIDs). Should a future daemon untie
// them, the ratchet settles on the smaller of the two — fewer IDs per request
// is always valid for both, so divergence costs round-trips, never correctness.
func (c *Client) effectiveBatchIDCap() int {
	if v := c.batchIDCap.Load(); v > 0 {
		return int(v)
	}
	return BatchGetBeadsMaxIDs
}

// noteBatchCapRejection inspects a failed batch request. If the daemon rejected
// it for exceeding its per-request ID cap, the client's effective cap is
// lowered and true is returned to tell the caller its request is worth
// retrying. Any other error returns false and must be surfaced unchanged.
//
// WHY THIS EXISTS (ka-0000001212). Raising the client cap to 500 is only safe
// against a daemon that accepts 500 — an old daemon answers HTTP 400 "maximum
// 50 IDs per batch request", and BatchGetBeads has ~148 call sites across the
// controller, so getting the order wrong is a broad outage rather than a
// /plans regression. The rollout rule (ship the daemon first) handles the
// planned deploy, but it is a rule a human has to keep holding: it is equally
// violated by rolling the daemon BACK for some unrelated reason months later,
// long after anyone remembers the coupling.
//
// So the client discovers the cap instead of assuming it. The first oversized
// request against an old daemon costs one wasted round-trip; after that this
// client is pinned to the cap that daemon actually advertised and behaves
// exactly like the pre-raise client. That turns a standing ordering constraint
// into a self-correcting one, in the safe direction — every failure mode here
// degrades to "more round-trips", never to a failed read.
//
// usedCap is the cap the failed request was built with. Lowering is guarded on
// it so that concurrent shards racing on the same rejection cannot ratchet the
// cap down repeatedly, and so the retry loop is guaranteed to terminate: each
// retry runs with a strictly smaller positive cap.
func (c *Client) noteBatchCapRejection(err error, usedCap int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	m := capRejectionRe.FindStringSubmatch(apiErr.Message)
	if m == nil {
		return false
	}
	// Trust the daemon's advertised limit when it parses to something sane;
	// otherwise fall back to the floor every daemon has always honoured.
	daemonCap, convErr := strconv.Atoi(m[1])
	if convErr != nil || daemonCap < 1 {
		daemonCap = batchIDCapFloor
	}
	if daemonCap >= usedCap {
		// The daemon rejected a request no larger than the cap it claims to
		// accept. Nothing we can usefully lower to — retrying would loop.
		return false
	}
	// Ratchet down, never up, and never past a lower value another shard
	// already learned.
	for {
		cur := c.batchIDCap.Load()
		if cur > 0 && cur <= int64(daemonCap) {
			return int64(usedCap) > cur
		}
		if c.batchIDCap.CompareAndSwap(cur, int64(daemonCap)) {
			return true
		}
	}
}

// maxBatchCapNarrowings bounds how many times one call will re-run after a
// cap rejection.
//
// A well-behaved daemon needs exactly ONE: it advertises a fixed limit, we
// adopt it, the retry succeeds. The strictly-decreasing ratchet alone already
// guarantees termination, but "terminates" is not the bar — a daemon that
// shaved one ID off the cap per rejection would technically converge while
// burning hundreds of round-trips, which to a caller is indistinguishable from
// a hang. Two narrowings leaves headroom for a rolling deploy that answers from
// two different daemon versions mid-call, and turns any weirder behavior into a
// prompt error instead of a stall.
const maxBatchCapNarrowings = 2

// withBatchCapRetry runs a sharded batch call, re-running it at a lower cap if
// the daemon rejects the request width. run must build its own result
// accumulator per attempt: a rejected attempt is discarded whole, so no partial
// shard results may survive into the retry.
func (c *Client) withBatchCapRetry(run func(perRequest int) error) error {
	for narrowed := 0; ; narrowed++ {
		perRequest := c.effectiveBatchIDCap()
		err := run(perRequest)
		if err == nil {
			return nil
		}
		if narrowed >= maxBatchCapNarrowings || !c.noteBatchCapRejection(err, perRequest) {
			return err
		}
	}
}
