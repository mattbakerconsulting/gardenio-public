package server

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"gardenio/kbeads/internal/model"
)

// heraldry_gate.go carries the staged warn→enforce heraldry gate (badges plan
// gd-pQZO9ie11D step 2, gd-fn7uPujOqj). It is applied at the createBead choke
// point (beads.go) right after the short_title gate, and modelled on it
// (short_title_gate.go): only a type=epic create is gated, and its warnings
// reach an HTTP caller as X-Beads-Warning headers through the same collector.
//
// Two envs, one per kind of project, because a tenant land is a project on
// the same daemon (channel_validation.go):
//   - BEADS_HERALDRY_GATE is the home garden's mode (code default warn);
//   - BEADS_HERALDRY_GATE_TENANT_LANDS is a tenant land's (code default off),
//     so arms stay a home-garden nudge until a land opts in.
//
// The gate reads nothing from the register (heraldry.go checkHeraldry owns
// that), so it is safe while the register's values are being rewritten.

// heraldryGateMode is the staged rollout posture of the heraldry gate.
type heraldryGateMode int

const (
	// heraldryGateOff — the kill switch: no metric, no warning, no refusal.
	heraldryGateOff heraldryGateMode = iota
	// heraldryGateWarn — the observation window and the home garden's CODE
	// DEFAULT. An epic create with no heraldry is allowed, records
	// action=allowed_warn and returns an X-Beads-Warning to an HTTP caller.
	heraldryGateWarn
	// heraldryGateEnforce — an epic create with no heraldry is REFUSED for a
	// non-human caller (static/fleet/machine) when the epic is a plan (labelled
	// plan) or a phase (its parent_bead_id is a plan). A human caller, and any
	// other epic, still only gets the warning.
	heraldryGateEnforce
)

func (m heraldryGateMode) String() string {
	switch m {
	case heraldryGateWarn:
		return "warn"
	case heraldryGateEnforce:
		return "enforce"
	}
	return "off"
}

// parseHeraldryGateMode parses a heraldry gate env value. "" yields def. The
// truthy set ("1"/"true"/"yes"/"on") and "enforce" select enforce; "warn"
// (and its spellings) selects warn; any other value, including a typo, is
// OFF — a typo can never silently start refusing creates.
func parseHeraldryGateMode(raw string, def heraldryGateMode) heraldryGateMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return def
	case "warn", "observe", "dry-run":
		return heraldryGateWarn
	case "1", "true", "yes", "on", "enforce":
		return heraldryGateEnforce
	}
	return heraldryGateOff
}

// heraldryGateModeFromEnv parses BEADS_HERALDRY_GATE, the home garden's mode.
// Like BEADS_SHORT_TITLE_GATE the default is WARN: an unset (or empty) env
// starts the observation window the moment the daemon rolls.
func heraldryGateModeFromEnv() heraldryGateMode {
	return parseHeraldryGateMode(os.Getenv("BEADS_HERALDRY_GATE"), heraldryGateWarn)
}

// heraldryGateTenantModeFromEnv parses BEADS_HERALDRY_GATE_TENANT_LANDS, a
// tenant land's mode. The default is OFF: a tenant never sees the nudge
// unless the operator turns it on for lands.
func heraldryGateTenantModeFromEnv() heraldryGateMode {
	return parseHeraldryGateMode(os.Getenv("BEADS_HERALDRY_GATE_TENANT_LANDS"), heraldryGateOff)
}

// isHeraldryGatedType reports whether a create of this bead type is held to
// the gate: epics only (plans and phases are epics). Projects may bear arms
// too (heraldryTypes) but are not nudged.
func isHeraldryGatedType(t model.BeadType) bool {
	return t == model.TypeEpic
}

// heraldryMissingWarning is the X-Beads-Warning for a gated create with no
// arms; kd create prints it as "warning: heraldry missing (kd update …)".
func heraldryMissingWarning(id string) string {
	return `heraldry missing (kd update ` + id + ` --heraldry "...")`
}

// heraldryRequiredError is the enforce refusal: the same words, naming the
// create flag since the bead does not exist yet.
func heraldryRequiredError() inputError {
	return inputError(`heraldry missing (kd create --heraldry "...") — a plan or phase epic needs arms: ` + model.HeraldryHelp)
}

// heraldryGateModeFor returns the gate mode for a create carrying labels: the
// tenant-land mode when its project is a tenant land, else the home mode. The
// project lookup runs only when the two modes differ. A failed lookup is
// logged and gets the less strict of the two, so a store blip never refuses.
func (s *BeadsServer) heraldryGateModeFor(ctx context.Context, labels []string) heraldryGateMode {
	home, tenant := heraldryGateModeFromEnv(), heraldryGateTenantModeFromEnv()
	if home == tenant {
		return home
	}
	project := projectFromLabelsOrEmpty(labels)
	if project == "" {
		return home
	}
	isTenant, err := s.isTenantLand(ctx, project)
	if err != nil {
		slog.Warn("heraldry gate: tenant land lookup", "project", project, "err", err)
		return min(home, tenant)
	}
	if isTenant {
		return tenant
	}
	return home
}

// isPlanEpicCreate reports whether an epic create is a plan (labelled plan)
// or a phase (its parent_bead_id is a plan epic). The parent is read only when
// the labels do not already answer it; a failed read counts as not a plan.
func (s *BeadsServer) isPlanEpicCreate(ctx context.Context, in createBeadInput) bool {
	if hasLabel(in.Labels, planMarkerLabel) {
		return true
	}
	if in.ParentBeadID == "" {
		return false
	}
	parent, err := s.store.GetBead(ctx, in.ParentBeadID)
	if err != nil || parent == nil {
		return false
	}
	return parent.Type == model.TypeEpic && hasLabel(parent.Labels, planMarkerLabel)
}

// applyHeraldryGate runs the heraldry gate for a create of beadType with the
// new bead's id. It returns the enforce refusal, else nil.
//
//	off     → no gate, no metric.
//	warn    → never refuses; records action=allowed_warn and adds an
//	          X-Beads-Warning for an HTTP caller.
//	enforce → refuses a non-human caller's plan or phase epic
//	          (action=refused); a human caller, or another epic, is warned.
//
// action=supplied counts the compliant creates so the ratio is observable.
func (s *BeadsServer) applyHeraldryGate(ctx context.Context, in createBeadInput, beadType model.BeadType, id string) error {
	if !isHeraldryGatedType(beadType) {
		return nil
	}
	mode := s.heraldryGateModeFor(ctx, in.Labels)
	if mode == heraldryGateOff {
		return nil
	}
	caller := heraldryGateCallerHuman
	if isNonHumanCaller(AuthIdentityFromContext(ctx)) {
		caller = heraldryGateCallerAgent
	}
	switch {
	case in.Heraldry != "":
		recordHeraldryGate(caller, heraldryGateActionSupplied)
	case mode == heraldryGateEnforce && caller == heraldryGateCallerAgent && s.isPlanEpicCreate(ctx, in):
		recordHeraldryGate(caller, heraldryGateActionRefused)
		return heraldryRequiredError()
	default:
		recordHeraldryGate(caller, heraldryGateActionAllowedWarn)
		addWarning(ctx, heraldryMissingWarning(id))
		slog.Warn("heraldry missing", "type", beadType, "id", id, "actor", actorFromContext(ctx), "mode", mode.String())
	}
	return nil
}
