package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

type userBeadIDKey struct{}

// WithUserBeadID returns a context with the user bead ID set.
func WithUserBeadID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userBeadIDKey{}, id)
}

// UserBeadIDFromContext returns the user bead ID from the context, or "".
func UserBeadIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userBeadIDKey{}).(string)
	return v
}

// groupProjectPrefix is the Keycloak group naming convention prefix.
// Groups named "/project-<name>" or "project-<name>" map to project "<name>".
const groupProjectPrefix = "project-"

// UserProvisioner auto-creates user beads for OIDC-authenticated users on first request.
// It also syncs Keycloak group membership to the user bead's allowed_projects field.
type UserProvisioner struct {
	store     store.Store
	publisher events.Publisher

	// cache maps email → user bead ID to avoid a DB lookup on every request.
	mu    sync.RWMutex
	cache map[string]string

	// groupCache maps email → sorted comma-joined project string for change detection.
	// Only updated when the user bead's allowed_projects is written.
	groupMu    sync.RWMutex
	groupCache map[string]string
}

// NewUserProvisioner creates a UserProvisioner backed by the given store.
func NewUserProvisioner(s store.Store, p events.Publisher) *UserProvisioner {
	return &UserProvisioner{
		store:      s,
		publisher:  p,
		cache:      make(map[string]string),
		groupCache: make(map[string]string),
	}
}

// Middleware returns an HTTP middleware that provisions user beads for OIDC
// users. Static-token and unauthenticated requests pass through unchanged.
func (up *UserProvisioner) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only provision for OIDC-authenticated requests with an email subject.
		if authMethodFromContext(r.Context()) != authMethodOIDC {
			next.ServeHTTP(w, r)
			return
		}
		id := AuthIdentityFromContext(r.Context())
		email := id.Subject
		if email == "" {
			next.ServeHTTP(w, r)
			return
		}

		beadID, err := up.ensureUserBead(r.Context(), email)
		if err != nil {
			slog.Error("user provisioning failed", "email", email, "error", err)
			// Don't block the request — provisioning is best-effort.
			next.ServeHTTP(w, r)
			return
		}

		// Sync Keycloak group membership to user bead allowed_projects.
		// A scoped principal always syncs, so its bead clears when its last
		// project group is removed. Everyone else syncs only when a project
		// group is present, as before: clearing a global operator's bead would
		// turn its empty allowed_projects into "unrestricted" reads.
		if groups := GroupsFromContext(r.Context()); len(groupsToProjects(groups)) > 0 || isScopedPrincipal(r.Context()) {
			if err := up.syncGroupProjects(r.Context(), email, beadID, groups); err != nil {
				slog.Warn("group sync failed", "email", email, "error", err)
			}
		}

		ctx := WithUserBeadID(r.Context(), beadID)
		w.Header().Set("X-User-Bead-ID", beadID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ensureUserBead returns the user bead ID for the email, creating one if needed.
func (up *UserProvisioner) ensureUserBead(ctx context.Context, email string) (string, error) {
	// Check cache first (hot path).
	up.mu.RLock()
	if id, ok := up.cache[email]; ok {
		up.mu.RUnlock()
		return id, nil
	}
	up.mu.RUnlock()

	// Look up by email field, case-insensitively, so a login whose case
	// differs from an admin-written bead finds it instead of creating a
	// duplicate (which lookupSpawnPermissions would then refuse as
	// ambiguous). An already-ambiguous email creates nothing.
	existing, err := userBeadByEmail(ctx, up.store, email)
	if err != nil {
		return "", err
	}
	if existing != nil {
		id := existing.ID
		up.mu.Lock()
		up.cache[email] = id
		up.mu.Unlock()
		return id, nil
	}

	// Not found — create a new user bead.
	return up.createUserBead(ctx, email)
}

func (up *UserProvisioner) createUserBead(ctx context.Context, email string) (string, error) {
	// Extract username from email for the title.
	username := email
	for i, c := range email {
		if c == '@' {
			username = email[:i]
			break
		}
	}

	id, err := idgen.Generate()
	if err != nil {
		return "", err
	}

	fields := map[string]string{
		"email":        email,
		"auth_methods": "gitlab-oauth",
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}

	// Resolve the OIDC operator FK so the insert satisfies the NOT NULL
	// constraint that migration 022 reinstated on beads.operated_by_id.
	// This mirrors what BeadsServer.resolveOperator does for every other
	// write path; UserProvisioner talks to the store directly so it has
	// to do the upsert inline (ka-0000007442).
	operatorID, err := up.resolveOIDCOperator(ctx, email)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	bead := &model.Bead{
		ID:                  id,
		Title:               username,
		Type:                "user",
		Kind:                "config",
		Status:              model.StatusOpen,
		CreatedBy:           "auto-provision",
		CreatedAt:           now,
		UpdatedAt:           now,
		Fields:              fieldsJSON,
		Fleet:               FleetFromContext(ctx),
		OperatedBy:          "oidc:" + email,
		OperatedByID:        &operatorID,
		CreatedByOperatorID: &operatorID,
	}

	if err := up.store.CreateBead(ctx, bead); err != nil {
		return "", err
	}

	slog.Info("auto-provisioned user bead", "id", id, "email", email)

	if err := up.publisher.Publish(ctx, events.TopicBeadCreated, bead); err != nil {
		slog.Warn("failed to publish user bead creation event", "id", id, "error", err)
	}

	up.mu.Lock()
	up.cache[email] = id
	up.mu.Unlock()

	return id, nil
}

// resolveOIDCOperator returns the operators(id) FK for the OIDC user, upserting
// the operator row and canonical alias on first sighting. Mirrors the cold-path
// branch of BeadsServer.resolveOperator (audit.go) so auto-provisioned user
// beads carry the same operator FK as any other write attributed to this OIDC
// subject. The error path is fatal for the caller: post-migration-022,
// beads.operated_by_id is NOT NULL, so a nil FK would fail the insert anyway —
// surfacing the upsert error here gives a clearer log line than the downstream
// constraint violation.
func (up *UserProvisioner) resolveOIDCOperator(ctx context.Context, email string) (int64, error) {
	if id, err := up.store.LookupOperatorAlias(ctx, "oidc", email); err == nil {
		return id, nil
	}
	id, err := up.store.UpsertOperator(ctx, "oidc", email)
	if err != nil {
		return 0, err
	}
	if err := up.store.UpsertOperatorAlias(ctx, id, "oidc", email, "auto-provision"); err != nil {
		// Operator row landed; alias didn't. Caller still gets the id so the
		// user-bead insert is attributed; the next write for the same operator
		// will retry the alias upsert via the cold path.
		slog.Warn("user-provision operator alias upsert failed",
			"email", email, "err", err.Error())
		return id, nil
	}
	return id, nil
}

// syncGroupProjects converts Keycloak groups to project names and updates the
// user bead's allowed_projects field, and its land_roles (each land's tier, for
// showing a land's crew; access never reads it, land_role.go), if either has
// changed. This is best-effort and does not block the request on failure.
func (up *UserProvisioner) syncGroupProjects(ctx context.Context, email, beadID string, groups []string) error {
	projects := groupsToProjects(groups)
	if projects == nil {
		// No project group: write an explicit empty list so the bead stops
		// carrying a revoked project (a returning early here left it behind).
		projects = []string{}
	}
	landRoles := map[string]string{}
	for p, r := range groupsToLandRoles(groups) {
		landRoles[p] = r.String()
	}

	// Build a stable key for change detection. The empty set is a key of its
	// own (the cache lookup checks presence), distinct from "never synced".
	sort.Strings(projects)
	keyParts := make([]string, len(projects))
	for i, p := range projects {
		keyParts[i] = p + "=" + landRoles[p]
	}
	key := strings.Join(keyParts, ",")

	up.groupMu.RLock()
	cached, seen := up.groupCache[email]
	up.groupMu.RUnlock()

	if seen && cached == key {
		return nil // no change
	}

	// Fetch the current bead to do a read-modify-write on its fields.
	bead, err := up.store.GetBead(ctx, beadID)
	if err != nil {
		return err
	}
	if bead == nil {
		return nil
	}

	// Parse existing fields, merge in allowed_projects.
	fields := make(map[string]any)
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &fields)
	}
	fields["allowed_projects"] = projects
	fields["land_roles"] = landRoles
	updated, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	bead.Fields = updated

	if err := up.store.UpdateBead(ctx, bead); err != nil {
		return err
	}

	slog.Info("synced group projects to user bead",
		"email", email, "bead_id", beadID, "projects", projects)

	up.groupMu.Lock()
	up.groupCache[email] = key
	up.groupMu.Unlock()

	return nil
}

// groupsToProjects converts Keycloak group paths to project names.
// Groups matching the pattern "/project-<name>" or "project-<name>" produce
// project name "<name>", as does a land-role subgroup of it, once. Groups that don't match the convention are ignored.
func groupsToProjects(groups []string) []string {
	var projects []string
	for _, g := range groups {
		// A land-role subgroup (/project-x/owners) names project x, not
		// "x/owners" (land_role.go).
		if name, _, ok := parseProjectGroup(g); ok && !slices.Contains(projects, name) {
			projects = append(projects, name)
		}
	}
	return projects
}
