package beadswire

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIsRetryableConnErr exercises the retry predicate directly across the
// distinguishing cases.
func TestIsRetryableConnErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"net.OpError", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{"generic error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableConnErr(tc.err); got != tc.want {
				t.Errorf("IsRetryableConnErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsIdempotentMethod covers the method allowlist that gates the

// TestIsTransientDBAuth exercises the retry predicate directly across the
// distinguishing cases: only a 503 that carries a Retry-After AND is NOT a
// query_canceled body is retryable.
func TestIsTransientDBAuth(t *testing.T) {
	qcBody := []byte(`{"error":"failed to list beads","code":"` + QueryCanceledErrorCode + `"}`)
	authBody := []byte(`{"error":"internal server error"}`)
	cases := []struct {
		name       string
		status     int
		retryAfter string
		body       []byte
		want       bool
	}{
		{"db-auth 503 with retry-after", http.StatusServiceUnavailable, "5", authBody, true},
		{"db-auth 503 empty body", http.StatusServiceUnavailable, "5", nil, true},
		{"query-canceled 503 not retryable", http.StatusServiceUnavailable, "2", qcBody, false},
		{"503 without retry-after", http.StatusServiceUnavailable, "", authBody, false},
		{"non-503", http.StatusInternalServerError, "5", authBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientDBAuth(tc.status, tc.retryAfter, tc.body); got != tc.want {
				t.Errorf("IsTransientDBAuth(%d, %q, %s) = %v, want %v",
					tc.status, tc.retryAfter, tc.body, got, tc.want)
			}
		})
	}
}

// TestResponseHasErrorCode covers the tolerant body-code parse: it matches only
// on an exact code and never panics on a non-JSON or empty body.
func TestResponseHasErrorCode(t *testing.T) {
	if !ResponseHasErrorCode([]byte(`{"code":"query_canceled"}`), "query_canceled") {
		t.Error("exact code match should be true")
	}
	if ResponseHasErrorCode([]byte(`{"code":"other"}`), "query_canceled") {
		t.Error("different code should be false")
	}
	if ResponseHasErrorCode([]byte(`not json`), "query_canceled") {
		t.Error("non-JSON body should be false, not a panic")
	}
	if ResponseHasErrorCode(nil, "query_canceled") {
		t.Error("empty body should be false")
	}
}

// TestDBAuthRetryDelay covers the per-attempt wait selection: a parseable
// Retry-After is honored (capped), otherwise the base backoff applies.
func TestDBAuthRetryDelay(t *testing.T) {
	base := 500 * time.Millisecond
	cases := []struct {
		retryAfter string
		want       time.Duration
	}{
		{"1", time.Second},
		{"3600", MaxRetryAfter}, // a large hint is capped so it cannot hang the caller
		{"", base},
		{"soon", base},
		{"0", base},
	}
	for _, tc := range cases {
		if got := DBAuthRetryDelay(tc.retryAfter, base); got != tc.want {
			t.Errorf("DBAuthRetryDelay(%q) = %v, want %v", tc.retryAfter, got, tc.want)
		}
	}
}

func TestRetryDBAuth(t *testing.T) {
	noSleep := func(context.Context, time.Duration) error { return nil }
	ended := func(context.Context, time.Duration) error { return context.Canceled }
	authBody := []byte(`{"error":"internal server error"}`)
	cases := []struct {
		name       string
		sleep      func(context.Context, time.Duration) error
		retryAfter string
		failFirst  int // sends (after the first response) that answer 503 again
		sendErr    error
		wantStatus int
		wantSends  int
		wantErr    bool
	}{
		{"recovers within the budget", noSleep, "5", DBAuthRetries - 1, nil, http.StatusOK, DBAuthRetries, false},
		{"gives up after the budget with the 503", noSleep, "5", 100, nil, http.StatusServiceUnavailable, DBAuthRetries, false},
		{"503 without Retry-After is not resent", noSleep, "", 100, nil, http.StatusServiceUnavailable, 0, false},
		{"ended context keeps the 503", ended, "5", 100, nil, http.StatusServiceUnavailable, 0, false},
		{"send error surfaces", noSleep, "5", 0, errors.New("boom"), 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			status, _, err := RetryDBAuth(context.Background(), DBAuthRetries, 0, tc.sleep,
				http.StatusServiceUnavailable, authBody, tc.retryAfter,
				func() (int, []byte, string, error) {
					sends++
					if tc.sendErr != nil {
						return 0, nil, "", tc.sendErr
					}
					if sends <= tc.failFirst {
						return http.StatusServiceUnavailable, authBody, tc.retryAfter, nil
					}
					return http.StatusOK, []byte(`{}`), "", nil
				})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if sends != tc.wantSends {
				t.Errorf("sends = %d, want %d", sends, tc.wantSends)
			}
		})
	}
}

// connection-reset retry.
func TestIsIdempotentMethod(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{http.MethodOptions, true},
		{http.MethodPost, false},
		{http.MethodPut, false},
		{http.MethodPatch, false},
		{http.MethodDelete, false},
	}
	for _, tc := range cases {
		if got := IsIdempotentMethod(tc.method); got != tc.want {
			t.Errorf("IsIdempotentMethod(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}

func TestRetryConn(t *testing.T) {
	reset := &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}
	noSleep := func(context.Context, time.Duration) error { return nil }
	cases := []struct {
		name      string
		method    string
		sleep     func(context.Context, time.Duration) error
		failFirst int
		err       error
		wantCalls int
		wantErr   bool
	}{
		{"GET recovers after resets", http.MethodGet, noSleep, ConnRetries, reset, ConnRetries + 1, false},
		{"GET gives up after the bound", http.MethodGet, noSleep, 100, reset, ConnRetries + 1, true},
		{"POST is never resent", http.MethodPost, noSleep, 100, reset, 1, true},
		{"non-connection error is not retried", http.MethodGet, noSleep, 100, errors.New("boom"), 1, true},
		{"ended context stops the retry", http.MethodGet, func(context.Context, time.Duration) error { return context.Canceled }, 100, reset, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := RetryConn(context.Background(), tc.method, tc.sleep, func() error {
				calls++
				if calls <= tc.failFirst {
					return tc.err
				}
				return nil
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestIsExpiredAccessTokenChallenge(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{`Bearer error="invalid_token", error_description="token expired"`, true},
		{`BEARER ERROR="INVALID_TOKEN", ERROR_DESCRIPTION="TOKEN EXPIRED"`, true},
		{`Bearer error="invalid_token", error_description="token revoked"`, false},
		{`Bearer error="insufficient_scope", error_description="token expired"`, false},
	}
	for _, tc := range cases {
		if got := IsExpiredAccessTokenChallenge(tc.header); got != tc.want {
			t.Errorf("IsExpiredAccessTokenChallenge(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestDecodeResponse(t *testing.T) {
	var out struct{ ID string }
	if err := DecodeResponse(http.StatusOK, []byte(`{"ID":"gd-1"}`), &out); err != nil || out.ID != "gd-1" {
		t.Fatalf("200: err=%v out=%+v", err, out)
	}
	if err := DecodeResponse(http.StatusNoContent, nil, &out); err != nil {
		t.Fatalf("204: err=%v", err)
	}

	var esc *EscalationCreated
	if err := DecodeResponse(http.StatusAccepted, []byte(`{"escalation_id":"gd-e","message":"filed"}`), nil); !errors.As(err, &esc) || esc.EscalationID != "gd-e" {
		t.Fatalf("202: err=%v, want *EscalationCreated gd-e", err)
	}

	var apiErr *APIError
	if err := DecodeResponse(http.StatusForbidden, []byte(`{"error":"denied"}`), nil); !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.Message != "denied" {
		t.Fatalf("403 json: err=%v", err)
	}
	if err := DecodeResponse(http.StatusBadGateway, []byte("upstream down"), nil); !errors.As(err, &apiErr) || apiErr.Message != "upstream down" {
		t.Fatalf("502 text: err=%v", err)
	}
}

func TestExchangeRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/refresh" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("X-Case") {
		case "denied":
			http.Error(w, "refresh revoked", http.StatusUnauthorized)
		case "empty":
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"access_token":"a2","refresh_token":"r2","refresh_token_rotated":true}`)
		}
	}))
	defer srv.Close()

	withCase := func(c string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set("X-Case", c)
			return http.DefaultTransport.RoundTrip(r)
		})}
	}
	ctx := context.Background()

	got, err := ExchangeRefreshToken(ctx, withCase("ok"), srv.URL, "r1")
	if err != nil || got != (RefreshedTokens{AccessToken: "a2", RefreshToken: "r2", Rotated: true}) {
		t.Fatalf("ok: got %+v err %v", got, err)
	}
	var apiErr *APIError
	if _, err := ExchangeRefreshToken(ctx, withCase("denied"), srv.URL, "r1"); !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || apiErr.Message != "refresh revoked" {
		t.Fatalf("denied: err=%v", err)
	}
	if _, err := ExchangeRefreshToken(ctx, withCase("empty"), srv.URL, "r1"); err == nil {
		t.Fatal("empty: want missing access_token error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
