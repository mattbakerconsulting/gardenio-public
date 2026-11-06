package main

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// inboxMRInfo carries the chip state derived from a type=merge-request bead.
// Empty MRState means "no linked MR" — the chip suppresses itself.
type inboxMRInfo struct {
	State string
	URL   string
}

// inboxMRBeadFinder is the subset of beadsapi.Client the resolver needs.
// Implemented by the production daemon client and by fakes in tests.
type inboxMRBeadFinder interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// inboxMRResolver batches per-task MR-bead lookups for one inbox render.
// Pattern mirrors agentResolver (ka-0000000899): the handler builds one
// resolver per request, prewarms it from the page's bead surface, and the
// row builders read from the populated cache — no per-row daemon calls.
//
// Lifetime: one HTTP request. The resolver is not safe for concurrent use
// across requests (cache is not goroutine-safe by design — inbox renders
// are sequential).
type inboxMRResolver struct {
	cache map[string]inboxMRInfo
}

// newInboxMRResolver returns an empty resolver. Nil-safe: a nil resolver
// makes Lookup a no-op miss.
func newInboxMRResolver() *inboxMRResolver {
	return &inboxMRResolver{cache: make(map[string]inboxMRInfo)}
}

// Prewarm batch-fetches forward `produces` deps for every non-MR task bead
// in beads, then batch-fetches the MR beads those edges point at, then
// populates the cache. Beads of type "merge-request" populate from their
// own fields without any daemon call.
//
// Errors on either batch round-trip degrade to "no chip" — the resolver
// stays usable, just empty for the unresolved IDs. The inbox is hot path,
// so a transient daemon hiccup never breaks the page; the chip is a polish
// affordance, not a correctness signal.
func (r *inboxMRResolver) Prewarm(ctx context.Context, finder inboxMRBeadFinder, beads []*beadsapi.BeadDetail) {
	if r == nil || finder == nil || len(beads) == 0 {
		return
	}
	taskIDs := make([]string, 0, len(beads))
	for _, b := range beads {
		if b == nil || b.ID == "" {
			continue
		}
		if b.Type == "merge-request" {
			r.cache[b.ID] = inboxMRInfo{
				State: mrFieldString(b.Fields, "mr_state", "state"),
				URL:   mrFieldString(b.Fields, "mr_url", "url", "gitlab_mr_url"),
			}
			continue
		}
		if _, ok := r.cache[b.ID]; ok {
			continue
		}
		taskIDs = append(taskIDs, b.ID)
	}
	if len(taskIDs) == 0 {
		return
	}
	depsByID, err := finder.BatchGetDependencies(ctx, taskIDs, "forward")
	if err != nil {
		// Cache misses for every input ID so a later Lookup short-circuits.
		for _, id := range taskIDs {
			r.cache[id] = inboxMRInfo{}
		}
		return
	}
	// Collect the dependended-on bead IDs that are candidates for type=merge-request.
	// Preserve the first-produces edge per task ID so we can map back after
	// the bead batch returns.
	firstProducesByTask := make(map[string]string, len(taskIDs))
	mrCandidates := make([]string, 0, len(taskIDs))
	seenCandidate := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		deps := depsByID[id]
		for _, d := range deps {
			if d.Type != "produces" || d.DependsOnID == "" {
				continue
			}
			if _, ok := firstProducesByTask[id]; !ok {
				firstProducesByTask[id] = d.DependsOnID
			}
			if _, ok := seenCandidate[d.DependsOnID]; ok {
				continue
			}
			seenCandidate[d.DependsOnID] = struct{}{}
			mrCandidates = append(mrCandidates, d.DependsOnID)
		}
	}
	// Cache misses up front so tasks with no produces edge short-circuit
	// even when we hit the daemon-failure path below.
	for _, id := range taskIDs {
		if _, hit := firstProducesByTask[id]; !hit {
			r.cache[id] = inboxMRInfo{}
		}
	}
	if len(mrCandidates) == 0 {
		return
	}
	beadByID, err := finder.BatchGetBeads(ctx, mrCandidates)
	if err != nil {
		for taskID := range firstProducesByTask {
			r.cache[taskID] = inboxMRInfo{}
		}
		return
	}
	for taskID, candidateID := range firstProducesByTask {
		b, ok := beadByID[candidateID]
		if !ok || b == nil || b.Type != "merge-request" {
			// First produces edge wasn't an MR — fall back to scanning the
			// rest of the edges for this task before giving up. Keeps the
			// resolver compatible with MRBeadForTask which walks all edges.
			info := inboxMRInfo{}
			for _, d := range depsByID[taskID] {
				if d.Type != "produces" || d.DependsOnID == "" || d.DependsOnID == candidateID {
					continue
				}
				alt, altOk := beadByID[d.DependsOnID]
				if !altOk || alt == nil || alt.Type != "merge-request" {
					continue
				}
				info = inboxMRInfo{
					State: mrFieldString(alt.Fields, "mr_state", "state"),
					URL:   mrFieldString(alt.Fields, "mr_url", "url", "gitlab_mr_url"),
				}
				break
			}
			r.cache[taskID] = info
			continue
		}
		r.cache[taskID] = inboxMRInfo{
			State: mrFieldString(b.Fields, "mr_state", "state"),
			URL:   mrFieldString(b.Fields, "mr_url", "url", "gitlab_mr_url"),
		}
	}
}

// Lookup returns the cached MR info for beadID. Nil resolver or unknown ID
// returns the zero value so callers can blindly populate row fields.
func (r *inboxMRResolver) Lookup(beadID string) inboxMRInfo {
	if r == nil || beadID == "" {
		return inboxMRInfo{}
	}
	return r.cache[beadID]
}

// mrFieldString reads the first non-empty value at any of keys from fields.
// Mirrors templates/partials/type_mr.html's `or (index $f ...)` lookup so
// the chip and the type page agree on which field aliases count as "state".
func mrFieldString(fields map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := fields[k]; v != "" {
			return v
		}
	}
	return ""
}
