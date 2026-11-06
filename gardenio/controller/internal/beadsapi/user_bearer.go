package beadsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"gardenio/clicreds/beadswire"
)

// ErrUserBearerRequired is returned by the daemon client when write-bearer
// enforcement is enabled (SetRequireUserBearerForWrites) but a mutating
// request reaches the transport without a per-request user bearer attached
// via WithUserBearer. The request is NOT sent — the write fails closed.
//
// This is the structural-attribution guarantee for ka-0000006379: a write
// that reaches this client without the human user's own OIDC credential is
// never sent, so human attribution stops depending on header discipline
// (X-Operated-By) and becomes a property of which credential authorized the
// call. The guarantee lives in THIS client gate. A read-only daemon token
// (--allowed-ops read) is a second line, not a hard one: kbeads enforces it
// only in authorizeWrite, and some handlers skip that check.
var ErrUserBearerRequired = errors.New("beadsapi: write requires per-request user bearer (none attached)")

// ErrUserBearerRequiredForRead is the read-side twin of ErrUserBearerRequired:
// returned when read-bearer enforcement is on (SetRequireUserBearerForReads)
// and a read reaches the transport with neither a user bearer (WithUserBearer),
// a machine principal (WithMachinePrincipal) nor the explicit WithSystemRead
// marker. The request is NOT sent. Reads then authenticate as the signed-in
// user, so the daemon's project scoping applies to what the viewer shows.
var ErrUserBearerRequiredForRead = errors.New("beadsapi: read requires per-request user bearer or an explicit system-read marker (none attached)")

// userBearerKeyType is the context key for the per-request user bearer token.
type userBearerKeyType struct{}

// WithUserBearer returns ctx carrying the human user's OIDC access token. When
// write-bearer enforcement is enabled, the next mutating request using this
// context authenticates to the daemon AS THE USER (overriding the client's
// static read token), so the daemon's OIDC RBAC path enforces RoleOperator and
// attributes the write to the cryptographically-verified JWT subject — not to
// the shared "gardenio" service identity.
//
// The viewer stamps this from the X-Auth-Request-Access-Token / Authorization
// bearer set by the OIDC ForwardAuth edge. Pass "" to attach nothing.
func WithUserBearer(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, userBearerKeyType{}, token)
}

// WithoutUserBearer returns ctx with any WithUserBearer bearer masked, so the
// next call goes out on the machine principal on ctx or the client's own
// credential. It is for a write the service makes on its own authority inside
// a person's request, such as the bridge minting a wish head, which kbeads
// refuses from a land member (gd-3HAfTiByJK). It does not lift
// WithUserBearerOnly.
func WithoutUserBearer(ctx context.Context) context.Context {
	if UserBearerFromContext(ctx) == "" {
		return ctx
	}
	return context.WithValue(ctx, userBearerKeyType{}, "")
}

// UserBearerFromContext returns the user bearer attached via WithUserBearer, or
// "" when none was set.
func UserBearerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userBearerKeyType{}).(string)
	return v
}

// userBearerOnlyKeyType is the context key for the WithUserBearerOnly marker.
type userBearerOnlyKeyType struct{}

// WithUserBearerOnly returns ctx on which every daemon call, read or write,
// authenticates as token, whatever the client's read and write modes. Such a
// call never falls back to the client's own credential: with token "" it
// fails closed (ErrUserBearerRequired) without being sent. It is for a handler
// that acts for a verified person on a client that otherwise talks to the
// daemon as its service, so the daemon authorizes and attributes the call to
// that person (the slack-bridge decisions web API, gd-Fhizmbk9Ee).
func WithUserBearerOnly(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, userBearerOnlyKeyType{}, token)
}

// userBearerOnlyFromContext returns the bearer attached via WithUserBearerOnly
// and whether the marker is present.
func userBearerOnlyFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(userBearerOnlyKeyType{}).(string)
	return v, ok
}

// SetRequireUserBearerForWrites toggles fail-closed write-bearer enforcement on
// the daemon client. When enabled, every mutating daemon call (POST/PUT/PATCH/
// DELETE, except read-only POST endpoints — see isReadOnlyPath) MUST carry a
// per-request user bearer via WithUserBearer; otherwise the client returns
// ErrUserBearerRequired without sending the request, and present bearers
// override the static read token on the wire.
//
// Default off: the client behaves exactly as before until a deployment opts in
// (BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1). This lets the read-token
// downscope + flag enablement land as an operator-coordinated step without a
// code change.
func (c *Client) SetRequireUserBearerForWrites(enabled bool) {
	c.requireUserBearerForWrites = enabled
}

// SetPreferUserBearerForWrites makes a per-request user bearer override the
// client's own credential on mutating calls, without turning on the
// fail-closed gate of SetRequireUserBearerForWrites: a mutating call that
// carries no user bearer still goes out on the client's token. It is the
// transitional mode for a component whose human-driven writes already carry
// the person's own token while its autonomous writes have not yet moved off
// its own credential (slack-bridge account linking, gd-OZJ6H2S3xx). With
// SetRequireUserBearerForWrites(true) this flag is redundant.
func (c *Client) SetPreferUserBearerForWrites(enabled bool) {
	c.preferUserBearerForWrites = enabled
}

// systemReadKeyType is the context key for the WithSystemRead marker.
type systemReadKeyType struct{}

// WithSystemRead marks ctx as a read that legitimately has no human behind it
// (the feed hub's upstream stream, startup caches). Under read enforcement such
// a read goes out on the client's own token instead of failing closed. It is a
// deliberately greppable, explicit marker: a request handler must never use it,
// since that would read as the shared service identity rather than the user. A
// user bearer on the same ctx still wins.
func WithSystemRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemReadKeyType{}, true)
}

// IsSystemRead reports whether ctx carries the WithSystemRead marker.
func IsSystemRead(ctx context.Context) bool {
	v, _ := ctx.Value(systemReadKeyType{}).(bool)
	return v
}

// SetRequireUserBearerForReads toggles fail-closed read-bearer enforcement on
// the daemon client. When enabled, every read (non-mutating method, or a
// read-only POST per isReadOnlyPath) authenticates as the user whose bearer is
// on the context, overriding the static token; a read with no bearer, no
// machine principal and no WithSystemRead marker returns
// ErrUserBearerRequiredForRead without being sent.
//
// Default off (BEADS_VIEWER_REQUIRE_USER_BEARER_READS=1 opts in), so this lands
// without changing behaviour until the deployment flips the flag.
func (c *Client) SetRequireUserBearerForReads(enabled bool) {
	c.requireUserBearerForReads = enabled
}

// isMutatingMethod reports whether an HTTP method mutates server state.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// readOnlyPostPaths are daemon endpoints that use POST for request-body reasons
// but perform NO mutation (so they must not be gated as writes). Keep this set
// minimal and explicit — a new mutating endpoint must NOT be added here.
var readOnlyPostPaths = map[string]bool{
	"/v1/search/semantic": true,
}

// isReadOnlyPath reports whether a POST path is a known read-only endpoint that
// is exempt from write-bearer enforcement.
func isReadOnlyPath(path string) bool {
	return readOnlyPostPaths[path]
}

// applyUserBearerWriteGate enforces the write-bearer policy for one request.
// It returns the bearer to use as the wire credential (empty means "leave the
// static token in place") and an error when the policy fails closed.
//
// When enforcement is off, or the request is a read (non-mutating method, or a
// read-only POST), it is a no-op. When on and the request is a write, a present
// user bearer is returned (to override the static token) and a missing one
// yields ErrUserBearerRequired.
func (c *Client) applyUserBearerWriteGate(ctx context.Context, method, path string) (string, error) {
	if !c.requireUserBearerForWrites {
		return "", nil
	}
	if !isMutatingMethod(method) || isReadOnlyPath(path) {
		return "", nil
	}
	bearer := UserBearerFromContext(ctx)
	if bearer == "" {
		return "", ErrUserBearerRequired
	}
	return bearer, nil
}

// applyUserBearerReadGate is the read-side counterpart of
// applyUserBearerWriteGate: a present user bearer is the wire credential, the
// system-read marker falls through to the client's own token, and anything else
// fails closed. A no-op when enforcement is off or the request is a write.
func (c *Client) applyUserBearerReadGate(ctx context.Context, method, path string) (string, error) {
	if !c.requireUserBearerForReads {
		return "", nil
	}
	if isMutatingMethod(method) && !isReadOnlyPath(path) {
		return "", nil
	}
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		return bearer, nil
	}
	if IsSystemRead(ctx) {
		return "", nil
	}
	return "", ErrUserBearerRequiredForRead
}

// doJSONAsUser performs one daemon round-trip that ALWAYS authenticates as
// the given bearer, regardless of SetRequireUserBearerForWrites — for the
// rare endpoint (POST /v1/taints, via CreateTaint) whose admin gate requires
// direct OIDC auth and never honors the client's usual static-token +
// X-Operated-By relay. Unlike doJSON it skips applyUserBearerWriteGate
// entirely (the caller has already decided this bearer is mandatory) and the
// static-token refresh-retry path (a per-request user bearer isn't the
// client's own refreshable credential, so there's nothing to refresh). It
// does share doJSON's transient DB-auth 503 retry (retryDBAuth), re-sent on
// the same bearer.
func (c *Client) doJSONAsUser(ctx context.Context, method, path, bearer string, body, result any) error {
	var bodyBytes []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyBytes = data
	}
	status, respBody, _, retryAfter, _, err := c.sendOnce(ctx, method, path, bodyBytes, bearer)
	if err != nil {
		return err
	}
	status, respBody, err = c.retryDBAuth(ctx, method, path, bodyBytes, bearer, status, respBody, retryAfter)
	if err != nil {
		return err
	}
	return beadswire.DecodeResponse(status, respBody, result)
}
