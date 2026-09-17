// Package mpv plays videos through the mpv binary and asks a running one what
// it is doing.
//
// Shelled out to rather than embedded, for the same reason the server shells
// out to yt-dlp: mpv is the thing that already knows how to stream YouTube, and
// it tracks YouTube's changes on its own release schedule.
//
// Every playback opens mpv's JSON IPC socket, which is what makes `ypl now`
// able to say which track of a two-hour mix is playing. It costs one argument
// and nothing when unused.
package mpv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/datapointchris/goclilogin"
)

// binary is what is run, resolved on PATH.
const binary = "mpv"

// dialTimeout bounds reaching a socket and waiting for what is on it. mpv
// answers a property read immediately or not at all.
const dialTimeout = 2 * time.Second

// pathLimit is how long a unix socket address may be. The field is fixed size
// in the kernel: 104 bytes on macOS and the BSDs, 108 on Linux. Over it, mpv
// logs `Could not create IPC socket` and carries on playing, so playback works
// and `ypl now` silently reports that nothing is on. The smaller of the two is
// checked, since the same XDG_STATE_HOME can be shared between machines.
const pathLimit = 104

// ErrNotPlaying is nothing listening on the socket. It is distinct from mpv
// being missing: the tool is installed and nothing is playing, which is an
// ordinary answer rather than a broken setup.
var ErrNotPlaying = errors.New("nothing is playing")

// ErrUnavailable is mpv not being on PATH.
var ErrUnavailable = errors.New("mpv is not installed")

// SocketPath is where `ypl play` opens mpv's IPC socket and `ypl now` looks for
// it. It sits with the CLI's other state rather than in the config directory,
// because it is a socket that exists only while something is playing.
func SocketPath() string {
	return filepath.Join(goclilogin.StateDir("ypl"), "mpv.sock")
}

// Addressable reports whether mpv can open a socket at this path.
func Addressable(path string) bool { return len([]byte(path)) < pathLimit }

// Play hands urls to mpv and waits for it, returning mpv's own exit code.
//
// The URLs are arguments rather than a playlist file, so a limit means the same
// thing here as everywhere else and nothing is written to disk to play a
// playlist the server already holds.
//
// A socketPath of "" plays without the IPC socket, which costs only `ypl now`.
func Play(ctx context.Context, socketPath string, extra, urls []string) (int, error) {
	found, err := exec.LookPath(binary)
	if err != nil {
		return 0, fmt.Errorf("%w — install it to play a playlist: %w", ErrUnavailable, err)
	}
	arguments := []string{}
	if socketPath != "" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			return 0, fmt.Errorf("make somewhere for the socket: %w", err)
		}
		clearStale(socketPath)
		arguments = append(arguments, "--input-ipc-server="+socketPath)
	}
	arguments = append(arguments, extra...)
	arguments = append(arguments, urls...)

	// mpv inherits this process's terminal. It draws a status line and takes
	// keys, so a captured stream would put its interface into a string and
	// leave it unable to be paused.
	player := exec.CommandContext(ctx, found, arguments...)
	player.Stdin, player.Stdout, player.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = player.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("run %s: %w", binary, err)
	}
	return 0, nil
}

// clearStale removes a socket left behind by an mpv that did not exit cleanly.
// mpv refuses to start when the path is taken, so a crash would otherwise make
// every later `ypl play` fail. It is removed only once nothing answers on it,
// so a running player is never cut off.
func clearStale(socketPath string) {
	if _, err := os.Stat(socketPath); err != nil {
		return
	}
	if connection, err := net.DialTimeout("unix", socketPath, dialTimeout); err == nil {
		_ = connection.Close()
		return
	}
	_ = os.Remove(socketPath)
}

// Properties reads several of mpv's properties over one connection.
//
// Batched because `ypl now` is the kind of command a status bar runs on a
// timer, and one connection per property would be three or four every tick. A
// property mpv will not answer for — no chapter in this video, no position yet
// — comes back absent rather than failing the whole read, so the optional ones
// stay optional.
//
// Replies are matched by request_id, because mpv pushes unsolicited event lines
// down the same socket and the first line back is often not the answer.
func Properties(socketPath string, names []string) (map[string]any, error) {
	connection, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: nothing is listening on %s", ErrNotPlaying, socketPath)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		return nil, err
	}

	pending := map[int]string{}
	var asked []byte
	for i, name := range names {
		id := i + 1
		pending[id] = name
		request, err := json.Marshal(map[string]any{"command": []string{"get_property", name}, "request_id": id})
		if err != nil {
			return nil, err
		}
		asked = append(asked, request...)
		asked = append(asked, '\n')
	}
	if _, err := connection.Write(asked); err != nil {
		return nil, fmt.Errorf("%w: lost the connection to %s", ErrNotPlaying, socketPath)
	}

	found := map[string]any{}
	decoder := json.NewDecoder(connection)
	for len(pending) > 0 {
		var reply struct {
			RequestID int    `json:"request_id"`
			Error     string `json:"error"`
			Data      any    `json:"data"`
		}
		if err := decoder.Decode(&reply); err != nil {
			return nil, fmt.Errorf("%w: %s answered nothing for %d of the properties asked", ErrNotPlaying, socketPath, len(pending))
		}
		name, asked := pending[reply.RequestID]
		if !asked {
			continue
		}
		delete(pending, reply.RequestID)
		if reply.Error == "success" {
			found[name] = reply.Data
		}
	}
	return found, nil
}
