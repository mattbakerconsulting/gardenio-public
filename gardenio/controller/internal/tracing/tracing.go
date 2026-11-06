// Package tracing wires OpenTelemetry trace export for the gardenio controller
// and its sibling bridge binaries. It is greenfield (epic ka-0000000598, task
// ka-0000005802): the spans land in the in-chart VictoriaTraces pod over
// OTLP/HTTP.
//
// The whole subsystem is default-off. When GARDENIO_OTEL_TRACES_ENABLED is not
// truthy, Init installs nothing and the OpenTelemetry global stays the built-in
// no-op TracerProvider, so the package-level Tracer below and any
// tracer.Start(...) call in a hot path costs a couple of nil-ish checks and
// allocates no span. That lets the reconciler / podmanager / beadsapi / bridge
// hot paths carry permanent span instrumentation without a runtime price in
// deployments that have not opted in.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Options carries the static identity a binary stamps onto every span it
// emits. ServiceName distinguishes controller / slack-bridge / kbeads in the
// VictoriaTraces Jaeger UI; Environment is the same cross-estate join key the
// metrics stack uses (GARDENIO_ENV), so a trace can be filtered to e2e vs prod.
type Options struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
}

// Tracer returns a tracer for the given instrumentation scope. It is always
// safe to call: before Init (or when tracing is disabled) it resolves to the
// global no-op provider, so package-level `var tracer = tracing.Tracer(...)`
// declarations in instrumented packages never need an enabled-check.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Init configures the global OpenTelemetry tracer provider and text-map
// propagator from the environment and returns a shutdown function that flushes
// pending spans. When tracing is disabled it returns a no-op shutdown and a nil
// error, leaving the global provider as the OTEL no-op.
//
// Environment:
//
//	GARDENIO_OTEL_TRACES_ENABLED       bool   — master switch (default false)
//	GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT  url — full OTLP/HTTP traces URL, e.g.
//	                                            http://gardenio-victoria-traces:10428/insert/opentelemetry/v1/traces
//	GARDENIO_OTEL_TRACES_SAMPLER_RATIO float  — head sampling ratio (default 1.0)
//	GARDENIO_OTEL_EXPORTER_TIMEOUT     dur    — per-export timeout (default 10s)
func Init(ctx context.Context, logger *slog.Logger, opts Options) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if !envBool("GARDENIO_OTEL_TRACES_ENABLED", false) {
		logger.Info("otel tracing disabled (GARDENIO_OTEL_TRACES_ENABLED not set)")
		return noop, nil
	}

	endpoint := strings.TrimSpace(os.Getenv("GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return noop, fmt.Errorf("GARDENIO_OTEL_TRACES_ENABLED is set but GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return noop, fmt.Errorf("invalid GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT %q: %w", endpoint, err)
	}

	exporterOpts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if parsed.Scheme == "http" {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	if to := envDuration("GARDENIO_OTEL_EXPORTER_TIMEOUT", 10*time.Second); to > 0 {
		exporterOpts = append(exporterOpts, otlptracehttp.WithTimeout(to))
	}

	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return noop, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	res, err := buildResource(ctx, opts)
	if err != nil {
		return noop, fmt.Errorf("building trace resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler()),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("otel error", "error", err)
	}))

	logger.Info("otel tracing enabled",
		"endpoint", endpoint,
		"service", opts.ServiceName,
		"env", opts.Environment,
		"sampler_ratio", envFloat("GARDENIO_OTEL_TRACES_SAMPLER_RATIO", 1.0))

	return tp.Shutdown, nil
}

func buildResource(ctx context.Context, opts Options) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(opts.ServiceName)}
	if opts.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(opts.ServiceVersion))
	}
	if opts.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironment(opts.Environment))
	}
	return resource.New(ctx,
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
}

// sampler returns a ParentBased(TraceIDRatioBased) sampler. Honouring the
// parent decision keeps an incoming sampled trace intact across gardenio's
// internal hops; the ratio governs roots this process starts itself.
func sampler() sdktrace.Sampler {
	ratio := envFloat("GARDENIO_OTEL_TRACES_SAMPLER_RATIO", 1.0)
	switch {
	case ratio >= 1.0:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case ratio <= 0:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
