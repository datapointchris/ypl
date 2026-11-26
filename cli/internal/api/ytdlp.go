package api

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The codes a run and a lease are refused with, beside the door's own.
const (
	// CodeArgumentRefused is the server refusing to pass an argument to
	// yt-dlp. The refusal's sentence names it and why.
	CodeArgumentRefused = "ytdlp_argument_refused"
	// CodeReadsBusy is the door staying busy with other reads for longer than
	// a person's turn may wait. Nothing ran, so the request can be sent again.
	CodeReadsBusy = "youtube_reads_busy"
	// CodeReadsPaused is the door's pause after YouTube refused the address,
	// or after reads failed in a row. Its Retry-After is when the pause ends.
	CodeReadsPaused = "youtube_reads_paused"
	// CodeReadsUnavailable is a server that runs no yt-dlp, and so has no
	// door.
	CodeReadsUnavailable = "youtube_reads_unavailable"
)

// The entries of a run's answer, in the order the server writes them: the
// streams and the exit code, each file under runFiles, then runEnd.
const (
	runStdout   = "stdout"
	runStderr   = "stderr"
	runExitCode = "exit-code"
	runFiles    = "files/"
	runEnd      = "end"
)

// The longest this client waits for the server to begin an answer. Each is
// longer than the server's own bound on the same wait: 2 minutes for a turn at
// the door, and 2 hours more for a run. Neither bounds reading an answer that
// has begun, since a run's files can take long to arrive.
const (
	leaseAnswerWithin = 3 * time.Minute
	runAnswerWithin   = 2*time.Hour + 5*time.Minute
)

// Run is what a yt-dlp run on the server answered.
type Run struct {
	archive *tar.Reader
	body    io.ReadCloser
	cancel  context.CancelFunc
}

// RunFile is one file a run wrote, open for reading until the next is asked
// for.
type RunFile struct {
	// Path is where under the run's directory yt-dlp wrote it, with slashes.
	Path     string
	Modified time.Time
	io.Reader
}

// RunYtdlp runs yt-dlp with args on the server, through its door. A refusal of
// an argument is a Refusal coded CodeArgumentRefused. It gives up where no
// answer begins within runAnswerWithin. The answer is read in order with Next,
// and closed once read.
func (c *Client) RunYtdlp(ctx context.Context, args []string) (*Run, error) {
	encoded, err := json.Marshal(struct {
		Args []string `json:"args"`
	}{Args: args})
	if err != nil {
		return nil, fmt.Errorf("encode the request body: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	silent := time.AfterFunc(runAnswerWithin, cancel)
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/ytdlp/runs", http.Header{"Accept": {"application/x-tar"}}, encoded)
	if !silent.Stop() {
		// The timer has already canceled ctx.
		if err == nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("the server began no answer to the run within %v", runAnswerWithin)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		return nil, refusalFrom(resp)
	}
	return &Run{archive: tar.NewReader(resp.Body), body: resp.Body, cancel: cancel}, nil
}

// Close ends the read of the answer.
func (r *Run) Close() error {
	defer r.cancel()
	return r.body.Close()
}

// Streams copies what yt-dlp printed to stdout and stderr, and returns the
// code it exited with. It reads the first three entries, so it is called
// before Next.
func (r *Run) Streams(stdout, stderr io.Writer) (int, error) {
	for _, entry := range []struct {
		name string
		to   io.Writer
	}{{runStdout, stdout}, {runStderr, stderr}} {
		if err := r.expect(entry.name); err != nil {
			return 0, err
		}
		if _, err := io.Copy(entry.to, r.archive); err != nil {
			return 0, fmt.Errorf("copy the run's %s: %w", entry.name, err)
		}
	}
	if err := r.expect(runExitCode); err != nil {
		return 0, err
	}
	held, err := io.ReadAll(io.LimitReader(r.archive, 16))
	if err != nil {
		return 0, fmt.Errorf("read the run's exit code: %w", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(held)))
	if err != nil {
		return 0, fmt.Errorf("the run's exit code %q is not a number", held)
	}
	return code, nil
}

// expect reads the next entry, which has to be name.
func (r *Run) expect(name string) error {
	header, err := r.archive.Next()
	if err != nil {
		return fmt.Errorf("read the run's %s: %w", name, err)
	}
	if header.Name != name {
		return fmt.Errorf("the run's answer holds %q where %q belongs", header.Name, name)
	}
	return nil
}

// Next is the next file the run wrote, and io.EOF once the answer's end entry
// is read. An answer that stops before that entry is an error, since a file
// the run wrote may be missing from it.
func (r *Run) Next() (RunFile, error) {
	header, err := r.archive.Next()
	if errors.Is(err, io.EOF) {
		return RunFile{}, errors.New("the server's answer stopped before its end, so a file the run wrote may be missing")
	}
	if err != nil {
		return RunFile{}, fmt.Errorf("read the run's files: %w", err)
	}
	if header.Name == runEnd {
		return RunFile{}, io.EOF
	}
	path, ok := strings.CutPrefix(header.Name, runFiles)
	if !ok || header.Typeflag != tar.TypeReg {
		return RunFile{}, fmt.Errorf("the run's answer holds %q, which is no file it wrote", header.Name)
	}
	return RunFile{Path: path, Modified: header.ModTime, Reader: r.archive}, nil
}

// Lease is a turn at the server's door, held while this machine reads YouTube
// itself.
type Lease struct {
	// ID names the lease to EndLease.
	ID string `json:"id"`
	// HeldUntil is when the door's hold for the read ends.
	HeldUntil time.Time `json:"held_until"`
}

// TakeLease waits for a person's turn at the server's door, and holds it
// while this machine reads. It gives up where no answer comes within
// leaseAnswerWithin.
func (c *Client) TakeLease(ctx context.Context) (Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, leaseAnswerWithin)
	defer cancel()
	return post[Lease](ctx, c, "/api/v1/ytdlp/leases", nil)
}

// EndLease tells the server's door how the read that lease id held for ended:
// yt-dlp's exit code, and the tail of what it wrote to stderr. The door reads
// that for YouTube's refusal of the address, as it reads its own reads.
func (c *Client) EndLease(ctx context.Context, id string, exitCode int, stderr string) error {
	_, err := c.send(ctx, http.MethodPut, "/api/v1/ytdlp/leases/"+ref(id)+"/outcome", nil, struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}{ExitCode: exitCode, Stderr: stderr}, nil)
	return err
}
