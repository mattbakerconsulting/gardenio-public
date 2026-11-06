package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/model"
)

// Window principals (Lands: the Window, plan gd-pz87vf3xsB, PW.1
// gd-sCLo2acEFb; narrative PW-D2, PW-D3). A land owner or an operator mints a
// window on a plan and gets a capability URL, /w/gw_<token>. The viewer
// forwards that token to kbeads as the bearer, and kbeads is the wall: a
// window opens exactly one route, GET /v1/windows/self/view, which returns an
// allowlisted projection of the one plan, or the one land (LW.1), the window
// is on (window_routes.go, window_view.go), and that route's live stream, GET
// /v1/windows/self/live, which sends the same projection again when it
// changes (PW.7, window_live.go).
//
// authMethodWindow is NOT a scoped principal and has no tier row. It carries
// no realm role and no group, and it is roleBearing, so every role gate
// enforces against its empty role set instead of waving it through as a
// service token. Three walls keep it on its one route, each enough alone:
//
//   - AuthMiddleware: a gw_ bearer is classified before the land-prefix and
//     fleet fallthrough and never compared with FleetConfig or verified as a
//     JWT. A live window is admitted only for GET /v1/windows/self/view and
//     GET /v1/windows/self/live; any other method or path is 403 before the
//     mux runs. An unknown, expired
//     or revoked one is 401 "invalid token", the body every other unknown
//     bearer gets.
//   - scopedRouteGate: refuses a window on every pattern but those two.
//   - roleBearing with no roles: requireRole refuses it at every rank, and
//     scopeFromContext gives it an empty scope.
//
// The gRPC interceptor refuses it outright: window principals are HTTP-only,
// as land and machine principals are.
//
// The token is generated here (128 bits from crypto/rand, base64url, prefixed
// gw_), returned once by POST /v1/windows, and stored only as its sha256 on
// the type=window bead. It never enters FleetConfig or the config store, which
// persist and compare tokens raw.

// authMethodWindow marks a window principal (see above).
const authMethodWindow authMethod = "window"

// windowTokenPrefix is the namespace of window bearers. A bearer with this
// prefix is only ever looked up as a window.
const windowTokenPrefix = "gw_"

// windowTokenBytes is the token's entropy: 128 bits.
const windowTokenBytes = 16

// windowTokenLen is the length of a well-formed window token: the prefix and
// 16 bytes in unpadded base64url (22 characters).
var windowTokenLen = len(windowTokenPrefix) + base64.RawURLEncoding.EncodedLen(windowTokenBytes)

// windowSubjectPrefix prefixes a window principal's AuthIdentity.Subject
// ("window:<bead id>"). It cannot collide with an OIDC subject (an email) or a
// land principal ("land:<land>").
const windowSubjectPrefix = "window:"

// windowViewPattern is the one route a window bearer opens.
const windowViewPattern = "GET /v1/windows/self/view"

// windowViewPath is windowViewPattern's path.
const windowViewPath = "/v1/windows/self/view"

// windowPrincipalContextKey holds the window bead a window principal is.
const windowPrincipalContextKey contextKey = "window.principal"

// isWindowBearer reports whether a bearer is in the window namespace.
func isWindowBearer(token string) bool {
	return strings.HasPrefix(token, windowTokenPrefix)
}

// wellFormedWindowToken reports whether token has the shape newWindowToken
// produces. A malformed one is not looked up.
func wellFormedWindowToken(token string) bool {
	if len(token) != windowTokenLen || !isWindowBearer(token) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(windowTokenPrefix):])
	return err == nil && len(raw) == windowTokenBytes
}

// newWindowToken returns a fresh window token and the hash stored for it.
func newWindowToken() (token, hash string, err error) {
	b := make([]byte, windowTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generating window token: %w", err)
	}
	token = windowTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, windowTokenHash(token), nil
}

// windowTokenHash is the lowercase hex sha256 of a window token, the value
// kept in the window bead's token_hash.
func windowTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// windowResolver resolves a window bearer to its live window bead. It returns
// nil and no error for a token that names no window, or one that is revoked,
// expired or closed.
type windowResolver interface {
	resolveWindowBearer(ctx context.Context, token string) (*model.Bead, error)
}

// resolveWindowBearer implements windowResolver over the store: the window
// bead whose token_hash is the bearer's sha256, read unrestricted.
func (s *BeadsServer) resolveWindowBearer(ctx context.Context, token string) (*model.Bead, error) {
	if !wellFormedWindowToken(token) {
		return nil, nil
	}
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{typeWindow},
		Fields: map[string]string{windowFieldTokenHash: windowTokenHash(token)},
		Limit:  2,
	})
	if err != nil {
		return nil, fmt.Errorf("looking up window: %w", err)
	}
	if len(beads) != 1 {
		// Two windows sharing a hash is corrupt data, not a credential.
		return nil, nil
	}
	if !windowLive(beads[0], time.Now()) {
		return nil, nil
	}
	return beads[0], nil
}

// windowPrincipalContext stamps a window principal onto ctx. No request header
// is honored for it, and it carries no role, group, project or fleet-token
// binding: its identity and its one plan come from the window bead.
func windowPrincipalContext(ctx context.Context, fleetCfg *FleetConfig, window *model.Bead) context.Context {
	ctx = withAuthMethod(ctx, authMethodWindow)
	ctx = context.WithValue(ctx, windowPrincipalContextKey, window)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "window", Subject: windowSubjectPrefix + window.ID})
	if fleetCfg != nil {
		ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
	}
	return ctx
}

// windowPrincipalFromContext returns the window bead the caller is a window
// principal for, false for every other caller.
func windowPrincipalFromContext(ctx context.Context) (*model.Bead, bool) {
	if authMethodFromContext(ctx) != authMethodWindow {
		return nil, false
	}
	b, _ := ctx.Value(windowPrincipalContextKey).(*model.Bead)
	return b, b != nil
}

// windowAuthHTTP is AuthMiddleware's window branch for a gw_ bearer. It always
// answers or admits: a gw_ bearer never falls through to the static, fleet,
// land or OIDC paths.
func windowAuthHTTP(w http.ResponseWriter, r *http.Request, windows windowResolver, fleetCfg *FleetConfig, next http.Handler, provided string) {
	var win *model.Bead
	if windows != nil {
		var err error
		win, err = windows.resolveWindowBearer(r.Context(), provided)
		if err != nil {
			slog.Error("window bearer lookup failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "window lookup unavailable")
			return
		}
	}
	if win == nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if r.Method != http.MethodGet || (r.URL.Path != windowViewPath && r.URL.Path != windowLivePath) {
		writeError(w, http.StatusForbidden, "a window opens only "+windowViewPattern)
		return
	}
	next.ServeHTTP(w, r.WithContext(windowPrincipalContext(r.Context(), fleetCfg, win)))
}

// windowAuthGRPC is the gRPC interceptors' refusal of a gw_ bearer: a live
// window is PermissionDenied (window principals are HTTP-only); anything else
// in the namespace is Unauthenticated, as any unknown bearer is.
func windowAuthGRPC(ctx context.Context, windows windowResolver, provided string) error {
	if windows != nil {
		win, err := windows.resolveWindowBearer(ctx, provided)
		if err != nil {
			return status.Error(codes.Unavailable, "window lookup unavailable")
		}
		if win != nil {
			return status.Error(codes.PermissionDenied, "window principals are HTTP-only")
		}
	}
	return status.Error(codes.Unauthenticated, "invalid token")
}
