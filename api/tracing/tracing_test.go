package tracing

import (
	"testing"

	"go.opentelemetry.io/otel"
)

func TestWithNoEndpointNoProviderIsInstalled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	before := otel.GetTracerProvider()

	shutdown, err := Start(t.Context(), "dev")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("a provider was installed with no endpoint set")
	}
}
