package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClassifyOperatorRefusal exhaustively exercises the precedence rules in
// classifyOperatorRefusal so a regression in the resolver's breadcrumb logic
// is caught by a unit test rather than by an agent harness blowing up in
// production. The four cases mirror the four refusalSource* labels the kd CLI
// keys on (ka-0000006731).
func TestClassifyOperatorRefusal(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *operatorRefusalTrace)
		want  string
	}{
		{
			name:  "no trace yields unknown",
			setup: nil,
			want:  refusalSourceUnknown,
		},
		{
			name:  "zero trace means header missing",
			setup: func(t *operatorRefusalTrace) {},
			want:  refusalSourceHeaderMissing,
		},
		{
			name: "header rejected wins over fallback",
			setup: func(t *operatorRefusalTrace) {
				t.headerSent = true
				t.headerRejected = true
				t.fallbackAttempted = true
				t.fallbackFoundNonHuman = true
			},
			want: refusalSourceHeaderNonHuman,
		},
		{
			name: "fallback non-human when header absent",
			setup: func(t *operatorRefusalTrace) {
				t.fallbackAttempted = true
				t.fallbackFoundNonHuman = true
			},
			want: refusalSourceFallbackAgentOnly,
		},
		{
			name: "header sent but no rejection and no fallback hit is unknown",
			setup: func(t *operatorRefusalTrace) {
				t.headerSent = true
			},
			want: refusalSourceUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.setup != nil {
				ctx = withOperatorRefusalTrace(ctx)
				trace := operatorRefusalTraceFromContext(ctx)
				if trace == nil {
					t.Fatalf("expected trace to be installed")
				}
				tc.setup(trace)
			}
			got := classifyOperatorRefusal(ctx)
			if got != tc.want {
				t.Fatalf("classifyOperatorRefusal: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestWithOperatorRefusalTraceIdempotent confirms the helper does not clobber
// an existing trace pointer — child contexts inherit the parent's breadcrumb
// struct so the static-token interceptor and the resolver can write to the
// same trace.
func TestWithOperatorRefusalTraceIdempotent(t *testing.T) {
	parent := withOperatorRefusalTrace(context.Background())
	parentTrace := operatorRefusalTraceFromContext(parent)
	parentTrace.headerSent = true

	child := withOperatorRefusalTrace(parent)
	childTrace := operatorRefusalTraceFromContext(child)
	if childTrace != parentTrace {
		t.Fatalf("child trace pointer diverged from parent; want shared")
	}
	if !childTrace.headerSent {
		t.Fatalf("child trace lost parent's headerSent=true breadcrumb")
	}
}

// TestWriteOperatorRefusalHTTPBody asserts that the HTTP 400 body carries the
// stable JSON shape the client decoder keys on. If the body shape drifts, the
// kd CLI will misclassify the refusal and fall back to a generic exit 1
// instead of the EX_USAGE-family 64.
func TestWriteOperatorRefusalHTTPBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOperatorRefusal(rec, refusalSourceHeaderNonHuman)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusBadRequest)
	}

	var body struct {
		Error          string `json:"error"`
		Code           string `json:"code"`
		ResolverSource string `json:"resolver_source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, rec.Body.String())
	}
	if body.Code != operatorRefusalErrorCode {
		t.Errorf("code: got %q want %q", body.Code, operatorRefusalErrorCode)
	}
	if body.ResolverSource != refusalSourceHeaderNonHuman {
		t.Errorf("resolver_source: got %q want %q", body.ResolverSource, refusalSourceHeaderNonHuman)
	}
	if body.Error != operatorRefusalMessage {
		t.Errorf("error: got %q want %q", body.Error, operatorRefusalMessage)
	}
}

// TestGRPCOperatorRefusalEmbedsJSON asserts the gRPC status message contains
// the canonical JSON payload so the gRPC-path client can recover the
// resolver_source symmetrically with the HTTP body.
func TestGRPCOperatorRefusalEmbedsJSON(t *testing.T) {
	err := grpcOperatorRefusal(refusalSourceFallbackAgentOnly)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code: got %v want %v", st.Code(), codes.InvalidArgument)
	}
	msg := st.Message()
	if !strings.Contains(msg, operatorRefusalMessage) {
		t.Errorf("message missing human prefix: %q", msg)
	}
	start := strings.Index(msg, "{")
	if start < 0 {
		t.Fatalf("message missing JSON payload: %q", msg)
	}
	var payload struct {
		Code           string `json:"code"`
		ResolverSource string `json:"resolver_source"`
	}
	if err := json.Unmarshal([]byte(msg[start:]), &payload); err != nil {
		t.Fatalf("unmarshal embedded payload: %v (msg=%s)", err, msg)
	}
	if payload.Code != operatorRefusalErrorCode {
		t.Errorf("code: got %q want %q", payload.Code, operatorRefusalErrorCode)
	}
	if payload.ResolverSource != refusalSourceFallbackAgentOnly {
		t.Errorf("resolver_source: got %q want %q", payload.ResolverSource, refusalSourceFallbackAgentOnly)
	}
}

// TestGRPCOperatorRefusalDefaultsToUnknown asserts the helper falls back to
// the "unknown" sentinel rather than emitting an empty resolver_source —
// the client decoder treats empty as unknown too, but explicit is safer.
func TestGRPCOperatorRefusalDefaultsToUnknown(t *testing.T) {
	err := grpcOperatorRefusal("")
	st, _ := status.FromError(err)
	if !strings.Contains(st.Message(), `"resolver_source":"unknown"`) {
		t.Errorf("expected resolver_source=unknown sentinel, got %q", st.Message())
	}
}
