// Command api is the ypl HTTP service. It applies its database migrations at
// startup, syncs the channel's playlists every SYNC_INTERVAL (an hour when
// unset) as the channel YOUTUBE_CLIENT_ID, YOUTUBE_CLIENT_SECRET and
// YOUTUBE_REFRESH_TOKEN name, answers liveness and readiness probes, logs JSON
// to stdout, and drains in-flight requests and the sync run on SIGINT or
// SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/datapointchris/ypl/api/reconcile"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// shutdownGrace bounds how long in-flight requests get to finish after the
// first SIGINT or SIGTERM.
const shutdownGrace = 10 * time.Second

// defaultSyncInterval is the wait between sync runs when SYNC_INTERVAL is unset.
// A run reads every page of every playlist at a unit a page, and the reads of a
// day's runs are held back from what writes may spend.
const defaultSyncInterval = time.Hour

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := start(context.Background()); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

// start reads the credentials and the sync interval, and opens the database,
// applying its migrations, before the port is bound, so the service answers
// /ready only once its schema is current.
func start(ctx context.Context) error {
	path, err := store.Path()
	if err != nil {
		return err
	}
	creds, err := youtube.CredentialsFromEnv()
	if err != nil {
		return err
	}
	interval, err := syncInterval()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	slog.Info("database ready", "path", path)

	channel, err := youtube.NewChannel(ctx, creds)
	if err != nil {
		return err
	}
	worker := reconcile.NewWorker(reconcile.NewRunner(st, channel, interval), interval, slog.Default())
	return run(ctx, ":"+envOr("PORT", "8080"), worker.Run)
}

// syncInterval is SYNC_INTERVAL as a duration, or defaultSyncInterval when it is
// unset.
func syncInterval() (time.Duration, error) {
	raw := os.Getenv("SYNC_INTERVAL")
	if raw == "" {
		return defaultSyncInterval, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("SYNC_INTERVAL %q is not a positive duration, such as 1h or 30m", raw)
	}
	return interval, nil
}

// run binds addr and serves on it, doing work beside the server. A port that
// cannot be bound is returned before anything is logged as listening.
func run(ctx context.Context, addr string, work func(context.Context)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, work)
}

// serve answers requests on ln, and does work beside them, until ctx ends or
// the first SIGINT or SIGTERM arrives. It then cancels work and drains requests
// for up to shutdownGrace, and returns once work has returned. The signal
// handler is released as the drain starts, so a second signal ends the process
// immediately.
func serve(ctx context.Context, ln net.Listener, work func(context.Context)) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	worked := make(chan struct{})
	go func() {
		work(workCtx)
		close(worked)
	}()
	defer func() {
		cancelWork()
		<-worked
	}()

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
	cancelWork()

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

// ok answers a probe. The database is open and migrated before the listener
// binds, so the service is live and ready as soon as it is bound.
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
