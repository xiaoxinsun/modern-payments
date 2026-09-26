// Package telemetry wires the maintained OpenTelemetry SDK and HTTP/PostgreSQL
// instrumentation without exporting credentials, SQL parameters, or statements.
package telemetry

import (
	"context"
	"errors"
	"net/http"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

type Providers struct {
	traces  *sdktrace.TracerProvider
	metrics *metric.MeterProvider
}

func Setup(serviceName string) (*Providers, error) {
	if serviceName == "" {
		return nil, errors.New("telemetry service name is required")
	}
	res := resource.NewSchemaless(
		semconv.ServiceName(serviceName),
	)
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	metrics := metric.NewMeterProvider(metric.WithResource(res))
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(metrics)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return &Providers{traces: traces, metrics: metrics}, nil
}

func (providers *Providers) Shutdown(ctx context.Context) error {
	return errors.Join(providers.metrics.Shutdown(ctx), providers.traces.Shutdown(ctx))
}

func HTTPHandler(serviceName string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, serviceName,
		otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
			return request.Method + " request"
		}),
	)
}

// HTTPTransport instruments outbound service calls without recording headers.
func HTTPTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

func ConfigurePGX(config *pgxpool.Config) {
	config.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithDisableSQLStatementInAttributes(),
		otelpgx.WithDisableConnectionDetailsInAttributes(),
	)
}

func RecordPGXStats(pool *pgxpool.Pool) error {
	return otelpgx.RecordStats(pool)
}
