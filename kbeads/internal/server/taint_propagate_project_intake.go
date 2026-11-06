package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"gardenio/kbeads/internal/model"
)

// resolveProjectIntakeTaints implements docs/adr/taints.md §10.3: every bead
// created under a project whose config bead declares intake_taints carries
// those taints from creation, with source TaintSourceInheritProjectIntake so
// the propagation-source UI (p1-bead-badges, p2-viewer-taint-detail) can
// distinguish "applied by project configuration" from agent-caused
// inheritance (inherit:create/inherit:parent) or an explicit grant.
//
// projectName is resolved by the caller from the new bead's own labels/fields
// (project:<name> label, falling back to fields.project — the same
// labels-then-fields precedent as resolveDedupProject) BEFORE the bead
// itself exists, since this runs inside resolveCreateTaints, ahead of the
// insert. An empty projectName, a project bead that doesn't exist yet (e.g.
// bootstrapping), or a project with no intake_taints are all no-ops — this
// never fails bead creation, but for the tenant land case below.
//
// A tenant land (land_kind=tenant) also names untrusted-input, and fails
// closed on it: while that taint is unregistered or not restrict-only, the
// create is refused (untrustedInputUnusableDenial, taint_untrusted_input.go)
// rather than born without it.
//
// Any other taint name in intake_taints that no longer resolves via GetTaint
// (e.g. the taint was deleted after the project bead was validated at write
// time, see validateProjectIntakeTaints) is skipped rather than erroring the
// create — project-write-time validation is what enforces "names must exist"
// (decision gd-H9L64SF3Bk); a later dangling reference is a project-bead
// hygiene issue, not a reason to block every subsequent create under it.
//
// Shaped so a later per-source predicate (gd-IyeXddvOPZ, ADR §15.5 — e.g.
// "only taint beads a bridge creates, not ones created directly") can be
// layered in by filtering the parsed intake_taints entries before the
// GetTaint loop, without changing this function's signature or callers.
//
// Checks taintRegistryNonEmpty first (ADR §10.1's zero-configuration
// short-circuit, the same gate taintGateGrants uses): an install with no
// taints created yet never issues a FindProjectByName/GetTaint query on this
// path, so a fleet that hasn't adopted taints pays nothing per create. A
// tenant land cannot exist on such an install: making a project bead one is
// refused while untrusted-input is unusable (validateTenantLandUntrustedInput).
func (s *BeadsServer) resolveProjectIntakeTaints(ctx context.Context, projectName string) ([]pendingBeadTaint, error) {
	if projectName == "" {
		return nil, nil
	}

	nonEmpty, err := s.taintRegistryNonEmpty(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking taint registry: %w", err)
	}
	if !nonEmpty {
		return nil, nil
	}

	project, err := s.store.FindProjectByName(ctx, projectName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolving project bead for intake taints: %w", err)
	}

	names := parseIntakeTaints(project.Fields)
	tenant := tenantLandFields(project.Fields)
	if tenant && !slices.Contains(names, untrustedInputTaint) {
		names = append(names, untrustedInputTaint)
	}
	if len(names) == 0 {
		return nil, nil
	}

	var pending []pendingBeadTaint
	for _, name := range names {
		taint, err := s.store.GetTaint(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			taint = nil
		} else if err != nil {
			return nil, fmt.Errorf("resolving intake taint %q: %w", name, err)
		}
		if tenant && name == untrustedInputTaint {
			if reason := untrustedInputUnusableReason(taint); reason != "" {
				return nil, untrustedInputUnusableDenial(projectName, "create", "", reason)
			}
		}
		if taint == nil {
			continue
		}
		pending = append(pending, pendingBeadTaint{
			TaintID:      taint.ID,
			Source:       model.TaintSourceInheritProjectIntake,
			SourceBeadID: project.ID,
		})
	}
	return pending, nil
}

// parseIntakeTaints decodes the type:project intake_taints field ([]string,
// e.g. ["jira-prod"]). Any absent/malformed field decodes to no names —
// shape is enforced at project write time (validateProjectIntakeTaints), not
// re-validated on every bead create.
func parseIntakeTaints(fields json.RawMessage) []string {
	if len(fields) == 0 {
		return nil
	}
	var f struct {
		IntakeTaints []string `json:"intake_taints"`
	}
	if json.Unmarshal(fields, &f) != nil {
		return nil
	}
	return f.IntakeTaints
}
