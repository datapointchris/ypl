// Command api is the ypl HTTP service. It holds a lock beside its database for
// as long as it runs, so a second server on the same store waits for the first
// to exit rather than syncing beside it. It applies its database migrations at
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
// video, and reading no video needs no yt-dlp. The same binary reads the watch
// history with the cookie jar `ypl server cookies set` stores, when asked and
// when YOUTUBE_COOKIE_KEY holds the key the jar is sealed under in the store.
//
// It answers /api/v1 only to a request carrying an access token the identity
// provider OIDC_ISSUER signed for a client whose id starts with
// CLI_CLIENT_ID_PREFIX (ypl-cli- when unset) or SERVICE_CLIENT_ID_PREFIX
// (ypl-svc- when unset). A service client reaches only the routes its scopes
// list. The provider is read beside the sync rather than before it, so a
// provider that is down holds back only the requests that need a token.
//
// With PUBLIC_URL set to the address clients reach it at, it serves the RFC
// 9728 metadata naming that address and OIDC_ISSUER without a token, which is
// how `ypl auth login <url>` finds the provider.
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

	"github.com/datapointchris/goapiauth"

	"github.com/datapointchris/ypl/api/enrich"
	"github.com/datapointchris/ypl/api/handlers"
	"github.com/datapointchris/ypl/api/jarseal"
	"github.com/datapointchris/ypl/api/reconcile"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// shutdownGrace bounds how long in-flight requests get to finish after the
// first SIGINT or SIGTERM. No playlist write or watch-history read begins once
// the drain starts and no push write once the sync is canceled, which happen
// together. One begun before then ends within handlers.WriteDuration,
// handlers.HistoryDuration or reconcile.WriteDuration. So the grace outlasts
// every one with 5 seconds to answer. A history read cut short loses the jar
// YouTube rotated during it, which is why it counts. A container's stop timeout
// has to be longer still.
const shutdownGrace = max(handlers.WriteDuration, handlers.HistoryDuration, reconcile.WriteDuration) + 5*time.Second

// defaultSyncInterval is the mean wait between ticks when SYNC_INTERVAL is
// unset. A tick reads the listing and one average playlist, which reconcile's
// quota guard keeps back for every tick left in the day.
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

// holdLimit bounds the wait for another server holding the store to exit. A
// healthy one has drained within shutdownGrace of its SIGTERM, and a container
// runtime kills it soon after, so a longer wait is on a process nothing will
// stop.
const holdLimit = shutdownGrace + time.Minute

// start reads the configuration before the port is bound, so a configuration
// that refuses to start never answers a probe. It then binds the port and
// holds requests while it takes the store's lock, waiting up to holdLimit for
// another server to let it go, and opens the database, applying its
// migrations. It then serves while the sync runs and the identity provider's
// discovery document and keys are read. /ready answers 200 once the store is
// open and they are read, which is when a token can be verified.
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
	issuer, clientIDPrefix, serviceClientIDPrefix, err := identityProvider()
	if err != nil {
		return err
	}
	history, err := watchHistory()
	if err != nil {
		return err
	}
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
	provider, err := goapiauth.NewConnecting(goapiauth.Config{Issuer: issuer, PersonPrefix: clientIDPrefix, ServicePrefix: serviceClientIDPrefix})
	if err != nil {
		return err
	}
	var metadata http.Handler
	if public := os.Getenv("PUBLIC_URL"); public != "" {
		metadata = goapiauth.ProtectedResourceMetadata{Resource: public, Issuer: issuer}
	} else {
		slog.Warn("PUBLIC_URL is unset, so no RFC 9728 metadata is served and `ypl auth login <url>` cannot discover the issuer")
	}

	// A failure work meets once the port is bound ends the serving through
	// ctx, and start returns it.
	ctx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	hold := newStoreHold()
	// Both are set by work and read once run has returned, which is after work
	// has, so the store outlives every request the drain let finish.
	var st *store.Store
	var release func()
	work := func(ctx context.Context) {
		var wg sync.WaitGroup
		defer wg.Wait()
		wg.Go(func() { provider.Run(ctx, slog.Default()) })
		// A server stopped while it waits or opens is stopping, not failing.
		var err error
		if release, err = takeServerLock(ctx, path, hold); err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		if st, err = store.Open(ctx, path); err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		slog.Info("database ready", "path", path)
		tally := enrich.NewTally()
		edits := reconcile.NewEdits()
		worker := reconcile.NewWorker(reconcile.NewRunner(st, syncChannel, tally, edits, interval), interval, slog.Default())
		api := handlers.New(st, apiChannel, handlers.Sync{Edits: edits, Interval: interval}, slog.Default())
		if history != nil {
			api.ReadHistoryWith(history.reader, history.sealer)
		}
		if !hold.open(handler(api, provider, metadata), api.Drain) {
			return
		}
		wg.Go(func() { worker.Run(ctx) })
		if reads {
			wg.Go(func() { _ = enrich.New(st, reader, limits, tally).Run(ctx) })
		}
	}
	err = run(ctx, ":"+envOr("PORT", "8080"), hold, work, hold.drain)
	if st != nil {
		_ = st.Close()
	}
	if release != nil {
		release()
	}
	if cause := context.Cause(ctx); err == nil && cause != nil {
		return cause
	}
	return err
}

// takeServerLock takes the lock of the store at path, telling hold who it
// waits on, and gives up after holdLimit naming the server that held it.
func takeServerLock(ctx context.Context, path string, hold *storeHold) (func(), error) {
	host, _ := os.Hostname()
	holder := fmt.Sprintf("pid %d on %s since %s", os.Getpid(), host, store.Timestamp(time.Now()))
	waitCtx, cancel := context.WithTimeout(ctx, holdLimit)
	defer cancel()
	release, err := store.TakeServerLock(waitCtx, path, holder, hold.waitingOn)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("the server holding the store did not exit within %v: %w", holdLimit, err)
	}
	return release, err
}

// defaultClientIDPrefix starts the id of every person's CLI client whose tokens
// the API accepts when CLI_CLIENT_ID_PREFIX is unset.
const defaultClientIDPrefix = "ypl-cli-"

// defaultServiceClientIDPrefix starts the id of every service client whose
// tokens the API accepts when SERVICE_CLIENT_ID_PREFIX is unset.
const defaultServiceClientIDPrefix = "ypl-svc-"

// identityProvider reads OIDC_ISSUER, which has no default, and the two client
// id prefixes, each falling back to its default constant.
func identityProvider() (issuer, clientIDPrefix, serviceClientIDPrefix string, err error) {
	issuer = os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		return "", "", "", errors.New("OIDC_ISSUER is unset: set it to the identity provider whose keys sign the CLI's access tokens")
	}
	return issuer, envOr("CLI_CLIENT_ID_PREFIX", defaultClientIDPrefix), envOr("SERVICE_CLIENT_ID_PREFIX", defaultServiceClientIDPrefix), nil
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
// ENRICH_PACE is unset. Each wait adds up to the pace again at random, so reads
// average one every pace and a half. yt-dlp reads from the server's own
// address, which YouTube throttles after reads that come too fast.
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

// tokenGate verifies tokens and says why it cannot yet, as
// *goapiauth.Connecting does.
type tokenGate interface {
	goapiauth.TokenVerifier
	Ready() error
}

// handler is every route: the probes, which answer without a token so a
// container healthcheck can call them, and the API, which answers only a
// request carrying a token gate accepts. A service client's token reaches only
// the routes handlers.ServiceRoutes lists for its scopes.
//
// metadata serves the RFC 9728 document without a token, and is nil while
// PUBLIC_URL is unset, since only the deployment knows the address it is
// reached at. The route is then absent.
func handler(api *handlers.Handlers, gate tokenGate, metadata http.Handler) http.Handler {
	mux := routes(gate.Ready)
	if metadata != nil {
		mux.Handle("GET "+goapiauth.ProtectedResourcePath, metadata)
	}
	api.Register(mux)
	return goapiauth.RequireBearer(gate, slog.Default(), "/health", "/ready")(goapiauth.LimitServices(mux, handlers.ServiceRoutes, slog.Default()))
}

// routes serves /health, which answers once the listener is bound, and /ready,
// which answers 200 once ready reports nil and 503 with what it reported before
// that.
func routes(ready func() error) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		probe(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if err := ready(); err != nil {
			wire.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting", "detail": err.Error()})
			return
		}
		probe(w, http.StatusOK, "ok")
	})
	return mux
}

func probe(w http.ResponseWriter, code int, status string) {
	wire.JSON(w, code, map[string]string{"status": status})
}

// historyReading is what the API reads the watch history through, and the
// sealer it stores the cookie jar under.
type historyReading struct {
	reader *ytdlp.Reader
	sealer *jarseal.Sealer
}

// watchHistory is how to read the watch history when yt-dlp is found and
// YOUTUBE_COOKIE_KEY is set, and nil otherwise, logging which is missing. A key
// that is set and unusable stops the server.
func watchHistory() (*historyReading, error) {
	key := os.Getenv("YOUTUBE_COOKIE_KEY")
	if key == "" {
		slog.Warn("no YOUTUBE_COOKIE_KEY, so the watch history is not read")
		return nil, nil
	}
	sealer, err := jarseal.New(key)
	if err != nil {
		return nil, fmt.Errorf("YOUTUBE_COOKIE_KEY: %w", err)
	}
	reader, err := ytdlp.NewReader(envOr("YTDLP_PATH", "yt-dlp"))
	if err != nil {
		slog.Warn("no yt-dlp, so the watch history is not read", "err", err)
		return nil, nil
	}
	return &historyReading{reader: reader, sealer: sealer}, nil
}

// envOr reads key, treating an empty value as unset. An exported empty PORT
// would otherwise bind ":", which is a random port.
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
