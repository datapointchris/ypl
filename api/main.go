// Command api is the ypl HTTP service. It answers liveness and readiness
// probes, logs JSON to stdout, and drains in-flight requests on SIGINT or
// SIGTERM.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// shutdownGrace bounds how long in-flight requests get to finish after the
// first SIGINT or SIGTERM.
const shutdownGrace = 10 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(context.Background(), ":"+envOr("PORT", "8080")); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

// run binds addr and serves on it. A port that cannot be bound is returned
// before anything is logged as listening.
func run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln)
}

// serve answers requests on ln until ctx ends or the first SIGINT or SIGTERM
// arrives, then drains for up to shutdownGrace. The signal handler is released
// as the drain starts, so a second signal ends the process immediately.
func serve(ctx context.Context, ln net.Listener) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	served := make(chan error, 1)
	slog.Info("listening", "addr", ln.Addr().String())
	go func() {
		served <- srv.Serve(ln)
	}()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	stop()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// routes serves /health and /ready. Authentication, when this service has it,
// has to leave both reachable, so a container healthcheck can call them.
func routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", ok)
	mux.HandleFunc("GET /ready", ok)
	return mux
}

// ok answers a probe. The service has no dependency to wait on, so it is live
// and ready as soon as its listener is bound.
func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// envOr reads key, treating an empty value as unset. An exported empty PORT
// would otherwise bind ":", which is a random port.
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
