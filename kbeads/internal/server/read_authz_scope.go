package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file adds cross-project read-scope enforcement to the by-id, batch,
// and aggregation read endpoints that build their own ListBeads /
// GetBeadsByIDs / SemanticSearch filters (or fetch a bead by id directly).
//
// Background (ka-0000008132, sibling of ka-0000003827 / MR !7235): the store
// layer applies NO implicit project scoping — read scope is entirely
// filter-driven from the server layer. handleGetBead / handleListBeads apply a
// two-gate check (a project-bound fleet token's required project: label, AND an
// OIDC user's allowed_projects), but a parallel family of RoleViewer read
// handlers skipped it, leaking scoped bead content to an out-of-scope OIDC user
// or project-bound fleet token. These helpers factor that exact two-gate check
// so every such handler enforces it identically.

// scopeListFilter appends the caller's project-scope predicates to a
// hand-built BeadFilter destined for store.ListBeads or store.SemanticSearch,
// mirroring handleListBeads (http_beads.go):
//
//   - a project-bound fleet token's required "project:<name>" label is ANDed
//     in via filter.Labels (every result must carry it);
//   - an OIDC user's allowed projects are ORed in via filter.LabelsAny (a
//     result matches when it carries AT LEAST ONE), narrowing — never widening
//     — the result set to the caller's authorized projects.
//
// A static/primary token (no token project) and an unrestricted OIDC user
// (admin, wildcard, no user bead, empty allowed_projects) both leave the filter
// untouched, preserving their existing unrestricted read access.
// It also installs the taint read gate's list predicate (ADR docs/adr/taints.md
// §5.1/§10.1): a THIRD, narrower check that a caller who passes both project
// gates above must still satisfy — see applyTaintGateToFilter. A tenant
// caller's list also excludes the types it may not read (read_authz_tenant.go).
func (s *BeadsServer) scopeListFilter(ctx context.Context, filter model.BeadFilter) model.BeadFilter {
	if proj := TokenProjectFromContext(ctx); proj != "" {
		_, isAgent := agentBeadIDFromContext(ctx)
		switch {
		case isAgent && filterOnlyLabelLessReadable(filter):
			// A per-agent token listing only advice and/or config also sees the
			// label-less (Global / role-only) beads: its project label OR no
			// project label. A tenant land's agent sees only the tenant-safe
			// ones: its project label OR tenantSafeLabel (read_authz_tenant.go).
			filter.LabelsOrNoProject = append(filter.LabelsOrNoProject, "project:"+proj)
			if s.callerIsTenant(ctx) {
				filter.LabelsAny = append(filter.LabelsAny, "project:"+proj, tenantSafeLabel)
			}
		case isAgent && filterOnlyProject(filter) && narrowTitlesToOwnProject(&filter, proj):
			// A project-only list sees its OWN project's label-less type:project
			// bead (matched by title), never another project's.
			filter.LabelsOrNoProject = append(filter.LabelsOrNoProject, "project:"+proj)
		case isAgent && filterOnlyAudit(filter) && pinOwnAuditActor(ctx, &filter):
			// An audit-only list sees the label-less audit beads recording its
			// OWN actions (fields.actor == its identity), never another actor's.
			filter.LabelsOrNoProject = append(filter.LabelsOrNoProject, "project:"+proj)
		default:
			filter.Labels = append(filter.Labels, "project:"+proj)
		}
	}
	if allowed := oidcAllowedProjects(ctx, s.store); allowed != nil {
		filter.LabelsAny = append(filter.LabelsAny, oidcProjectLabels(allowed)...)
	}
	filter.ExcludeTypes = append(filter.ExcludeTypes, s.tenantHiddenReadTypes(ctx)...)
	return s.applyTaintGateToFilter(ctx, filter)
}

// filterOnlyLabelLessReadable reports whether the filter's type predicate admits
// nothing but advice and/or config, so the label-less read exemption can never
// leak another type. A filter with no type predicate returns false.
func filterOnlyLabelLessReadable(filter model.BeadFilter) bool {
	if len(filter.Type) == 0 {
		return false
	}
	for _, t := range filter.Type {
		if !labelLessReadableType(t) {
			return false
		}
	}
	return true
}

// filterOnlyProject reports whether the filter's type predicate is exactly
// type:project.
func filterOnlyProject(filter model.BeadFilter) bool {
	return len(filter.Type) == 1 && filter.Type[0] == model.BeadType("project")
}

// filterOnlyAudit reports whether the filter's type predicate is exactly
// type:audit.
func filterOnlyAudit(filter model.BeadFilter) bool {
	return len(filter.Type) == 1 && filter.Type[0] == model.BeadType("audit")
}

// pinOwnAuditActor restricts filter to audit beads whose fields.actor is the
// caller's own authenticated identity (operator decision gd-brxrFiZ1Z3:
// own_actor). An actor the caller already asked for must be that identity. It
// reports false when the caller has no usable identity or asked for another
// actor, so the caller falls back to the strict project predicate. The
// caller's Fields map is copied, never written.
func pinOwnAuditActor(ctx context.Context, filter *model.BeadFilter) bool {
	self := AuthIdentityFromContext(ctx).Subject
	if self == "" || self == "*" {
		return false
	}
	if asked, ok := filter.Fields["actor"]; ok && asked != self {
		return false
	}
	fields := make(map[string]string, len(filter.Fields)+1)
	maps.Copy(fields, filter.Fields)
	fields["actor"] = self
	filter.Fields = fields
	return true
}

// auditBeadOwnedByCaller reports whether b is an audit bead recording the
// caller's own action: type=audit and fields.actor exactly the caller's
// authenticated identity. It is the by-id half of the own_actor read
// exemption (pinOwnAuditActor is the list half); tokenReadsLabelLessBead
// applies it only to a label-less bead and a per-agent token.
func auditBeadOwnedByCaller(ctx context.Context, b *model.Bead) bool {
	if b.Type != model.BeadType("audit") {
		return false
	}
	self := AuthIdentityFromContext(ctx).Subject
	if self == "" {
		return false
	}
	actor, present, err := exactFieldString(b.Fields, "actor")
	return present && err == nil && actor == self
}

// narrowTitlesToOwnProject restricts filter.Titles to the titles a type:project
// bead for proj goes by ("<proj>" or "Project: <proj>"), intersecting with any
// titles the caller already asked for. It reports false when that leaves
// nothing, so the caller falls back to the strict project predicate.
func narrowTitlesToOwnProject(filter *model.BeadFilter, proj string) bool {
	own := []string{proj, "Project: " + proj}
	if len(filter.Titles) == 0 {
		filter.Titles = own
		return true
	}
	var kept []string
	for _, t := range filter.Titles {
		if slices.Contains(own, t) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return false
	}
	filter.Titles = kept
	return true
}

// eventReadScope resolves the caller's project-visibility predicates for the
// global event surface (GET /v1/events), mirroring the three-gate check
// scopeListFilter builds for ListBeads. Events are
// keyed by bead_id and carry no labels, so the store applies these predicates by
// joining through the bead's labels (ka-0000006863). A project-bound fleet
// token's "project:<name>" label is ANDed in (RequireAllLabels); an OIDC user's
// allowed projects are ORed in (RequireAnyLabels). An unscoped caller (static
// token, admin, wildcard, unrestricted OIDC user) yields the zero value, leaving
// the aggregation unrestricted.
//
// It also installs the taint read gate's list predicate (ADR docs/adr/taints.md
// §4.3/§5.1) via TaintGrantIDs — events carry no taint columns of their own, so
// the store query joins through bead_taints on events.bead_id, the same third,
// narrower check applyTaintGateToFilter installs on a BeadFilter. A tenant
// caller's scope also excludes the events of the bead types it may not read
// (read_authz_tenant.go).
func (s *BeadsServer) eventReadScope(ctx context.Context) model.EventReadScope {
	var scope model.EventReadScope
	if proj := TokenProjectFromContext(ctx); proj != "" {
		scope.RequireAllLabels = append(scope.RequireAllLabels, "project:"+proj)
	}
	if allowed := oidcAllowedProjects(ctx, s.store); allowed != nil {
		scope.RequireAnyLabels = append(scope.RequireAnyLabels, oidcProjectLabels(allowed)...)
	}
	for _, t := range s.tenantHiddenReadTypes(ctx) {
		scope.ExcludeBeadTypes = append(scope.ExcludeBeadTypes, string(t))
	}
	grants, exempt, err := s.taintGateGrants(ctx)
	if err != nil {
		slog.Warn("taint read gate: failed to resolve caller grants, failing closed", "error", err)
		empty := []int64{}
		scope.TaintGrantIDs = &empty
		return scope
	}
	if !exempt {
		scope.TaintGrantIDs = &grants
	}
	return scope
}

// beadReadAuthorized reports whether the caller may read a bead carrying the
// given labels and taint ids under ALL THREE read gates — the fleet-token
// project gate, the OIDC allowed-projects gate, and the taint gate
// (taintsOf(bead) ⊆ grantsOf(caller), ADR docs/adr/taints.md §5.1) — the same
// three-gate check handleGetBead applies. The taint gate is narrower: it can
// only deny a bead the first two gates already allowed, never widen access.
//
// allowedOIDC must be the result of a single oidcAllowedProjects(ctx) call
// (nil = unrestricted), and taintGate the result of a single
// s.resolveBeadTaintGateState(ctx) call, both hoisted OUT of any per-bead loop
// so a batch filter does not re-query the caller's permissions once per row.
// taintIDs is this specific bead's own taint ids (nil/empty when the caller's
// taintGate is exempt and the batch never fetched per-bead taints at all).
// A bead of a type a tenant caller may not read is denied to a tenant
// (read_authz_tenant.go).
func (s *BeadsServer) beadReadAuthorized(ctx context.Context, b *model.Bead, allowedOIDC []string, taintIDs []int64, taintGate beadTaintGateState) bool {
	if !beadReadableByTokenProject(ctx, b) {
		return false
	}
	if !beadMatchesOIDCProjects(b.Labels, allowedOIDC) {
		return false
	}
	if s.readHidesBead(ctx, b) {
		return false
	}
	return taintGate.allows(taintIDs)
}

// beadHiddenFromCaller reports whether bead is outside the caller's read
// scope: its token project or its OIDC projects. Every by-id route answers
// such a bead exactly as it answers a missing one (404 and the route's own
// not-found body), never 403, so a caller cannot learn that a bead outside
// its scope exists (Lands spec §5.6 "404, never 403", gd-7APTAIhdC5;
// TestCrossLandMatrix checks it). Neither the taint gate (it already answers
// 404) nor readHidesBead is part of it: a tenant is refused its own land's
// account bead with a 403, which tells it nothing about another land.
func (s *BeadsServer) beadHiddenFromCaller(ctx context.Context, b *model.Bead) bool {
	if !beadReadableByTokenProject(ctx, b) {
		return true
	}
	allowed := oidcAllowedProjects(ctx, s.store)
	return allowed != nil && !beadMatchesOIDCProjects(b.Labels, allowed)
}

// refuseHidden is how a write route answers a write it refuses: when the
// caller cannot read bead it writes 404 "bead not found", as for a missing
// bead, and returns true; otherwise it writes nothing and the route writes
// its own refusal. It is called only on a refusal, so a write the gates
// allow on a bead the caller cannot read (a laptop runner stamping
// delivered_at on its agent's mail, beads_update_steps.go) is untouched.
func (s *BeadsServer) refuseHidden(w http.ResponseWriter, r *http.Request, bead *model.Bead) bool {
	if !s.beadHiddenFromCaller(r.Context(), bead) {
		return false
	}
	writeError(w, http.StatusNotFound, "bead not found")
	return true
}

// refusedGRPC is refuseHidden for a gRPC write: the refusal err, or
// codes.NotFound when the caller cannot read bead.
func (s *BeadsServer) refusedGRPC(ctx context.Context, bead *model.Bead, err error) error {
	if s.beadHiddenFromCaller(ctx, bead) {
		return status.Error(codes.NotFound, "bead not found")
	}
	return err
}

// callerReadScoped reports whether some existing bead can be out of the
// caller's read scope: a project-bound token, or an OIDC user, scoped
// principal or runner with a project allowlist. Only such a caller can tell
// a hidden bead from a missing one, so only it gets a 404 for a missing
// parent on the sub-resource reads (authorizeSubresourceRead); an unscoped
// caller keeps their empty 200.
func (s *BeadsServer) callerReadScoped(ctx context.Context) bool {
	return TokenProjectFromContext(ctx) != "" || oidcAllowedProjects(ctx, s.store) != nil
}

// authorizeBeadRead runs the read-scope check on an already-fetched bead and,
// when the caller may not see it, writes 404 with notFound (the route's own
// missing-bead message) and returns false. A true return means the caller
// may return the bead. This is the by-id single fetch entry point
// (decisions/escalations/releases GET-by-id). A bead the taint read gate
// denies (ADR docs/adr/taints.md §5.1) answers as missing, as on GET
// /v1/beads/{id}.
func (s *BeadsServer) authorizeBeadRead(w http.ResponseWriter, r *http.Request, bead *model.Bead, notFound string) bool {
	if s.beadHiddenFromCaller(r.Context(), bead) {
		writeError(w, http.StatusNotFound, notFound)
		return false
	}
	if s.readHidesBead(r.Context(), bead) {
		writeError(w, http.StatusForbidden, errReadTypeDenied.Error())
		return false
	}
	if denied, err := s.subresourceTaintDenied(r.Context(), bead.ID); err != nil {
		writeServerError(r.Context(), w, "authorizeBeadRead", err, "failed to resolve taint grants")
		return false
	} else if denied {
		recordTaintReadDenial(r.Context(), "read", string(bead.Type))
		writeError(w, http.StatusNotFound, notFound)
		return false
	}
	return true
}

// authorizeResolutionTarget fetches the escalation/decision bead by id and
// enforces the same cross-project scope gate as authorizeBeadRead BEFORE any
// mutation runs. It is the by-id entry point for the escalation/decision
// resolution WRITE verbs (approve/reject/result, resolve/cancel).
//
// Background (ka-0000002666): those verbs are wrapped only in
// requireRole(RoleOperator, …), which short-circuits with zero checks for any
// non-OIDC caller (interceptors.go) — including every project-bound fleet
// token. Their handlers then merged fields, updated, and closed the bead (and,
// on approve, replayed the originally-denied privileged op through a
// bypass-write-policy static-token context) with NO project-scope check, even
// though GET /v1/escalations/{id} on the same token returns 403. This closes
// that read/write asymmetry: a fleet token scoped to project A cannot resolve
// an escalation/decision that lives in project B.
//
// On the happy path it returns the fetched bead so callers reuse it instead of
// issuing a second GetBead (fixing approve's prior merge-before-fetch ordering).
// On any failure it has already written the response (404 not found or out of
// scope / 5xx get error) and returns ok=false; callers must return
// immediately. kind ("escalation" / "decision") lets each verb keep its
// existing 404 / 5xx wording.
func (s *BeadsServer) authorizeResolutionTarget(w http.ResponseWriter, r *http.Request, id, kind, op string) (*model.Bead, bool) {
	// A decision is answered by a human (or the bridge relaying one), never by an
	// agent credential: refuse before even fetching the bead (gd-Du6wHclx4g).
	if kind == "decision" && !s.authorizeDecisionResolver(w, r, id, "close") {
		return nil, false
	}
	bead, ok := s.fetchResolutionTarget(w, r, id, kind, op)
	if !ok {
		return nil, false
	}
	// Resolving or cancelling is a write: a scoped principal may answer a
	// decision in its project and nothing else (no escalation verbs).
	if denial := scopedWriteDenial(r.Context(), "close", bead.Type, bead.Labels); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return nil, false
	}
	// Nor in a frozen land (land_frozen.go).
	if denial := frozenLandDenial(r.Context(), s.store, "close", bead.Type, bead.Labels); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return nil, false
	}
	return bead, true
}

// fetchResolutionTarget is the fetch + read-scope half of
// authorizeResolutionTarget. Approve and reject of an escalation call it
// directly and apply scopedApproverAllowed in place of the type gate, which
// would otherwise refuse every escalation verb to a scoped principal.
func (s *BeadsServer) fetchResolutionTarget(w http.ResponseWriter, r *http.Request, id, kind, op string) (*model.Bead, bool) {
	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, kind+" not found")
		return nil, false
	}
	if err != nil {
		writeServerError(r.Context(), w, op, err, "failed to get "+kind)
		return nil, false
	}
	if !s.authorizeBeadRead(w, r, bead, kind+" not found") {
		return nil, false
	}
	return bead, true
}

// authorizeEscalationVerb is fetchResolutionTarget plus the scoped-approver
// gate and the lands-tenant refusal, for POST /v1/escalations/{id}/approve and
// /reject. verb ("approve" or "reject") is what a denial is audited as.
func (s *BeadsServer) authorizeEscalationVerb(w http.ResponseWriter, r *http.Request, id, op, verb string) (*model.Bead, bool) {
	bead, ok := s.fetchResolutionTarget(w, r, id, "escalation", op)
	if !ok {
		return nil, false
	}
	// A lands-tenant agent approves and rejects nothing (Lands Track B §5.1),
	// except a steward approving a merge in its own land, which
	// checkMayApprove then holds to D4/D8 (escalation-routing Task 11).
	tenantOp := opEscalationReject
	if verb == actionApprove {
		tenantOp = opEscalationApprove
	}
	if d := s.restrictedOpDenial(r.Context(), restrictedOpRequest{Op: tenantOp, Action: verb, BeadType: model.TypeEscalation, Escalation: bead}); d != nil {
		s.auditDenial(r, d, id)
		writeAuthzDenial(w, d)
		return nil, false
	}
	if allowed, reason := s.scopedApproverAllowed(r.Context(), bead); !allowed {
		s.auditDenial(r, &AuthzDenial{Action: verb, BeadType: string(bead.Type), Message: reason}, id)
		writeError(w, http.StatusForbidden, reason)
		return nil, false
	}
	return bead, true
}
