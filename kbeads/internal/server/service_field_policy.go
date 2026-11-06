package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/kbeads/internal/model"
)

// This file gates the service-managed-fields ownership bypass
// (serviceManagedFieldsByType, ka-0000003039) for SHARED cross-environment
// bead types. Root cause it closes (ka-0000006237 / epic ka-0000005107): a
// sandbox jira-bridge wired to the prod daemon loaded prod trigger:jira beads
// and used the bypass to stamp `last_checked` ~10s before every prod poll,
// permanently tripping prod's cooldown gate — the prod bridge never fired
// again. The bypass let ANY fleet token advance last_checked on ANY trigger
// bead; this policy narrows it to designated writer fleets.

// serviceFieldGatedTypes are the bead types whose service-managed-field
// ownership bypass is subject to the writer-fleet policy. trigger and
// schedule beads are long-lived SHARED beads that a misconfigured foreign
// environment can reach and starve (the outage class). The agent-type bypass
// (context_tokens / live_cost_usd) is per-agent plumbing and stays ungated.
var serviceFieldGatedTypes = map[model.BeadType]bool{
	"trigger":  true,
	"schedule": true,
}

// gardenio_service_field_crossfleet_write_total is Alert A of the
// ka-0000005107 prevention plan: it increments whenever an ACCEPTED
// service-managed-field bypass write on a gated bead type comes from a fleet
// other than the bead's owning fleet. Healthy prod polls DO emit this
// (trigger beads are fleet="default", the prod bridge writes as
// fleet="jira-bridge"), so the paging rule must filter writer_fleet to
// values OUTSIDE the expected prod-writer set — a naive rate()>0 would page
// on every healthy poll. See the serviceFieldCrossFleet PrometheusRule in
// the gardenio Helm chart.
//
// Labels are bounded: bead_type goes through authzBeadTypeLabel (hard
// allowlist), and fleet names come from operator-registered bridge tokens
// plus "default" — not user-supplied input.
var serviceFieldCrossFleetWrite = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_service_field_crossfleet_write_total",
		Help: "Accepted service-managed-field bypass writes (trigger/schedule) where the writing fleet differs from the bead's owning fleet. Page on writer_fleet outside the expected bridge set.",
	},
	[]string{"bead_type", "bead_fleet", "writer_fleet"},
)

// fleetMetricLabel normalizes a fleet name for metric labels. An empty fleet
// (legacy rows) collapses to "default" to match FleetFromContext semantics.
func fleetMetricLabel(f string) string {
	if f == "" {
		return "default"
	}
	return f
}

// hasProjectLabel reports whether the labels carry any non-empty
// "project:<name>" label. Distinct from projectFromLabels, which returns the
// sentinel "unknown" for unlabeled beads — the require-project gate must key
// off true ABSENCE of a project label.
func hasProjectLabel(labels []string) bool {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			return true
		}
	}
	return false
}

// SetServiceFieldPolicy configures the writer-fleet policy for the
// service-managed-fields ownership bypass on gated bead types
// (trigger/schedule). Call once at startup before serving.
//
//   - fleets (BEADS_SERVICE_FIELD_FLEETS): non-privileged FLEET tokens get the
//     bypass only when their fleet name is listed. Empty = allow all fleets
//     (back-compat no-op for deployments that have not set it). A "*" entry
//     also means allow-all — the explicit opt-out for Helm overlays, where an
//     empty value would fall back to the chart default.
//   - requireProject (BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT): when true,
//     an UNRESTRICTED fleet token (no project scope) is additionally refused
//     the bypass on any project-labeled gated bead. Default off — flip only
//     after the deployment's own bridge tokens are project-scoped, or the
//     gate denies the legitimate writer too (see ka-0000006237 design notes).
func (s *BeadsServer) SetServiceFieldPolicy(fleets []string, requireProject bool) {
	set := make(map[string]bool, len(fleets))
	for _, f := range fleets {
		f = strings.TrimSpace(f)
		if f == "*" {
			// Explicit allow-all opt-out (e.g. a Helm overlay that cannot
			// express "empty" past the chart default).
			set = nil
			break
		}
		if f != "" {
			set[f] = true
		}
	}
	s.serviceFieldFleets = set
	s.serviceFieldRequireProject = requireProject
}

// authorizeServiceFieldBypass decides whether a bypass-eligible update (the
// patch touches ONLY service-managed fields for the bead's type) may actually
// skip the ownership gate. Called from updateBead INSTEAD of
// authorizeOwnership when patchOnlyChangesServiceManagedFields is true.
//
// Non-fleet callers (OIDC users, the primary static token) and PRIVILEGED
// fleet tokens keep the unconditional bypass — unchanged behavior. Ordinary
// fleet tokens writing a gated type (trigger/schedule) must pass the
// writer-fleet allowlist (and, when enabled, the project-scope requirement);
// on refusal the update falls back to the plain ownership gate, so a fleet
// that legitimately owns the bead is still allowed.
//
// Accepted cross-fleet writes emit the Alert-A crossfleet metric so a foreign
// environment stamping shared beads is visible the instant it happens instead
// of after 400+ starved poll cycles (ka-0000005107).
func (s *BeadsServer) authorizeServiceFieldBypass(ctx context.Context, action string, bead *model.Bead) *AuthzDenial {
	id := AuthIdentityFromContext(ctx)
	// A machine principal (gd-gQ4iLIpWCm) is ownership-gated like an ordinary
	// fleet token, not "non-fleet": it gets no unconditional service-field
	// bypass. A future capability grant may widen this deliberately.
	if id.Type == "machine" {
		return s.authorizeOwnership(ctx, action, bead)
	}
	if id.Type != "fleet" || id.Privileged {
		return nil
	}
	if !serviceFieldGatedTypes[bead.Type] {
		return nil
	}
	fleet := FleetFromContext(ctx)
	if len(s.serviceFieldFleets) > 0 && !s.serviceFieldFleets[fleet] {
		slog.Warn("service-managed-field bypass refused: writer fleet not in BEADS_SERVICE_FIELD_FLEETS — falling back to ownership gate",
			"bead", bead.ID, "bead_type", bead.Type,
			"writer_fleet", fleet, "writer_subject", id.Subject)
		return s.authorizeOwnership(ctx, action, bead)
	}
	if s.serviceFieldRequireProject && TokenProjectFromContext(ctx) == "" && hasProjectLabel(bead.Labels) {
		slog.Warn("service-managed-field bypass refused: BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT is on and the writer token has no project scope — falling back to ownership gate",
			"bead", bead.ID, "bead_type", bead.Type,
			"writer_fleet", fleet, "writer_subject", id.Subject)
		return s.authorizeOwnership(ctx, action, bead)
	}
	if fleet != bead.Fleet {
		serviceFieldCrossFleetWrite.WithLabelValues(
			authzBeadTypeLabel(string(bead.Type)),
			fleetMetricLabel(bead.Fleet),
			fleetMetricLabel(fleet),
		).Inc()
	}
	return nil
}
