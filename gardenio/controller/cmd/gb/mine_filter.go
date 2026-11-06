package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ownershipScope describes how a `--mine` / `--lane` gate listing is narrowed
// to a single lane's gates. It is shared by `gb decision list` and
// `gb escalation list` so "mine" means the same thing across the pair.
//
// A gate (decision or escalation) is "owned" by a lane when its DURABLE
// owner_seat matches the lane's seat_key — the stable lane key that survives
// respawn — OR, as a fallback, when its EPHEMERAL created_by matches the
// current actor.
//
// created_by alone is not enough, and that was the bug (ka-0000005138):
// created_by is the agent INSTANCE name, which changes on every respawn. A
// gardener that filed a decision while running as instance -7gxg could not find
// it after respawning as -nhdq, because `--mine` filtered created_by == the
// live instance name. owner_seat (stamped from the agent bead's seat_key by
// `gb decision create`, ka-0000005031) is the durable key that fixes that: a
// respawned gardener resolves its own seat and matches every decision its seat
// filed, under any past instance name. The created_by fallback is retained so a
// row THIS instance filed before owner_seat was stamped (owner-stamp is
// best-effort) still resolves.
type ownershipScope struct {
	seat      string // match owner_seat == seat (the durable lane key)
	createdBy string // fallback: match created_by == createdBy (this instance)
}

// active reports whether the scope narrows the listing at all. An inactive
// scope is the default "show me every open gate" listing.
func (s ownershipScope) active() bool { return s.seat != "" || s.createdBy != "" }

// resolveOwnershipScope turns the shared `--mine` / `--lane` flags into an
// ownershipScope.
//
//   - `--mine` scopes to the CALLER's own lane: it resolves the caller's
//     seat_key off its agent bead and unions it with a created_by == actor
//     fallback (so a row this instance filed pre-owner-stamp still resolves).
//   - `--lane <seat>` inspects ANOTHER lane by its seat_key, with NO created_by
//     fallback — you are not looking at your own instance's rows.
//
// The two are mutually exclusive.
//
// The actor == "unknown" guard is load-bearing for the `--mine` fallback:
// defaultActor() falls back to the literal "unknown" when neither KD_ACTOR nor
// git user.name is set, and filtering created_by == "unknown" would silently
// return whatever some *other* caller filed under "unknown" — a wrong answer
// wearing the costume of a right one, in exactly the double-dispatch guard a
// gardener uses to decide whether to spawn crew. When `--mine` resolves a real
// seat we can proceed on the seat alone even if the actor is unknown; only when
// NEITHER a trustworthy seat NOR a trustworthy actor is available do we fail
// loud.
func resolveOwnershipScope(cmd *cobra.Command) (ownershipScope, error) {
	mine, _ := cmd.Flags().GetBool("mine")
	lane, _ := cmd.Flags().GetString("lane")

	if lane != "" {
		if mine {
			return ownershipScope{}, fmt.Errorf("use --mine or --lane, not both: --mine scopes to your own lane, --lane <seat> inspects another")
		}
		return ownershipScope{seat: lane}, nil
	}
	if !mine {
		return ownershipScope{}, nil
	}

	seat := resolveActorSeat(cmd.Context())
	actorOK := actor != "" && actor != "unknown"

	if seat == "" && !actorOK {
		// cmd.CommandPath() already carries the root command name ("gb …").
		return ownershipScope{}, fmt.Errorf("--mine needs to know who you are, but neither your seat_key nor a resolvable actor is available (actor=%q).\n\nSet one of:\n  export KD_ACTOR=<your-agent-or-user-name>\n  git config user.name <name>\n  %s --actor <name> --mine\n\nOr inspect a specific lane directly:\n  %s --lane <seat-key>", actor, cmd.CommandPath(), cmd.CommandPath())
	}

	s := ownershipScope{seat: seat}
	if actorOK {
		s.createdBy = actor
	}
	return s, nil
}

// resolveActorSeat resolves the CALLER's durable seat_key off its own agent
// bead (KD_AGENT_ID). Returns "" when there is no agent bead or it carries no
// seat_key — the caller decides whether that is fatal. Best-effort: a daemon
// error yields "". This is the read that makes `--mine` survive respawn: the
// agent bead's seat_key is stable across the instance-name churn that broke the
// created_by filter.
func resolveActorSeat(ctx context.Context) string {
	agentID, err := resolveAgentID("")
	if err != nil || agentID == "" {
		return ""
	}
	seat, _ := resolveDecisionOwner(ctx, agentID)
	return seat
}

// listOwnedBeads runs base under scope, unioning the owner_seat match and the
// created_by match into one de-duplicated result. When scope is inactive it is
// a single passthrough query (byte-for-byte the pre-scope behavior).
//
// The daemon ANDs its filters, so owner_seat and created_by cannot be OR'd in a
// single query; when both predicates are active we run each and union
// client-side. A "mine" listing is a single lane's own open gates — small — so
// the extra round trip and client-side merge/sort are cheap. When only one
// predicate is active we return the daemon result directly, preserving the
// daemon's authoritative Total (which the truncation notices depend on).
//
// walkAll selects the exhaustive page-walk (ListAllBeadsFiltered, used by
// `gb escalation list`'s default unbounded path) over a single bounded
// ListBeadsFiltered. When walkAll is true base.Limit/Offset must be zero (the
// page-walker owns paging); each variant inherits that.
func listOwnedBeads(ctx context.Context, base beadsapi.ListBeadsQuery, scope ownershipScope, walkAll bool) (*beadsapi.ListBeadsResult, error) {
	run := func(q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
		if walkAll {
			return daemon.ListAllBeadsFiltered(ctx, q)
		}
		return daemon.ListBeadsFiltered(ctx, q)
	}

	if !scope.active() {
		return run(base)
	}

	var variants []beadsapi.ListBeadsQuery
	if scope.seat != "" {
		q := base
		q.Fields = mergedFields(base.Fields, "owner_seat", scope.seat)
		variants = append(variants, q)
	}
	if scope.createdBy != "" {
		q := base
		q.CreatedBy = scope.createdBy
		variants = append(variants, q)
	}

	if len(variants) == 1 {
		// Single predicate: let the daemon sort and count. Preserves the exact
		// pre-union behavior, including an accurate Total for truncation notices.
		return run(variants[0])
	}

	seen := make(map[string]struct{})
	var merged []*beadsapi.BeadDetail
	for _, q := range variants {
		res, err := run(q)
		if err != nil {
			return nil, err
		}
		for _, b := range res.Beads {
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}

	sortBeads(merged, base.Sort)

	total := len(merged)
	if !walkAll && base.Limit > 0 && len(merged) > base.Limit {
		merged = merged[:base.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: merged, Total: total}, nil
}

// mergedFields returns a copy of base with k=v set, leaving base untouched so
// the two query variants never alias the same map.
func mergedFields(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for bk, bv := range base {
		out[bk] = bv
	}
	out[k] = v
	return out
}

// sortBeads orders a union result to match the daemon's sort key for the two
// gate-list callers: "-created_at" (newest first) and "priority,-created_at"
// (P0 first, then newest within a tier). These are the only two sort shapes the
// gate lists produce; any other key falls back to newest-first. Only invoked on
// the two-predicate union path — a single-predicate query is sorted by the
// daemon.
func sortBeads(beads []*beadsapi.BeadDetail, sortKey string) {
	byPriority := strings.Contains(sortKey, "priority")
	sort.SliceStable(beads, func(i, j int) bool {
		a, b := beads[i], beads[j]
		if byPriority && a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
}
