package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/oidcauth"
)

// landNamePattern is the Lands contract's LandName, the same pattern the edge
// captures into $land_door_name (gardenio/helm/gardenio-ziti/public-edge.yaml).
var landNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// deniedRoutes are the daemon routes the land door 404s, matched on the path
// after /v1/ exactly as the edge's two `return 404` locations inside
// ^~ /land/ match them: the token and admin routes, the machine-only hooks and
// embeddings, and the two SSE streams. TestEdgeParity holds the two lists
// together.
var deniedRoutes = []string{
	"bridge-tokens", "fleet-tokens", "auth", "admin", "hooks", "embeddings",
	"events/stream", "bus/events",
}

// blankedHeaders are dropped from every forwarded request, as the edge's land
// door blanks them: nothing a caller sends may name an actor, fleet, operator,
// owner override or edge envelope, so the daemon takes its OIDC path on the
// bearer alone. The X-Auth-Request- and X-Edge-Auth- families go by prefix.
// agentOnlyHeaders and the admission check's marker go too.
var blankedHeaders = append([]string{
	"Cookie", "X-Actor", "X-Fleet", "X-System-Operator", "X-Owner-Override", "X-Operated-By",
	landDoorCheckHeader,
}, agentOnlyHeaders...)

// agentOnlyHeaders are the headers kbeads honours for a per-agent token
// (kbeads interceptors.go): a triggered-by edge, a user bead, the owner,
// auto-parent and auto-inherit opt-outs, and the closed-parent override. They
// are an in-cluster agent pod's; a request through the door never carries one
// (gd-Utbyy4xJRX).
var agentOnlyHeaders = []string{
	"X-Trigger-Bead", "X-User-Bead-Id", "X-No-Owner-Default", "X-No-Auto-Parent",
	"X-No-Auto-Inherit", "X-Force-Parent-Edge",
}

var blankedHeaderPrefixes = []string{"X-Auth-Request-", "X-Edge-Auth-"}

// landDoorHeader names the land door a request came through; kbeads refuses
// it unless the token was minted for that land (kbeads LandDoorHeader).
const landDoorHeader = "X-Land-Door"

// landDoorCheckHeader marks admitLandToken's GET /v1/me as the door's own
// admission check, which kbeads leaves out of the rate limit (kbeads
// LandDoorCheckHeader). scrub drops it from what a caller sends.
const landDoorCheckHeader = "X-Land-Door-Check"

// landAudiencePrefix prefixes the per-land audience a land-<name> client
// scope adds to a token (owner decision gd-zf6eceweED; kbeads
// LandAudiencePrefix).
const landAudiencePrefix = "land-"

// landTokenBearer is the edge's $land_door_land_token: an Authorization
// header whose bearer is one base64url segment, the shape of a minted land
// token (10.11 land principal, D.17). It has no dot, so it can never be a
// JWT: kbeads can only resolve it as a registered token, never as a Keycloak
// one. Every other bearer, a JWT or anything malformed, takes the oidcauth
// path as before.
var landTokenBearer = regexp.MustCompile(`^Bearer [A-Za-z0-9_-]+$`)

// landTokenCheckTimeout bounds landgate's GET /v1/me for a land token.
const landTokenCheckTimeout = 5 * time.Second

// landSubjectPrefix prefixes a land principal's operator id ("land:<land>",
// kbeads landSubjectPrefix).
const landSubjectPrefix = "land:"

// meJSON is the part of kbeads' GET /v1/me body admitLandToken checks
// (kbeads me.go meJSON).
type meJSON struct {
	OperatorID   string `json:"operator_id"`
	Unrestricted bool   `json:"unrestricted"`
	TokenProject string `json:"token_project"`
	AgentBead    string `json:"agent_bead"`
	Lands        []struct {
		Name string `json:"name"`
	} `json:"lands"`
}

// admits reports whether me is a credential land's door forwards: land's
// land principal, scoped to land alone, or a per-agent token kbeads binds
// to land's project. kbeads has already refused, with 403, an agent token
// that is not a tenant land's own-machine agent's (kbeads
// land_door_agent.go). An agent token is not a
// scoped principal, so kbeads reports it unrestricted and in no land; its
// project binding is what confines it.
func (me meJSON) admits(land string) bool {
	if me.AgentBead != "" {
		return me.TokenProject == land && len(me.Lands) == 0
	}
	return me.OperatorID == landSubjectPrefix+land && !me.Unrestricted && len(me.Lands) == 1 && me.Lands[0].Name == land
}

// badPath is the edge's $public_edge_bad_path on the raw request target:
// traversal or ambiguous encodings in the path. A doubled slash is refused
// too, so the path landgate checks is the path the daemon routes on.
var badPath = regexp.MustCompile(`(?i)^[^?]*(\.\.|%2e|%2f|%5c|\\|;|//)`)

// gate is one land's transparent proxy. It holds no credential of its own:
// every request it forwards carries the caller's bearer and nothing else that
// names an identity, so kbeads verifies and scopes it again. It enforces
// nothing the edge's land door does not already enforce; it is the second,
// independent copy of that check, nearer the daemon.
type gate struct {
	land   string
	groups []string // the Keycloak groups that admit a caller to land
	auth   func(http.Handler) http.Handler
	proxy  *httputil.ReverseProxy
	logger *slog.Logger
	// me is kbeads' GET /v1/me, which admitLandToken asks about a land
	// token; client sends it.
	me     *url.URL
	client *http.Client
}

func newGate(land string, upstream *url.URL, auth func(http.Handler) http.Handler, logger *slog.Logger) *gate {
	g := &gate{
		land: land,
		// The edge's /oauth2/auth-land allowed_groups: the land, its owners and
		// its guests, and nobody else (/gardenio-admins included).
		groups: []string{"/project-" + land, "/project-" + land + "/owners", "/project-" + land + "/guests"},
		auth:   auth,
		logger: logger,
		me:     upstream.JoinPath("/v1/me"),
		client: &http.Client{Timeout: landTokenCheckTimeout},
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Tell kbeads which land's door this is, so it also requires the
			// token's land audience to be this land (kbeads land_audience.go).
			// The header only narrows; whatever the edge sent is replaced.
			pr.Out.Header.Set(landDoorHeader, land)
			// Rewrite strips the inbound X-Forwarded-*; the edge is the only
			// caller the NetworkPolicy admits, so its values are the client's.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
				if v := pr.In.Header.Get(h); v != "" {
					pr.Out.Header.Set(h, v)
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("upstream request failed", "path", r.URL.Path, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return g
}

// handler serves the probes and sends everything else to the gate. It is
// not an http.ServeMux: the mux cleans the path and redirects before any
// handler runs, and the gate must see the raw request target to refuse it.
func (g *gate) handler(ready func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.RequestURI == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.RequestURI == "/readyz":
			if !ready() {
				http.Error(w, "oidc provider not ready", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			g.ServeHTTP(w, r)
		}
	})
}

// ServeHTTP checks the path, scrubs the headers, verifies the bearer and its
// land, then forwards. A Keycloak JWT is verified here (oidcauth, admit); a
// minted land token is opaque, so kbeads is asked about it (admitLandToken). The path checks run first so a denied or unlisted
// route (routes.go) is 404 for every caller, signed in or not, as at the
// edge.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if badPath.MatchString(r.RequestURI) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") || isDenied(r.URL.Path) || !landDoorRoutes.allows(r) {
		http.NotFound(w, r)
		return
	}
	scrub(r.Header)
	// oidcauth also reads the token from X-Auth-Request-Access-Token, which
	// scrub has just removed: the Authorization bearer is the only credential.
	if !hasBearer(r) {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	if landTokenBearer.MatchString(r.Header.Get("Authorization")) {
		g.admitLandToken(w, r)
		return
	}
	g.auth(http.HandlerFunc(g.admit)).ServeHTTP(w, r)
}

// admitLandToken admits a minted land token (Door D.18a gd-WTyoLCSMUp): the
// 10.11 land principal a tenant's laptop agent runs kd, gb and gl under, or
// an agent's own per-agent token bound to this land (gd-mh9ANzCHIE), which
// gb stop needs to reach the agent's own bead. Both are opaque, so landgate
// asks kbeads who it is: GET /v1/me with the caller's
// bearer, this land's door and the admission check's marker, nothing else. kbeads' 401 (unknown, expired or
// revoked token) or 403 (another land's token, or a fleet or bridge token,
// refused by landDoorDenial) goes back to the caller; any other answer is
// 502. On 200 landgate checks the body itself rather than trust the door
// check alone: the caller must be this land's land principal, scoped, and to
// this land only (meJSON.admits). Nothing is cached, so a revoke bites on the next request.
func (g *gate) admitLandToken(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, g.me.String(), nil)
	if err != nil {
		g.logger.Error("land token check: building request", "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set(landDoorHeader, g.land)
	req.Header.Set(landDoorCheckHeader, "1")
	resp, err := g.client.Do(req)
	if err != nil {
		g.logger.Error("land token check failed", "path", r.URL.Path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		http.Error(w, "invalid bearer token", http.StatusUnauthorized)
		return
	case http.StatusForbidden:
		g.logger.Info("denied: land token refused at this land's door", "path", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	default:
		g.logger.Error("land token check: unexpected status", "status", resp.StatusCode, "path", r.URL.Path)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	var me meJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me); err != nil {
		g.logger.Error("land token check: decoding /v1/me", "error", err, "path", r.URL.Path)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if !me.admits(g.land) {
		g.logger.Info("denied: token is not this land's land principal or agent", "operator_id", me.OperatorID,
			"unrestricted", me.Unrestricted, "lands", len(me.Lands), "token_project", me.TokenProject, "agent_bead", me.AgentBead, "path", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// admit runs after the bearer verified: the caller must be a member of this
// land, a principal kbeads scopes to its lands, and hold a token minted for
// this land alone. An admin, or a realm user without the project-member
// role, is unrestricted in kbeads, so landgate refuses it rather than forward
// a bearer the daemon would not confine. Operators use their own door (Design
// B §6). A token with no land audience, another land's, or several is
// refused: a user in two lands must not reach the second through this one's
// door (gd-eAfptUBh9Z).
func (g *gate) admit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	roles := oidcauth.RolesFromContext(ctx)
	if !slices.Contains(roles, oidcauth.RoleProjectMember) || oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin) {
		g.logger.Info("denied: not a scoped land principal", "email", oidcauth.EmailFromContext(ctx), "path", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !slices.ContainsFunc(oidcauth.GroupsFromContext(ctx), func(grp string) bool { return slices.Contains(g.groups, grp) }) {
		g.logger.Info("denied: not a member of this land", "email", oidcauth.EmailFromContext(ctx), "path", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if lands := tokenLands(oidcauth.AudiencesFromContext(ctx)); len(lands) != 1 || lands[0] != g.land {
		g.logger.Info("denied: token not minted for this land", "email", oidcauth.EmailFromContext(ctx), "token_lands", lands, "path", r.URL.Path)
		http.Error(w, "forbidden: token not minted for this land (request scope "+landAudiencePrefix+g.land+")", http.StatusForbidden)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// tokenLands returns the lands named by the land- audiences in aud, deduped.
func tokenLands(aud []string) []string {
	var lands []string
	for _, a := range aud {
		if land, ok := strings.CutPrefix(a, landAudiencePrefix); ok && !slices.Contains(lands, land) {
			lands = append(lands, land)
		}
	}
	return lands
}

func isDenied(path string) bool {
	rest := strings.TrimPrefix(path, "/v1/")
	for _, d := range deniedRoutes {
		if rest == d || strings.HasPrefix(rest, d+"/") {
			return true
		}
	}
	return false
}

func scrub(h http.Header) {
	for _, name := range blankedHeaders {
		h.Del(name)
	}
	for name := range h {
		for _, p := range blankedHeaderPrefixes {
			if strings.HasPrefix(name, p) {
				h.Del(name)
			}
		}
	}
}

func hasBearer(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != ""
}
