package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestIsQueryCanceledError exercises the SQLSTATE 57014 classifier across the
// same shapes isDBAuthError is tested against: a typed pq error, a wrapped
// typed error, a stringified gRPC-flattened error, and negatives that must not
// trip it (a different SQLSTATE, and an unrelated string that merely mentions
// "57014"). See bug ka-0000003621 / design ka-0000004060.
func TestIsQueryCanceledError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			name: "typed pq 57014",
			err:  &fakePQError{code: "57014", msg: "pq: canceling statement due to user request"},
			want: true,
		},
		{
			name: "typed pq 57014 wrapped",
			err:  fmt.Errorf("list beads: %w", &fakePQError{code: "57014", msg: "pq: canceling statement due to user request"}),
			want: true,
		},
		{
			name: "stringified gRPC-wrapped 57014 (ListBeads path)",
			// Mirrors the observed shape: the typed pq error flattened into a
			// gRPC status string, losing the type but preserving the SQLSTATE.
			err:  errors.New(`rpc error: code = Internal desc = list beads: pq: canceling statement due to user request (57014)`),
			want: true,
		},
		{
			name: "different SQLSTATE (auth) is not query-canceled",
			err:  &fakePQError{code: "28P01", msg: "pq: password authentication failed"},
			want: false,
		},
		{
			name: "unrelated error mentioning 57014 but not a cancellation",
			err:  errors.New("audit note: ticket 57014 filed"),
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
			if got := isQueryCanceledError(tt.err); got != tt.want {
				t.Errorf("isQueryCanceledError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWriteServerErrorQueryCanceledReclassifies verifies a 57014 error is
// downgraded from 500 to 503 with a Retry-After header (the retryable "slow,
// back off" contract), while a non-57014 error stays a plain 500. It also
// confirms the 57014 branch does not collide with the 28P01 branch — a 28P01
// keeps its own (longer) Retry-After.
func TestWriteServerErrorQueryCanceledReclassifies(t *testing.T) {
	t.Run("57014 -> 503 + Retry-After + query_canceled code", func(t *testing.T) {
		rec := httptest.NewRecorder()
		cancelErr := &fakePQError{code: "57014", msg: "pq: canceling statement due to user request"}
		writeServerError(context.Background(), rec, "handleListBeads", cancelErr, "failed to list beads")

		if rec.Code != 503 {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != queryCanceledRetryAfterSeconds {
			t.Errorf("Retry-After = %q, want %q", ra, queryCanceledRetryAfterSeconds)
		}
		// The body must carry the stable machine-readable code so the kd client
		// can decline to retry a deterministically-slow query (bug ka-0000000979).
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Code != queryCanceledErrorCode {
			t.Errorf("body code = %q, want %q", body.Code, queryCanceledErrorCode)
		}
		if body.Error != "failed to list beads" {
			t.Errorf("body error = %q, want %q", body.Error, "failed to list beads")
		}
	})

	t.Run("28P01 still uses its own Retry-After (branch precedence)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		authErr := &fakePQError{code: "28P01", msg: `pq: password authentication failed for user "gardenio"`}
		writeServerError(context.Background(), rec, "handleListBeads", authErr, "internal server error")

		if rec.Code != 503 {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != dbAuthErrorRetryAfterSeconds {
			t.Errorf("Retry-After = %q, want %q (auth, not query-canceled)", ra, dbAuthErrorRetryAfterSeconds)
		}
	})

	t.Run("non-cancel error -> 500, no Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeServerError(context.Background(), rec, "handleListBeads", errors.New("boom"), "failed to list beads")

		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != "" {
			t.Errorf("Retry-After = %q, want empty", ra)
		}
	})
}

// TestQueryCanceledErrorsMetricIncrements verifies the 5xx logging choke point
// bumps gardenio_beads_query_canceled_errors_total{handler} on a 57014 and leaves it
// untouched on an unrelated error — the signal for `kd search` crossing the 30s
// deadline again (bug ka-0000003621).
func TestQueryCanceledErrorsMetricIncrements(t *testing.T) {
	const handler = "handleListBeads_test"
	queryCanceledErrorsTotal.Reset()

	cancelErr := errors.New(`rpc error: code = Internal desc = list beads: pq: canceling statement due to user request (57014)`)
	if !isQueryCanceledError(cancelErr) {
		t.Fatal("precondition: expected cancelErr to classify as query-canceled")
	}

	if isQueryCanceledError(cancelErr) {
		queryCanceledErrorsTotal.WithLabelValues(handler).Inc()
	}
	if got := testutil.ToFloat64(queryCanceledErrorsTotal.WithLabelValues(handler)); got != 1 {
		t.Errorf("counter = %v, want 1", got)
	}

	// A non-cancel error must not touch the counter.
	if isQueryCanceledError(errors.New("some other failure")) {
		queryCanceledErrorsTotal.WithLabelValues(handler).Inc()
	}
	if got := testutil.ToFloat64(queryCanceledErrorsTotal.WithLabelValues(handler)); got != 1 {
		t.Errorf("counter after non-cancel error = %v, want 1 (unchanged)", got)
	}
}
