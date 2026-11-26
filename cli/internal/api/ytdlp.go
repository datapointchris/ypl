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

// The entries of a run's answer, in the order the server writes them.
const (
	runStdout   = "stdout"
	runStderr   = "stderr"
	runExitCode = "exit-code"
	runFiles    = "files/"
)

// Run is what a yt-dlp run on the server answered.
type Run struct {
	archive *tar.Reader
	body    io.ReadCloser
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
// an argument is a Refusal coded CodeArgumentRefused. The answer is read in
// order with Next, and closed once read.
func (c *Client) RunYtdlp(ctx context.Context, args []string) (*Run, error) {
	encoded, err := json.Marshal(struct {
		Args []string `json:"args"`
	}{Args: args})
	if err != nil {
		return nil, fmt.Errorf("encode the request body: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/ytdlp/runs", http.Header{"Accept": {"application/x-tar"}}, encoded)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, refusalFrom(resp)
	}
	return &Run{archive: tar.NewReader(resp.Body), body: resp.Body}, nil
}

// Close ends the read of the answer.
func (r *Run) Close() error { return r.body.Close() }

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

// Next is the next file the run wrote, and io.EOF after the last.
func (r *Run) Next() (RunFile, error) {
	header, err := r.archive.Next()
	if errors.Is(err, io.EOF) {
		return RunFile{}, io.EOF
	}
	if err != nil {
		return RunFile{}, fmt.Errorf("read the run's files: %w", err)
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
	// HeldUntil is when the door's hold for the read ends.
	HeldUntil time.Time `json:"held_until"`
}

// TakeLease waits for a person's turn at the server's door, and holds it
// while this machine reads.
func (c *Client) TakeLease(ctx context.Context) (Lease, error) {
	return post[Lease](ctx, c, "/api/v1/ytdlp/leases", nil)
}
