package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/kbeads/internal/model"
)

// The untrusted-input taint on tenant lands (Lands Track B spec §5.1, step
// 10.1 gd-g77WcV22SK; docs/adr/taints.md §15.7). Every bead created in a
// tenant land (a project bead with land_kind=tenant) carries it from
// creation, as if the land's intake_taints named it
// (resolveProjectIntakeTaints): a stranger wrote it, or an agent that read
// what a stranger wrote did. Its effect is restrict, so it gates no read;
// inherit:read carries it onto every agent that reads such a bead, and the
// shared restricted-ops check (restricted_ops.go) then refuses that agent
// the restricted ops until a reviewer lifts it or its owner grants it.
//
// The registry needs a live owner team, so the daemon does not mint the taint;
// the operator creates it once:
//
//	kd taint create untrusted-input --owner <team> --effect restrict
//
// Until it exists, or while it is unusable (no restrict effect, or a gate
// effect, which would hide a land's Commons from its own members), the daemon
// fails closed rather than create a tenant bead without it (gd-Ms3cmrdW49):
// a bead create in a tenant land is refused, and so is making a project bead a
// tenant land, each counted in gardenio_lands_untrusted_input_unusable_total.
// The registry never shrinks, so refusing the land itself is what covers an
// empty registry, where the create path looks no project up (ADR §10.1).

// untrustedInputTaint is the taint's registry name.
const untrustedInputTaint = "untrusted-input"

// deniedReasonUntrustedInputUnusable is AuthzDenial.DeniedReason for a write
// refused because the untrusted-input taint is unusable.
const deniedReasonUntrustedInputUnusable = "untrusted_input_unusable"

// Reasons the taint is unusable: the reason label of
// gardenio_lands_untrusted_input_unusable_total.
const (
	untrustedInputUnregistered = "unregistered"
	untrustedInputNoRestrict   = "no_restrict_effect"
	untrustedInputGate         = "gate_effect"
)

// landsUntrustedInputUnusableTotal counts tenant-land writes refused because
// the untrusted-input taint is unusable, by land and reason. Any increase
// means a land is wedged until the operator fixes the taint (alert
// LandUntrustedInputUnusable).
var landsUntrustedInputUnusableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_lands_untrusted_input_unusable_total",
		Help: "Tenant-land writes refused because the untrusted-input taint is unregistered or not restrict-only, by land and reason.",
	},
	[]string{"land", "reason"},
)

// untrustedInputUnusableReason reports why t (nil: not registered) may not be
// applied to tenant beads, or "" when it may: restrict, and never gate.
func untrustedInputUnusableReason(t *model.Taint) string {
	switch {
	case t == nil:
		return untrustedInputUnregistered
	case model.HasEffect(t.Effects, model.EffectGate):
		return untrustedInputGate
	case !model.HasEffect(t.Effects, model.EffectRestrict):
		return untrustedInputNoRestrict
	}
	return ""
}

// validateTenantLandUntrustedInput refuses a project write that makes the
// bead a tenant land (land_kind=tenant on create, or set by this update; prior
// is nil on create) while the taint is unusable. An existing tenant land's own
// edits, a freeze among them, are never refused here.
func (s *BeadsServer) validateTenantLandUntrustedInput(ctx context.Context, bead *model.Bead, prior json.RawMessage, action string) error {
	if !tenantLandFields(bead.Fields) || (prior != nil && tenantLandFields(prior)) {
		return nil
	}
	t, err := s.store.GetTaint(ctx, untrustedInputTaint)
	if errors.Is(err, sql.ErrNoRows) {
		t = nil
	} else if err != nil {
		return fmt.Errorf("looking up the %s taint: %w", untrustedInputTaint, err)
	}
	if reason := untrustedInputUnusableReason(t); reason != "" {
		return untrustedInputUnusableDenial(landNameFromTitle(bead.Title), action, "project", reason)
	}
	return nil
}

// untrustedInputUnusableDenial counts and logs one refused tenant-land write
// and returns its denial: 403, not escalatable, since only the operator's
// fix to the taint clears it.
func untrustedInputUnusableDenial(land, action, beadType, reason string) *AuthzDenial {
	landsUntrustedInputUnusableTotal.WithLabelValues(land, reason).Inc()
	slog.Error("lands: tenant-land write refused, the untrusted-input taint is unusable",
		"land", land, "action", action, "bead_type", beadType, "reason", reason,
		"fix", "kd taint create "+untrustedInputTaint+" --owner <team> --effect restrict")
	return &AuthzDenial{
		Action:       action,
		BeadType:     beadType,
		DeniedReason: deniedReasonUntrustedInputUnusable,
		Message: action + " denied: land " + strconvQuote(land) + " is a tenant land and the " + untrustedInputTaint +
			" taint is unusable (" + reason + "), so its beads cannot be marked as untrusted; the operator runs " +
			"kd taint create " + untrustedInputTaint + " --owner <team> --effect restrict",
	}
}

// untrustedInputCreateError is the taint registry's check on a create of the
// untrusted-input taint: it must be usable, so the tenant lands it guards
// are never wedged by it.
func untrustedInputCreateError(t *model.Taint) string {
	if t.Name != untrustedInputTaint {
		return ""
	}
	if reason := untrustedInputUnusableReason(t); reason != "" {
		return "taint " + untrustedInputTaint + " must have the restrict effect and not gate (" + reason +
			"): it marks every tenant-land bead, see docs/adr/taints.md §15.7"
	}
	return ""
}
