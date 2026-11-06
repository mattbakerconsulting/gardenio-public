package viewer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

type fakeLookup struct {
	calls atomic.Int64
	resp  *beadsapi.LookupOperatorByAliasResponse
	err   error
}

func (f *fakeLookup) LookupOperatorByAlias(_ context.Context, _, _ string) (*beadsapi.LookupOperatorByAliasResponse, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// fixed-email extractor; the test controls what each ctx maps to via the
// email argument captured here.
func extractor(email string) EmailExtractor {
	return func(_ context.Context) string { return email }
}

// G2.a — happy path: provisioned OIDC subject resolves to a non-sentinel
// operator id.
func TestOperatorID_ResolvesProvisionedSubject(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   42,
			OperatorKind: "human",
			Found:        true,
		},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	id, found, err := r.OperatorID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if id != 42 {
		t.Fatalf("got id %d, want 42", id)
	}
}

// G2.a — cache hit: a second call within TTL does not re-hit the daemon.
func TestOperatorID_CachesWithinTTL(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   42,
			OperatorKind: "human",
			Found:        true,
		},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	for i := range 5 {
		if _, _, err := r.OperatorID(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := lookup.calls.Load(); got != 1 {
		t.Fatalf("expected 1 daemon call, got %d", got)
	}
}

// G2.a — TTL expiry: after the TTL elapses, a fresh call hits the daemon
// again.
func TestOperatorID_RefetchesAfterTTL(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   42,
			OperatorKind: "human",
			Found:        true,
		},
	}
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	r := NewResolver(lookup,
		WithEmailExtractor(extractor("alice@example.com")),
		WithTTL(60*time.Second),
		WithClock(clock),
	)
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second) // past TTL
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Fatalf("expected 2 daemon calls after TTL expiry, got %d", got)
	}
}

// G2.a — explicit invalidation: Invalidate(subject) drops the cache entry
// so the next call re-fetches. This is the SSE-driven invalidation hook.
func TestOperatorID_InvalidateForcesRefetch(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   42,
			OperatorKind: "human",
			Found:        true,
		},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lookup.calls.Load(); got != 1 {
		t.Fatalf("pre-invalidate: expected 1 daemon call, got %d", got)
	}
	r.Invalidate("alice@example.com")
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Fatalf("post-invalidate: expected 2 daemon calls, got %d", got)
	}
}

// G2.a — no viewer identity: anonymous ctx returns (0, false, nil) without
// touching the daemon.
func TestOperatorID_AnonymousReturnsFalse(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{Found: false},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("")))
	id, found, err := r.OperatorID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found || id != 0 {
		t.Fatalf("expected (0,false,nil); got (%d,%v,nil)", id, found)
	}
	if got := lookup.calls.Load(); got != 0 {
		t.Fatalf("anonymous ctx should not hit daemon; got %d calls", got)
	}
}

// G2.a — unprovisioned subject: alias miss returns (0, false, nil) and the
// negative result is cached so repeat calls don't slam the daemon.
func TestOperatorID_UnprovisionedSubjectCached(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{Found: false},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("new@example.com")))
	for i := range 3 {
		id, found, err := r.OperatorID(context.Background())
		if err != nil || found || id != 0 {
			t.Fatalf("call %d: got (%d,%v,%v); want (0,false,nil)", i, id, found, err)
		}
	}
	if got := lookup.calls.Load(); got != 1 {
		t.Fatalf("expected unprovisioned miss to be cached (1 daemon call); got %d", got)
	}
}

// G2.b — sentinel-safe contract: an alias that points at a system/fleet
// operator id MUST be refused. The error wraps ErrSentinelOperator so
// callers can detect this contract violation.
func TestOperatorID_RefusesSentinelKind(t *testing.T) {
	for _, kind := range []string{"system", "fleet"} {
		t.Run(kind, func(t *testing.T) {
			lookup := &fakeLookup{
				resp: &beadsapi.LookupOperatorByAliasResponse{
					OperatorID:   1,
					OperatorKind: kind,
					Found:        true,
				},
			}
			r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
			id, found, err := r.OperatorID(context.Background())
			if err == nil {
				t.Fatalf("expected error for sentinel kind=%s", kind)
			}
			if !errors.Is(err, ErrSentinelOperator) {
				t.Fatalf("error should wrap ErrSentinelOperator; got %v", err)
			}
			if id != 0 || found {
				t.Fatalf("got (%d,%v); want (0,false) on sentinel reject", id, found)
			}
		})
	}
}

// G2.b — sentinel result is NOT cached, so a corrected alias (operator
// re-pointed) becomes visible immediately on the next request.
func TestOperatorID_SentinelResultIsNotCached(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   1,
			OperatorKind: "system",
			Found:        true,
		},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	if _, _, err := r.OperatorID(context.Background()); err == nil {
		t.Fatal("expected sentinel error on first call")
	}
	// Operator re-pointed to a real human via UpsertOperatorAlias.
	lookup.resp = &beadsapi.LookupOperatorByAliasResponse{
		OperatorID:   42,
		OperatorKind: "human",
		Found:        true,
	}
	id, found, err := r.OperatorID(context.Background())
	if err != nil {
		t.Fatalf("post-fix: unexpected error %v", err)
	}
	if !found || id != 42 {
		t.Fatalf("post-fix: got (%d,%v); want (42,true)", id, found)
	}
}

// G2.a — transport error path: the cache is NOT populated on error, and
// the error is surfaced unchanged.
func TestOperatorID_TransportErrorNotCached(t *testing.T) {
	want := errors.New("boom")
	lookup := &fakeLookup{err: want}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	if _, _, err := r.OperatorID(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first call: got %v, want %v", err, want)
	}
	if _, _, err := r.OperatorID(context.Background()); !errors.Is(err, want) {
		t.Fatalf("second call: got %v, want %v", err, want)
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Fatalf("transport error should not be cached; got %d calls (want 2)", got)
	}
}

// G2.c — InvalidateAll evicts every entry; used when a global operators
// mutation event arrives.
func TestInvalidateAll(t *testing.T) {
	lookup := &fakeLookup{
		resp: &beadsapi.LookupOperatorByAliasResponse{
			OperatorID:   42,
			OperatorKind: "human",
			Found:        true,
		},
	}
	r := NewResolver(lookup, WithEmailExtractor(extractor("alice@example.com")))
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.InvalidateAll()
	if _, _, err := r.OperatorID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lookup.calls.Load(); got != 2 {
		t.Fatalf("InvalidateAll should drop entries; got %d calls", got)
	}
}
