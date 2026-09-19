// Command api is the ypl HTTP service. It applies its database migrations at
// startup, syncs the channel's playlists as the channel YOUTUBE_CLIENT_ID,
// YOUTUBE_CLIENT_SECRET and YOUTUBE_REFRESH_TOKEN name, answers liveness and
// readiness probes, logs JSON to stdout, and drains in-flight requests and the
// sync on SIGINT or SIGTERM. The sync makes a full pass as it starts, then a
// tick every SYNC_INTERVAL (5m when unset) give or take a fifth, and a pass at
// once whenever an edit arrives.
//
// Beside the sync it reads tracklists with the yt-dlp binary YTDLP_PATH names
// (yt-dlp on PATH when unset), one video at a time, at least ENRICH_PACE apart
// (90s when unset) and up to twice that at random. ENRICH_PACE=off reads no
// video, and reading no video needs no yt-dlp.
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
	"github.com/datapointchris/ypl/api/enrich"
	"github.com/datapointchris/ypl/api/handlers"
	"github.com/datapointchris/ypl/api/reconcile"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// shutdownGrace bounds how long in-flight requests get to finish after the
// first SIGINT or SIGTERM. No playlist write begins once the drain starts and no
// push write once the sync is canceled, which happen together, and a write begun
// before then ends within handlers.WriteDuration or reconcile.WriteDuration. So
// the grace outlasts every write with 5 seconds to answer. A container's stop
// timeout has to be longer still.
const shutdownGrace = max(handlers.WriteDuration, reconcile.WriteDuration) + 5*time.Second

// defaultSyncInterval is the mean wait between ticks when SYNC_INTERVAL is
// unset. A tick reads the listing and one playlist, a few units, so a day of
// them spends under a tenth of the quota.
const defaultSyncInterval = 5 * time.Minute

// leastSyncInterval is the shortest SYNC_INTERVAL the server takes. A day of
// ticks any closer spends on reads alone what the pushes need.
const leastSyncInterval = time.Minute

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
	limits, reads, err := enrichment()
	if err != nil {
		return err
	}
	// A configuration that reads no video needs no yt-dlp, so the binary is
	// looked for only where the reading would run it.
	var reader enrich.Reader
	if reads {
		reader, err = ytdlp.NewReader(envOr("YTDLP_PATH", "yt-dlp"))
		if err != nil {
			return fmt.Errorf("%w: install yt-dlp or set YTDLP_PATH to it", err)
		}
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
	tally := enrich.NewTally()
	edits := reconcile.NewEdits()
	runner := reconcile.NewRunner(st, syncChannel, tally, edits, interval)
	worker := reconcile.NewWorker(runner, interval, slog.Default())
	provider := auth.NewConnecting(issuer, clientIDPrefix)
	api := handlers.New(st, apiChannel, handlers.Sync{Edits: edits, Interval: interval}, slog.Default())
	work := func(ctx context.Context) {
		var wg sync.WaitGroup
		wg.Go(func() { worker.Run(ctx) })
		wg.Go(func() { provider.Run(ctx, slog.Default()) })
		if reads {
			wg.Go(func() { _ = enrich.New(st, reader, limits, tally).Run(ctx) })
		}
		wg.Wait()
	}
	return run(ctx, ":"+envOr("PORT", "8080"), handler(api, provider), work, api.Drain)
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
	if err != nil || interval < leastSyncInterval {
		return 0, fmt.Errorf("SYNC_INTERVAL %q is not a duration of at least %v, such as 5m", raw, leastSyncInterval)
	}
	return interval, nil
}

// defaultEnrichPace is the least time between two reads of videos when
// ENRICH_PACE is unset, about 26 reads an hour once each wait adds its jitter.
// yt-dlp reads from the server's own address, which YouTube throttles after
// reads that come too fast.
const defaultEnrichPace = 90 * time.Second

// enrichment is the limits the reading of videos keeps to, from ENRICH_PACE or
// defaultEnrichPace when it is unset, and whether to read videos at all, which
// ENRICH_PACE=off says not to.
func enrichment() (enrich.Limits, bool, error) {
	if raw := os.Getenv("ENRICH_VIDEOS_PER_RUN"); raw != "" {
		return enrich.Limits{}, false, fmt.Errorf("ENRICH_VIDEOS_PER_RUN %q is not a setting this server reads: videos are read one at a time, ENRICH_PACE apart, and ENRICH_PACE=off reads none", raw)
	}
	raw := os.Getenv("ENRICH_PACE")
	switch raw {
	case "":
		return enrich.Limits{Pace: defaultEnrichPace}, true, nil
	case "off":
		return enrich.Limits{}, false, nil
	}
	pace, err := time.ParseDuration(raw)
	if err != nil || pace <= 0 {
		return enrich.Limits{}, false, fmt.Errorf("ENRICH_PACE %q is not a positive duration, such as 90s, or off to read no video", raw)
	}
	return enrich.Limits{Pace: pace}, true, nil
}

// run binds addr and serves h on it, doing work beside the server. A port that
// cannot be bound is returned before anything is logged as listening.
func run(ctx context.Context, addr string, h http.Handler, work func(context.Context), drain func()) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, h, work, drain)
}

// serve answers requests on ln with h, and does work beside them, until ctx
// ends or the first SIGINT or SIGTERM arrives. It then cancels work, calls
// drain, and drains requests for up to shutdownGrace, and returns once work has
// returned. The signal handler is released as the drain starts, so a second
// signal ends the process immediately.
func serve(ctx context.Context, ln net.Listener, h http.Handler, work func(context.Context), drain func()) error {
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
	drain()

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
