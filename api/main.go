// Command api is the ypl HTTP service. It applies its database migrations at
// startup, syncs the channel's playlists every SYNC_INTERVAL (an hour when
// unset) as the channel YOUTUBE_CLIENT_ID, YOUTUBE_CLIENT_SECRET and
// YOUTUBE_REFRESH_TOKEN name, answers liveness and readiness probes, logs JSON
// to stdout, and drains in-flight requests and the sync run on SIGINT or
// SIGTERM.
//
// It answers /api/v1 only to a request carrying an access token the identity
// provider OIDC_ISSUER signed for a client whose id starts with
// CLI_CLIENT_ID_PREFIX (ypl-cli- when unset). The provider is read beside the
// sync rather than before it, so a provider that is down holds back only the
// requests that need a token.
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
	"sync"
	"syscall"
	"time"

	"github.com/datapointchris/ypl/api/auth"
	"github.com/datapointchris/ypl/api/handlers"
	"github.com/datapointchris/ypl/api/reconcile"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// shutdownGrace bounds how long in-flight requests get to finish after the
// first SIGINT or SIGTERM.
const shutdownGrace = 10 * time.Second

// defaultSyncInterval is the wait between sync runs when SYNC_INTERVAL is unset.
// A run reads every page of every playlist at a unit a page, so a day of runs
// has to fit the day's quota.
const defaultSyncInterval = time.Hour

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := start(context.Background()); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

// start reads the configuration and opens the database, applying its
// migrations, before the port is bound. It then serves while the sync runs and
// the identity provider's discovery document and keys are read. /ready answers
// 200 once they are, which is when a token can be verified.
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
	issuer, clientIDPrefix, err := identityProvider()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	slog.Info("database ready", "path", path)

	// The sync and the API each get a channel, because a run counts its own
	// requests and units from its channel's totals.
	syncChannel, err := youtube.NewChannel(ctx, creds)
	if err != nil {
		return err
	}
	apiChannel, err := youtube.NewChannel(ctx, creds)
	if err != nil {
		return err
	}
	worker := reconcile.NewWorker(reconcile.NewRunner(st, syncChannel, interval), interval, slog.Default())
	provider := auth.NewConnecting(issuer, clientIDPrefix)
	api := handler(handlers.New(st, apiChannel, slog.Default()), provider)
	work := func(ctx context.Context) {
		var wg sync.WaitGroup
		wg.Go(func() { worker.Run(ctx) })
		wg.Go(func() { provider.Run(ctx, slog.Default()) })
		wg.Wait()
	}
	return run(ctx, ":"+envOr("PORT", "8080"), api, work)
}

// defaultClientIDPrefix starts the id of every client whose tokens the API
// accepts when CLI_CLIENT_ID_PREFIX is unset.
const defaultClientIDPrefix = "ypl-cli-"

// identityProvider is OIDC_ISSUER, which has no default, and
// CLI_CLIENT_ID_PREFIX, or defaultClientIDPrefix when it is unset.
func identityProvider() (issuer, clientIDPrefix string, err error) {
	issuer = os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		return "", "", errors.New("OIDC_ISSUER is unset: set it to the identity provider whose keys sign the CLI's access tokens")
	}
	return issuer, envOr("CLI_CLIENT_ID_PREFIX", defaultClientIDPrefix), nil
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

// run binds addr and serves h on it, doing work beside the server. A port that
// cannot be bound is returned before anything is logged as listening.
func run(ctx context.Context, addr string, h http.Handler, work func(context.Context)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, h, work)
}

// serve answers requests on ln with h, and does work beside them, until ctx
// ends or the first SIGINT or SIGTERM arrives. It then cancels work and drains
// requests for up to shutdownGrace, and returns once work has returned. The
// signal handler is released as the drain starts, so a second signal ends the
// process immediately.
func serve(ctx context.Context, ln net.Listener, h http.Handler, work func(context.Context)) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           h,
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

// tokenGate verifies tokens and says whether it can yet, as *auth.Connecting
// does.
type tokenGate interface {
	auth.TokenVerifier
	Ready() bool
}

// handler is every route: the probes, which answer without a token so a
// container healthcheck can call them, and the API, which answers only a
// request carrying a token gate accepts.
func handler(api *handlers.Handlers, gate tokenGate) http.Handler {
	mux := routes(gate.Ready)
	api.Register(mux)
	return auth.RequireBearer(gate, slog.Default())(mux)
}

// routes serves /health, which answers once the listener is bound, and /ready,
// which answers 200 while ready reports true and 503 before that. The database
// is open and migrated before the listener binds.
func routes(ready func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		probe(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			probe(w, http.StatusServiceUnavailable, "starting")
			return
		}
		probe(w, http.StatusOK, "ok")
	})
	return mux
}

func probe(w http.ResponseWriter, code int, status string) {
	wire.JSON(w, code, map[string]string{"status": status})
}

// envOr reads key, treating an empty value as unset. An exported empty PORT
// would otherwise bind ":", which is a random port.
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
