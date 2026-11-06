package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestClassifyOperatedBy pins the bounded label vocabulary M2 emits. The
// resolver only ever produces values whose prefix is in this table; novel
// shapes collapse to (sentinel, "") so cardinality stays inside the budget.
func TestClassifyOperatedBy(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantKind     string
		wantProvider string
	}{
		{"empty", "", "empty", ""},
		{"slack real", "slack:U0OPERATOR2", "real", "slack"},
		{"oidc real", "oidc:matt@acme.example", "real", "oidc"},
		{"system sentinel", "system:nightly", "sentinel", "system"},
		{"fleet sentinel", "fleet:gardenio", "sentinel", "fleet"},
		{"no colon", "lonewolf", "sentinel", ""},
		{"unknown prefix", "weird:value", "sentinel", ""},
		{"trailing colon", "slack:", "real", "slack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotProvider := classifyOperatedBy(tc.in)
			if gotKind != tc.wantKind || gotProvider != tc.wantProvider {
				t.Errorf("classifyOperatedBy(%q) = (%q, %q); want (%q, %q)",
					tc.in, gotKind, gotProvider, tc.wantKind, tc.wantProvider)
			}
		})
	}
}

// TestNormalizeAuthTypeLabel pins the M1 auth_type label to the four R9 §4.1
// values. Anything else collapses to "anonymous" — a future expansion of
// AuthIdentity.Type values must not silently widen series cardinality.
func TestNormalizeAuthTypeLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"oidc", "oidc"},
		{"static", "static"},
		{"fleet", "fleet"},
		{"machine", "machine"},
		{"runner", "runner"},
		{"anonymous", "anonymous"},
		{"", "anonymous"},
		{"none", "anonymous"},
		{"refresh", "anonymous"},
		{"weird", "anonymous"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeAuthTypeLabel(tc.in); got != tc.want {
				t.Errorf("normalizeAuthTypeLabel(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRecordHeaderArrival pins M1's bounded label set: every (auth_type,
// header_present) combination produces the expected series, and unknown
// auth types collapse to anonymous rather than expand cardinality.
func TestRecordHeaderArrival(t *testing.T) {
	operatedByHeaderArrivalTotal.Reset()

	recordHeaderArrival("oidc", true)
	recordHeaderArrival("oidc", false)
	recordHeaderArrival("static", true)
	recordHeaderArrival("fleet", false)
	recordHeaderArrival("anonymous", false)
	recordHeaderArrival("not-a-real-type", true) // → anonymous,true

	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("oidc", "true")); got != 1 {
		t.Errorf("oidc/true got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("oidc", "false")); got != 1 {
		t.Errorf("oidc/false got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("static", "true")); got != 1 {
		t.Errorf("static/true got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("fleet", "false")); got != 1 {
		t.Errorf("fleet/false got %v; want 1", got)
	}
	// Unknown auth_type collapses into anonymous,true (no new series).
	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("anonymous", "true")); got != 1 {
		t.Errorf("anonymous/true (collapsed) got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByHeaderArrivalTotal.WithLabelValues("anonymous", "false")); got != 1 {
		t.Errorf("anonymous/false got %v; want 1", got)
	}
}

// TestRecordOperatedByWrite pins M2: every resolver call increments exactly
// one (kind, provider) counter, derived from the resolved value.
func TestRecordOperatedByWrite(t *testing.T) {
	operatedByWritesTotal.Reset()

	recordOperatedByWrite("")
	recordOperatedByWrite("oidc:matt@acme.example")
	recordOperatedByWrite("slack:U0OPERATOR2")
	recordOperatedByWrite("slack:U0OPERATOR2")
	recordOperatedByWrite("system:nightly")
	recordOperatedByWrite("fleet:gardenio")
	recordOperatedByWrite("weird-noprefix")

	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("empty", "")); got != 1 {
		t.Errorf("empty/\"\" got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("real", "oidc")); got != 1 {
		t.Errorf("real/oidc got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("real", "slack")); got != 2 {
		t.Errorf("real/slack got %v; want 2", got)
	}
	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("sentinel", "system")); got != 1 {
		t.Errorf("sentinel/system got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("sentinel", "fleet")); got != 1 {
		t.Errorf("sentinel/fleet got %v; want 1", got)
	}
	// Unknown shapes fold into sentinel,"" — must not invent a new provider.
	if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues("sentinel", "")); got != 1 {
		t.Errorf("sentinel/\"\" (novel shape) got %v; want 1", got)
	}
}

// TestRecordResolverError pins M3: the three named buckets each take their
// increment, and unknown error_type collapses to "unexpected" (does NOT add
// a new series).
func TestRecordResolverError(t *testing.T) {
	operatedByResolverErrorsTotal.Reset()

	recordResolverError("sanitize_reject")
	recordResolverError("agent_lookup_fail")
	recordResolverError("agent_lookup_fail")
	recordResolverError("unexpected")
	recordResolverError("not-a-real-bucket")

	if got := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("sanitize_reject")); got != 1 {
		t.Errorf("sanitize_reject got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("agent_lookup_fail")); got != 2 {
		t.Errorf("agent_lookup_fail got %v; want 2", got)
	}
	if got := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("unexpected")); got != 2 {
		t.Errorf("unexpected (incl. unknown collapse) got %v; want 2", got)
	}
	// Total label series stays at exactly the three R9 §4.1 buckets.
	if n := testutil.CollectAndCount(operatedByResolverErrorsTotal); n != 3 {
		t.Errorf("unexpected series cardinality: got %d; want 3", n)
	}
}

// TestRecordFallback pins M4: result label is bound to {resolved, empty}.
func TestRecordFallback(t *testing.T) {
	operatedByFallbackTotal.Reset()

	recordFallback(true)
	recordFallback(false)
	recordFallback(false)

	if got := testutil.ToFloat64(operatedByFallbackTotal.WithLabelValues("resolved")); got != 1 {
		t.Errorf("resolved got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByFallbackTotal.WithLabelValues("empty")); got != 2 {
		t.Errorf("empty got %v; want 2", got)
	}
}

// TestNormalizeFleetSourceLabel pins M10's cardinality guard: known fleet
// services pass through verbatim; every other subject collapses to "other".
func TestNormalizeFleetSourceLabel(t *testing.T) {
	cases := map[string]string{
		"slack-bridge":  "slack-bridge",
		"jira-bridge":   "jira-bridge",
		"gitlab-bridge": "gitlab-bridge",
		"scheduler":     "scheduler",
		"gardenio":      "other",
		"qa-fleet":      "other",
		"":              "other",
	}
	for in, want := range cases {
		if got := normalizeFleetSourceLabel(in); got != want {
			t.Errorf("normalizeFleetSourceLabel(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestRecordFleetFallback pins M10: the source label is normalized to the
// bounded fleet-service vocabulary and increments once per call.
func TestRecordFleetFallback(t *testing.T) {
	operatorFleetFallbackTotal.Reset()

	recordFleetFallback("slack-bridge")
	recordFleetFallback("jira-bridge")
	recordFleetFallback("jira-bridge")
	recordFleetFallback("some-unlisted-fleet")

	if got := testutil.ToFloat64(operatorFleetFallbackTotal.WithLabelValues("slack-bridge")); got != 1 {
		t.Errorf("slack-bridge got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatorFleetFallbackTotal.WithLabelValues("jira-bridge")); got != 2 {
		t.Errorf("jira-bridge got %v; want 2", got)
	}
	if got := testutil.ToFloat64(operatorFleetFallbackTotal.WithLabelValues("other")); got != 1 {
		t.Errorf("other got %v; want 1", got)
	}
	if n := testutil.CollectAndCount(operatorFleetFallbackTotal); n != 3 {
		t.Errorf("series count got %d; want 3 (slack-bridge, jira-bridge, other)", n)
	}
}

// TestResolveOperatedByPure_FleetSentinel_EmitsFleetFallback proves M10 fires
// at the exact resolver moment a header-less fleet-token write settles on the
// fleet source override — and does NOT fire when an X-Operated-By header
// re-attributes the write to a human (the header path wins before the
// terminal branch). Regression guard for ka-0000002480.
func TestResolveOperatedByPure_FleetSentinel_EmitsFleetFallback(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "off")
	operatorFleetFallbackTotal.Reset()
	srv := newTestServerWithAgentBead(nil)

	// Header-less fleet token → fleet sentinel → counter increments by source.
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	if value, source := srv.resolveOperatedByPure(ctx); value != "fleet:slack-bridge" || source != operatedBySourceFleet {
		t.Fatalf("resolveOperatedByPure = (%q,%q); want (fleet:slack-bridge,%q)", value, source, operatedBySourceFleet)
	}
	if got := testutil.ToFloat64(operatorFleetFallbackTotal.WithLabelValues("slack-bridge")); got != 1 {
		t.Errorf("fleet-fallback slack-bridge got %v; want 1", got)
	}

	// Header-attributed fleet write → human wins → counter must NOT increment.
	ctxHdr := WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	if value, _ := srv.resolveOperatedByPure(ctxHdr); value != "slack:U0OPERATOR1" {
		t.Fatalf("header path value = %q; want slack:U0OPERATOR1", value)
	}
	if got := testutil.ToFloat64(operatorFleetFallbackTotal.WithLabelValues("slack-bridge")); got != 1 {
		t.Errorf("fleet-fallback slack-bridge got %v after header write; want still 1", got)
	}
}

// TestApplyOperatedByHeader_Reject pins the sanitize-reject path: a bad
// header increments M3 with error_type=sanitize_reject AND emits a structured
// log carrying operated_by_rejected=true plus rejected_length, but NOT the
// raw value (which is by definition unsafe to log).
func TestApplyOperatedByHeader_Reject(t *testing.T) {
	operatedByResolverErrorsTotal.Reset()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	bad := "evil\r\nX-Injected: yes"
	ctx := applyOperatedByHeader(context.Background(), bad, "static", "grpc:/probe")

	if v := operatedByHeaderFromContext(ctx); v != "" {
		t.Errorf("rejected header should not be stamped into ctx; got %q", v)
	}
	if got := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("sanitize_reject")); got != 1 {
		t.Errorf("sanitize_reject counter got %v; want 1", got)
	}

	logLine := buf.String()
	if !strings.Contains(logLine, `"operated_by_rejected":true`) {
		t.Errorf("expected operated_by_rejected=true in log; got: %s", logLine)
	}
	if !strings.Contains(logLine, `"rejected_length":`) {
		t.Errorf("expected rejected_length in log; got: %s", logLine)
	}
	if strings.Contains(logLine, "X-Injected") || strings.Contains(logLine, "evil") {
		t.Errorf("raw rejected value leaked into log: %s", logLine)
	}
}

// TestApplyOperatedByHeader_Accept verifies a clean header passes through
// without firing M3 and is stamped into the context.
func TestApplyOperatedByHeader_Accept(t *testing.T) {
	operatedByResolverErrorsTotal.Reset()

	ctx := applyOperatedByHeader(context.Background(), "slack:U0OPERATOR2", "static", "http:POST /v1/beads")
	if v := operatedByHeaderFromContext(ctx); v != "slack:U0OPERATOR2" {
		t.Errorf("clean header should be stamped into ctx; got %q", v)
	}
	if n := testutil.CollectAndCount(operatedByResolverErrorsTotal); n != 0 {
		t.Errorf("clean header should not increment M3; got %d series", n)
	}
}

// TestApplyOperatedByHeader_Empty pins that an empty raw header is a no-op:
// no M3 increment, no log line, ctx unchanged.
func TestApplyOperatedByHeader_Empty(t *testing.T) {
	operatedByResolverErrorsTotal.Reset()

	got := applyOperatedByHeader(context.Background(), "", "static", "callsite")
	if v := operatedByHeaderFromContext(got); v != "" {
		t.Errorf("empty header should not be stamped; got %q", v)
	}
	if n := testutil.CollectAndCount(operatedByResolverErrorsTotal); n != 0 {
		t.Errorf("empty header should not increment M3; got %d series", n)
	}
}

// TestHeaderPresent pins the predicate that drives M1's header_present label.
func TestHeaderPresent(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"nil", nil, false},
		{"empty slice", []string{}, false},
		{"single empty", []string{""}, false},
		{"single non-empty", []string{"slack:U1"}, true},
		{"multiple empty", []string{"", "", ""}, false},
		{"first empty rest set", []string{"", "slack:U1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerPresent(tc.in); got != tc.want {
				t.Errorf("headerPresent(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveOperatedBy_FiresM2 pins the per-write metric: a single resolver
// call (write path) bumps M2 exactly once with the expected (kind, provider)
// labels, regardless of which ladder step fired.
func TestResolveOperatedBy_FiresM2(t *testing.T) {
	bead := &model.Bead{ID: "agent-x", Fields: json.RawMessage(`{"slack_user_id":"U999"}`)}
	srv := newTestServerWithAgentBead(bead)

	cases := []struct {
		name         string
		ctx          context.Context
		wantKind     string
		wantProvider string
	}{
		{
			name:         "oidc",
			ctx:          WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"}),
			wantKind:     "real",
			wantProvider: "oidc",
		},
		{
			name: "static-header",
			ctx: WithOperatedByHeader(
				WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"}),
				"slack:U0OPERATOR2",
			),
			wantKind:     "real",
			wantProvider: "slack",
		},
		{
			name:         "static-fallback",
			ctx:          withTestAgent(context.Background(), "agent-x"),
			wantKind:     "real",
			wantProvider: "slack",
		},
		{
			name:         "fleet sentinel",
			ctx:          WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio"}),
			wantKind:     "sentinel",
			wantProvider: "fleet",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operatedByWritesTotal.Reset()
			_, _ = srv.resolveOperatedBy(tc.ctx)
			if got := testutil.ToFloat64(operatedByWritesTotal.WithLabelValues(tc.wantKind, tc.wantProvider)); got != 1 {
				t.Errorf("%s/%s got %v; want 1", tc.wantKind, tc.wantProvider, got)
			}
		})
	}
}

// TestResolveOperatedBy_FiresM4 pins that M4 (fallback outcome) increments
// only on the static-token agent-bead fallback path, not on OIDC or
// static+header.
func TestResolveOperatedBy_FiresM4(t *testing.T) {
	bead := &model.Bead{ID: "agent-x", Fields: json.RawMessage(`{"slack_user_id":"U999"}`)}
	srv := newTestServerWithAgentBead(bead)

	t.Run("static-fallback resolved", func(t *testing.T) {
		operatedByFallbackTotal.Reset()
		ctx := withTestAgent(context.Background(), "agent-x")
		_, _ = srv.resolveOperatedBy(ctx)
		if got := testutil.ToFloat64(operatedByFallbackTotal.WithLabelValues("resolved")); got != 1 {
			t.Errorf("resolved got %v; want 1", got)
		}
	})

	t.Run("static-fallback empty", func(t *testing.T) {
		operatedByFallbackTotal.Reset()
		emptySrv := newTestServerWithAgentBead(nil)
		ctx := withTestAgent(context.Background(), "no-such-agent")
		_, _ = emptySrv.resolveOperatedBy(ctx)
		if got := testutil.ToFloat64(operatedByFallbackTotal.WithLabelValues("empty")); got != 1 {
			t.Errorf("empty got %v; want 1", got)
		}
	})

	t.Run("oidc does not fire M4", func(t *testing.T) {
		operatedByFallbackTotal.Reset()
		ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"})
		_, _ = srv.resolveOperatedBy(ctx)
		if n := testutil.CollectAndCount(operatedByFallbackTotal); n != 0 {
			t.Errorf("OIDC must not fire M4; got %d series", n)
		}
	})

	t.Run("static+header does not fire M4", func(t *testing.T) {
		operatedByFallbackTotal.Reset()
		ctx := WithOperatedByHeader(
			withTestAgent(context.Background(), "agent-x"),
			"slack:UABC",
		)
		_, _ = srv.resolveOperatedBy(ctx)
		if n := testutil.CollectAndCount(operatedByFallbackTotal); n != 0 {
			t.Errorf("static+header must not fire M4; got %d series", n)
		}
	})
}

// TestOperatedByFromAgentBead_FiresM3 pins that a GetBead error increments
// M3 with error_type=agent_lookup_fail. We use a custom mock that returns
// an error on GetBead since the standard mockStore returns nil-bead-no-err.
func TestOperatedByFromAgentBead_FiresM3(t *testing.T) {
	srv := &BeadsServer{store: &errOnGetBeadStore{}}

	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "agent-x")
	if got != "" {
		t.Errorf("expected empty on store error; got %q", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("agent_lookup_fail")); n != 1 {
		t.Errorf("agent_lookup_fail got %v; want 1", n)
	}
}

// TestMetricsHandler_AllOperatedBySeriesExposed pins that all five metrics
// are registered with the /metrics handler — i.e. their HELP/TYPE lines show
// up even before any increment, so Prometheus scrapers see them as soon as
// kbeads boots.
func TestMetricsHandler_AllOperatedBySeriesExposed(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	// Tick each metric at least once so a sample exists. Without a sample
	// the prometheus text format omits the metric entirely.
	operatedByHeaderArrivalTotal.Reset()
	operatedByWritesTotal.Reset()
	operatedByResolverErrorsTotal.Reset()
	operatedByFallbackTotal.Reset()
	operatedByAmbiguousAgentTotal.Add(0) // touch but do not increment past 0

	recordHeaderArrival("static", true)
	recordOperatedByWrite("oidc:probe@example")
	recordResolverError("sanitize_reject")
	recordFallback(true)

	handler := srv.MetricsHandler(t.Context())
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE gardenio_beads_operated_by_header_arrival_total counter",
		"# TYPE gardenio_beads_operated_by_writes_total counter",
		"# TYPE gardenio_beads_operated_by_resolver_errors_total counter",
		"# TYPE gardenio_beads_operated_by_fallback_total counter",
		"# TYPE gardenio_beads_operated_by_ambiguous_agent_total counter",
		`gardenio_beads_operated_by_header_arrival_total{auth_type="static",header_present="true"}`,
		`gardenio_beads_operated_by_writes_total{kind="real",provider="oidc"}`,
		`gardenio_beads_operated_by_resolver_errors_total{error_type="sanitize_reject"}`,
		`gardenio_beads_operated_by_fallback_total{result="resolved"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output", want)
		}
	}
}

// errOnGetBeadStore is a mockStore that returns an error on GetBead — used
// to exercise the M3 agent_lookup_fail path. All other Store methods panic
// because they should not be reached by these tests.
type errOnGetBeadStore struct {
	mockStore // embed to inherit nil-safe defaults; we override GetBead below
}

func (e *errOnGetBeadStore) GetBead(_ context.Context, _ string) (*model.Bead, error) {
	return nil, errFakeStore
}

var errFakeStore = &fakeStoreError{}

type fakeStoreError struct{}

func (*fakeStoreError) Error() string { return "fake store error" }

// TestNormalizeSourceLabel pins M8's source label to the bounded R9 §4.4
// operated_by_source vocabulary. A novel source collapses to "empty" so the
// series count can never exceed 5 sources × 3 kinds without a deliberate code
// change (ka-0000003187).
func TestNormalizeSourceLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{operatedBySourceOIDC, operatedBySourceOIDC},
		{operatedBySourceHeader, operatedBySourceHeader},
		{operatedBySourceFallback, operatedBySourceFallback},
		{operatedBySourceFleet, operatedBySourceFleet},
		{operatedBySourceMachine, operatedBySourceMachine},
		{operatedBySourceEmpty, operatedBySourceEmpty},
		{"", operatedBySourceEmpty},
		{"legacy_sentinel_cutover", operatedBySourceEmpty}, // refusal source, not a resolver source
		{"weird", operatedBySourceEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeSourceLabel(tc.in); got != tc.want {
				t.Errorf("normalizeSourceLabel(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRecordOperatedByWriteBySource pins M8: every resolver call increments
// exactly one (source, kind) counter. The kind bucket reuses classifyOperatedBy
// so kind=sentinel selects precisely the residual sentinel writes the rollout
// must attribute to a resolver branch (ka-0000003187).
func TestRecordOperatedByWriteBySource(t *testing.T) {
	operatedByWritesBySourceTotal.Reset()

	// A healthy human write via the OIDC branch.
	recordOperatedByWriteBySource("oidc:matt@acme.example", operatedBySourceOIDC)
	// A residual fleet-sentinel write (gate off): source=fleet_token, kind=sentinel.
	recordOperatedByWriteBySource("fleet:slack-bridge", operatedBySourceFleet)
	recordOperatedByWriteBySource("fleet:slack-bridge", operatedBySourceFleet)
	// A refused write resolved empty.
	recordOperatedByWriteBySource("", operatedBySourceEmpty)
	// A header-carried human write.
	recordOperatedByWriteBySource("slack:U0OPERATOR2", operatedBySourceHeader)
	// A novel source collapses to empty; the value is still classified.
	recordOperatedByWriteBySource("system:nightly", "not-a-source")

	if got := testutil.ToFloat64(operatedByWritesBySourceTotal.WithLabelValues(operatedBySourceOIDC, "real")); got != 1 {
		t.Errorf("oidc_subject/real got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByWritesBySourceTotal.WithLabelValues(operatedBySourceFleet, "sentinel")); got != 2 {
		t.Errorf("fleet_token/sentinel got %v; want 2", got)
	}
	if got := testutil.ToFloat64(operatedByWritesBySourceTotal.WithLabelValues(operatedBySourceEmpty, "empty")); got != 1 {
		t.Errorf("empty/empty got %v; want 1", got)
	}
	if got := testutil.ToFloat64(operatedByWritesBySourceTotal.WithLabelValues(operatedBySourceHeader, "real")); got != 1 {
		t.Errorf("static_header/real got %v; want 1", got)
	}
	// Unknown source collapsed to empty, sentinel value preserved.
	if got := testutil.ToFloat64(operatedByWritesBySourceTotal.WithLabelValues(operatedBySourceEmpty, "sentinel")); got != 1 {
		t.Errorf("empty(collapsed)/sentinel got %v; want 1", got)
	}
}

// TestRecordAuthoringGate pins M9's bounded (bead_type, action) label set for
// the staged warn→enforce authoring gate (ka-0000003187).
func TestRecordAuthoringGate(t *testing.T) {
	authoringHumanGateTotal.Reset()

	recordAuthoringGate("trigger", authoringGateActionAllowedHuman)
	recordAuthoringGate("trigger", authoringGateActionAllowedWarn)
	recordAuthoringGate("trigger", authoringGateActionAllowedWarn)
	recordAuthoringGate("schedule", authoringGateActionRefused)

	if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("trigger", authoringGateActionAllowedHuman)); got != 1 {
		t.Errorf("trigger/allowed_human got %v; want 1", got)
	}
	if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("trigger", authoringGateActionAllowedWarn)); got != 2 {
		t.Errorf("trigger/allowed_warn got %v; want 2", got)
	}
	if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("schedule", authoringGateActionRefused)); got != 1 {
		t.Errorf("schedule/refused got %v; want 1", got)
	}
}

// TestRequestsByAuthType_StaticVsFleetWithHeader pins the deprecation-gate
// signal (gd-M4pFzv6bUq): a call on the primary static token increments
// auth_type=static, while a per-agent fleet-token call carrying X-Operated-By
// (which resolveOperatedByPure labels operated_by_source=static_header) does
// NOT. Drives the real gRPC AuthInterceptor + LoggingInterceptor chain.
func TestRequestsByAuthType_StaticVsFleetWithHeader(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)

	call := func(method, tok string, hdr bool) {
		t.Helper()
		pairs := []string{"authorization", "Bearer " + tok}
		if hdr {
			pairs = append(pairs, "x-operated-by", "slack:U0OPERATOR1")
		}
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
		chain := func(ctx context.Context, req any) (any, error) {
			return LoggingInterceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: method},
				func(context.Context, any) (any, error) { return "ok", nil })
		}
		if _, err := AuthInterceptor(withTestToken(fc, "secret"), nil)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, chain); err != nil {
			t.Fatal(err)
		}
	}
	val := func(authType, class string) float64 {
		return testutil.ToFloat64(requestsByAuthTypeTotal.WithLabelValues(authType, class))
	}

	requestsByAuthTypeTotal.Reset()
	call("/beads.v1.BeadsService/CreateBead", "agent-tok", true)
	if got := val("static", "write"); got != 0 {
		t.Errorf("fleet call with X-Operated-By counted as static: %v", got)
	}
	if got := val("fleet", "write"); got != 1 {
		t.Errorf("fleet/write = %v; want 1", got)
	}

	// The former shared admin bearer is a plain fleet token now: it can no
	// longer count as "static", which is what the retirement gate watches.
	call("/beads.v1.BeadsService/CreateBead", "secret", true)
	call("/beads.v1.BeadsService/GetBead", "secret", false)
	if got := val("static", "write") + val("static", "read"); got != 0 {
		t.Errorf("static count = %v; want 0 (the static token is retired)", got)
	}
	if got := val("fleet", "write"); got != 2 {
		t.Errorf("fleet/write = %v; want 2", got)
	}
	if got := val("fleet", "read"); got != 1 {
		t.Errorf("fleet/read = %v; want 1", got)
	}
}

// TestRequestsByAuthType_HTTP asserts the HTTP path counts once per
// authenticated request through AuthMiddleware + loggingMiddleware, classifying
// by verb, and that a rejected (unauthenticated) request is not counted.
func TestRequestsByAuthType_HTTP(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	h := AuthMiddleware(withTestToken(fc, "secret"), nil,
		loggingMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))

	do := func(method, tok string) {
		req := httptest.NewRequest(method, "/v1/beads", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	val := func(authType, class string) float64 {
		return testutil.ToFloat64(requestsByAuthTypeTotal.WithLabelValues(authType, class))
	}

	requestsByAuthTypeTotal.Reset()
	do(http.MethodPost, "agent-tok")
	do(http.MethodGet, "secret")
	do(http.MethodPatch, "secret")
	do(http.MethodGet, "")
	do(http.MethodGet, "bogus")
	if got := val("static", "write") + val("static", "read"); got != 0 {
		t.Errorf("static count = %v; want 0 (the static token is retired)", got)
	}
	if got := val("fleet", "read"); got != 1 {
		t.Errorf("fleet/read = %v; want 1", got)
	}
	if got := val("fleet", "write"); got != 2 {
		t.Errorf("fleet/write = %v; want 2", got)
	}
	if got := val("other", "read"); got != 0 {
		t.Errorf("rejected requests were counted: other/read = %v", got)
	}
}

func TestGRPCMethodIsWrite(t *testing.T) {
	for m, want := range map[string]bool{
		"/beads.v1.BeadsService/GetBead":         false,
		"/beads.v1.BeadsService/ListBeads":       false,
		"/beads.v1.BeadsService/SemanticSearch":  false,
		"/beads.v1.BeadsService/Whoami":          false,
		"/beads.v1.BeadsService/CreateBead":      true,
		"/beads.v1.BeadsService/TrimComments":    true,
		"/beads.v1.BeadsService/BrandNewMutator": true,
	} {
		if got := grpcMethodIsWrite(m); got != want {
			t.Errorf("grpcMethodIsWrite(%q) = %v; want %v", m, got, want)
		}
	}
}
