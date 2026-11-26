package handlers

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// maxRunBody bounds the body of a run, many times a long command line.
const maxRunBody = 64 << 10

// runRequest is the body of POST /api/v1/ytdlp/runs: a person's arguments to
// yt-dlp, as they typed them.
type runRequest struct {
	Args []string `json:"args"`
}

// The entries of a run's answer, in the order they are written. The run's
// files follow them, each under runFiles, and runEnd closes the answer, so an
// answer cut short between two files is told from a whole one.
const (
	runStdout   = "stdout"
	runStderr   = "stderr"
	runExitCode = "exit-code"
	runFiles    = "files/"
	runEnd      = "end"
)

// runYtdlp runs a person's yt-dlp through the door and answers with one tar
// stream: what it printed to stdout and stderr, its exit code, each file it
// wrote under files/, and the end. A request through the public address gets
// at most maxPublic bytes of those, and no download larger. No service scope
// lists this route, so a service's token is refused it.
func (h *Handlers) runYtdlp(w http.ResponseWriter, r *http.Request) {
	if !h.hasDoor(w) {
		return
	}
	body, ok := decodeJSON[runRequest](w, r, maxRunBody, `a run of yt-dlp, {"args": [...]}`)
	if !ok {
		return
	}
	args, err := ytdlp.ReadRunArguments(body.Args)
	var refused *ytdlp.RefusedArgument
	if errors.As(err, &refused) {
		wire.RefuseField(w, http.StatusUnprocessableEntity, wire.CodeArgumentRefused, refused.Argument, "%s", refused.Error())
		return
	}
	var most int64
	if h.videos.public(r) {
		most = h.videos.maxPublic
	}
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	run, err := h.videos.door.Run(ctx, classOf(r), args, most)
	switch {
	case errors.Is(err, ytdlp.ErrRunCutShort) && errors.Is(context.Cause(ctx), errPastPublicBudget):
		// Sending it again would read YouTube from the start and be cut again.
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeRunPastPublicBudget,
			"the run took longer than the %v a request through the public address may take; run it through the server's own address", h.videos.budget)
		return
	case errors.Is(err, ytdlp.ErrDiskLow):
		h.refuseAndLog(w, r, slog.LevelWarn, http.StatusConflict, wire.CodeDiskLow, err, "the server's disk needs room before a run")
		return
	case h.refuseVideoRead(ctx, w, r, err):
		return
	}
	defer func() { _ = run.Close() }()
	size, err := run.Size()
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	if most > 0 && size > most {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeAnswerTooLarge,
			"the run wrote %d bytes, more than the %d this server sends through its public address; run it through the server's own address", size, most)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if err := writeRun(w, run, h.now()); err != nil {
		h.log.ErrorContext(r.Context(), "the answer of a run of yt-dlp ended before its end entry", "err", err)
	}
}

// writeRun writes run as a tar stream: its stdout, its stderr and its exit
// code, each stamped at now, then each file it wrote under runFiles with the
// time yt-dlp gave it, then runEnd.
func writeRun(w io.Writer, run *ytdlp.Run, now time.Time) error {
	archive := tar.NewWriter(w)
	for _, stream := range []struct{ name, path string }{{runStdout, run.StdoutPath()}, {runStderr, run.StderrPath()}} {
		if err := addFile(archive, stream.name, stream.path, now); err != nil {
			return err
		}
	}
	code := []byte(strconv.Itoa(run.ExitCode) + "\n")
	if err := archive.WriteHeader(&tar.Header{Name: runExitCode, Mode: 0o644, Size: int64(len(code)), ModTime: now}); err != nil {
		return err
	}
	if _, err := archive.Write(code); err != nil {
		return err
	}
	files, err := run.Files()
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := addFile(archive, runFiles+file.Path, filepath.Join(run.FilesDir(), filepath.FromSlash(file.Path)), file.Modified); err != nil {
			return err
		}
	}
	if err := archive.WriteHeader(&tar.Header{Name: runEnd, Mode: 0o644, ModTime: now}); err != nil {
		return err
	}
	return archive.Close()
}

// addFile writes the file at path into archive as name, stamped at modified.
func addFile(archive *tar.Writer, name, path string, modified time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: info.Size(), ModTime: modified}); err != nil {
		return err
	}
	_, err = io.Copy(archive, file)
	return err
}

// leaseAnswer is the body of POST /api/v1/ytdlp/leases: the lease's id, which
// its outcome names, and when the door's hold for the caller's own read ends.
type leaseAnswer struct {
	ID        string `json:"id"`
	HeldUntil string `json:"held_until"`
}

// leaseYtdlp holds the door for a read that the caller makes itself, once its
// turn comes, and answers when the hold ends. The pause and the busy wait
// answer as a read's do. No service scope lists this route.
func (h *Handlers) leaseYtdlp(w http.ResponseWriter, r *http.Request) {
	if !h.hasDoor(w) {
		return
	}
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	lease, err := h.videos.door.Lease(ctx, classOf(r))
	if h.refuseVideoRead(ctx, w, r, err) {
		return
	}
	wire.JSON(w, http.StatusCreated, leaseAnswer{ID: lease.ID, HeldUntil: store.Timestamp(lease.HeldUntil)})
}

// maxOutcomeBody bounds the body of a lease's outcome, the tail of a read's
// stderr with room to spare.
const maxOutcomeBody = 128 << 10

// leaseOutcome is the body of PUT /api/v1/ytdlp/leases/{id}/outcome: how the
// caller's own read ended, and the tail of what its yt-dlp wrote to stderr.
type leaseOutcome struct {
	ExitCode int    `json:"exit_code"`
	Stderr   string `json:"stderr"`
}

// endLease takes how the read a lease held for ended, and lets the door go.
// Where YouTube refused that read, the door pauses as it does for a read of
// its own. It answers 204 whatever the read met. No service scope lists this
// route.
func (h *Handlers) endLease(w http.ResponseWriter, r *http.Request) {
	if !h.hasDoor(w) {
		return
	}
	body, ok := decodeJSON[leaseOutcome](w, r, maxOutcomeBody, `how a read ended, {"exit_code": 1, "stderr": "..."}`)
	if !ok {
		return
	}
	h.videos.door.EndLease(r.PathValue("id"), body.ExitCode, body.Stderr)
	w.WriteHeader(http.StatusNoContent)
}

// hasDoor is whether the server has a door to run yt-dlp through. ok is false
// once it has answered 503.
func (h *Handlers) hasDoor(w http.ResponseWriter) bool {
	if h.videos == nil {
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsUnavailable, "this server runs no yt-dlp: it has none")
		return false
	}
	return true
}
