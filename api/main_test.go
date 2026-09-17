package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/datapointchris/ypl/api/auth"
	"github.com/datapointchris/ypl/api/handlers"
	"github.com/datapointchris/ypl/api/reconcile"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
)

// serveChild makes this test binary run the real main when a test starts it as
// a child process.
const serveChild = "YPL_API_TEST_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(serveChild) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func alwaysReady() bool { return true }

func TestReadyAnswersOnlyOnceItsCheckReportsReady(t *testing.T) {
	ready := false
	mux := routes(func() bool { return ready })
	for _, c := range []struct {
		ready  bool
		code   int
		status string
	}{
		{false, http.StatusServiceUnavailable, "starting"},
		{true, http.StatusOK, "ok"},
	} {
		ready = c.ready
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != c.code || body["status"] != c.status {
			t.Errorf("/ready while ready is %v = %d %s, want %d with status %s", c.ready, rec.Code, rec.Body, c.code, c.status)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d, want 200 whatever ready reports", rec.Code)
	}
}

func TestProbesAnswerGetAndHeadAndRefuseWrites(t *testing.T) {
	cases := []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodHead, http.StatusOK},
		{http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodPut, http.StatusMethodNotAllowed},
		{http.MethodDelete, http.StatusMethodNotAllowed},
	}
	for _, path := range []string{"/health", "/ready"} {
		for _, tc := range cases {
			rec := httptest.NewRecorder()
			routes(alwaysReady).ServeHTTP(rec, httptest.NewRequest(tc.method, path, http.NoBody))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, path, rec.Code, tc.want)
			}
		}
	}
}

func TestProbeBodyIsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	routes(alwaysReady).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestEmptyEnvironmentValueFallsBack(t *testing.T) {
	t.Setenv("PORT", "")
	if got := envOr("PORT", "8080"); got != "8080" {
		t.Fatalf("envOr with PORT empty = %q, want 8080", got)
	}
	t.Setenv("PORT", "9000")
	if got := envOr("PORT", "8080"); got != "9000" {
		t.Fatalf("envOr with PORT=9000 = %q, want 9000", got)
	}
}

func TestRunReturnsTheBindError(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer func() { _ = taken.Close() }()

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(previous)

	err = run(context.Background(), taken.Addr().String(), routes(alwaysReady), func(context.Context) {}, func() {})
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("run on an occupied port = %v, want EADDRINUSE", err)
	}
	if strings.Contains(logged.String(), `"msg":"listening"`) {
		t.Fatalf("a port that was never bound was logged as listening:\n%s", logged.String())
	}
}

func TestServeAnswersUntilCanceledThenReturnsNil(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, routes(alwaysReady), func(context.Context) {}, func() {}) }()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + ln.Addr().String() + "/ready")
	if err != nil {
		t.Fatalf("GET /ready: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ready = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve after cancel = %v, want nil", err)
		}
	case <-time.After(shutdownGrace):
		t.Fatal("serve did not return after its context was canceled")
	}
}

// The work runs until serve stops, and serve returns only after it has.
func TestServeStopsItsWorkBeforeReturning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := false

	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, routes(alwaysReady), func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			finished = true
		}, func() {})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil || !finished {
			t.Fatalf("serve = %v with the work finished %v, want nil after the work returned", err, finished)
		}
	case <-time.After(shutdownGrace):
		t.Fatal("serve did not return after its context was canceled")
	}
}

// The handler finishes its request only once drain has been called, as a
// playlist write begun before the drain does, so the request is answered only
// if serve drains the API before it waits on requests in flight.
func TestServeDrainsTheAPIBeforeWaitingOnRequestsInFlight(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlight, drained := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-drained
		w.WriteHeader(http.StatusNoContent)
	})

	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, h, func(context.Context) {}, func() { close(drained) }) }()
	answered := make(chan int, 1)
	go func() {
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Post("http://"+ln.Addr().String()+"/api/v1/playlists", "application/json", strings.NewReader(`{}`))
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-inFlight
	cancel()

	select {
	case status := <-answered:
		if status != http.StatusNoContent {
			t.Fatalf("the request in flight answered %d, want 204", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight was not answered: serve did not drain the API")
	}
	if err := <-done; err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}
}

func TestTheShutdownGraceOutlastsAPlaylistWrite(t *testing.T) {
	if shutdownGrace <= handlers.WriteDuration || shutdownGrace <= reconcile.WriteDuration {
		t.Fatalf("shutdownGrace %v, want longer than a playlist write's %v and a push write's %v", shutdownGrace, handlers.WriteDuration, reconcile.WriteDuration)
	}
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("read the README: %v", err)
	}
	if want := fmt.Sprintf("up to %d seconds to finish", int(shutdownGrace.Seconds())); !strings.Contains(string(readme), want) {
		t.Fatalf("the README does not say %q, which a container's stop timeout is set from", want)
	}
}

func TestSyncIntervalDefaultsToAnHourAndRefusesAnythingButAPositiveDuration(t *testing.T) {
	t.Setenv("SYNC_INTERVAL", "")
	if got, err := syncInterval(); err != nil || got != time.Hour {
		t.Fatalf("syncInterval unset = %v, %v, want 1h", got, err)
	}
	t.Setenv("SYNC_INTERVAL", "30m")
	if got, err := syncInterval(); err != nil || got != 30*time.Minute {
		t.Fatalf("syncInterval 30m = %v, %v, want 30m", got, err)
	}
	for _, raw := range []string{"0s", "-1h", "hourly"} {
		t.Setenv("SYNC_INTERVAL", raw)
		if _, err := syncInterval(); err == nil {
			t.Errorf("syncInterval %q succeeded, want a refusal", raw)
		}
	}
}

func TestTheREADMEStatesTheEnrichmentDefaults(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(readme)), " ")
	want := fmt.Sprintf("at most `ENRICH_VIDEOS_PER_RUN` videos, %d when unset, `ENRICH_PACE` apart, %d seconds when unset", defaultEnrichVideos, int(defaultEnrichPace.Seconds()))
	if !strings.Contains(text, want) {
		t.Fatalf("the README does not say %q", want)
	}
}

func TestEnrichmentDefaultsAndRefusesAnythingButAPositivePaceAndACount(t *testing.T) {
	t.Setenv("ENRICH_PACE", "")
	t.Setenv("ENRICH_VIDEOS_PER_RUN", "")
	if pace, videos, err := enrichment(); err != nil || pace != defaultEnrichPace || videos != defaultEnrichVideos {
		t.Fatalf("enrichment unset = %v, %d, %v, want %v and %d", pace, videos, err, defaultEnrichPace, defaultEnrichVideos)
	}
	t.Setenv("ENRICH_PACE", "30s")
	t.Setenv("ENRICH_VIDEOS_PER_RUN", "0")
	if pace, videos, err := enrichment(); err != nil || pace != 30*time.Second || videos != 0 {
		t.Fatalf("enrichment of 30s and 0 = %v, %d, %v, want 30s and no videos", pace, videos, err)
	}
	for name, env := range map[string][2]string{
		"no pace":       {"0s", "30"},
		"a pace behind": {"-10s", "30"},
		"a pace word":   {"slow", "30"},
		"a count below": {"10s", "-1"},
		"a count word":  {"10s", "thirty"},
	} {
		t.Setenv("ENRICH_PACE", env[0])
		t.Setenv("ENRICH_VIDEOS_PER_RUN", env[1])
		if _, _, err := enrichment(); err == nil {
			t.Errorf("enrichment with %s succeeded, want a refusal", name)
		}
	}
}

func TestIdentityProviderRequiresAnIssuerAndDefaultsThePrefix(t *testing.T) {
	t.Setenv("OIDC_ISSUER", "")
	if _, _, err := identityProvider(); err == nil {
		t.Fatal("identityProvider with OIDC_ISSUER unset succeeded, want a refusal")
	}

	t.Setenv("OIDC_ISSUER", "https://id.example")
	t.Setenv("CLI_CLIENT_ID_PREFIX", "")
	if issuer, prefix, err := identityProvider(); err != nil || issuer != "https://id.example" || prefix != "ypl-cli-" {
		t.Fatalf("identityProvider = %q, %q, %v, want the issuer and ypl-cli-", issuer, prefix, err)
	}
	t.Setenv("CLI_CLIENT_ID_PREFIX", "ypl-test-")
	if _, prefix, err := identityProvider(); err != nil || prefix != "ypl-test-" {
		t.Fatalf("identityProvider prefix = %q, %v, want ypl-test-", prefix, err)
	}
}

// acceptOnly is ready, and accepts the one token it holds and rejects every
// other.
type acceptOnly string

func (a acceptOnly) Verify(_ context.Context, raw string) (auth.Identity, error) {
	if raw != string(a) {
		return auth.Identity{}, auth.ErrUnauthorized
	}
	return auth.Identity{Subject: "user", ClientID: "ypl-cli-test"}, nil
}

func (acceptOnly) Ready() bool { return true }

func TestTheAPIAnswersOnlyAVerifiedTokenAndTheProbesAnswerAnyone(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer func() { _ = st.Close() }()
	h := handler(handlers.New(st, nil, slog.Default()), acceptOnly("good"))

	cases := []struct {
		path, token string
		want        int
	}{
		{"/api/v1/playlists", "", http.StatusUnauthorized},
		{"/api/v1/playlists", "bad", http.StatusUnauthorized},
		{"/api/v1/playlists", "good", http.StatusOK},
		{"/api/v1/status", "good", http.StatusOK},
		{"/health", "", http.StatusOK},
		{"/ready", "", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, http.NoBody)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("GET %s with token %q = %d, want %d", c.path, c.token, rec.Code, c.want)
		}
	}
}

// identityProviderStub serves a discovery document naming itself as the issuer
// and a JWKS holding one signing key, which is all the service reads from the
// provider before it is ready.
func identityProviderStub(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		self := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": self, "jwks_uri": self + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), KeyID: "main", Algorithm: string(jose.RS256), Use: "sig"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// child is the service started as a child process against the identity
// provider issuer.
type child struct {
	cmd      *exec.Cmd
	logs     *bufio.Scanner
	port     string
	database string
	client   *http.Client
}

func startChild(t *testing.T, issuer string) *child {
	t.Helper()
	database := filepath.Join(t.TempDir(), "api.db")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// The credentials are placeholders, and every request the sync makes goes to
	// a proxy port nothing listens on, so no request leaves the machine. The
	// identity provider is on the loopback address, which Go never proxies. The
	// service refuses to start without a yt-dlp to read videos with, and this
	// test binary stands in for one: the store holds no video, so enrichment
	// reads none and never runs it.
	cmd.Env = append(os.Environ(), serveChild+"=1", "PORT=0", "DATABASE_PATH="+database,
		"YOUTUBE_CLIENT_ID=id", "YOUTUBE_CLIENT_SECRET=secret", "YOUTUBE_REFRESH_TOKEN=token",
		"OIDC_ISSUER="+issuer, "YTDLP_PATH="+os.Args[0],
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	logs := bufio.NewScanner(stdout)
	addr := awaitLog(t, logs, "listening")["addr"]
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("listening addr %q: %v", addr, err)
	}
	return &child{cmd: cmd, logs: logs, port: port, database: database, client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}}
}

// get answers GET path with the bearer token, when one is given, and the body.
func (c *child) get(t *testing.T, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+c.port+path, http.NoBody)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	return resp.StatusCode, body.Bytes()
}

// awaitReady asks /ready until it answers 200.
func (c *child) awaitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := c.get(t, "/ready", ""); code == http.StatusOK {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("/ready did not answer 200 with the identity provider up")
}

// The sync needs no token, so a provider that is down when the service starts
// leaves it serving, not ready, and refusing tokens as the provider's outage.
func TestAProviderDownAtStartLeavesTheServiceServing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the service as a child process")
	}
	c := startChild(t, "http://127.0.0.1:1")

	if code, _ := c.get(t, "/health", ""); code != http.StatusOK {
		t.Errorf("/health = %d, want 200", code)
	}
	if code, _ := c.get(t, "/ready", ""); code != http.StatusServiceUnavailable {
		t.Errorf("/ready = %d, want 503 while the provider is unread", code)
	}
	code, body := c.get(t, "/api/v1/status", "a.b.c")
	var refusal wire.Refusal
	if err := json.Unmarshal(body, &refusal); err != nil || code != http.StatusServiceUnavailable || refusal.Code != wire.CodeIdentityProviderUnavailable {
		t.Errorf("/api/v1/status with a token = %d %s, want 503 %s", code, body, wire.CodeIdentityProviderUnavailable)
	}

	// YouTube is unreachable too, so the sync's first run records a failure,
	// which is what shows it ran.
	st, err := store.Open(context.Background(), c.database)
	if err != nil {
		t.Fatalf("open the child's store: %v", err)
	}
	defer func() { _ = st.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := st.Queries.ListNewestSyncRuns(context.Background(), 1)
		if err == nil && len(runs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sync run was recorded with the provider down: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSecondSignalEndsTheDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the service as a child process")
	}
	c := startChild(t, identityProviderStub(t))
	c.awaitReady(t)
	child, logs, port := c.cmd, c.logs, c.port

	if code, _ := c.get(t, "/api/v1/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/status without a token = %d, want 401", code)
	}

	// A request whose headers never finish keeps the connection active, so
	// Shutdown waits on it for the whole grace period.
	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("dial child: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /health HTTP/1.1\r\nHost: test\r\n")); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("first SIGINT: %v", err)
	}
	awaitLog(t, logs, "shutting down")

	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("second SIGINT: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	// Well inside shutdownGrace, which is how long a swallowed second signal
	// leaves the process draining.
	select {
	case <-exited:
		if child.ProcessState.Success() {
			t.Fatal("the second SIGINT let the drain finish instead of ending the process")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second SIGINT did not end the drain")
	}
}

// awaitLog reads JSON log lines until one carries msg, and returns its fields.
func awaitLog(t *testing.T, logs *bufio.Scanner, msg string) map[string]string {
	t.Helper()
	for logs.Scan() {
		var fields map[string]any
		if json.Unmarshal(logs.Bytes(), &fields) != nil {
			continue
		}
		if fields["msg"] != msg {
			continue
		}
		out := make(map[string]string, len(fields))
		for key, value := range fields {
			if s, ok := value.(string); ok {
				out[key] = s
			}
		}
		return out
	}
	t.Fatalf("child exited before logging %q", msg)
	return nil
}
