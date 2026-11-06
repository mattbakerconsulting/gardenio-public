package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// oidcAllowedProjects resolves the project list that an OIDC-authenticated user
// is authorized to read. It returns nil when the user has unrestricted access
// (admin role, wildcard "*", no user bead, or empty allowed_projects for backward
// compatibility). A non-nil return contains project names (not labels); it is
// empty when more than one user bead carries the user's email.
//
// A scoped principal (OIDC, not admin, holds RoleProjectMember; see
// scopeFromContext) is resolved from the verified token's groups and never
// from the user bead. It gets a NON-NIL result even when that is empty: a
// non-nil empty slice means "match nothing", so every consumer must test
// `!= nil`, never `len() > 0`, to decide whether to restrict.
//
// Static/fleet token requests always return nil (unrestricted) since fleet tokens
// have their own project scoping via TokenProjectFromContext. A runner bearer
// returns its project allowlist (runner_auth.go).
func oidcAllowedProjects(ctx context.Context, s store.Store) []string {
	allowed, err := resolveOIDCAllowedProjects(ctx, s)
	if err != nil {
		slog.Warn("oidc read filter: failed to look up user permissions",
			"email", AuthIdentityFromContext(ctx).Subject, "error", err)
		return nil // fail open for reads — backward compat
	}
	return allowed
}

// resolveOIDCAllowedProjects is oidcAllowedProjects without the fail-open:
// a transient user-bead lookup error is returned rather than read as
// unrestricted, for a caller that already holds a scope and can keep it
// (the SSE refresh tick, D.9 gd-Z9VVGjvzBk).
func resolveOIDCAllowedProjects(ctx context.Context, s store.Store) ([]string, error) {
	// A runner reads its project allowlist, never more. Task 7 refuses an
	// empty allowlist at register; should one appear anyway, non-nil empty
	// reads nothing.
	if authMethodFromContext(ctx) == authMethodRunner {
		if p := RunnerProjectsFromContext(ctx); p != nil {
			return p, nil
		}
		return []string{}, nil
	}
	if !roleBearing(authMethodFromContext(ctx)) {
		return nil, nil
	}

	// Admin role bypasses all read restrictions.
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		return nil, nil
	}

	// Scoped principal: token groups only, fail closed.
	if d := scopeFromContext(ctx); !d.Unrestricted {
		if len(d.Projects) == 0 {
			return []string{}, nil
		}
		return d.Projects, nil
	}

	id := AuthIdentityFromContext(ctx)
	email := id.Subject
	if email == "" {
		return nil, nil // no identity — allow unrestricted (backward compat)
	}

	perms, err := lookupSpawnPermissions(ctx, s, email)
	if errors.Is(err, errAmbiguousUserBead) {
		// Not transient: two user beads for one email would make the read
		// scope depend on row order, so read nothing (gd-jTIz9PSHlK).
		slog.Warn("oidc read filter: email is carried by more than one user bead; denying", "email", email)
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if perms == nil {
		return nil, nil // no user bead — unrestricted (backward compat)
	}

	if len(perms.AllowedProjects) == 0 {
		return nil, nil // no project restrictions configured
	}

	// Wildcard means unrestricted.
	for _, p := range perms.AllowedProjects {
		if p == "*" {
			return nil, nil
		}
	}

	return perms.AllowedProjects, nil
}

// denyAllProjectLabel is a label no bead carries (the unit separator is not a
// legal project-name character), used to make a LabelsAny / RequireAnyLabels
// predicate match nothing for a scoped principal with no projects.
const denyAllProjectLabel = "project:\x1fdeny-all"

// oidcProjectLabels converts a project name list into "project:<name>" labels
// suitable for BeadFilter.LabelsAny (OR-match semantics).
//
// An empty project list returns denyAllProjectLabel rather than an empty slice:
// every consumer appends these labels to a filter, and an empty any-of set adds
// no predicate at all, which would list everything instead of nothing.
func oidcProjectLabels(projects []string) []string {
	if len(projects) == 0 {
		return []string{denyAllProjectLabel}
	}
	labels := make([]string, len(projects))
	for i, p := range projects {
		labels[i] = "project:" + p
	}
	return labels
}

// beadMatchesOIDCProjects checks whether a bead's labels include at least one
// of the allowed project labels. Returns true if allowedProjects is nil
// (unrestricted access).
func beadMatchesOIDCProjects(labels []string, allowedProjects []string) bool {
	if allowedProjects == nil {
		return true
	}
	for _, beadLabel := range labels {
		for _, proj := range allowedProjects {
			if beadLabel == "project:"+proj {
				return true
			}
		}
	}
	return false
}

// errReadDenied is returned when an OIDC user attempts to read a bead outside
// their authorized project scope.
var errReadDenied = errors.New("not authorized to read beads in this project")

// errReadTokenDenied is returned when a project-bound fleet token attempts to
// read a bead outside its project scope.
var errReadTokenDenied = errors.New("token not authorized for this bead's project")

// errReadTypeDenied is returned when a tenant caller reads a bead of a type it
// may not read (read_authz_tenant.go). It wraps errReadDenied, so every
// caller that maps errReadDenied to a 403 maps this one too.
var errReadTypeDenied = fmt.Errorf("%w: a tenant may not read %s beads or the fleet's label-less advice and config", errReadDenied, model.TypeAccount)

// getBeadFiltered retrieves a bead and enforces read-side project isolation
// for BOTH project-bound fleet tokens and OIDC users — the same two-gate check
// handleGetBead / the gRPC search path apply. Returns errReadTokenDenied or
// errReadDenied when the bead is outside the caller's scope; the parent bead is
// otherwise returned so a caller can proceed to a sub-resource read.
//
// This is the read-side twin of loadBeadForWriteCore: the per-bead sub-resource
// read handlers (comments, events, mutations, labels, dependencies,
// operator-history, followers, mentions) MUST route their authorization through
// here so a caller cannot enumerate a scoped bead's contents by asking for a
// child collection that skips the primary-bead gate (ka-0000003827).
func (s *BeadsServer) getBeadFiltered(ctx context.Context, id string) (*model.Bead, error) {
	bead, err := s.store.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if bead == nil {
		return nil, errors.New("bead not found")
	}
	if !beadReadableByTokenProject(ctx, bead) {
		return nil, errReadTokenDenied
	}
	if allowed := oidcAllowedProjects(ctx, s.store); allowed != nil {
		if !beadMatchesOIDCProjects(bead.Labels, allowed) {
			return nil, errReadDenied
		}
	}
	if s.readHidesBead(ctx, bead) {
		return nil, errReadTypeDenied
	}
	return bead, nil
}

// authorizeSubresourceRead is the HTTP entry point the per-bead sub-resource
// GET handlers use to enforce read-side project scope before returning a child
// collection (ka-0000003827). A true return means the caller may proceed to
// read {beadID}'s sub-resources; on false it has written the response.
//
// A parent the caller may not see answers 404 "bead not found", and so does a
// missing parent for a caller whose scope can hide one (callerReadScoped), so
// the two cannot be told apart (gd-7APTAIhdC5). For an unscoped caller a
// missing parent passes through to the handler's empty 200, which callers
// outside any scope (bridges, controller reconcilers) have always had.
// A parent the taint read gate denies answers as a missing one, with empty
// (the handler's own empty collection) as the unscoped caller's body.
func (s *BeadsServer) authorizeSubresourceRead(w http.ResponseWriter, r *http.Request, beadID string, empty any) bool {
	switch err := s.subresourceParentReadable(r.Context(), beadID); {
	case err == nil:
		return true
	case errors.Is(err, errSubresourceParentUnseen):
		writeError(w, http.StatusNotFound, "bead not found")
	case errors.Is(err, errSubresourceTaintDenied):
		s.writeSubresourceMissing(w, r, empty)
	case errors.Is(err, errReadTypeDenied):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		writeServerError(r.Context(), w, "authorizeSubresourceRead", err, "failed to authorize read")
	}
	return false
}

// authorizeSubresourceReadGRPC is the gRPC twin of authorizeSubresourceRead
// (GetComments, GetEvents, GetDependencies, GetLabels, …): codes.NotFound
// where the HTTP twin answers 404, ok when the caller may proceed. ok=false
// with a nil error is the HTTP twin's empty 200: the handler answers its
// empty response.
func (s *BeadsServer) authorizeSubresourceReadGRPC(ctx context.Context, beadID string) (ok bool, err error) {
	switch err := s.subresourceParentReadable(ctx, beadID); {
	case err == nil:
		return true, nil
	case errors.Is(err, errSubresourceParentUnseen):
		return false, status.Error(codes.NotFound, "bead not found")
	case errors.Is(err, errSubresourceTaintDenied):
		if s.callerReadScoped(ctx) {
			return false, status.Error(codes.NotFound, "bead not found")
		}
		return false, nil
	case errors.Is(err, errReadTypeDenied):
		return false, status.Error(codes.PermissionDenied, err.Error())
	default:
		return false, status.Errorf(codes.Internal, "failed to authorize read: %v", err)
	}
}

// errSubresourceParentUnseen is subresourceParentReadable's answer for a
// parent hidden from the caller, or missing when the caller is read-scoped.
var errSubresourceParentUnseen = errors.New("bead not found")

// errSubresourceTaintDenied is subresourceParentReadable's answer for a
// parent the taint read gate denies (ADR docs/adr/taints.md §5.1). It is
// answered as a missing parent (writeSubresourceMissing), so the denial does
// not tell the caller the bead exists.
var errSubresourceTaintDenied = errors.New("bead not found")

// subresourceParentReadable is the decision both authorizeSubresourceRead
// twins make: nil to proceed, errSubresourceParentUnseen,
// errSubresourceTaintDenied, errReadTypeDenied for a tenant on an account
// bead, or a store error. On nil the reading agent has acquired the parent's
// taints (inherit:read): a sub-resource is the parent's text (gd-WdnPW8TLPq).
func (s *BeadsServer) subresourceParentReadable(ctx context.Context, beadID string) error {
	bead, err := s.store.GetBead(ctx, beadID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bead == nil) {
		if s.callerReadScoped(ctx) {
			return errSubresourceParentUnseen
		}
		return nil
	}
	if err != nil {
		return err
	}
	if s.beadHiddenFromCaller(ctx, bead) {
		return errSubresourceParentUnseen
	}
	if s.readHidesBead(ctx, bead) {
		return errReadTypeDenied
	}
	if denied, err := s.subresourceTaintDenied(ctx, bead.ID); err != nil {
		return fmt.Errorf("resolving taint read gate: %w", err)
	} else if denied {
		recordTaintReadDenial(ctx, "read", string(bead.Type))
		return errSubresourceTaintDenied
	}
	return s.propagateReadTaint(ctx, bead.ID)
}

// writeSubresourceMissing answers a sub-resource read as authorizeSubresourceRead
// answers one whose parent is missing: 404 for a read-scoped caller, else
// empty (the handler's empty collection) with 200.
func (s *BeadsServer) writeSubresourceMissing(w http.ResponseWriter, r *http.Request, empty any) {
	if s.callerReadScoped(r.Context()) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	writeJSON(w, http.StatusOK, empty)
}
