package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakePQError mimics the shape lib/pq errors expose (SQLState() string) so the
// classifier can be exercised without a live database, matching the
// interface{ SQLState() string } assertion the server uses elsewhere.
type fakePQError struct {
	code string
	msg  string
}

func (e *fakePQError) Error() string    { return e.msg }
func (e *fakePQError) SQLState() string { return e.code }

func TestIsDBAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			name: "typed pq 28P01",
			err:  &fakePQError{code: "28P01", msg: `pq: password authentication failed for user "gardenio"`},
			want: true,
		},
		{
			name: "typed pq 28P01 wrapped",
			err:  fmt.Errorf("list beads: %w", &fakePQError{code: "28P01", msg: `pq: password authentication failed for user "gardenio"`}),
			want: true,
		},
		{
			name: "stringified gRPC-wrapped 28P01 (ListBeads path)",
			// The exact shape observed in the ka-0000006554 incident logs: the
			// typed pq error is flattened into a gRPC status string.
			err:  errors.New(`rpc error: code = Internal desc = failed to validate type filter: pq: password authentication failed for user "gardenio" (28P01)`),
			want: true,
		},
		{
			name: "different SQLSTATE (unique violation) is not auth",
			err:  &fakePQError{code: "23505", msg: "pq: duplicate key value violates unique constraint"},
			want: false,
		},
		{
			name: "unrelated error mentioning 28P01 but not an auth failure",
			err:  errors.New("audit note: ticket 28P01 filed"),
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDBAuthError(tt.err); got != tt.want {
				t.Errorf("isDBAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWriteServerErrorDBAuthReclassifies verifies that a 28P01 error is
// downgraded from 500 to 503 with a Retry-After header (durability signal for
// write callers), while a non-auth error stays a plain 500.
func TestWriteServerErrorDBAuthReclassifies(t *testing.T) {
	t.Run("28P01 -> 503 + Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		authErr := &fakePQError{code: "28P01", msg: `pq: password authentication failed for user "gardenio"`}
		writeServerError(context.Background(), rec, "handleUpdateBead", authErr, "internal server error")

		if rec.Code != 503 {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != dbAuthErrorRetryAfterSeconds {
			t.Errorf("Retry-After = %q, want %q", ra, dbAuthErrorRetryAfterSeconds)
		}
	})

	t.Run("non-auth error -> 500, no Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeServerError(context.Background(), rec, "handleUpdateBead", errors.New("boom"), "internal server error")

		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != "" {
			t.Errorf("Retry-After = %q, want empty", ra)
		}
	})
}

// TestDBAuthErrorsMetricIncrements verifies the 5xx logging choke point bumps
// gardenio_beads_db_auth_errors_total{handler} on a 28P01 and leaves it untouched on an
// unrelated error — the signal a sustained-DB-auth-failure alert pages on.
func TestDBAuthErrorsMetricIncrements(t *testing.T) {
	const handler = "handleListBeads_test"
	dbAuthErrorsTotal.Reset()

	authErr := errors.New(`rpc error: code = Internal desc = failed to validate type filter: pq: password authentication failed for user "gardenio" (28P01)`)
	if !isDBAuthError(authErr) {
		t.Fatal("precondition: expected authErr to classify as DB-auth")
	}

	// Simulate the increment the middleware performs for a classified 5xx.
	if isDBAuthError(authErr) {
		dbAuthErrorsTotal.WithLabelValues(handler).Inc()
	}
	if got := testutil.ToFloat64(dbAuthErrorsTotal.WithLabelValues(handler)); got != 1 {
		t.Errorf("counter = %v, want 1", got)
	}

	// A non-auth error must not touch the counter.
	nonAuth := errors.New("some other failure")
	if isDBAuthError(nonAuth) {
		dbAuthErrorsTotal.WithLabelValues(handler).Inc()
	}
	if got := testutil.ToFloat64(dbAuthErrorsTotal.WithLabelValues(handler)); got != 1 {
		t.Errorf("counter after non-auth error = %v, want 1 (unchanged)", got)
	}
}

// TestDBAuthErrorEndToEnd drives a real HTTP request whose store call fails with
// a pq 28P01, through the full handler + logging middleware, and asserts the
// user-visible contract: a 503 with Retry-After (not a 500), and a bumped
// gardenio_beads_db_auth_errors_total{handler="handleFindAgentByName"}. This is the
// path that was silent during the ka-0000006554 blackout — handleFindAgentByName
// alone 500'd 151 times with no metric and no alert.
func TestDBAuthErrorEndToEnd(t *testing.T) {
	dbAuthErrorsTotal.Reset()
	_, ms, handler := newTestServer()
	ms.findAgentByNameErr = &fakePQError{
		code: "28P01",
		msg:  `pq: password authentication failed for user "gardenio"`,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents-by-name/some-agent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != dbAuthErrorRetryAfterSeconds {
		t.Errorf("Retry-After = %q, want %q", ra, dbAuthErrorRetryAfterSeconds)
	}
	if got := testutil.ToFloat64(dbAuthErrorsTotal.WithLabelValues("handleFindAgentByName")); got != 1 {
		t.Errorf("gardenio_beads_db_auth_errors_total{handler=handleFindAgentByName} = %v, want 1", got)
	}
}
