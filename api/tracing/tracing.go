// Package tracing puts each request the API serves into the trace of whatever
// called it, and exports the spans over OTLP where the OTEL_* variables name an
// endpoint.
//
// Every member of the baggage a request carries is copied onto each span, so a
// caller's own context reaches the API's spans without this code naming it.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/datapointchris/goapiauth"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/processors/baggagecopy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// serviceName is the spans' service.name unless OTEL_SERVICE_NAME names another.
const serviceName = "ypl"

// untraced is the paths no span is started for. A container healthcheck calls
// the probes every thirty seconds, and their spans would name neither a caller
// nor any work.
var untraced = map[string]bool{"/health": true, "/ready": true}

// Start installs a tracer provider that exports over OTLP/HTTP as the OTEL_*
// variables configure it, naming version as the service's version. With
// neither OTEL_EXPORTER_OTLP_ENDPOINT nor OTEL_EXPORTER_OTLP_TRACES_ENDPOINT set
// it installs nothing, so the server runs as it would with no tracing.
//
// An export that fails is logged rather than returned, since no request waits
// on it. The returned func flushes what is still buffered and stops the
// exporter.
func Start(ctx context.Context, version string) (shutdown func(context.Context) error, err error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("build the OTLP trace exporter: %w", err)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("spans were not exported", "err", err)
	}))
	provider, err := Install(ctx, version, sdktrace.WithBatcher(exporter))
	if err != nil {
		return nil, err
	}
	return provider.Shutdown, nil
}

// Install makes the process's tracer provider one that hands each span to
// export, with the W3C trace-context and baggage propagators. Start installs it
// with an OTLP exporter, and a test with one that holds the spans in memory.
func Install(ctx context.Context, version string, export sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	// A later detector wins, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
	// override what is set first.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("describe the server as a trace resource: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(baggagecopy.NewSpanProcessor(baggagecopy.AllowAllMembers)),
		export,
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return provider, nil
}

// Handler starts a server span for each request, as a child of the
// traceparent the request carries and holding its baggage. The span is named
// by the method alone until NameSpanByRoute names it by the route.
func Handler(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, serviceName,
		otelhttp.WithFilter(func(r *http.Request) bool { return !untraced[r.URL.Path] }))
}

// NameSpanByRoute names the request's server span by the route that matched,
// and sets on it what the path names and who called.
//
// It wraps each handler the mux calls rather than the mux, because the mux sets
// the route on the request it passes down. The auth layers above it each hand
// on a copy of the request, so the route never reaches otelhttp's.
func NameSpanByRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() && r.Pattern != "" {
			method, route, found := strings.Cut(r.Pattern, " ")
			if !found {
				method, route = r.Method, r.Pattern
			}
			span.SetName(method + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
			span.SetAttributes(pathReferences(route, r)...)
			span.SetAttributes(callerOf(r.Context())...)
		}
		next.ServeHTTP(w, r)
	})
}

// referenceAttributes names the attribute a path wildcard's value is set under,
// by the collection the wildcard follows in the route. A path names a playlist,
// a video, a play or a rating by anything a person can retype, such as a title,
// so the value is the reference as sent rather than a key.
var referenceAttributes = map[string]attribute.Key{
	"playlists": "ypl.playlist.reference",
	"videos":    "ypl.video.reference",
	"plays":     "ypl.play.reference",
	"ratings":   "ypl.rating.reference",
}

// pathReferences is each reference the request's path holds, under the
// attribute its collection names.
func pathReferences(route string, r *http.Request) []attribute.KeyValue {
	segments := strings.Split(route, "/")
	var attrs []attribute.KeyValue
	for i := 1; i < len(segments); i++ {
		wildcard, isWildcard := strings.CutPrefix(segments[i], "{")
		wildcard, closed := strings.CutSuffix(wildcard, "}")
		key, known := referenceAttributes[segments[i-1]]
		if !isWildcard || !closed || !known {
			continue
		}
		if value := r.PathValue(wildcard); value != "" {
			attrs = append(attrs, key.String(value))
		}
	}
	return attrs
}

// callerOf is the person and the client the auth layer established for the
// request. A service's token carries no person.
func callerOf(ctx context.Context) []attribute.KeyValue {
	identity, ok := goapiauth.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	var attrs []attribute.KeyValue
	if identity.Subject != "" {
		attrs = append(attrs, semconv.EnduserID(identity.Subject))
	}
	if identity.ClientID != "" {
		attrs = append(attrs, attribute.String("ypl.client.id", identity.ClientID))
	}
	return attrs
}
