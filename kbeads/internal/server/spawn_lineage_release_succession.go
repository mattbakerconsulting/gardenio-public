package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Seal the next release's gardener from the gardener of the release before it
// (owner decision gd-cDG0zTiNSk option a, "succession"; task gd-9aclijPx85).
//
// An open release has its own gardener (controller internal/releasegardener):
// a trigger pinned by gardener-release:<release> that spawns the privileged
// release-captain role. The operator's OIDC `gb release open` / `gb release
// gardener` seals one as usual. The run that cuts a release binds the NEXT
// release's gardener on its own agent token (release-cut.sh runs gb release
// record, which opens the next release and binds its gardener), so
// stampSpawnLineage never seals it. The admin who stands behind the cutting
// run's gardener stands behind its successor, so the server seals that trigger
// to the same admin when ALL hold:
//
//   - the caller writes on its own per-agent token; its agent bead is open, in
//     the trigger's project, carries the release-captain role and names a
//     trigger_id (fixed at spawn: planScopeBindingPatchDenial; spawning the
//     privileged role at all passes the spawn gate);
//   - that trigger is an open release gardener (release-gardener label,
//     gardener-release:<prev>) of the same project, for another release of
//     it, with a valid seal that is still fresh;
//   - the new trigger is open, enabled, spawns release-captain in that
//     project, and is pinned to a release of that project that is not yet
//     live or superseded;
//   - no other open trigger for that release carries a valid seal, so one
//     gardener buys one successor lane.
//
// The new seal keeps the old seal's author AND its iat: succession never
// extends an admin's seal, so the chain lapses one seal TTL after an admin
// last sealed a link of it, and an admin's reseal (or OIDC bind) restarts it.

const (
	// releaseGardenerMarkerLabel and releaseGardenerLabelPrefix mirror the
	// controller's internal/releasegardener MarkerLabel and ReleaseLabel.
	releaseGardenerMarkerLabel = "release-gardener"
	releaseGardenerLabelPrefix = "gardener-release:"
)

// releaseGardenerRelease is the release a release gardener's labels pin it
// to, "" when the bead is not one.
func releaseGardenerRelease(labels []string) string {
	if !hasLabel(labels, releaseGardenerMarkerLabel) {
		return ""
	}
	for _, l := range labels {
		if rel, ok := strings.CutPrefix(l, releaseGardenerLabelPrefix); ok && strings.TrimSpace(rel) != "" {
			return strings.TrimSpace(rel)
		}
	}
	return ""
}

// stampSpawnLineageFromReleaseSuccession seals bead from the caller's own
// release gardener when the conditions in the file header hold. It must run on
// the final fields.
func (s *BeadsServer) stampSpawnLineageFromReleaseSuccession(ctx context.Context, bead *model.Bead) bool {
	if !isAutonomousBeadType(bead.Type) || isOIDCAdminSession(ctx) || validSpawnSeal(bead) != nil {
		return false
	}
	if bead.Status == model.StatusClosed || !effectiveGardenerEnabled(bead.Type, bead.Fields) ||
		triggerSpawnRole(bead) != releaseCaptainRole {
		return false
	}
	relID := releaseGardenerRelease(bead.Labels)
	project := projectFromLabelsOrEmpty(bead.Labels)
	if relID == "" || project == "" || strings.TrimSpace(fieldsStringValue(bead.Fields, "project")) != project ||
		!s.releaseStillTended(ctx, relID, project) {
		return false
	}
	prev := s.callerReleaseGardener(ctx, project)
	if prev == nil || releaseGardenerRelease(prev.Labels) == relID {
		return false
	}
	seal := validSpawnSeal(prev)
	if seal == nil || !s.spawnSealFresh(seal) || s.releaseHasSealedGardener(ctx, relID, bead.ID) {
		return false
	}
	if !sealSpawnLineageCarried(bead, seal) {
		return false
	}
	slog.Info("spawn lineage: release gardener sealed from its predecessor",
		"trigger", bead.ID, "release", relID, "predecessor", prev.ID, "author", seal.Author)
	return true
}

// releaseStillTended reports whether relID is a release of project that is not
// yet live or superseded.
func (s *BeadsServer) releaseStillTended(ctx context.Context, relID, project string) bool {
	rel, err := s.store.GetBead(ctx, relID)
	if err != nil || rel == nil || rel.Type != model.TypeRelease {
		return false
	}
	v := releaseViewOf(rel)
	return v.Project == project && v.Status != releaseStatusLive && v.Status != releaseStatusSuperseded
}

// callerReleaseGardener is the open release gardener of project the caller is
// a run of, nil when the caller is not one.
func (s *BeadsServer) callerReleaseGardener(ctx context.Context, project string) *model.Bead {
	agentID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return nil
	}
	agent, err := s.store.GetBead(ctx, agentID)
	if err != nil || agent == nil || agent.Type != "agent" || agent.Status == model.StatusClosed ||
		projectFromFields(agent.Fields) != project || !agentHasRole(agent, releaseCaptainRole) {
		return nil
	}
	trigID := strings.TrimSpace(fieldsStringValue(agent.Fields, triggerLineageField))
	if trigID == "" {
		return nil
	}
	trig, err := s.store.GetBead(ctx, trigID)
	if err != nil || trig == nil || !isAutonomousBeadType(trig.Type) || trig.Status == model.StatusClosed ||
		projectFromLabelsOrEmpty(trig.Labels) != project || triggerSpawnRole(trig) != releaseCaptainRole {
		return nil
	}
	prevRel := releaseGardenerRelease(trig.Labels)
	if prevRel == "" {
		return nil
	}
	if r, err := s.store.GetBead(ctx, prevRel); err != nil || r == nil || r.Type != model.TypeRelease ||
		releaseViewOf(r).Project != project {
		return nil
	}
	return trig
}

// releaseHasSealedGardener reports whether an open trigger other than selfID
// is pinned to relID and validly sealed. A failed lookup reports true, so the
// caller does not seal.
func (s *BeadsServer) releaseHasSealedGardener(ctx context.Context, relID, selfID string) bool {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"trigger"},
		Labels: []string{releaseGardenerLabelPrefix + relID},
		Limit:  100,
	})
	if err != nil {
		return true
	}
	for _, b := range beads {
		if b != nil && b.ID != selfID && b.Status != model.StatusClosed && validSpawnSeal(b) != nil {
			return true
		}
	}
	return false
}

// sealSpawnLineageCarried seals bead to from's author with from's iat, over
// bead's own content.
func sealSpawnLineageCarried(bead *model.Bead, from *spawnLineageSeal) bool {
	if from == nil || !strings.HasPrefix(from.Author, "oidc:") || from.IAT <= 0 {
		return false
	}
	sealJSON, err := json.Marshal(spawnLineageSeal{V: spawnLineageSealVersion, Author: from.Author, Digest: spawnLineageDigest(bead.Fields), IAT: from.IAT})
	if err != nil {
		return false
	}
	fields := fieldsAsMap(bead.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields[spawnLineageField] = string(sealJSON)
	merged, err := json.Marshal(fields)
	if err != nil {
		return false
	}
	bead.Fields = merged
	return true
}
