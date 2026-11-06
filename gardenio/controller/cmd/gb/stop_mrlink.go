package main

// stop_mrlink.go — wrap-up MR auto-link guard.
//
// Object lesson ka-0000000816 (2026-06-01): a thread-spawned agent opened a
// GitLab MR and listed it in `gb stop --wrapup` pull_requests, but never ran
// `gb mr link`. The consequences cascade:
//   - the task bead has no mr_url → beads-viewer renders no MR node/pill,
//   - the gitlab-bridge never dual-writes the type=merge-request bead, so
//     there is no `produces` edge and the agent card's produced-MRs section
//     walks an empty graph,
//   - GitLab state/pipeline/merge tracking never starts (advice ka-0000006733).
//
// This guard closes the gap at the cheapest enforcement point: when an agent
// supplies a wrap-up with a pull_requests list, every GitLab MR URL in it that
// is not already linked to a bead is auto-linked by setting mr_url on the
// best-candidate task bead the agent worked on — exactly what `gb mr link`
// does. The gitlab-bridge then dual-writes the MR bead + produces edge on its
// next poll. It is warn-and-auto-link by default and never blocks the stop, so
// an honest despawn is never wedged; --force skips it entirely.

import (
	"context"
	"fmt"
	"sort"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// mrLinkDaemon is the subset of the beads client the auto-link guard needs.
// Kept small so the logic is unit-testable with a fake.
type mrLinkDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// autoLinkWrapupMRs back-fills mr_url for any GitLab MR URL listed in the
// wrap-up's pull_requests that is not yet linked to a bead. It returns
// human-readable status lines (links performed, or why a URL could not be
// auto-linked) for the caller to surface; it never returns a fatal error —
// failing to auto-link must not block a despawn.
//
// agentID is the agent bead ID. wrapup is the parsed wrap-up (may be nil).
func autoLinkWrapupMRs(ctx context.Context, d mrLinkDaemon, agentID string, wrapup *WrapUp) []string {
	if d == nil || wrapup == nil || len(wrapup.PullRequests) == 0 {
		return nil
	}

	// Collect the GitLab MR URLs from the wrap-up. Non-GitLab-MR entries
	// (GitHub PRs, plain links) are left untouched — ParseMRURL returns nil.
	var mrURLs []string
	for _, u := range wrapup.PullRequests {
		if bridge.ParseMRURL(u) != nil {
			mrURLs = append(mrURLs, u)
		}
	}
	if len(mrURLs) == 0 {
		return nil
	}

	// Discover the task beads this agent worked on, split into the unlinked
	// candidate pool (where we can attach an MR) and the set of MR URLs already
	// present on the agent's tasks. Done once up front; the pool shrinks as
	// URLs are assigned so two URLs never land on the same bead.
	candidates, linkedURLs := agentTaskCandidates(ctx, d, agentID)

	var msgs []string
	for _, u := range mrURLs {
		// Already linked? Skip silently — the common case once agents adopt
		// `gb mr link`. Check both the agent's own tasks (covers the window
		// before the gitlab-bridge has created the MR bead) and the canonical
		// type=merge-request bead keyed on the URL.
		if linkedURLs[u] {
			continue
		}
		if mrBead, err := d.MRBeadByURL(ctx, u); err == nil && mrBead != nil {
			continue
		}

		// Find the best unlinked candidate and attach the URL to it.
		idx := bestCandidateIndex(candidates)
		if idx < 0 {
			msgs = append(msgs, fmt.Sprintf(
				"MR %s in wrap-up is not linked to any bead, and no unlinked task bead was found to attach it to — run `gb mr link <bead-id> %s` so the gitlab-bridge can track it.",
				u, u))
			continue
		}

		target := candidates[idx]
		if err := d.UpdateBeadFields(ctx, target.ID, map[string]string{"mr_url": u}); err != nil {
			msgs = append(msgs, fmt.Sprintf("could not auto-link MR %s to %s: %v", u, target.ID, err))
			continue
		}
		// Mark consumed so the next URL picks a different bead.
		candidates[idx].Fields["mr_url"] = u
		msgs = append(msgs, fmt.Sprintf(
			"Auto-linked MR %s → %s (no `gb mr link` was run). The gitlab-bridge will now create the MR bead + produces edge and track state.",
			u, target.ID))
	}
	return msgs
}

// agentTaskCandidates returns the issue-kind task beads this agent worked on
// that do not already carry an mr_url (the candidate pool for attachment,
// ordered best-first), plus the set of MR URLs already present on the agent's
// tasks (so an already-linked URL is recognised even before the gitlab-bridge
// has minted its MR bead). Two signals are unioned:
//   - beads assigned to the agent (the object-lesson task was assignee-linked
//     but had no produces edge), and
//   - beads the agent `produces` (the canonical work-graph edge).
//
// Best-first = most-recently-updated, so a fresh MR lands on the task the
// agent was just working on rather than a stale one.
func agentTaskCandidates(ctx context.Context, d mrLinkDaemon, agentID string) ([]*beadsapi.BeadDetail, map[string]bool) {
	agentName := agentNameForID(ctx, d, agentID)

	linkedURLs := map[string]bool{}
	seen := map[string]bool{agentID: true}
	var out []*beadsapi.BeadDetail
	add := func(b *beadsapi.BeadDetail) {
		if b == nil || seen[b.ID] {
			return
		}
		// Only issue-kind beads are MR-bearing work; skip the agent/config
		// beads, decisions, mail, and epics.
		if b.Kind != "issue" {
			return
		}
		if b.Type == "merge-request" || b.Type == "epic" {
			return
		}
		seen[b.ID] = true
		// A bead already carrying an mr_url is not an attachment candidate, but
		// its URL counts as already-linked so we don't double-link it.
		if b.Fields != nil && b.Fields["mr_url"] != "" {
			linkedURLs[b.Fields["mr_url"]] = true
			return
		}
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		out = append(out, b)
	}

	if agentName != "" {
		if res, err := d.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Assignee: agentName,
			Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
			Limit:    200,
		}); err == nil && res != nil {
			for _, b := range res.Beads {
				add(b)
			}
		}
	}

	// Produces graph: agent --produces--> task.
	if deps, err := d.GetDependencies(ctx, agentID); err == nil {
		for _, dep := range deps {
			if dep.Type != "produces" || dep.DependsOnID == "" {
				continue
			}
			if b, err := d.GetBead(ctx, dep.DependsOnID); err == nil {
				add(b)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, linkedURLs
}

// bestCandidateIndex returns the index of the best still-unlinked candidate
// (the first one whose mr_url is still empty), or -1 when the pool is
// exhausted. Candidates are pre-sorted best-first.
func bestCandidateIndex(candidates []*beadsapi.BeadDetail) int {
	for i, c := range candidates {
		if c.Fields["mr_url"] == "" {
			return i
		}
	}
	return -1
}

// agentNameForID resolves the human-facing agent name used as the assignee on
// task beads. Agent beads store it in fields["agent"]; the title is the same
// string in practice, used as a fallback.
func agentNameForID(ctx context.Context, d mrLinkDaemon, agentID string) string {
	bead, err := d.GetBead(ctx, agentID)
	if err != nil || bead == nil {
		return ""
	}
	if bead.Fields != nil {
		if n := bead.Fields["agent"]; n != "" {
			return n
		}
	}
	return bead.Title
}
