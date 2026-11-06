package advice

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// AdviceDiff represents the difference in matched advice between two
// subscription sets — typically the agent's current role vs. the target
// step's role.
type AdviceDiff struct {
	Added   []MatchedAdvice // advice items gained by the new subscriptions
	Removed []MatchedAdvice // advice items lost from the old subscriptions
}

// DiffAdviceForRole computes the advice difference when an agent transitions
// from its current subscriptions to an augmented set that includes a target
// role. The caller provides the agent's current matched advice and the target
// role. Returns the diff (added/removed items).
func DiffAdviceForRole(ctx context.Context, daemon *beadsapi.Client, agentID, targetRole string, currentMatched []MatchedAdvice) (*AdviceDiff, error) {
	allAdvice, err := ListOpenAdvice(ctx, daemon)
	if err != nil {
		return nil, err
	}

	// Build target subscriptions: start with agent's base subscriptions,
	// then add the target role subscription.
	targetSubs := BuildAgentSubscriptions(agentID, []string{"role:" + targetRole})
	targetSubs = EnrichAgentSubscriptions(ctx, daemon, agentID, targetSubs)

	// Compute advice for the target role subscriptions. Apply the same
	// per-bead delivery denies as ListAdviceForAgent so the diff cannot
	// resurrect beads the delivery path drops.
	var targetMatched []MatchedAdvice
	for _, bead := range allAdvice {
		if IsFixture(bead) || IsDeniedBroadcast(bead) {
			continue
		}
		if MatchesSubscriptions(bead.Labels, targetSubs) {
			ml := FindMatchedLabels(bead.Labels, targetSubs)
			scope, target := CategorizeScopeForMatch(bead.Labels, ml)
			targetMatched = append(targetMatched, MatchedAdvice{
				Bead:          bead,
				MatchedLabels: ml,
				Scope:         scope,
				ScopeTarget:   target,
				ScopeHeader:   BuildScopeHeader(scope, target),
			})
		}
	}

	// Build sets of bead IDs for comparison.
	currentIDs := make(map[string]bool, len(currentMatched))
	for _, m := range currentMatched {
		currentIDs[m.Bead.ID] = true
	}
	targetIDs := make(map[string]bool, len(targetMatched))
	for _, m := range targetMatched {
		targetIDs[m.Bead.ID] = true
	}

	diff := &AdviceDiff{}

	// Added: in target but not in current.
	for _, m := range targetMatched {
		if !currentIDs[m.Bead.ID] {
			diff.Added = append(diff.Added, m)
		}
	}

	// Removed: in current but not in target.
	for _, m := range currentMatched {
		if !targetIDs[m.Bead.ID] {
			diff.Removed = append(diff.Removed, m)
		}
	}

	return diff, nil
}

// IsEmpty returns true if the diff contains no changes.
func (d *AdviceDiff) IsEmpty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0
}
