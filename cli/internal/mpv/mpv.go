// Package mpv plays videos through the mpv binary and asks a running one what
// it is doing.
//
// Shelled out to rather than embedded, for the same reason the server shells
// out to yt-dlp: mpv is the thing that already knows how to stream YouTube, and
// it tracks YouTube's changes on its own release schedule.
//
// Nothing of mpv's own vocabulary leaves this package. A caller is given this
// tool's words for how a playback ended and for what is loaded, because two
// vocabularies meeting anywhere else means every caller interprets the player
// for itself. mpv's exit codes overlap ypl's and mean different things at the
// values they share, and mpv's property replies are untyped.
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

// stopDelay is how long mpv is given to finish after its context ends, before
// its streams are taken from it. It is set beside the context unconditionally:
// this call site hands mpv the terminal's own files rather than pipes, so
// nothing blocks on a grandchild today, and a later change to the streams would
// reinstate that without anything reporting it.
const stopDelay = 5 * time.Second

// pathLimit is how long a unix socket address may be. The field is fixed size
// in the kernel: 104 bytes on macOS and the BSDs, 108 on Linux. Over it, mpv
// logs `Could not create IPC socket` and carries on playing, so playback works
// and `ypl now` silently reports that nothing is on. The smaller of the two is
// checked, since the same XDG_STATE_HOME can be shared between machines.
const pathLimit = 104

// Arguments are options a caller passes straight through to mpv.
type Arguments []string

// WatchURLs are the addresses mpv is asked to play, in the order to play them.
type WatchURLs []string

// ErrNotPlaying is nothing playing: nothing listening on the socket, or an mpv
// that is running with no file loaded. It is distinct from mpv being missing,
// and distinct from a socket that could not be read.
var ErrNotPlaying = errors.New("nothing is playing")

// ErrUnavailable is mpv not being on PATH.
var ErrUnavailable = errors.New("mpv is not installed")

// ErrUnreadable is a socket that answered and could not be read: a write that
// failed, a reply that would not decode, or a player that did not answer inside
// the deadline. It is separate from ErrNotPlaying because reporting it as
// nothing playing makes the command wrong about the one thing it is asked.
var ErrUnreadable = errors.New("what is playing could not be read")

// SocketPath is where `ypl playlists play` opens mpv's IPC socket and `ypl now` looks for
// it. It sits with the CLI's other state rather than in the config directory,
// because it is a socket that exists only while something is playing.
func SocketPath() string {
	return filepath.Join(goclilogin.StateDir("ypl"), "mpv.sock")
}

// Addressable reports whether mpv can open a socket at this path.
func Addressable(path string) bool { return len([]byte(path)) < pathLimit }

// Available reports whether mpv can be run at all, so a caller can refuse
// before it does any work of its own.
func Available() error {
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("%w — install it to play a playlist: %w", ErrUnavailable, err)
	}
	return nil
}

// Outcome is how a playback ended, in this tool's words.
//
// mpv's own status is carried only as something to tell the person. It is never
// returned as this tool's exit code: mpv spends 2 on a file it cannot open and
// ypl spends 2 on an invocation it would not accept, so passing it through
// makes a geo-blocked video and a mistyped command line the same answer.
type Outcome struct {
	// Failed is mpv ending in anything but success.
	Failed bool
	// Says is what to tell the person about how it ended, and "" where it
	// ended well.
	Says string
}

// State is what mpv reports about what it is playing, decoded into this tool's
// own fields. A property mpv did not answer for is absent here rather than
// zero, so a caller can tell a position of nothing from a position of zero.
type State struct {
	// Path is the address mpv has loaded.
	Path string
	// Title is what mpv calls it, which is the only name for a video the
	// server has never heard of.
	Title string
	// Position is how far in mpv is, in whole seconds.
	Position *int64
	// Duration is how long mpv believes the video is, in whole seconds.
	Duration *int64
}

// Play hands urls to mpv and waits for it.
//
// The URLs are arguments rather than a playlist file, so nothing is written to
// disk to play a playlist the server already holds.
//
// A socketPath of "" plays without the IPC socket, which costs only `ypl now`.
func Play(ctx context.Context, socketPath string, extra Arguments, urls WatchURLs) (Outcome, error) {
	found, err := exec.LookPath(binary)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w — install it to play a playlist: %w", ErrUnavailable, err)
	}
	arguments := []string{}
	if socketPath != "" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			return Outcome{}, fmt.Errorf("make somewhere for the socket: %w", err)
		}
		arguments = append(arguments, "--input-ipc-server="+socketPath)
	}
	arguments = append(arguments, extra...)
	arguments = append(arguments, urls...)

	// mpv inherits this process's terminal. It draws a status line and takes
	// keys, so a captured stream would put its interface into a string and
	// leave it unable to be paused.
	player := exec.CommandContext(ctx, found, arguments...)
	player.Stdin, player.Stdout, player.Stderr = os.Stdin, os.Stdout, os.Stderr
	player.WaitDelay = stopDelay
	err = player.Run()
	return ended(ctx, err)
}

// ended is how mpv finished, said in this tool's words.
//
// The context is read before the status, because a process the context stopped
// reports the same -1 as one that took a signal of its own, and -1 is not an
// exit code at all.
func ended(ctx context.Context, err error) (Outcome, error) {
	if err == nil {
		return Outcome{}, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return Outcome{}, fmt.Errorf("run %s: %w", binary, err)
	}
	if ctx.Err() != nil {
		return Outcome{Failed: true, Says: "Playback was stopped before it finished."}, nil
	}
	if code := exit.ExitCode(); code >= 0 {
		return Outcome{Failed: true, Says: fmt.Sprintf("mpv stopped early and exited %d.", code)}, nil
	}
	return Outcome{Failed: true, Says: fmt.Sprintf("mpv was killed by a signal (%s).", exit.String())}, nil
}

// asked is the properties a read wants, and the order they are asked in.
var asked = []string{"path", "time-pos", "duration", "media-title"}

// Read is what the running mpv is playing.
//
// One connection carries every property, because `ypl now` is the kind of
// command a status bar runs on a timer and one connection per property would be
// four every tick. A property mpv will not answer for — no position yet —
// arrives absent rather than failing the read, so the optional ones stay
// optional.
//
// Replies are matched by request_id, because mpv pushes unsolicited event lines
// down the same socket and the first line back is often not the answer.
func Read(socketPath string) (State, error) {
	connection, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return State{}, fmt.Errorf("%w: nothing is listening on %s", ErrNotPlaying, socketPath)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		return State{}, unreadable("%s would not hold a deadline: %v", socketPath, err)
	}

	pending := map[int]string{}
	var request []byte
	for i, name := range asked {
		id := i + 1
		pending[id] = name
		line, err := json.Marshal(map[string]any{"command": []string{"get_property", name}, "request_id": id})
		if err != nil {
			return State{}, unreadable("%s could not be asked for %s: %v", socketPath, name, err)
		}
		request = append(request, line...)
		request = append(request, '\n')
	}
	if _, err := connection.Write(request); err != nil {
		return State{}, unreadable("lost the connection to %s", socketPath)
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
			return State{}, unreadable("%s answered nothing for %d of the properties asked", socketPath, len(pending))
		}
		name, wanted := pending[reply.RequestID]
		if !wanted {
			continue
		}
		delete(pending, reply.RequestID)
		if reply.Error == "success" {
			found[name] = reply.Data
		}
	}

	state := State{
		Path:     text(found["path"]),
		Title:    text(found["media-title"]),
		Position: seconds(found["time-pos"]),
		Duration: seconds(found["duration"]),
	}
	// An mpv running with nothing loaded answers every read and holds no path.
	// It is reachable without anyone asking for it, because `ypl playlists play` runs mpv
	// with the person's own config and `idle=yes` in it leaves the player alive
	// after the last video.
	if state.Path == "" {
		return State{}, fmt.Errorf("%w: %s is running and has nothing loaded", ErrNotPlaying, socketPath)
	}
	return state, nil
}

// text is a property mpv answered with as a string, and "" for one it did not
// answer at all.
func text(value any) string {
	answer, _ := value.(string)
	return answer
}

// seconds is a property mpv answered with as whole seconds. mpv sends a
// position as a float, and nothing here shows anything finer than a second.
func seconds(value any) *int64 {
	answer, ok := value.(float64)
	if !ok {
		return nil
	}
	whole := int64(answer)
	return &whole
}

// unreadable is every way a socket that answered the dial fails afterwards.
// One site rather than four, so the choice between this and ErrNotPlaying is
// made once — the two are a sentence apart to a reader and opposite answers to
// the person, who is told either that nothing is on or that something is wrong.
func unreadable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, fmt.Sprintf(format, args...))
}
