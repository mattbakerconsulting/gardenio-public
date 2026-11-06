// Package beadswire holds the beads daemon HTTP/JSON wire protocol shared by
// the two clients that speak it: gardenio/controller/internal/beadsapi (the
// controller, gb and the bridges) and kbeads/internal/client (kd). Each client
// keeps its own request headers and typed results; what lives here is the
// part both sides must agree on: the error and 202-escalation bodies, the
// connection-reset retry policy, the expired-token challenge and the
// refresh-token exchange (gd-VEsyCxkCkp).
//
// It sits in the gardenio/clicreds module because both modules already
// require that module and every image that builds either one already copies
// it, so sharing code here needs no go.mod, Dockerfile or build-context
// change. Like the rest of the module it depends only on the stdlib.
package beadswire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError represents an error response from the daemon HTTP API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}

// EscalationCreated is returned (as an error, via errors.As) when the daemon
// responds 202 Accepted after auto-creating an escalation bead in response
// to an X-Escalate: true request that hit an escalatable denial.
type EscalationCreated struct {
	EscalationID string `json:"escalation_id"`
	Message      string `json:"message"`
	DeniedAction string `json:"denied_action"`
	BeadType     string `json:"bead_type"`
}

func (e *EscalationCreated) Error() string {
	return fmt.Sprintf("escalation created (%s): %s", e.EscalationID, e.Message)
}

type escalateKeyType struct{}

// WithEscalate returns a context that signals the client to set
// X-Escalate: true on the next HTTP request made with it. The daemon
// honors the header only when the request would otherwise be denied by an
// escalatable authz gate (e.g. a non-human spawn of a privileged role,
// kbeads/internal/server/spawn_authz.go): instead of a 403 it creates an
// escalation bead in the same round trip and responds 202 Accepted, which
// DecodeResponse turns into an *EscalationCreated error. Used by the kd
// --escalate flag and `gb spawn --escalate` (gd-lsgF8rL4Zm).
func WithEscalate(ctx context.Context) context.Context {
	return context.WithValue(ctx, escalateKeyType{}, true)
}

// EscalateFromContext reports whether ctx was marked via WithEscalate.
func EscalateFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(escalateKeyType{}).(bool)
	return v
}

// DecodeResponse maps a status+body pair to either a typed APIError, an
// *EscalationCreated (202 — an X-Escalate: true request hit an escalatable
// denial and the daemon auto-filed an escalation bead instead), or a
// successful unmarshal into result. If result is nil the body is discarded.
func DecodeResponse(status int, respBody []byte, result any) error {
	if status == http.StatusNoContent {
		return nil
	}
	if status == http.StatusAccepted {
		var esc EscalationCreated
		if json.Unmarshal(respBody, &esc) == nil && esc.EscalationID != "" {
			return &esc
		}
	}
	if status >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return &APIError{StatusCode: status, Message: errResp.Error}
		}
		return &APIError{StatusCode: status, Message: string(respBody)}
	}
	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// ConnRetries bounds extra attempts after a transport-level connection
// failure (reset/refused/EOF) on an idempotent (GET/HEAD/OPTIONS) request —
// e.g. the beads daemon pod being evicted/restarted mid-request (Phase 3
// drill 3, gd-Mc3cPhsInt: sendOnce previously surfaced such an error to the
// caller on the very first attempt, with no retry at all). Non-idempotent
// methods are deliberately excluded: a connection reset gives no guarantee
// the daemon never applied the mutation — retrying a POST here could double
// a write (a duplicate escalation, comment, or bead create).
const (
	ConnRetries      = 2
	ConnRetryBackoff = 150 * time.Millisecond
)

// IsRetryableConnErr reports whether err looks like a transport-level
// connection failure — a dial/read/write error (surfaced as *net.OpError) or
// an unexpected EOF — as opposed to a context cancellation/deadline or an
// HTTP-level error the server actually returned.
func IsRetryableConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// IsIdempotentMethod reports whether method is safe to blindly resend after a
// connection failure whose outcome on the server is unknown.
func IsIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// SleepWithContext sleeps for d or until ctx is done, whichever comes first,
// returning ctx.Err() if the context ended first.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryConn calls send once and, for an idempotent method only, calls it up
// to ConnRetries more times while it fails with a retryable connection error,
// doubling ConnRetryBackoff between attempts. sleep waits out each backoff
// (nil means SleepWithContext); if it fails (the context ended) the last send
// error is returned. send records its own results in the caller's variables.
func RetryConn(ctx context.Context, method string, sleep func(context.Context, time.Duration) error, send func() error) error {
	err := send()
	if err == nil || !IsIdempotentMethod(method) {
		return err
	}
	if sleep == nil {
		sleep = SleepWithContext
	}
	backoff := ConnRetryBackoff
	for attempt := 0; attempt < ConnRetries && IsRetryableConnErr(err); attempt++ {
		if serr := sleep(ctx, backoff); serr != nil {
			return err
		}
		backoff *= 2
		err = send()
	}
	return err
}

// DBAuthRetries / DBAuthRetryBackoff bound the client-side retry on the
// daemon's transient DB-auth 503: the daemon could not authenticate to
// Postgres (SQLSTATE 28P01, a mounted password lagging an RDS rotation;
// ka-0000006554) and answered 503 + Retry-After. Two extra attempts at ~500ms
// cover the common case — a single unlucky new-connection dial landing in a
// stale-password window while other pooled connections still authenticate —
// without stalling an interactive call. The full multi-minute rotation window
// is NOT something a client waits out; that is what the daemon's 503 and the
// BeadsDBAuthErrors page are for.
//
// Unlike ConnRetries this applies to EVERY method, POST creates included. A
// 28P01 fails at connection dial, before a statement runs, and the daemon's
// create and update paths do their writes in one transaction, so the refused
// request changed nothing and a re-send recovers a write that would otherwise
// be dropped. The one known exception is POST /v1/beads/{id}/comments: it
// persists the comment, then propagates the write taint, and a 28P01 on that
// second step answers 503 for a comment that already landed, so a re-send
// can duplicate the comment (documented on the daemon's handleAddComment).
const (
	DBAuthRetries      = 2
	DBAuthRetryBackoff = 500 * time.Millisecond
	// MaxRetryAfter caps how long a server-sent Retry-After can make a
	// client wait per attempt, so a large hint cannot hang the caller.
	MaxRetryAfter = 2 * time.Second
)

// QueryCanceledErrorCode is the structured `code` value the daemon stamps on a
// 503 body when the request's statement was cancelled by the 30s deadline
// (SQLSTATE 57014). IsTransientDBAuth keys on it to skip the 503+Retry-After
// retry: unlike a 28P01 stale-password 503 (where a re-send recovers a
// would-be-dropped write), a 57014 means the query is deterministically too
// slow, so re-running it burns another full deadline and turns one ~30s
// failure into a multi-retry ~90s hang (bug ka-0000000979).
const QueryCanceledErrorCode = "query_canceled"

// IsTransientDBAuth reports whether a response is the daemon's transient
// DB-auth 503 — a 503 that carries a Retry-After header. The daemon sends
// Retry-After on a writeServerError 503 reclassification, so this distinguishes
// the retryable credential blip from a permanent 503 like an unconfigured
// charts backend.
//
// It excludes the statement-cancellation 503 (SQLSTATE 57014), which the daemon
// marks with QueryCanceledErrorCode in the body. That case ALSO carries a
// Retry-After, but retrying it is pure harm (bug ka-0000000979). The exclusion
// is an opt-OUT keyed on the body code, so an older daemon that does not yet
// stamp the code still gets the retry (no regression under a rolling deploy).
func IsTransientDBAuth(status int, retryAfter string, body []byte) bool {
	if status != http.StatusServiceUnavailable || retryAfter == "" {
		return false
	}
	return !ResponseHasErrorCode(body, QueryCanceledErrorCode)
}

// ResponseHasErrorCode reports whether an error-response body is JSON carrying
// {"code": <code>}. Tolerant of a non-JSON or empty body (returns false), so a
// caller can branch on a specific server-sent code without assuming the body
// shape.
func ResponseHasErrorCode(body []byte, code string) bool {
	if len(body) == 0 {
		return false
	}
	var parsed struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	return parsed.Code == code
}

// DBAuthRetryDelay picks the wait before the next DB-auth retry: the server's
// Retry-After hint (whole seconds) when present and parseable, capped at
// MaxRetryAfter, otherwise backoff.
func DBAuthRetryDelay(retryAfter string, backoff time.Duration) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > MaxRetryAfter {
			return MaxRetryAfter
		}
		return d
	}
	return backoff
}

// RetryDBAuth re-sends a request the daemon answered with a transient DB-auth
// 503 (IsTransientDBAuth), up to retries more times, waiting DBAuthRetryDelay
// between attempts. status, body and retryAfter are the response already in
// hand; send performs one more round-trip and returns the same three. It
// returns the last response, or send's error. sleep waits out each delay (nil
// means SleepWithContext); if it fails (the context ended) the last 503 is
// returned so the caller surfaces the actionable HTTP status rather than a
// bare context error.
func RetryDBAuth(ctx context.Context, retries int, backoff time.Duration, sleep func(context.Context, time.Duration) error,
	status int, body []byte, retryAfter string, send func() (int, []byte, string, error)) (int, []byte, error) {
	if sleep == nil {
		sleep = SleepWithContext
	}
	for attempt := 0; attempt < retries && IsTransientDBAuth(status, retryAfter, body); attempt++ {
		if err := sleep(ctx, DBAuthRetryDelay(retryAfter, backoff)); err != nil {
			return status, body, nil
		}
		var err error
		status, body, retryAfter, err = send()
		if err != nil {
			return 0, nil, err
		}
	}
	return status, body, nil
}

// IsExpiredAccessTokenChallenge reports whether the WWW-Authenticate header
// is the daemon's "token expired" signal. We require BOTH error="invalid_token"
// AND the "token expired" hint so other invalid_token reasons (revoked,
// malformed, missing scope) do NOT silently trigger a refresh — those are
// real auth failures the caller should see.
func IsExpiredAccessTokenChallenge(wwwAuth string) bool {
	if wwwAuth == "" {
		return false
	}
	lower := strings.ToLower(wwwAuth)
	if !strings.Contains(lower, `error="invalid_token"`) {
		return false
	}
	return strings.Contains(lower, "token expired")
}

// RefreshedTokens is the daemon's answer to a refresh-token exchange.
// RefreshToken is set only when the daemon rotates refresh tokens; Rotated
// reports whether the caller should replace its stored refresh token.
type RefreshedTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Rotated      bool   `json:"refresh_token_rotated"`
}

// ExchangeRefreshToken trades refreshToken for a new access token via
// POST baseURL/v1/auth/refresh. A non-200 answer is an *APIError carrying the
// trimmed body. It does no locking: callers serialise concurrent exchanges
// themselves so one expired-token burst costs one round-trip.
func ExchangeRefreshToken(ctx context.Context, hc *http.Client, baseURL, refreshToken string) (RefreshedTokens, error) {
	var rr RefreshedTokens
	reqBody, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return rr, fmt.Errorf("marshal refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/auth/refresh", bytes.NewReader(reqBody))
	if err != nil {
		return rr, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return rr, fmt.Errorf("perform refresh: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return rr, fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return rr, &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(respBytes))}
	}
	if err := json.Unmarshal(respBytes, &rr); err != nil {
		return rr, fmt.Errorf("decode refresh response: %w", err)
	}
	if rr.AccessToken == "" {
		return rr, fmt.Errorf("refresh response missing access_token")
	}
	return rr, nil
}
