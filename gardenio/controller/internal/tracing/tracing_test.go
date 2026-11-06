package tracing

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInitDisabledByDefault(t *testing.T) {
	t.Setenv("GARDENIO_OTEL_TRACES_ENABLED", "")
	shutdown, err := Init(context.Background(), quietLogger(), Options{ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init returned error when disabled: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
}

func TestInitEnabledRequiresEndpoint(t *testing.T) {
	t.Setenv("GARDENIO_OTEL_TRACES_ENABLED", "true")
	t.Setenv("GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, err := Init(context.Background(), quietLogger(), Options{ServiceName: "test"}); err == nil {
		t.Fatal("expected error when enabled with empty endpoint")
	}
}

func TestInitEnabledRejectsBadEndpoint(t *testing.T) {
	t.Setenv("GARDENIO_OTEL_TRACES_ENABLED", "true")
	t.Setenv("GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT", "://nonsense")
	if _, err := Init(context.Background(), quietLogger(), Options{ServiceName: "test"}); err == nil {
		t.Fatal("expected error for malformed endpoint")
	}
}

func TestInitEnabledWiresProvider(t *testing.T) {
	t.Setenv("GARDENIO_OTEL_TRACES_ENABLED", "1")
	t.Setenv("GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT", "http://gardenio-victoria-traces:10428/insert/opentelemetry/v1/traces")
	shutdown, err := Init(context.Background(), quietLogger(), Options{
		ServiceName:    "gardenio-controller",
		ServiceVersion: "test",
		Environment:    "e2e",
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Starting a span must not panic once a real provider is installed.
	_, span := Tracer("test").Start(context.Background(), "unit")
	span.End()
}

func TestSamplerRatioBounds(t *testing.T) {
	cases := map[string]string{
		"full": "1.0",
		"zero": "0",
		"frac": "0.25",
		"over": "5",
	}
	for name, ratio := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GARDENIO_OTEL_TRACES_SAMPLER_RATIO", ratio)
			if s := sampler(); s == nil {
				t.Fatal("sampler() returned nil")
			}
		})
	}
}
