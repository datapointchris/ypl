package youtube

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// A caller's trace and baggage stop at the server. The Data API client wraps
// its transport in otelhttp unless told not to, so with the server's
// propagators installed it would send both to YouTube. The channel here is
// built through that client's own transport, as production's is.
func TestAWriteUnderACallersTraceSendsYouTubeNeither(t *testing.T) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	c, err := NewChannel(t.Context(), Credentials{},
		option.WithEndpoint(server.URL+"/"),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "access"})))
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	ctx := otel.GetTextMapPropagator().Extract(t.Context(), propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"baggage":     "caller.flow.id=7d3c0a91e5f24b6c8a1d2e3f4b5c6d7e",
	})

	if err := c.DeletePlaylist(ctx, "PL1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, header := range []string{"traceparent", "tracestate", "baggage"} {
		if value := seen.Get(header); value != "" {
			t.Errorf("YouTube was sent %s %q", header, value)
		}
	}
}
