package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The read-only verifier (agent access programme P9.2, gd-6ujfeTZfNL): a fleet
// agent checks a viewer page live on its own per-agent token, from inside the
// cluster, with no human browser step. Behind the edge the viewer runs
// devAuthBypass and refuses a direct call with no edge proof to every route but
// the public and machine ones (scopeMiddleware); verifierMiddleware admits one
// such call when all of these hold:
//
//   - it is a GET or HEAD, so a verifier never writes through the viewer, or a
//     POST on verifierSteerRoutes, which steers only the caller's own stream;
//   - the route is on verifierRoutes (verifierSteerRoutes for a POST), an
//     allow-list of pages whose data the daemon serves on the caller's bearer,
//     or the farm (not terminals, transcripts, stores or proxies that answer on
//     the viewer's own credential);
//   - kbeads' /v1/me names the bearer a per-agent token that is unrestricted,
//     which is what a fleet agent's kd already reads with; a land agent's token
//     is restricted and never admitted, since the viewer's shared hubs carry
//     every land;
//   - that agent bead's role is on the viewer.verifier_roles tunable (comma
//     separated; empty, the default, admits no one).
//
// Every daemon read the page makes then goes out on the agent's own bearer
// (operatorMiddleware put it on the context), so kbeads authorizes it as the
// agent. The role is a narrowing on top of the daemon's verdict, not the
// boundary: the boundary is GET-only and an unrestricted fleet token.
const (
	verifierRolesKey = "viewer.verifier_roles"
	verifierRolesEnv = "GARDENIO_VIEWER_VERIFIER_ROLES"
)

// verifierRoutes are the only GETs a verifier reaches, by exact mux pattern:
// the bead, plan and queue pages a crew checks live, the /decisions,
// /escalations and /inbox redirects into /garden (gd-XLra7iK3XE), and the farm
// /garden draws with its stream (gd-TRdxFvBjW2). The pages read the daemon on
// the request's bearer; the farm document and stream are the viewer's shared
// hub, which an unrestricted fleet token may read (owner grant gd-rO1s5UC5m2).
// On GET /garden/live/events only the streams listed here are carried
// (scopeMiddleware). Everything else stays the 403 a direct call
// gets, so a route added later (or one like /cassettes and /image, which read a
// store on the viewer's own credentials) is closed to a verifier until it is
// listed here.
var verifierRoutes = map[string]bool{
	"GET /{$}":                true,
	"GET /{id}":               true,
	"GET /active":             true,
	"GET /decisions":          true,
	"GET /escalations":        true,
	"GET /inbox":              true,
	"GET /inbox/{id}":         true,
	"GET /garden":             true,
	"GET /garden/farm":        true,
	"GET /garden/farm/events": true,
	"GET /garden/live/events": true,
	"GET /plans":              true,
	"GET /plans/{id}":         true,
	"GET /p/{project}":        true,
	"GET /releases":           true,
	"GET /release/{id}":       true,
}

// verifierSteerRoutes are the POSTs a verifier reaches: each only steers what a
// stream the caller opened is sent, keyed by that stream's sid, and writes
// nothing else. The farm view opens no transcript for a verifier
// (gardenhub.Handler.MayTranscript): transcripts are not daemon reads.
var verifierSteerRoutes = map[string]bool{
	"POST /garden/farm/events/view": true,
	"POST /garden/live/sub":         true,
}

// verifierAdmits reports whether a verifier may make a method call to the route
// at pattern: a GET or HEAD on verifierRoutes, a POST on verifierSteerRoutes.
func verifierAdmits(method, pattern string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return verifierRoutes[pattern]
	case http.MethodPost:
		return verifierSteerRoutes[pattern]
	}
	return false
}

// verifierAgentRole returns the role field of an agent bead, read on bearer alone.
type verifierAgentRole func(ctx context.Context, bearer, agentBead string) (string, error)

type verifierKey struct{}

// isVerifier reports whether verifierMiddleware admitted this request.
func isVerifier(ctx context.Context) bool {
	v, _ := ctx.Value(verifierKey{}).(bool)
	return v
}

// parseVerifierRoles splits the viewer.verifier_roles value into role names.
func parseVerifierRoles(raw string) []string {
	var out []string
	for _, r := range strings.Split(raw, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// verifierMiddleware admits a read-only verifier (see above). It acts only on a
// direct call with no edge proof, a bearer and no viewer identity; everything
// else, and every caller it does not admit, passes on unchanged for
// scopeMiddleware to answer. A daemon that cannot say who the caller is, is a
// 502, as on the machine routes.
//
// It runs after operatorMiddleware (the bearer is on the context) and before
// scopeMiddleware. roles is read per request, so the tunable flips without a
// restart.
func verifierMiddleware(next http.Handler, patternOf func(*http.Request) string, roles func() string, who machineWho, roleOf verifierAgentRole, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if who == nil || roleOf == nil || !noEdgeProofFromContext(ctx) {
			next.ServeHTTP(w, r)
			return
		}
		bearer := beadsapi.UserBearerFromContext(ctx)
		if bearer == "" || oidcauth.EmailFromContext(ctx) != "" {
			next.ServeHTTP(w, r)
			return
		}
		pattern := patternOf(r)
		if !verifierAdmits(r.Method, pattern) {
			next.ServeHTTP(w, r)
			return
		}
		allowed := parseVerifierRoles(roles())
		if len(allowed) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		askCtx, cancel := context.WithTimeout(ctx, machineMeTimeout)
		defer cancel()
		me, err := who(askCtx, bearer)
		if err != nil {
			if refusedByDaemon(err) {
				next.ServeHTTP(w, r)
				return
			}
			logger.Warn("verifier: the daemon did not say who the caller is", "path", r.URL.Path, "error", err)
			http.Error(w, "could not verify the caller", http.StatusBadGateway)
			return
		}
		if me == nil || me.Agent == "" || me.AgentBead == "" || !me.Unrestricted {
			next.ServeHTTP(w, r)
			return
		}
		role, err := roleOf(askCtx, bearer, me.AgentBead)
		if err != nil {
			if refusedByDaemon(err) {
				next.ServeHTTP(w, r)
				return
			}
			logger.Warn("verifier: could not read the agent's role", "agent", me.Agent, "path", r.URL.Path, "error", err)
			http.Error(w, "could not verify the caller", http.StatusBadGateway)
			return
		}
		if !slices.Contains(allowed, role) {
			next.ServeHTTP(w, r)
			return
		}
		logger.Info("verifier: read-only agent admitted", "agent", me.Agent, "role", role, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, verifierKey{}, true)))
	})
}

// refusedByDaemon reports whether err is the daemon refusing the bearer or the
// bead (401, 403 or 404), as opposed to failing to answer.
func refusedByDaemon(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound)
}

// daemonAgentRole is the production verifierAgentRole: the agent bead read on
// the agent's own bearer alone.
func daemonAgentRole(daemon *beadsapi.Client) verifierAgentRole {
	return func(ctx context.Context, bearer, agentBead string) (string, error) {
		b, err := daemon.GetBead(beadsapi.WithUserBearerOnly(ctx, bearer), agentBead)
		if err != nil {
			return "", err
		}
		return b.Fields["role"], nil
	}
}
