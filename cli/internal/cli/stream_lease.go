package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// stoppedPath is where a stand-in leaves why it stopped the playback, for
// `ypl play` to say once mpv has gone.
func stoppedPath() string { return filepath.Join(goclilogin.StateDir("ypl"), "mpv-stopped") }

// stopWithin bounds the stand-in's wait for mpv to quit.
const stopWithin = 5 * time.Second

// stopPlayback ends the playback of the mpv that started this stand-in, since
// the door gives no stream a turn. It leaves why for `ypl play`, tells mpv to
// quit, and waits for it to go. A resolve that only failed would send mpv to
// open the watch URL itself and then on to the next, each a request to
// YouTube outside the door.
func stopPlayback(why error) {
	player := os.Getppid()
	_ = os.WriteFile(stoppedPath(), []byte(why.Error()), 0o600)
	if syscall.Kill(player, syscall.SIGTERM) != nil {
		return
	}
	for deadline := time.Now().Add(stopWithin); os.Getppid() == player && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
}

// doorAttempts is how many times a request for a turn is sent while the
// server's door answers busy. The server already waits up to 2 minutes for a
// turn before it answers, so three attempts wait several minutes in all.
const doorAttempts = 3

// busyWaitCap bounds one wait between attempts while the door is busy. A wait
// for a pause to end is the pause's own length.
const busyWaitCap = 30 * time.Second

// atTheDoor sends ask until the server's door answers it. While the door is
// busy it sends it again after the server's Retry-After, up to doorAttempts in
// all, and then fails with a noAnswer. While the door is paused it waits for
// the pause to end where waitOutPause holds, and fails where it does not. It
// says once why it waits.
func atTheDoor[T any](ctx context.Context, ask func(context.Context) (T, error), waitOutPause bool, waiting func(string)) (T, error) {
	var none T
	said, busy := false, 0
	for {
		got, err := ask(ctx)
		var refusal *api.Refusal
		if !errors.As(err, &refusal) {
			return got, err
		}
		wait := max(refusal.RetryAfter, time.Second)
		switch {
		case refusal.Code == api.CodeReadsBusy:
			busy++
			if busy == doorAttempts {
				return none, &noAnswer{fmt.Errorf("the server's door stayed busy through %d attempts: %w", doorAttempts, refusal)}
			}
			wait = min(wait, busyWaitCap)
		case refusal.Code == api.CodeReadsPaused && waitOutPause:
		default:
			return got, err
		}
		if !said {
			waiting(fmt.Sprintf("Waiting for a turn at the server's door: %s.", refusal.Message))
			said = true
		}
		select {
		case <-ctx.Done():
			return none, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// leaseTheDoor takes a turn at the server's door for a read that this machine
// makes, as atTheDoor asks for one. A paused door that it does not wait out,
// and a door busy through every attempt, are errors. Where the server gives
// neither a turn nor one of those refusals, the read goes ahead with no lease
// and the Lease is zero, as every read did before the door. That covers a
// server that runs no yt-dlp or predates the lease route, which is said to no
// one, and a server that is restarting or cannot be reached, which is said to
// noDoor.
func leaseTheDoor(ctx context.Context, client *api.Client, waitOutPause bool, waiting func(string), noDoor func(error)) (api.Lease, error) {
	lease, err := atTheDoor(ctx, client.TakeLease, waitOutPause, waiting)
	var refusal *api.Refusal
	var busy *noAnswer
	switch {
	case err == nil:
		return lease, nil
	case ctx.Err() != nil:
		return api.Lease{}, ctx.Err()
	case errors.As(err, &busy), errors.As(err, &refusal) && refusal.Code == api.CodeReadsPaused:
		return api.Lease{}, err
	case errors.As(err, &refusal) && (refusal.Code == api.CodeReadsUnavailable || refusal.Status == http.StatusNotFound):
		return api.Lease{}, nil
	}
	noDoor(err)
	return api.Lease{}, nil
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
	if lease.ID != "" {
		held, err := json.Marshal(lease)
		if err == nil {
			err = os.WriteFile(handedLeasePath(), held, 0o600)
		}
		if err != nil {
			return mpv.Resolver{}, fmt.Errorf("leave the lease for the first stream: %w", err)
		}
	}
	env := []string{realYtdlpEnv + "=" + ytdlp}
	if apiOverride != "" {
		env = append(env, apiOverrideEnv+"="+apiOverride)
	}
	return mpv.Resolver{Path: path, Env: env}, nil
}

// handedLease is the lease `ypl play` took before mpv started, where this
// resolve is the one to take it. The file is removed by the one resolve that
// takes it, and the lease is used only while its hold lasts.
func handedLease(now time.Time) (api.Lease, bool) {
	path := handedLeasePath()
	held, err := os.ReadFile(path)
	if err != nil || os.Remove(path) != nil {
		return api.Lease{}, false
	}
	var lease api.Lease
	if json.Unmarshal(held, &lease) != nil || !now.Before(lease.HeldUntil) {
		return api.Lease{}, false
	}
	return lease, true
}

// The tail of a resolve's stderr that the door is told, and the longest the
// telling may take. yt-dlp writes the error that ended a read last.
const (
	outcomeTail   = 32 << 10
	outcomeWithin = 5 * time.Second
)

// standIn is the stand-in's whole run. While mpv resolves a stream, it takes a
// turn at the door, then runs the real yt-dlp with mpv's arguments and exits
// as that exits. A door that is paused, or busy through every attempt, stops
// the playback rather than wait, since mpv shows nothing while a resolve
// waits. A resolve that fails is told to the door, which pauses every read
// where YouTube refused the address.
func (a *app) standIn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ytdlp := os.Getenv(realYtdlpEnv)
	if ytdlp == "" {
		_, _ = fmt.Fprintf(stderr, "error: %s is the yt-dlp `ypl play` hands mpv, and runs only under it\n", os.Args[0])
		return 1
	}
	apiOverride = os.Getenv(apiOverrideEnv)
	withoutTurn := func(err error) {
		_, _ = fmt.Fprintf(stderr, "Resolving without a turn at the server's door: %v\n", reported(err))
	}
	lease, handed := handedLease(time.Now())
	if !handed {
		client, err := a.client(ctx)
		if err != nil {
			withoutTurn(err)
		} else if lease, err = leaseTheDoor(ctx, client, false, func(string) {}, withoutTurn); err != nil {
			report(stderr, reported(err))
			stopPlayback(reported(err))
			return 1
		}
	}
	tail := &lastBytes{most: outcomeTail}
	resolve := exec.CommandContext(ctx, ytdlp, args...)
	resolve.Stdin, resolve.Stdout, resolve.Stderr = os.Stdin, stdout, io.MultiWriter(stderr, tail)
	resolve.WaitDelay = time.Second
	err := resolve.Run()
	code := -1
	if resolve.ProcessState != nil {
		code = resolve.ProcessState.ExitCode()
	}
	if code < 0 {
		report(stderr, fmt.Errorf("run %s: %w", ytdlp, err))
		return 1
	}
	if code != 0 && lease.ID != "" {
		a.tellTheDoor(lease.ID, code, string(tail.held), stderr)
	}
	return code
}

// tellTheDoor tells the server's door how the read that lease id held for
// ended. A failure to tell it is said on stderr, which mpv shows beside the
// failed resolve.
func (a *app) tellTheDoor(id string, code int, said string, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), outcomeWithin)
	defer cancel()
	client, err := a.client(ctx)
	if err == nil {
		err = client.EndLease(ctx, id, code, said)
	}
	if err != nil {
		report(stderr, fmt.Errorf("tell the server's door how the read ended: %w", reported(err)))
	}
}

// lastBytes keeps the last most bytes written to it.
type lastBytes struct {
	most int
	held []byte
}

func (l *lastBytes) Write(p []byte) (int, error) {
	l.held = append(l.held, p...)
	if over := len(l.held) - l.most; over > 0 {
		l.held = l.held[over:]
	}
	return len(p), nil
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
