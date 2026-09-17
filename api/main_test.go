package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
			routes().ServeHTTP(rec, httptest.NewRequest(tc.method, path, http.NoBody))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, path, rec.Code, tc.want)
			}
		}
	}
}

func TestProbeBodyIsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

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

	err = run(context.Background(), taken.Addr().String(), func(context.Context) {})
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
	go func() { done <- serve(ctx, ln, func(context.Context) {}) }()

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
		done <- serve(ctx, ln, func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			finished = true
		})
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

func TestSecondSignalEndsTheDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the service as a child process")
	}

	child := exec.Command(os.Args[0], "-test.run=^$")
	// The credentials are placeholders, and every request the sync makes goes to
	// a proxy port nothing listens on, so no request leaves the machine.
	child.Env = append(os.Environ(), serveChild+"=1", "PORT=0", "DATABASE_PATH="+filepath.Join(t.TempDir(), "api.db"),
		"YOUTUBE_CLIENT_ID=id", "YOUTUBE_CLIENT_SECRET=secret", "YOUTUBE_REFRESH_TOKEN=token",
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() { _ = child.Process.Kill() }()

	logs := bufio.NewScanner(stdout)
	addr := awaitLog(t, logs, "listening")["addr"]
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("listening addr %q: %v", addr, err)
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
