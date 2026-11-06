package perftracesrelay

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestJaegerTracesToOTLP_BasicSpan(t *testing.T) {
	traces := []jaegerTrace{
		{
			TraceID: "abc123",
			Processes: map[string]jaegerProcess{
				"p1": {ServiceName: "site-api", Tags: []jaegerKeyValue{
					{Key: "deployment.namespace", Type: "string", Value: rawJSON(t, "k6-perf")},
				}},
			},
			Spans: []jaegerSpan{
				{
					TraceID:       "abc123",
					SpanID:        "def456",
					OperationName: "GET /studies",
					StartTime:     1_700_000_000_000_000, // micros
					Duration:      1500,                  // micros
					ProcessID:     "p1",
					Tags: []jaegerKeyValue{
						{Key: "http.status_code", Type: "int64", Value: rawJSON(t, "200")},
						{Key: "span.kind", Type: "string", Value: rawJSON(t, "server")},
						{Key: "otel.status_code", Type: "string", Value: rawJSON(t, "OK")},
					},
				},
			},
		},
	}

	rs := jaegerTracesToOTLP(traces)
	if len(rs) != 1 {
		t.Fatalf("expected 1 ResourceSpans, got %d", len(rs))
	}
	if got := resourceServiceName(rs[0]); got != "site-api" {
		t.Fatalf("service.name = %q, want site-api", got)
	}
	if len(rs[0].ScopeSpans) != 1 || len(rs[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("expected 1 span, got %+v", rs[0].ScopeSpans)
	}
	span := rs[0].ScopeSpans[0].Spans[0]

	if span.Name != "GET /studies" {
		t.Errorf("name = %q", span.Name)
	}
	if span.StartTimeUnixNano != 1_700_000_000_000_000_000 {
		t.Errorf("start = %d", span.StartTimeUnixNano)
	}
	if span.EndTimeUnixNano != span.StartTimeUnixNano+1_500_000 {
		t.Errorf("end = %d, want start+1500000ns", span.EndTimeUnixNano)
	}
	if span.Kind != tracepb.Span_SPAN_KIND_SERVER {
		t.Errorf("kind = %v", span.Kind)
	}
	if span.Status == nil || span.Status.Code != tracepb.Status_STATUS_CODE_OK {
		t.Errorf("status = %v", span.Status)
	}
	if hex.EncodeToString(span.TraceId) != "00000000000000000000000000abc123" {
		// 16 bytes = 32 hex chars, left-padded
		t.Errorf("traceID = %s (len %d)", hex.EncodeToString(span.TraceId), len(span.TraceId))
	}
	if len(span.TraceId) != 16 {
		t.Errorf("traceID len = %d, want 16", len(span.TraceId))
	}
	if len(span.SpanId) != 8 {
		t.Errorf("spanID len = %d, want 8", len(span.SpanId))
	}

	if v := attrInt(span.Attributes, "http.status_code"); v != 200 {
		t.Errorf("http.status_code attr = %d, want 200", v)
	}
}

func TestJaegerTracesToOTLP_ParentChild(t *testing.T) {
	traces := []jaegerTrace{
		{
			TraceID:   "aa",
			Processes: map[string]jaegerProcess{"p1": {ServiceName: "sponsor-api"}},
			Spans: []jaegerSpan{
				{
					TraceID: "aa", SpanID: "c0ffee", ProcessID: "p1",
					OperationName: "child-op",
					References: []jaegerRef{
						{RefType: "CHILD_OF", TraceID: "aa", SpanID: "abcdef"},
					},
				},
			},
		},
	}
	rs := jaegerTracesToOTLP(traces)
	span := rs[0].ScopeSpans[0].Spans[0]
	if len(span.ParentSpanId) != 8 {
		t.Fatalf("parent span id len = %d, want 8", len(span.ParentSpanId))
	}
	if got := hex.EncodeToString(span.ParentSpanId); got != "0000000000abcdef" {
		t.Errorf("parent span id = %s, want 0000000000abcdef", got)
	}
}

func TestJaegerTracesToOTLP_GroupsByService(t *testing.T) {
	traces := []jaegerTrace{
		{
			TraceID: "a1",
			Processes: map[string]jaegerProcess{
				"p1": {ServiceName: "site-api"},
				"p2": {ServiceName: "keycloak"},
			},
			Spans: []jaegerSpan{
				{TraceID: "a1", SpanID: "01", ProcessID: "p1", OperationName: "a"},
				{TraceID: "a1", SpanID: "02", ProcessID: "p2", OperationName: "b"},
				{TraceID: "a1", SpanID: "03", ProcessID: "p1", OperationName: "c"},
			},
		},
	}
	rs := jaegerTracesToOTLP(traces)
	if len(rs) != 2 {
		t.Fatalf("expected 2 services, got %d", len(rs))
	}
	counts := map[string]int{}
	for _, r := range rs {
		counts[resourceServiceName(r)] += len(r.ScopeSpans[0].Spans)
	}
	if counts["site-api"] != 2 {
		t.Errorf("site-api spans = %d, want 2", counts["site-api"])
	}
	if counts["keycloak"] != 1 {
		t.Errorf("keycloak spans = %d, want 1", counts["keycloak"])
	}
}

func TestJaegerTracesToOTLP_SkipsBadIDs(t *testing.T) {
	traces := []jaegerTrace{
		{
			TraceID:   "a1",
			Processes: map[string]jaegerProcess{"p1": {ServiceName: "site-api"}},
			Spans: []jaegerSpan{
				{TraceID: "a1", SpanID: "zznothex", ProcessID: "p1", OperationName: "bad"},
				{TraceID: "a1", SpanID: "0a", ProcessID: "p1", OperationName: "good"},
			},
		},
	}
	rs := jaegerTracesToOTLP(traces)
	if len(rs) != 1 {
		t.Fatalf("expected 1 ResourceSpans, got %d", len(rs))
	}
	if got := len(rs[0].ScopeSpans[0].Spans); got != 1 {
		t.Fatalf("expected 1 valid span, got %d", got)
	}
	if rs[0].ScopeSpans[0].Spans[0].Name != "good" {
		t.Errorf("kept wrong span: %q", rs[0].ScopeSpans[0].Spans[0].Name)
	}
}

func TestJaegerTagToOTLP_Types(t *testing.T) {
	cases := []struct {
		name string
		tag  jaegerKeyValue
		want any
	}{
		{"bool", jaegerKeyValue{Key: "k", Type: "bool", Value: rawJSON(t, true)}, true},
		{"int64-string", jaegerKeyValue{Key: "k", Type: "int64", Value: rawJSON(t, "42")}, int64(42)},
		{"int64-number", jaegerKeyValue{Key: "k", Type: "int64", Value: rawJSON(t, 42)}, int64(42)},
		{"float64", jaegerKeyValue{Key: "k", Type: "float64", Value: rawJSON(t, 1.5)}, 1.5},
		{"string", jaegerKeyValue{Key: "k", Type: "string", Value: rawJSON(t, "hi")}, "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := jaegerTagToOTLP(tc.tag)
			if kv == nil {
				t.Fatal("nil kv")
			}
			switch want := tc.want.(type) {
			case bool:
				if kv.Value.GetBoolValue() != want {
					t.Errorf("got %v want %v", kv.Value.GetBoolValue(), want)
				}
			case int64:
				if kv.Value.GetIntValue() != want {
					t.Errorf("got %v want %v", kv.Value.GetIntValue(), want)
				}
			case float64:
				if kv.Value.GetDoubleValue() != want {
					t.Errorf("got %v want %v", kv.Value.GetDoubleValue(), want)
				}
			case string:
				if kv.Value.GetStringValue() != want {
					t.Errorf("got %v want %v", kv.Value.GetStringValue(), want)
				}
			}
		})
	}
}

func TestParseHexID_Padding(t *testing.T) {
	tid := parseTraceID("abc")
	if len(tid) != 16 {
		t.Fatalf("trace id len = %d, want 16", len(tid))
	}
	if hex.EncodeToString(tid) != "00000000000000000000000000000abc" {
		t.Errorf("padded trace id = %s", hex.EncodeToString(tid))
	}
	sid := parseSpanID("1")
	if len(sid) != 8 || hex.EncodeToString(sid) != "0000000000000001" {
		t.Errorf("span id = %s len %d", hex.EncodeToString(sid), len(sid))
	}
	if parseSpanID("nothex!!") != nil {
		t.Error("expected nil for invalid hex")
	}
	if parseTraceID("") != nil {
		t.Error("expected nil for empty")
	}
}

func resourceServiceName(rs *tracepb.ResourceSpans) string {
	for _, a := range rs.Resource.Attributes {
		if a.Key == serviceNameKey {
			return a.Value.GetStringValue()
		}
	}
	return ""
}

func attrInt(attrs []*commonpb.KeyValue, key string) int64 {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.GetIntValue()
		}
	}
	return -1
}
