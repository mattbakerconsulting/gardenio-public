package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Gardener chaining (C1) — schema-adjacent helpers: the cycle/depth guard and
// the agent-side v0 handoff nudge composer.
//
// The schema itself (ChainSpec / ChainTo) lives in triggers.go next to
// GardenerSpec because it is parsed off the same trigger bead. This file holds
// the *behavior* that consumes a parsed chain: refusing runaway chains (§3.4)
// and telling a predecessor gardener how to hand the baton (§3.2 v0).
//
// Design source of truth: gardenio/docs/gardener-chaining-design-2026-06-18.md.

// maxChainDepth caps the total number of gardeners in one succession chain,
// counting the heir about to be created. A→B→C→…→heir longer than this is
// refused (design §3.4 guard rail #1; the doc's default is 8). The cap bounds
// the blast radius of a misconfigured chain that would otherwise spawn
// gardeners indefinitely.
const maxChainDepth = 8

var (
	// errChainTooDeep is returned by ChainHeirGuard when adding the heir would
	// exceed maxChainDepth.
	errChainTooDeep = errors.New("gardener chain would exceed max depth")
	// errChainCycle is returned when the proposed heir re-introduces an
	// ancestor already present in the chain (A→B→A), or when the existing
	// succeeds chain already contains a loop.
	errChainCycle = errors.New("gardener chain cycle detected")
)

// ChainLink is one gardener in a succession chain — the identity-bearing
// subset of a gardener bead used by the cycle guard. Identity is matched on
// PromptAdvice first (the stable, versioned handle for "which gardener this
// is"), falling back to Phase+Epic when no prompt advice is declared.
type ChainLink struct {
	ID           string
	PromptAdvice string
	Phase        string
	Epic         string
}

// chainResolver resolves one hop of the `succeeds:` provenance chain. Given a
// gardener bead ID it returns that bead's identity and the ID of the
// predecessor it declares `succeeds: <predecessor>` (empty when the bead is a
// chain root). Implemented against the beads daemon in production
// (daemonChainResolver) and with a map in tests.
type chainResolver interface {
	ResolveChainLink(ctx context.Context, beadID string) (link ChainLink, predecessorID string, err error)
}

// chainIdentityKey reduces a gardener identity to the key the cycle guard
// compares on. PromptAdvice wins because it is the stable handle for "which
// gardener this is"; otherwise phase|epic is a coarse fallback. Returns "" when
// nothing distinguishing is set — an empty key never matches, so an
// under-specified heir is never falsely flagged as a cycle (depth still bounds
// it).
func chainIdentityKey(promptAdvice, phase, epic string) string {
	if pa := strings.TrimSpace(promptAdvice); pa != "" {
		return "advice:" + pa
	}
	phase = strings.TrimSpace(phase)
	epic = strings.TrimSpace(epic)
	if phase == "" && epic == "" {
		return ""
	}
	return "phase:" + phase + "|epic:" + epic
}

// walkSucceedsChain walks the `succeeds:` chain upward from startID, returning
// the links encountered (startID first, then its predecessor, …) in order.
// Walking stops at a chain root, when `limit` links have been collected, or on
// an error. A repeated ID along the way means the stored chain already
// contains a cycle and is reported as errChainCycle.
func walkSucceedsChain(ctx context.Context, r chainResolver, startID string, limit int) ([]ChainLink, error) {
	var chain []ChainLink
	seen := map[string]bool{}
	cur := strings.TrimSpace(startID)
	for cur != "" {
		if seen[cur] {
			return chain, fmt.Errorf("%w: existing chain loops at %s", errChainCycle, cur)
		}
		seen[cur] = true
		link, pred, err := r.ResolveChainLink(ctx, cur)
		if err != nil {
			return chain, err
		}
		if link.ID == "" {
			link.ID = cur
		}
		chain = append(chain, link)
		if limit > 0 && len(chain) >= limit {
			break
		}
		cur = strings.TrimSpace(pred)
	}
	return chain, nil
}

// ChainHeirGuard checks whether creating heir as a successor of predID is
// safe (design §3.4). It walks the existing `succeeds:` chain rooted at predID
// and refuses when:
//
//   - adding the heir would make the chain longer than maxChainDepth, or
//   - the heir's identity already appears as predID or one of its ancestors
//     (a cycle, e.g. A→B→A), or
//   - the existing stored chain already loops.
//
// On success it returns nil and the current chain length (predID + ancestors),
// so callers — including the nudge composer — can surface "this will be link
// N of M". The guard is pure given the resolver, so it is reusable for both the
// v0 agent-side preflight and a future v1 engine-side check.
func ChainHeirGuard(ctx context.Context, r chainResolver, predID string, heir *ChainTo) (chainLen int, err error) {
	if heir == nil {
		return 0, nil
	}
	// Collect at most maxChainDepth existing links — that's all we need to
	// decide the depth verdict (one more link would already be over the cap).
	existing, err := walkSucceedsChain(ctx, r, predID, maxChainDepth)
	if err != nil {
		return len(existing), err
	}
	// existing = [predID, …ancestors]. Adding the heir yields len(existing)+1.
	if len(existing)+1 > maxChainDepth {
		return len(existing), fmt.Errorf(
			"%w: %d existing links + heir > cap %d", errChainTooDeep, len(existing), maxChainDepth)
	}
	heirKey := chainIdentityKey(heir.PromptAdvice, heir.Phase, heir.Epic)
	if heirKey != "" {
		for _, link := range existing {
			if chainIdentityKey(link.PromptAdvice, link.Phase, link.Epic) == heirKey {
				return len(existing), fmt.Errorf(
					"%w: heir identity %q already present as %s", errChainCycle, heirKey, link.ID)
			}
		}
	}
	return len(existing), nil
}

// chainLinkFetcher is the minimal daemon surface the production chain resolver
// needs: fetch a bead's detail (for chain/gardener identity fields) and its
// forward mentions (to find the `succeeds:` predecessor edge). *beadsapi.Client
// satisfies it. Kept narrow so the resolver stays unit-testable.
type chainLinkFetcher interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetMentions(ctx context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error)
}

// daemonChainResolver resolves succession links against the live beads daemon.
type daemonChainResolver struct {
	f chainLinkFetcher
}

// NewDaemonChainResolver returns a chainResolver backed by the beads daemon.
// Pass a *beadsapi.Client (it satisfies chainLinkFetcher). The result is the
// production input to ChainHeirGuard at heir-creation time.
func NewDaemonChainResolver(f chainLinkFetcher) chainResolver {
	return daemonChainResolver{f: f}
}

// ResolveChainLink reads beadID's identity from its `chain.to` / `gardener`
// fields and finds the predecessor it declares via a `succeeds:` typed
// mention (model.DepChainsTo, surface verb "succeeds"). When several succeeds
// edges exist (malformed), the lexicographically-first target is used so the
// walk is deterministic.
func (d daemonChainResolver) ResolveChainLink(ctx context.Context, beadID string) (ChainLink, string, error) {
	detail, err := d.f.GetBead(ctx, beadID)
	if err != nil {
		return ChainLink{}, "", fmt.Errorf("chain resolve %s: %w", beadID, err)
	}
	link := ChainLink{ID: beadID}
	if detail != nil {
		// Identity from this gardener's own chain.to (preferred) or gardener block.
		if raw := strings.TrimSpace(detail.Fields["chain"]); raw != "" {
			var ch ChainSpec
			if json.Unmarshal([]byte(raw), &ch) == nil && ch.To != nil {
				link.PromptAdvice = ch.To.PromptAdvice
				link.Phase = ch.To.Phase
				link.Epic = ch.To.Epic
			}
		}
		if link.PromptAdvice == "" {
			link.PromptAdvice = strings.TrimSpace(detail.Fields["prompt_advice"])
		}
		if link.Epic == "" {
			link.Epic = strings.TrimSpace(detail.Fields["parent"])
		}
	}
	pred := ""
	mentions, err := d.f.GetMentions(ctx, beadID, 0)
	if err != nil {
		return link, "", fmt.Errorf("chain mentions %s: %w", beadID, err)
	}
	for _, m := range mentions {
		if m.RelationshipType == nil {
			continue
		}
		// succeeds is the inverse surface verb of the DepChainsTo type; the
		// stored relationship_type is the surface verb the author wrote.
		if strings.EqualFold(*m.RelationshipType, "succeeds") {
			if pred == "" || m.ID < pred {
				pred = m.ID
			}
		}
	}
	return link, pred, nil
}
