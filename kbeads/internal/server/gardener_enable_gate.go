// R6 gardener-enable gate — daemon-side pre-flight on the gardener ENABLE write.
//
// A /plans plan epic moves through an authoring lifecycle (plan_state: draft →
// ready → building → complete). While the plan is still being authored
// (plan_state ∈ {draft, ready}) its bound gardener must NOT be enabled, so a
// half-baked plan can never auto-build behind the operator's back — "Approve &
// build" (the ready→building transition) is the sole gesture that enables the
// gardener. The /plans cockpit already enforces this in its UI
// (gardenerEnableGatedByState in cmd/beads-viewer/plans_lifecycle.go); this file
// mirrors that gate at the daemon's updateBead / createBead choke point so NO
// API caller — not just the cockpit — can enable a gardener on a draft/ready
// plan (task ka-0000006796, epic ka-0000006896, risk R6).
//
// The gate binds a gardener to its plan via the `gardener-epic:<plan-id>` label
// (the plangardener.GardenerEpicLabel convention). A trigger/schedule bead with
// no such label is not a plan gardener and is never gated; a gardener bound to a
// non-plan epic (plan_state unset) is ungated too — backward compatibility with
// generic epic gardeners and legacy/hand-marked plans is load-bearing (honoring
// ka-0000002616, exactly as the sibling plan_state guard does).
//
// It gates only the transition INTO enabled: pausing (enabled→disabled) is
// always allowed, and the frequent operational writes the engine stamps on an
// already-(dis)abled gardener (last_run, last_agent_id, …) never trip it because
// the effective enabled state is unchanged. So the extra plan-epic lookup only
// happens on a genuine enable, which is a rare operator action.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// gardenerEpicLabelPrefix pins a gardener trigger/schedule bead to the plan epic
// it drives. Kept in sync with plangardener.GardenerEpicLabel
// ("gardener-epic:"+epicID) and the viewer's gardenerEpicLabelFor.
const gardenerEpicLabelPrefix = "gardener-epic:"

// roleGardenerLabel marks a trigger/schedule bead as a gardener loop, independent
// of its spawn `role` field (a plan gardener sets role=plan-gardener so its
// iterations take that role, yet the bead itself is still a gardener). Mirrors
// plangardener.IsGardenerBead / the viewer's isGardenerBeadView.
const roleGardenerLabel = "role:gardener"

// gardenerEnableGatedByPlanState reports whether a plan's lifecycle state forbids
// ENABLING its bound gardener — the R6 gate. It mirrors the viewer's
// gardenerEnableGatedByState (cmd/beads-viewer/plans_lifecycle.go) exactly:
// draft and ready are gated (still authoring); building/complete are past the
// approve→build gate and unset never opted in, so both are ungated.
func gardenerEnableGatedByPlanState(st planStateValue) bool {
	switch st {
	case planStateDraft, planStateReady:
		return true
	default: // planStateUnset (ungated) / planStateBuilding / planStateComplete / planStateAbandoned
		return false
	}
}

// fieldsAsMap unmarshals a raw fields blob into a generic map, degrading an
// empty or malformed blob to nil (the structural validator upstream is what
// rejects bad JSON). Shared by the gardener-bead classifier and the
// effective-enabled reader.
func fieldsAsMap(fields json.RawMessage) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		return nil
	}
	return m
}

// isGardenerBead reports whether a trigger/schedule bead is a gardener loop. It
// keys off the gardener markers, NOT the spawn-role field (a gardener may set
// role=<other> so its iterations take that role), mirroring
// plangardener.IsGardenerBead so the daemon gate and the viewer resolvers never
// disagree about what a gardener is. Only trigger/schedule beads can be
// gardeners; every other type short-circuits false.
func isGardenerBead(beadType model.BeadType, labels []string, fields json.RawMessage) bool {
	if beadType != "trigger" && beadType != "schedule" {
		return false
	}
	if slices.Contains(labels, roleGardenerLabel) {
		return true
	}
	m := fieldsAsMap(fields)
	if r, _ := m["role"].(string); r == "gardener" {
		return true
	}
	if c, _ := m["gardener"].(string); strings.TrimSpace(c) != "" {
		return true
	}
	return false
}

// effectiveGardenerEnabled computes whether a gardener bead is EFFECTIVELY enabled
// from its raw fields, honoring the asymmetric loader defaults the trigger
// engine uses (internal/bridge/trigger_source_*.go) and the viewer reads:
//
//   - trigger:  enabled UNLESS the enabled field is explicitly false
//     (absent ⇒ enabled) — the trigger engine's `enabled == "false"` skip.
//   - schedule: enabled IFF the enabled field is explicitly true
//     (absent ⇒ disabled).
//
// The `enabled` field is schema-declared boolean (config.go), so a well-formed
// write stores a JSON bool; the string forms "true"/"false" are also accepted
// defensively in case a legacy/grandfathered bead carries them.
func effectiveGardenerEnabled(beadType model.BeadType, fields json.RawMessage) bool {
	m := fieldsAsMap(fields)
	var isTrue, isFalse bool
	switch v := m["enabled"].(type) {
	case bool:
		isTrue, isFalse = v, !v
	case string:
		isTrue, isFalse = v == "true", v == "false"
	}
	if beadType == "schedule" {
		// Enabled only when explicitly true (absent ⇒ disabled).
		return isTrue
	}
	// trigger (and any gardener type that defaults on): enabled unless explicitly
	// false (absent ⇒ enabled).
	return !isFalse
}

// gardenerEpicFromLabels extracts the bound plan epic id from a gardener bead's
// `gardener-epic:<plan-id>` label, or "" when the bead carries none (not a
// plan-bound gardener). The first well-formed label wins.
func gardenerEpicFromLabels(labels []string) string {
	for _, l := range labels {
		if id, ok := strings.CutPrefix(l, gardenerEpicLabelPrefix); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
	}
	return ""
}

// gardenerEnableGateCheck is the shared verdict: given a gardener bead that is
// (about to be) effectively enabled, reject the write when the gardener is bound
// to a plan epic whose lifecycle state forbids enabling (draft/ready). It is
// best-effort on the plan lookup: a gardener with no gardener-epic label (generic
// epic gardener / non-plan trigger), a dangling label whose plan is gone, or a
// transient lookup error all degrade to ALLOW — the gate is defense-in-depth
// mirroring the front-line cockpit gate, so it must never wedge gardener writes
// on an unrelated failure. Returns an inputError (→ HTTP 400 / InvalidArgument)
// when the enable is gated.
func (s *BeadsServer) gardenerEnableGateCheck(ctx context.Context, labels []string) error {
	planID := gardenerEpicFromLabels(labels)
	if planID == "" {
		return nil // not a plan-bound gardener — nothing to gate
	}
	plan, err := s.store.GetBead(ctx, planID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("gardener-enable gate: plan lookup failed; allowing enable",
				"plan", planID, "error", err)
		}
		return nil // plan gone or unreadable — cannot determine the gate, allow
	}
	if plan == nil {
		return nil // plan not found — dangling label, allow
	}
	st := planStateFromFields(plan.Fields)
	if !gardenerEnableGatedByPlanState(st) {
		return nil // building / complete / unset — enabling is permitted
	}
	return inputError(fmt.Sprintf(
		"gardener enable blocked: plan %s is %s, not yet approved for build — "+
			"mark it ready and use \"Approve & build\" (ready→building) before enabling its gardener",
		planID, st))
}

// validateGardenerEnableTransition is the updateBead pre-flight. It fires only on
// the transition INTO effectively-enabled: an already-enabled gardener (the
// dominant operational-write case) and a write that leaves the gardener disabled
// (a pause, or an unrelated field patch on a paused gardener) both pass without
// even looking up the plan. priorFields is the byte-identical pre-patch blob;
// nextFields is the merged post-patch blob.
func (s *BeadsServer) validateGardenerEnableTransition(ctx context.Context, beadType model.BeadType, labels []string, priorFields, nextFields json.RawMessage) error {
	if !isGardenerBead(beadType, labels, nextFields) {
		return nil
	}
	if effectiveGardenerEnabled(beadType, priorFields) {
		return nil // already enabled — not an enable transition
	}
	if !effectiveGardenerEnabled(beadType, nextFields) {
		return nil // not being enabled (pause / unrelated write)
	}
	return s.gardenerEnableGateCheck(ctx, labels)
}

// validateGardenerEnableOnCreate is the createBead pre-flight: reject a gardener
// born effectively-enabled while bound to a still-authoring plan. The plan
// gardener bind path already births such gardeners paused
// (plangardener.EnabledOnBind), so this closes the direct-API-create bypass and
// makes "no API caller can enable a gardener on a draft/ready plan" hold at the
// daemon rather than only in the cockpit.
func (s *BeadsServer) validateGardenerEnableOnCreate(ctx context.Context, beadType model.BeadType, labels []string, fields json.RawMessage) error {
	if !isGardenerBead(beadType, labels, fields) {
		return nil
	}
	if !effectiveGardenerEnabled(beadType, fields) {
		return nil // born paused — fine on any plan state
	}
	return s.gardenerEnableGateCheck(ctx, labels)
}
