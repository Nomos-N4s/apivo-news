// Package telemetry builds this process's OpenTelemetry providers (ADR-0007).
//
// It is owned the way internal/platform/logging is owned: one place that
// knows the vendor, and callers above it that do not. Everything outside this
// package passes a context.Context and reads a *Provider; nothing else names
// OpenTelemetry. That is the same rule the ledger port, the network adapters
// and payout.DetailsVault already follow, and it is what makes the backend a
// deployment decision rather than a rewrite.
//
// Two invariants from ADR-0007 are enforced here rather than documented:
//
//   - Invariant 1, secrets: every span is scrubbed on the way out. See
//     scrub.go, which is the whole of that argument.
//   - Invariant 2, the request path: exporters are batched, asynchronous and
//     BOUNDED. A collector that is down, full or slow drops spans. It does
//     not add a millisecond to a wallet read, and it cannot take the
//     newspaper down. The constitution now requires this of any sidecar that
//     is not needed to answer a request.
//
// A deployment that configures no endpoint gets a working process with a
// no-op provider, one line at start-up saying so, and no telemetry. That is
// the same stance cmd/apivo/vault.go takes toward an unconfigured vault, for
// the same reason: observability is not availability.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// exportTimeout bounds one attempt to hand spans to the collector. Short
	// on purpose: a slow collector must fail fast and drop, not queue.
	exportTimeout = 5 * time.Second
	// queueSize bounds what is held for a collector that is not answering.
	// When it fills, the batch processor DROPS - it is non-blocking by
	// default and this package never asks for the blocking variant, which is
	// the whole of Invariant 2's implementation.
	queueSize = 2048
	// shutdownTimeout bounds the flush at exit. A process must not fail to
	// stop because a trace store is unreachable.
	shutdownTimeout = 5 * time.Second
)

// Config is what a deployment says about its telemetry.
type Config struct {
	// Endpoint is the OTLP/HTTP collector root, e.g.
	// http://apivo-qa-alloy:4318. EMPTY MEANS OFF, and off is a supported
	// state rather than a misconfiguration.
	Endpoint string
	// ServiceName identifies this process in every backend. Defaults to
	// DefaultServiceName.
	ServiceName string
	// ServiceVersion is the build's version string, so a regression can be
	// attributed to the deploy that introduced it.
	ServiceVersion string
	// Environment is qa, staging or prod - the same value APP_ENV carries.
	Environment string
	// SampleRatio is the fraction of root traces recorded, 0..1. Zero means
	// DefaultSampleRatio rather than "record nothing": a deployment that
	// configured an endpoint and left this alone meant to see traces.
	SampleRatio float64
}

// DefaultServiceName names this process where a deployment does not.
const DefaultServiceName = "apivo-api"

// DefaultSampleRatio records every root trace.
//
// One is right for this deployment and would be wrong for a busy one. The
// volume here is a newspaper and a wallet on one box; a ratio below one would
// buy nothing and would lose exactly the trace somebody went looking for,
// which is the failure mode that makes people distrust tracing.
const DefaultSampleRatio = 1.0

// Provider holds what this process built, and how to take it down.
//
// The zero value is not usable; New returns one that is, enabled or not.
type Provider struct {
	enabled  bool
	tracer   trace.TracerProvider
	registry *prometheus.Registry
	shutdown []func(context.Context) error
}

// New builds the providers this deployment asked for.
//
// An empty Endpoint is not an error: it returns a disabled Provider that is
// safe to use everywhere, and says so once at ERROR - loud, because a
// deployment nobody can see into is a deployment whose next incident is
// diagnosed by guesswork, and quiet enough to be missed is how it stays that
// way. An endpoint that is SET and unusable IS an error, on the same
// reasoning the vault applies: a deployment that named a collector meant it.
func New(ctx context.Context, log *slog.Logger, cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		log.Error("OTEL_EXPORTER_OTLP_ENDPOINT is unset, so this deployment emits no traces and no metrics: an incident here is diagnosed from container logs alone",
			"key", "OTEL_EXPORTER_OTLP_ENDPOINT")
		return &Provider{enabled: false, tracer: noop.NewTracerProvider()}, nil
	}
	endpoint, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT must be an absolute http or https URL")
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", orDefault(cfg.ServiceName, DefaultServiceName)),
		attribute.String("service.version", cfg.ServiceVersion),
		attribute.String("deployment.environment.name", cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: describing this service: %w", err)
	}

	p := &Provider{enabled: true}
	if err := p.startTracing(ctx, endpoint, res, cfg.SampleRatio); err != nil {
		return nil, err
	}
	if err := p.startMetrics(res); err != nil {
		return nil, err
	}

	log.Info("telemetry configured",
		"endpoint_host", endpoint.Host, "service", orDefault(cfg.ServiceName, DefaultServiceName))
	return p, nil
}

// startTracing builds the tracer provider, its exporter and its scrubber.
func (p *Provider) startTracing(ctx context.Context, endpoint *url.URL, res *resource.Resource, ratio float64) error {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(endpoint.JoinPath("v1", "traces").String()),
		otlptracehttp.WithTimeout(exportTimeout),
	}
	if endpoint.Scheme == "http" {
		// The collector is a sidecar on an internal network, exactly as the
		// ledger is reached over plain HTTP by container name. Saying so
		// explicitly beats letting the SDK infer it from the scheme.
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return fmt.Errorf("telemetry: building the trace exporter: %w", err)
	}

	if ratio <= 0 {
		ratio = DefaultSampleRatio
	}
	provider := sdktrace.NewTracerProvider(
		// The scrubber wraps the exporter, so nothing reaches the collector
		// unfiltered however the span was made - including by
		// instrumentation added after this line was written.
		sdktrace.WithBatcher(scrubbingExporter{inner: exporter},
			sdktrace.WithMaxQueueSize(queueSize),
			sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
		// ParentBased so a sampled trace stays sampled across a boundary:
		// half a trace is worse than none, because it reads as a complete
		// picture of something that did not happen that way.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	p.tracer = provider
	p.shutdown = append(p.shutdown, provider.Shutdown)
	return nil
}

// startMetrics builds the meter provider behind a Prometheus registry.
//
// Metrics are PULLED rather than pushed, which is why there is no exporter
// endpoint here: the collector scrapes [Provider.MetricsHandler]. That keeps
// the process from queueing metric writes toward something unreachable, and
// makes "is it up" answerable by curl rather than by reading a dashboard.
func (p *Provider) startMetrics(res *resource.Resource) error {
	registry := prometheus.NewRegistry()
	reader, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return fmt.Errorf("telemetry: building the metrics reader: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	p.registry = registry
	p.shutdown = append(p.shutdown, provider.Shutdown)
	return nil
}

// Enabled reports whether this deployment configured telemetry.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// TracerProvider answers the provider to instrument with. It is never nil and
// is a no-op when telemetry is off, so a caller never guards a call.
func (p *Provider) TracerProvider() trace.TracerProvider {
	if p == nil || p.tracer == nil {
		return noop.NewTracerProvider()
	}
	return p.tracer
}

// Middleware instruments an HTTP handler, or returns it untouched when
// telemetry is off.
//
// The route pattern is what names the span, never the request path: a path
// carries a member id or a merchant slug, and a span name that varies per
// request makes every trace its own unique operation and every metric label
// unbounded. This is the single most expensive mistake available here.
func (p *Provider) Middleware(pattern string, next http.Handler) http.Handler {
	if !p.Enabled() {
		return next
	}
	return otelhttp.NewHandler(next, pattern,
		otelhttp.WithTracerProvider(p.tracer),
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{})),
		// Without this the span is named for the METHOD alone - every
		// request in the process becomes an operation called "GET" - because
		// otelhttp will not guess a route it was not told. The name is
		// `{method} {route}`, which is OpenTelemetry's own convention for a
		// server span and is what every backend's grouping assumes.
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + pattern
		}),
	)
}

// MetricsHandler serves the Prometheus exposition for the collector to
// scrape. It answers 503 when telemetry is off, rather than an empty page
// that reads as a process with nothing to say.
func (p *Provider) MetricsHandler() http.Handler {
	if !p.Enabled() {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "telemetry is not configured on this deployment", http.StatusServiceUnavailable)
		})
	}
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}

// Shutdown flushes what it can, bounded, and gives up rather than hanging.
//
// Every provider is shut down even if an earlier one failed: a flush that
// errors must not leave a second one running, because the process is on its
// way out and nothing will come back to it.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || len(p.shutdown) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	var errs []error
	for _, stop := range p.shutdown {
		if err := stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// orDefault answers value, or fallback where value is blank.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
