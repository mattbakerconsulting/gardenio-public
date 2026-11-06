package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gardenio/kbeads/internal/model"
)

// beads_mr_dedup.go — daemon-enforced dedup of type=merge-request beads keyed on
// fields.mr_url (ka-0000002762).
//
// A type=merge-request bead is the canonical projection of a GitLab MR: the
// gitlab-bridge upserts one per mr_url on webhook + poll events. The bridge's
// UpsertMergeRequestBead (gardenio beadsapi/client_mr.go) does find-then-create
// serialised only by an IN-PROCESS mutex keyed on mr_url — which closes the
// single-replica race it was written for (ka-0000007606) but nothing more: two
// bridge replicas, or one replica across a restart, each miss the client-side
// find and each CreateBead a duplicate. The store had no uniqueness guard, so
// every insert landed: a full census (2026-08-18) measured 396 mr_urls carrying
// >1 canonical MR bead, up to 7 each, 282 disagreeing on mr_state.
//
// The durable fix moves the dedup INTO the daemon, which is the single
// serialization point every bridge replica funnels through (the beads daemon
// runs replicaCount:1; even were it scaled out, the createBead lookup-or-return
// below plus the future partial-unique index — HALF-2, ka-0000002762 follow-up —
// close the window a client-side lock never can). createBead calls
// preCreateMergeRequest before the insert transaction; on a hit it returns the
// existing bead with NO duplicate insert and NO second BeadCreated event,
// mirroring the idempotency-key (ka-0000002856) and type=rating upsert paths.
//
// Scope note: task beads ALSO carry a legacy fields->>'mr_url' (the pre-epic
// dual-write, gardenio bridge). The lookup here is type-scoped to merge-request
// so it never collides with a producing task bead that happens to mirror the
// same URL — and the migration-041 index carries the same `type='merge-request'`
// predicate for the same reason.

// mrURLField is the Fields[] key under which the canonical GitLab MR URL is
// persisted on a type=merge-request bead. The bridge's mrFieldsMap sets it.
const mrURLField = "mr_url"

// mrUpsertKey returns the per-mr_url mutex that serialises
// preCreateMergeRequest's lookup-or-return critical section within this daemon
// process. LoadOrStore makes first-use race-safe so two goroutines for the same
// URL share one mutex. This is the intra-process half of the guarantee; the
// migration-041 index (non-unique today, promoted to unique after the HALF-2
// reconciliation) is the cross-process backstop. Analogous to the bridge's own
// mrUpsertLock, but authoritative because every replica's CreateBead lands here.
func (s *BeadsServer) mrUpsertKey(mrURL string) *sync.Mutex {
	v, _ := s.mrUpsertLocks.LoadOrStore(mrURL, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// mrURLFromFields extracts a non-empty fields.mr_url from a bead's raw Fields,
// or "" when absent/blank/malformed.
func mrURLFromFields(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	if v, ok := fields[mrURLField].(string); ok {
		return v
	}
	return ""
}

// lookupMergeRequestByURL returns the existing type=merge-request bead whose
// fields.mr_url == mrURL within the caller's fleet, or nil when none exists.
// All statuses are in scope: a merged MR's canonical bead is status=closed
// (type:merge-request config), so a closed duplicate MUST be reachable or the
// upsert would resurrect it. On the (pre-reconciliation) duplicate sets the
// store may hold >1 match; ListBeads returns them newest-first and we take the
// first — deterministic and, post-HALF-2, unique.
func (s *BeadsServer) lookupMergeRequestByURL(ctx context.Context, fleet, mrURL string) (*model.Bead, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"merge-request"},
		Fields: map[string]string{mrURLField: mrURL},
		Fleet:  fleet,
		Limit:  1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up merge-request bead by mr_url: %w", err)
	}
	if len(beads) > 0 {
		return beads[0], nil
	}
	return nil, nil
}

// lockMergeRequestUpsert acquires and returns the per-mr_url mutex for a
// type=merge-request bead, or nil when the bead carries no mr_url (nothing to
// dedup on). The CALLER must hold the returned mutex — via `defer mu.Unlock()`
// — across BOTH the lookup AND the insert transaction, so a concurrent create
// for the same URL cannot interleave between "found nothing" and "inserted".
// Releasing after only the lookup would reopen the exact race this closes: two
// racers each find nothing, each release, each insert. Because the beads daemon
// is single-replica and every bridge replica's CreateBead funnels through it,
// this in-process serialization IS the store-level guarantee today; the
// migration-041 index (promoted to unique after HALF-2 reconciliation) will make
// it survive a hypothetical multi-replica daemon too.
func (s *BeadsServer) lockMergeRequestUpsert(fields json.RawMessage) *sync.Mutex {
	mrURL := mrURLFromFields(fields)
	if mrURL == "" {
		return nil
	}
	mu := s.mrUpsertKey(mrURL)
	mu.Lock()
	return mu
}

// preCreateMergeRequest is the type=merge-request-specific dedup gate. It looks
// up an existing MR bead for the same fields.mr_url and returns it so createBead
// returns that bead verbatim (no duplicate insert, no second BeadCreated event)
// — turning a concurrent double-create from two bridge events into a single
// canonical bead. Returns (nil, nil) when the bead should be created fresh (no
// mr_url, or no existing bead).
//
// The CALLER must already hold the per-mr_url mutex (via lockMergeRequestUpsert)
// for the whole lookup+insert window — this helper does not lock, so the atomic
// critical section is owned by createBead's deferred unlock.
func (s *BeadsServer) preCreateMergeRequest(ctx context.Context, bead *model.Bead) (*model.Bead, error) {
	mrURL := mrURLFromFields(bead.Fields)
	if mrURL == "" {
		// An MR bead with no mr_url is degenerate but not this guard's concern;
		// let it create so field validation / callers surface the problem.
		return nil, nil
	}
	return s.lookupMergeRequestByURL(ctx, bead.Fleet, mrURL)
}
