package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// A plan's gardener may idle its own trigger until a named bead closes (step
// P-G2 of plan gd-fRmqrpHIwk; runbook kbeads/docs/ownership-grants.md,
// "Gardener self-idle"). The row is gardener-self-idle in the grant table. It
// names fields, so authorizeOwnership (which never sees the patch) skips it and
// the update path checks the patch here.
//
// The wake is an event, never a timer: closing the awaited bead turns the
// trigger back on (wakeGardenersAwaiting, run from afterBeadClosed).

const (
	// gardenerAwaitingField names the bead whose close turns an idled
	// gardener back on.
	gardenerAwaitingField = "awaiting"
	gardenerWakeOnField   = "wake_on"
	gardenerSelfIdleGrant = "gardener-self-idle"
	gardenerWakeActor     = "system:gardener-wake"
)

// gardenerSelfIdleFields are the only fields the row lets a gardener write.
var gardenerSelfIdleFields = []string{"enabled", "cron", gardenerWakeOnField, gardenerAwaitingField}

// callerRunsGardenerTrigger reports whether the caller is an iteration of the
// gardener trigger bead: it writes on its own per-agent token, its agent bead
// is open in the token's project and names bead as its trigger_id (a link an
// agent token may not change, planScopeBindingPatchDenial), and bead is pinned
// by gardener-epic:<plan> to an open plan of that project (agentIsPlanGardener).
func (s *BeadsServer) callerRunsGardenerTrigger(ctx context.Context, bead *model.Bead) bool {
	if bead == nil || bead.Status == model.StatusClosed || !isGardenerBead(bead.Type, bead.Labels, bead.Fields) {
		return false
	}
	agentBeadID, ok := agentBeadIDFromContext(ctx)
	project := TokenProjectFromContext(ctx)
	if !ok || project == "" || projectFromLabelsOrEmpty(bead.Labels) != project {
		return false
	}
	agent, err := s.store.GetBead(ctx, agentBeadID)
	if err != nil || agent == nil || agent.Type != "agent" || agent.Status == model.StatusClosed ||
		projectFromFields(agent.Fields) != project ||
		strings.TrimSpace(fieldsStringValue(agent.Fields, triggerLineageField)) != bead.ID {
		return false
	}
	plan, err := s.store.GetBead(ctx, gardenerEpicFromLabels(bead.Labels))
	if err != nil || plan == nil || plan.Type != model.TypeEpic || !hasLabel(plan.Labels, planMarkerLabel) ||
		plan.Status == model.StatusClosed {
		return false
	}
	return s.agentIsPlanGardener(ctx, plan, agent)
}

// gardenerMayIdleOwnTrigger reports whether the patch is a gardener idling its
// own trigger: it changes only gardenerSelfIdleFields, sets awaiting to an open
// bead, and does not turn the trigger on (only the awaited close does that).
// The bead is the trigger before the patch.
func (s *BeadsServer) gardenerMayIdleOwnTrigger(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || !patchOnlyChangesFields(gardenerSelfIdleFields, in, bead.Fields) {
		return false
	}
	patch := fieldsAsMap(in.Fields)
	awaiting, _ := patch[gardenerAwaitingField].(string)
	if strings.TrimSpace(awaiting) == "" || awaiting != strings.TrimSpace(awaiting) || awaiting == bead.ID {
		return false
	}
	if v, set := patch["enabled"]; set && v != false && v != "false" {
		return false
	}
	awaited, err := s.store.GetBead(ctx, awaiting)
	if err != nil || awaited == nil || awaited.Status == model.StatusClosed {
		return false
	}
	return s.callerRunsGardenerTrigger(ctx, bead)
}

// carrySpawnSealAcrossSelfIdle re-stamps the seal the trigger held before a
// self-idle patch over the patched content, with the same author and iat, so a
// cron change does not stop the trigger firing and the seal's age is not reset.
// It runs on the final merged fields, and only when the patch voided the seal.
func carrySpawnSealAcrossSelfIdle(bead *model.Bead, prior *spawnLineageSeal) bool {
	if prior == nil || validSpawnSeal(bead) != nil {
		return false
	}
	sealJSON, err := json.Marshal(spawnLineageSeal{V: spawnLineageSealVersion, Author: prior.Author, Digest: spawnLineageDigest(bead.Fields), IAT: prior.IAT})
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

// wakeGardenersAwaiting turns back on every open gardener trigger idled on the
// bead that just closed, and clears its awaiting field. Best-effort, like the
// rest of the close cascade. The seal survives: neither field is spawn content,
// and this write does not pass through updateBead's reactivation rule.
func (s *BeadsServer) wakeGardenersAwaiting(ctx context.Context, closed *model.Bead) {
	triggers, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"trigger", "schedule"},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred},
		Fields: map[string]string{gardenerAwaitingField: closed.ID},
	})
	if err != nil {
		slog.Warn("gardener wake: list awaiting triggers failed", "closed", closed.ID, "err", err.Error())
		return
	}
	for _, trig := range triggers {
		fields := fieldsAsMap(trig.Fields)
		if awaiting, _ := fields[gardenerAwaitingField].(string); awaiting != closed.ID {
			continue
		}
		old := string(trig.Fields)
		delete(fields, gardenerAwaitingField)
		fields["enabled"] = true
		merged, err := json.Marshal(fields)
		if err != nil {
			continue
		}
		trig.Fields = merged
		trig.UpdatedAt = time.Now().UTC()
		if err := s.store.UpdateBead(ctx, trig); err != nil {
			slog.Warn("gardener wake: re-enable failed", "trigger", trig.ID, "closed", closed.ID, "err", err.Error())
			continue
		}
		s.recordAndPublish(ctx, events.TopicBeadUpdated, trig.ID, gardenerWakeActor, events.BeadUpdated{
			Bead:    trig,
			Changes: map[string]any{"fields": trig.Fields, "enabled": true, gardenerAwaitingField: nil},
			Actor:   gardenerWakeActor,
		})
		s.recordMutation(ctx, trig.ID, gardenerWakeActor, "fields", old, string(trig.Fields), model.MutationTypeUpdate)
		s.addAutoComment(ctx, trig.ID, fmt.Sprintf("re-enabled: %s, which this gardener was awaiting, closed.", closed.ID))
		slog.Info("gardener wake: re-enabled trigger on awaited close", "trigger", trig.ID, "closed", closed.ID)
	}
}
