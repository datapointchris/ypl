package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/goclilogin"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
)

// The stand-in is this binary, run under the name standInName. `ypl play`
// hands it to mpv as the program that turns a watch URL into a stream. It
// takes a turn at the server's door, then runs the real yt-dlp, which reads
// YouTube from this machine. The server and this machine often reach YouTube
// from one address, and the door is what keeps their reads apart.
const standInName = "mpv-yt-dlp"

// The stand-in's environment, which `ypl play` sets on mpv.
const (
	// realYtdlpEnv names the yt-dlp the stand-in runs once its turn comes.
	realYtdlpEnv = "YPL_MPV_YTDLP"
	// apiOverrideEnv carries --api from `ypl play`, so the playback and the
	// stand-in reach one server.
	apiOverrideEnv = "YPL_MPV_API"
)

// ytdlpThroughYpl is what a script holds when it hands yt-dlp's arguments to
// `ypl ytdlp`, within its first scriptHead bytes. mpv cannot stream through
// such a script, because the link it answers is resolved on the server and
// plays only at the server's address.
const (
	ytdlpThroughYpl = "ypl ytdlp"
	scriptHead      = 4 << 10
)

// standInPath is where `ypl play` links the stand-in to this binary.
func standInPath() string { return filepath.Join(goclilogin.StateDir("ypl"), standInName) }

// handedLeasePath is where `ypl play` leaves the lease it took before mpv
// started. The first stream's resolve uses it rather than take another.
func handedLeasePath() string { return filepath.Join(goclilogin.StateDir("ypl"), "mpv-lease") }

// leaseTheDoor takes a turn at the server's door for a read that this machine
// makes. While the door is busy it asks again, and so it does while the door is
// paused where waitOutPause holds. It says once why it waits. A server that
// runs no yt-dlp has no door, and neither has one older than the lease route,
// which answers 404. The read then goes ahead with no lease, and the Lease is
// zero.
func leaseTheDoor(ctx context.Context, client *api.Client, waitOutPause bool, waiting func(string)) (api.Lease, error) {
	said := false
	for {
		lease, err := client.TakeLease(ctx)
		var refusal *api.Refusal
		if !errors.As(err, &refusal) {
			return lease, err
		}
		switch {
		case refusal.Code == api.CodeReadsUnavailable, refusal.Status == http.StatusNotFound:
			return api.Lease{}, nil
		case refusal.Code == api.CodeReadsBusy, refusal.Code == api.CodeReadsPaused && waitOutPause:
		default:
			return api.Lease{}, err
		}
		if !said {
			waiting(fmt.Sprintf("Waiting for a turn at the server's door: %s.", refusal.Message))
			said = true
		}
		select {
		case <-ctx.Done():
			return api.Lease{}, ctx.Err()
		case <-time.After(max(refusal.RetryAfter, time.Second)):
		}
	}
}

// streamResolver is the stand-in for mpv to resolve each stream with, behind
// the lease taken for the first one. It links the stand-in to this binary,
// and leaves the lease for the first resolve to find.
func streamResolver(lease api.Lease, ytdlp string) (mpv.Resolver, error) {
	self, err := os.Executable()
	if err != nil {
		return mpv.Resolver{}, fmt.Errorf("find this binary to hand mpv: %w", err)
	}
	path := standInPath()
	// mpv reads its ytdl_path as a list of paths, split where the separator is.
	if strings.ContainsRune(path, os.PathListSeparator) {
		return mpv.Resolver{}, fmt.Errorf("%s holds a %q, which mpv reads as two paths; set XDG_STATE_HOME to a path without one", path, os.PathListSeparator)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return mpv.Resolver{}, fmt.Errorf("make somewhere for the stand-in: %w", err)
	}
	// Linked beside the path and renamed over it, so a playback starting at
	// the same moment never finds the path missing.
	beside := path + "." + strconv.Itoa(os.Getpid())
	if err := os.Symlink(self, beside); err != nil {
		return mpv.Resolver{}, fmt.Errorf("link the stand-in: %w", err)
	}
	if err := os.Rename(beside, path); err != nil {
		_ = os.Remove(beside)
		return mpv.Resolver{}, fmt.Errorf("link the stand-in: %w", err)
	}
	if !lease.HeldUntil.IsZero() {
		if err := os.WriteFile(handedLeasePath(), []byte(lease.HeldUntil.Format(time.RFC3339Nano)), 0o600); err != nil {
			return mpv.Resolver{}, fmt.Errorf("leave the lease for the first stream: %w", err)
		}
	}
	env := []string{realYtdlpEnv + "=" + ytdlp}
	if apiOverride != "" {
		env = append(env, apiOverrideEnv+"="+apiOverride)
	}
	return mpv.Resolver{Path: path, Env: env}, nil
}

// handedLease reports whether this resolve takes the lease `ypl play` took
// before mpv started. The file is removed by the one resolve that takes it,
// and the lease is used only while its hold lasts.
func handedLease(now time.Time) bool {
	path := handedLeasePath()
	held, err := os.ReadFile(path)
	if err != nil || os.Remove(path) != nil {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(held)))
	return err == nil && now.Before(until)
}

// standIn is the stand-in's whole run. While mpv resolves a stream, it takes a
// turn at the door, then runs the real yt-dlp with mpv's arguments and
// exits as that exits. A paused door fails the resolve rather than waiting it
// out. mpv shows the stand-in's stderr when a resolve fails, and nothing while
// one waits.
func (a *app) standIn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ytdlp := os.Getenv(realYtdlpEnv)
	if ytdlp == "" {
		_, _ = fmt.Fprintf(stderr, "error: %s is the yt-dlp `ypl play` hands mpv, and runs only under it\n", os.Args[0])
		return 1
	}
	if !handedLease(time.Now()) {
		apiOverride = os.Getenv(apiOverrideEnv)
		client, err := a.client(ctx)
		if err == nil {
			_, err = leaseTheDoor(ctx, client, false, func(string) {})
		}
		if err != nil {
			report(stderr, reported(err))
			return 1
		}
	}
	resolve := exec.CommandContext(ctx, ytdlp, args...)
	resolve.Stdin, resolve.Stdout, resolve.Stderr = os.Stdin, stdout, stderr
	err := resolve.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return exit.ExitCode()
	case err != nil:
		report(stderr, fmt.Errorf("run %s: %w", ytdlp, err))
		return 1
	}
	return 0
}

// realYtdlp is the first yt-dlp on PATH that is yt-dlp itself, rather than
// this binary or a script that runs `ypl ytdlp`.
func realYtdlp() (string, error) {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return "", fmt.Errorf("find this binary: %w", err)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		found, err := exec.LookPath(filepath.Join(dir, "yt-dlp"))
		if err != nil || runsThroughYpl(found, self) {
			continue
		}
		return found, nil
	}
	return "", errors.New("yt-dlp is not installed — install mpv and yt-dlp to play a playlist")
}

// runsThroughYpl reports whether path is this binary, or a script that hands
// its arguments to `ypl ytdlp`.
func runsThroughYpl(path, self string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved == self {
		return true
	}
	script, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = script.Close() }()
	head := make([]byte, scriptHead)
	n, _ := io.ReadFull(script, head)
	return bytes.Contains(head[:n], []byte(ytdlpThroughYpl))
}
