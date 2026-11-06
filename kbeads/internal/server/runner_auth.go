package server

// Runner bearer authentication and the runner route allowlist (local-agents
// spec §4.2 "The token is a bearer with role RoleRunner" and §3.3; plan
// gd-fwFefkDP4H Task 8 gd-9toFMX9JjW).
//
// A "gr_..." bearer is looked up by hash in runner_tokens (Task 7, runners.go)
// and resolves to the runner bead's owner (D6): writes attribute to the owner
// through the server-stamped operated_by header, never a client one. The
// caller gets its own auth method, authMethodRunner, which is not roleBearing
// and is never a static or fleet identity, so:
//
//   - runnerRouteGate (installed per route by scopeGatedMux) and requireRole
//     admit it only on the routes in runnerAllowedPatterns, whatever role the
//     route asks for: a runner is admitted by route, never by rank;
//   - reads are confined to the runner's project allowlist
//     (resolveOIDCAllowedProjects' runner branch), and writes to beads in
//     those projects (runnerProjectDenial in authorizeWriteFrozen);
//   - the event stream must name an agent bead running on this runner
//     (runnerStreamDenial), and a runner may set runtime_id only on its own
//     agents, to its own id (runnerRuntimeIDDenial), and reaches only its
//     own agents' gates (runnerAgentOwned).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// authMethodRunner marks a request authenticated with a runner bearer.
const authMethodRunner authMethod = "runner"

// runnerListenerHeader is stamped by the public runner IngressRoute (Task 10)
// so the daemon can refuse any bearer that is not a runner token there.
const runnerListenerHeader = "X-Gardenio-Listener"

const runnerTokenUnknownMsg = "runner token revoked or unknown"

type runnerProjectsKey struct{}

// WithRunnerProjects stamps the authenticated runner's project allowlist.
func WithRunnerProjects(ctx context.Context, projects []string) context.Context {
	return context.WithValue(ctx, runnerProjectsKey{}, projects)
}

// RunnerProjectsFromContext returns the project allowlist stamped by
// WithRunnerProjects, nil for a non-runner caller.
func RunnerProjectsFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(runnerProjectsKey{}).([]string)
	return v
}

// runnerAllowedPatterns are the routes a runner bearer may call, as the Go 1.22
// mux reports them in r.Pattern. Everything else is 403 for a runner.
var runnerAllowedPatterns = map[string]bool{
	"POST /v1/runners/{id}/heartbeat":  true,
	"GET /v1/runners/{id}":             true,
	"GET /v1/events/stream":            true,
	"GET /v1/beads":                    true,
	"GET /v1/beads/{id}":               true,
	"GET /v1/beads/{id}/comments":      true,
	"GET /v1/beads/{id}/dependencies":  true,
	"GET /v1/beads/{id}/dependents":    true,
	"POST /v1/beads":                   true,
	"PATCH /v1/beads/{id}":             true,
	"POST /v1/beads/{id}/close":        true,
	"POST /v1/beads/{id}/comments":     true,
	"POST /v1/hooks/emit":              true,
	"POST /v1/escalations/{id}/result": true,
	// What an agent session on the runner token calls (gd-JmUt9B2iLb, spec
	// §4.2): config reads (sensitive keys stay OIDC-admin, config_key_authz.go;
	// writes are not here), the owner on /v1/me, agent lookups and semantic
	// search under the read scope, and links whose two ends are both in the
	// allowlist (runnerDependencyTargetDenial). Escalations and decisions are
	// created and read on the bead routes above; no approve, reject, resolve
	// or cancel route is here.
	"GET /v1/configs":                    true,
	"GET /v1/configs/{key...}":           true,
	"GET /v1/me":                         true,
	"GET /v1/agents-by-name/{name}":      true,
	"POST /v1/beads/{id}/dependencies":   true,
	"DELETE /v1/beads/{id}/dependencies": true,
	"POST /v1/search/semantic":           true,
	// The rest of an agent session's calls (gd-pJlQEIdlRD): the escalation
	// list and batch read under the read scope, as GET /v1/beads; labels on
	// beads the write gate admits, never adding a project: label outside the
	// allowlist or removing one (runnerLabelChangeDenial); and the gates of
	// its own agents (runnerAgentOwned). hooks/publish (a raw bus subject),
	// the roster (the presence actors carry no project) and
	// mentions/mentioned-by (their rows name beads in any project) are not
	// here, and degrade.
	"GET /v1/escalations":                          true,
	"GET /v1/batch/beads":                          true,
	"POST /v1/beads/{id}/labels":                   true,
	"DELETE /v1/beads/{id}/labels/{label}":         true,
	"POST /v1/agents/{id}/gates/{gate_id}/satisfy": true,
}

func runnerRouteAllowed(pattern string) bool { return runnerAllowedPatterns[pattern] }

// runnerDependencySourceHidden reports whether a runner links or unlinks from
// a bead it cannot read: both ends of an edge a runner writes must be in its
// project allowlist, and the write path alone lets a bead with no project
// label through. false for every non-runner caller.
func (s *BeadsServer) runnerDependencySourceHidden(ctx context.Context, bead *model.Bead) bool {
	return authMethodFromContext(ctx) == authMethodRunner && s.beadHiddenFromCaller(ctx, bead)
}

// runnerDependencyTargetDenial is scopedDependencyTargetDenial for a runner:
// a target that is missing or outside its allowlist is hidden, so a link
// cannot tell the runner that a bead in another project exists, and one
// whose labels or fields.project leave the allowlist is denied.
func (s *BeadsServer) runnerDependencyTargetDenial(ctx context.Context, dependsOnID string) (hidden bool, denial *AuthzDenial) {
	target, err := s.store.GetBead(ctx, dependsOnID)
	if err != nil || target == nil || s.beadHiddenFromCaller(ctx, target) {
		return true, nil
	}
	return false, runnerProjectDenial(ctx, "update", target.Labels, target.Fields)
}

// runnerRouteGate refuses every route outside runnerAllowedPatterns for a
// runner caller, including routes registered without requireRole. It reads
// r.Pattern, so scopeGatedMux installs it per route, inside the mux match.
func runnerRouteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authMethodFromContext(r.Context()) == authMethodRunner && !runnerRouteAllowed(r.Pattern) {
			writeRunnerRouteDenied(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeRunnerRouteDenied(w http.ResponseWriter, r *http.Request) {
	slog.Debug("RBAC denied (runner token)", "path", r.URL.Path, "method", r.Method, "pattern", r.Pattern)
	writeError(w, http.StatusForbidden, "runner tokens may not call this route")
}

// runnerAuthMiddleware authenticates "gr_" bearers against runner_tokens and
// sends them to authenticated (the chain below auth), skipping AuthMiddleware
// and the edge envelope, neither of which knows the token. Every other request
// goes to next untouched, except on the public runner listener, where a bearer
// that is not a runner token is refused (spec §4.3). Register carries no
// bearer and passes.
func (s *BeadsServer) runnerAuthMiddleware(fleetCfg *FleetConfig, authenticated, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		bearer, isBearer := strings.CutPrefix(auth, "Bearer ")
		if !isBearer || !strings.HasPrefix(bearer, runnerTokenPrefix) {
			if r.Header.Get(runnerListenerHeader) == "runner" && (auth != "" || r.Header.Get("X-Auth-Request-Access-Token") != "") {
				writeError(w, http.StatusUnauthorized, "this listener accepts runner tokens only")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		ctx, ok := s.runnerContext(r.Context(), fleetCfg, bearer)
		if !ok {
			writeError(w, http.StatusUnauthorized, runnerTokenUnknownMsg)
			return
		}
		recordHeaderArrival(runnerIdentityType, false)
		authenticated.ServeHTTP(w, r.WithContext(ctx))
	})
}

// runnerContext resolves a runner bearer to its runner and returns the
// request context for it. ok is false for an unknown or revoked token, a
// missing, closed or revoked runner bead, or one with no valid owner.
func (s *BeadsServer) runnerContext(ctx context.Context, fleetCfg *FleetConfig, bearer string) (context.Context, bool) {
	rt, err := s.store.GetRunnerTokenByHash(ctx, runnerTokenHash(bearer))
	if err != nil || rt == nil || rt.RevokedAt != nil {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("runner auth: token lookup failed", "error", err)
		}
		return nil, false
	}
	runner, err := s.store.GetBead(ctx, rt.RunnerID)
	if err != nil || runner == nil || runner.Type != "runner" || runner.Status == model.StatusClosed {
		return nil, false
	}
	var f struct {
		Owner    string   `json:"owner"`
		Projects []string `json:"projects"`
		State    string   `json:"state"`
	}
	if len(runner.Fields) > 0 {
		if err := json.Unmarshal(runner.Fields, &f); err != nil {
			return nil, false
		}
	}
	owner, ok := sanitizeOperatedByHeader(f.Owner)
	if !ok || owner == "" || f.State == runnerStateRevoked {
		return nil, false
	}
	ctx = withAuthMethod(ctx, authMethodRunner)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: runnerIdentityType, Subject: owner})
	ctx = WithRunnerID(ctx, runner.ID)
	ctx = WithRunnerProjects(ctx, f.Projects)
	ctx = WithOperatedByHeader(ctx, owner)
	if fleetCfg != nil {
		ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
	}
	return ctx, true
}

// runnerProjectDenial refuses a runner's write to a bead labelled with a
// project outside its allowlist, or whose fields.project (where the controller
// reads an agent's project from) is outside it: a label inside the allowlist
// does not cover a field outside it. A bead with neither (the agent's own bead,
// mail) is allowed. nil for every non-runner caller.
func runnerProjectDenial(ctx context.Context, action string, labels []string, fields json.RawMessage) *AuthzDenial {
	if authMethodFromContext(ctx) != authMethodRunner {
		return nil
	}
	allowed := RunnerProjectsFromContext(ctx)
	deny := &AuthzDenial{Action: action, Message: "runner is not allowlisted for this project"}
	for _, l := range labels {
		p, ok := strings.CutPrefix(l, "project:")
		if ok && !slices.Contains(allowed, p) {
			return deny
		}
	}
	if p := projectFromFields(fields); p != "" && !slices.Contains(allowed, p) {
		return deny
	}
	return nil
}

// runnerRuntimeIDDenial refuses a runner's write that sets, changes or clears
// runtime_id on a bead unless the stored value is already this runner's id,
// and refuses one that names any runner but this one. runtime_id is what
// runnerStreamDenial trusts, and agent beads are KindConfig so ownership does
// not guard them: without this a runner could stamp its own id onto another
// operator's agent and then stream it. A key spelled in another case counts,
// since a struct decode folds it onto runtime_id. existing is the stored bead,
// nil on create. nil for every non-runner caller.
func runnerRuntimeIDDenial(ctx context.Context, action string, patch json.RawMessage, existing *model.Bead) *AuthzDenial {
	if authMethodFromContext(ctx) != authMethodRunner || len(patch) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(patch, &m) != nil {
		return nil
	}
	self := RunnerIDFromContext(ctx)
	deny := &AuthzDenial{Action: action, Message: "runner may set runtime_id only on its own agents, to its own runner id"}
	for k, raw := range m {
		if !strings.EqualFold(k, runtimeIDField) {
			continue
		}
		if k != runtimeIDField {
			return deny
		}
		if existing != nil {
			if stored, _, _ := exactFieldString(existing.Fields, runtimeIDField); stored != self {
				return deny
			}
		}
		var v *string
		if json.Unmarshal(raw, &v) != nil || (v != nil && *v != "" && *v != self) {
			return deny
		}
	}
	return nil
}

// runnerRuntimeIDFieldsDenial is runnerRuntimeIDDenial for a flat field map,
// the shape mergeBeadFields and the close body carry.
func runnerRuntimeIDFieldsDenial(ctx context.Context, action string, fields map[string]any, existing *model.Bead) *AuthzDenial {
	if authMethodFromContext(ctx) != authMethodRunner || !hasRuntimeIDKey(fields) {
		return nil
	}
	patch, err := json.Marshal(fields)
	if err != nil {
		return &AuthzDenial{Action: action, Message: "runner may set runtime_id only on its own agents, to its own runner id"}
	}
	return runnerRuntimeIDDenial(ctx, action, patch, existing)
}

// hasRuntimeIDKey reports whether fields names runtime_id in any letter case.
func hasRuntimeIDKey(fields map[string]any) bool {
	for k := range fields {
		if strings.EqualFold(k, runtimeIDField) {
			return true
		}
	}
	return false
}

const runtimeIDField = "runtime_id"

// runnerLabelChangeDenial gates the label a runner adds or removes; the bead
// itself is gated by loadBeadForWrite. An added project: label must be in the
// allowlist, so a label cannot move a bead out of it, and a project: label is
// never removed, so a bead cannot leave the read scope by losing it (as
// scopedLabelChangeDenial holds a scoped principal). nil for every non-runner
// caller and for any other label.
func runnerLabelChangeDenial(ctx context.Context, label string, add bool) *AuthzDenial {
	if authMethodFromContext(ctx) != authMethodRunner || !strings.HasPrefix(label, "project:") {
		return nil
	}
	if !add {
		return &AuthzDenial{Action: "update", Message: "runner may not remove a project label"}
	}
	return runnerProjectDenial(ctx, "update", []string{label}, nil)
}

// runnerAgentOwned reports whether agentID is an agent bead whose runtime_id
// is the calling runner, as runnerStreamDenial requires of a stream. A
// missing bead answers false, like another runner's. true for every
// non-runner caller.
func (s *BeadsServer) runnerAgentOwned(ctx context.Context, agentID string) bool {
	if authMethodFromContext(ctx) != authMethodRunner {
		return true
	}
	bead, err := s.store.GetBead(ctx, agentID)
	if err != nil || bead == nil || len(bead.Fields) == 0 {
		return false
	}
	rid, _, err := exactFieldString(bead.Fields, runtimeIDField)
	return err == nil && rid != "" && rid == RunnerIDFromContext(ctx)
}

// runnerStreamDenial checks that a runner's event stream names an agent bead
// (agent=<bead id>) whose runtime_id is this runner. It returns the status and
// message to refuse with, or 0 for a non-runner caller or a permitted stream.
func (s *BeadsServer) runnerStreamDenial(r *http.Request) (int, string) {
	ctx := r.Context()
	if authMethodFromContext(ctx) != authMethodRunner {
		return 0, ""
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		return http.StatusBadRequest, "runner streams must name their agent (agent=<bead>)"
	}
	const notOurs = "agent does not belong to this runner"
	bead, err := s.store.GetBead(ctx, agent)
	if err != nil || bead == nil || len(bead.Fields) == 0 {
		return http.StatusForbidden, notOurs
	}
	// Exact key only: a struct decode would fold a case-variant key onto it.
	if rid, _, err := exactFieldString(bead.Fields, runtimeIDField); err != nil || rid == "" || rid != RunnerIDFromContext(ctx) {
		return http.StatusForbidden, notOurs
	}
	return 0, ""
}
