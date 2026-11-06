package perftracesrelay

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const serviceNameKey = "service.name"

// jaegerTracesToOTLP converts a batch of Jaeger traces into OTLP
// ResourceSpans, grouping spans by their process (service) so each resulting
// ResourceSpans carries the source service.name. Spans whose IDs fail to parse
// are skipped.
func jaegerTracesToOTLP(traces []jaegerTrace) []*tracepb.ResourceSpans {
	byService := map[string]*serviceSpans{}
	order := []string{}

	for _, tr := range traces {
		for _, span := range tr.Spans {
			proc, ok := tr.Processes[span.ProcessID]
			service := proc.ServiceName
			otlpSpan := jaegerSpanToOTLP(span)
			if otlpSpan == nil {
				continue
			}
			grp, exists := byService[service]
			if !exists {
				grp = &serviceSpans{
					service:       service,
					processTags:   nil,
					hasProcess:    ok,
					processExists: ok,
				}
				if ok {
					grp.processTags = proc.Tags
				}
				byService[service] = grp
				order = append(order, service)
			}
			grp.spans = append(grp.spans, otlpSpan)
		}
	}

	out := make([]*tracepb.ResourceSpans, 0, len(order))
	for _, service := range order {
		grp := byService[service]
		out = append(out, grp.toResourceSpans())
	}
	return out
}

type serviceSpans struct {
	service       string
	processTags   []jaegerKeyValue
	hasProcess    bool
	processExists bool
	spans         []*tracepb.Span
}

func (s *serviceSpans) toResourceSpans() *tracepb.ResourceSpans {
	attrs := []*commonpb.KeyValue{
		stringAttr(serviceNameKey, s.service),
	}
	for _, t := range s.processTags {
		if t.Key == serviceNameKey {
			continue
		}
		if kv := jaegerTagToOTLP(t); kv != nil {
			attrs = append(attrs, kv)
		}
	}
	return &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: attrs},
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Scope: &commonpb.InstrumentationScope{Name: scopeName},
				Spans: s.spans,
			},
		},
	}
}

func jaegerSpanToOTLP(span jaegerSpan) *tracepb.Span {
	traceID := parseTraceID(span.TraceID)
	spanID := parseSpanID(span.SpanID)
	if traceID == nil || spanID == nil {
		return nil
	}
	startNanos := uint64(span.StartTime) * 1000
	endNanos := startNanos + uint64(span.Duration)*1000

	out := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            spanID,
		Name:              span.OperationName,
		StartTimeUnixNano: startNanos,
		EndTimeUnixNano:   endNanos,
		Kind:              tracepb.Span_SPAN_KIND_UNSPECIFIED,
	}

	for _, ref := range span.References {
		if ref.RefType != "CHILD_OF" {
			continue
		}
		if pid := parseSpanID(ref.SpanID); pid != nil {
			out.ParentSpanId = pid
			break
		}
	}

	attrs := make([]*commonpb.KeyValue, 0, len(span.Tags))
	for _, t := range span.Tags {
		switch t.Key {
		case "otel.status_code":
			out.Status = jaegerStatus(t)
		case "span.kind":
			out.Kind = jaegerSpanKind(t)
		}
		if kv := jaegerTagToOTLP(t); kv != nil {
			attrs = append(attrs, kv)
		}
	}
	out.Attributes = attrs
	return out
}

func jaegerStatus(t jaegerKeyValue) *tracepb.Status {
	var s string
	_ = json.Unmarshal(t.Value, &s)
	code := tracepb.Status_STATUS_CODE_UNSET
	switch strings.ToUpper(s) {
	case "OK":
		code = tracepb.Status_STATUS_CODE_OK
	case "ERROR":
		code = tracepb.Status_STATUS_CODE_ERROR
	}
	return &tracepb.Status{Code: code}
}

func jaegerSpanKind(t jaegerKeyValue) tracepb.Span_SpanKind {
	var s string
	_ = json.Unmarshal(t.Value, &s)
	switch strings.ToLower(s) {
	case "server":
		return tracepb.Span_SPAN_KIND_SERVER
	case "client":
		return tracepb.Span_SPAN_KIND_CLIENT
	case "producer":
		return tracepb.Span_SPAN_KIND_PRODUCER
	case "consumer":
		return tracepb.Span_SPAN_KIND_CONSUMER
	case "internal":
		return tracepb.Span_SPAN_KIND_INTERNAL
	}
	return tracepb.Span_SPAN_KIND_UNSPECIFIED
}

func jaegerTagToOTLP(t jaegerKeyValue) *commonpb.KeyValue {
	switch t.Type {
	case "bool":
		var b bool
		if err := json.Unmarshal(t.Value, &b); err != nil {
			return nil
		}
		return &commonpb.KeyValue{Key: t.Key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}}
	case "int64":
		i, ok := jaegerInt(t.Value)
		if !ok {
			return nil
		}
		return &commonpb.KeyValue{Key: t.Key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}}
	case "float64":
		var f float64
		if err := json.Unmarshal(t.Value, &f); err != nil {
			return nil
		}
		return &commonpb.KeyValue{Key: t.Key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}}
	default:
		return stringAttr(t.Key, jaegerString(t.Value))
	}
}

// jaegerInt accepts both a JSON number and a JSON string holding an integer,
// since Jaeger encodes int64 tags as strings to avoid precision loss.
func jaegerInt(raw json.RawMessage) (int64, bool) {
	var i int64
	if err := json.Unmarshal(raw, &i); err == nil {
		return i, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if parsed, err := strconv.ParseInt(s, 10, 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func jaegerString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func parseTraceID(s string) []byte {
	return parseHexID(s, 16)
}

func parseSpanID(s string) []byte {
	return parseHexID(s, 8)
}

// parseHexID decodes a Jaeger hex ID, left-padding to the OTLP fixed width.
// Jaeger renders IDs without leading zeros, so a 16-byte trace ID can arrive
// shorter than 32 hex chars.
func parseHexID(s string, width int) []byte {
	if s == "" {
		return nil
	}
	if len(s) < width*2 {
		s = strings.Repeat("0", width*2-len(s)) + s
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != width {
		return nil
	}
	return b
}
