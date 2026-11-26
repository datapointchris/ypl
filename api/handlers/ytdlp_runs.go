package handlers

import (
	"archive/tar"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// PersonRoutes is every route that only a person's token reaches, by the
// scope a person's token holds for it. goapiauth lets a person's token reach
// every route and keeps a service's to ServiceRoutes, so none of these is
// listed there. A run of yt-dlp and a lease of the door are a person's: what
// they reach is a person's choosing, and a service's reads have routes of
// their own.
var PersonRoutes = map[string][]string{
	"ypl.ytdlp.run": {"POST /api/v1/ytdlp/runs", "POST /api/v1/ytdlp/leases"},
}

// maxRunBody bounds the body of a run, many times a long command line.
const maxRunBody = 64 << 10

// runRequest is the body of POST /api/v1/ytdlp/runs: a person's arguments to
// yt-dlp, as they typed them.
type runRequest struct {
	Args []string `json:"args"`
}

// The entries of a run's answer, in the order they are written. The run's
// files follow them, each under runFiles.
const (
	runStdout   = "stdout"
	runStderr   = "stderr"
	runExitCode = "exit-code"
	runFiles    = "files/"
)

// runYtdlp runs a person's yt-dlp through the door and answers with one tar
// stream: what it printed to stdout and stderr, its exit code, and each file
// it wrote under files/. A request through the public address gets at most
// maxPublic bytes of those, and no download larger.
func (h *Handlers) runYtdlp(w http.ResponseWriter, r *http.Request) {
	if !h.forAPerson(w, r) {
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
	if h.refuseVideoRead(ctx, w, r, err) {
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
		h.log.InfoContext(r.Context(), "the answer of a run of yt-dlp ended early", "err", err)
	}
}

// writeRun writes run as a tar stream: its stdout, its stderr and its exit
// code, each stamped at now, then each file it wrote under runFiles with the
// time yt-dlp gave it.
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

// leaseAnswer is the body of POST /api/v1/ytdlp/leases: when the door's hold
// for the caller's own read ends.
type leaseAnswer struct {
	HeldUntil string `json:"held_until"`
}

// leaseYtdlp holds the door for a read that the caller makes itself, once its
// turn comes, and answers when the hold ends. The pause and the busy wait
// answer as a read's do.
func (h *Handlers) leaseYtdlp(w http.ResponseWriter, r *http.Request) {
	if !h.forAPerson(w, r) {
		return
	}
	ctx, cancel := h.videos.readContext(r)
	defer cancel()
	until, err := h.videos.door.Lease(ctx, classOf(r))
	if h.refuseVideoRead(ctx, w, r, err) {
		return
	}
	wire.JSON(w, http.StatusCreated, leaseAnswer{HeldUntil: store.Timestamp(until)})
}

// forAPerson is whether r is a person's, on a server with a door to run
// yt-dlp through. ok is false once it has answered. LimitServices already
// keeps a service from PersonRoutes, and this holds the same line should a
// scope ever list one.
func (h *Handlers) forAPerson(w http.ResponseWriter, r *http.Request) bool {
	if classOf(r) == ytdlp.Service {
		wire.Refuse(w, http.StatusForbidden, wire.CodeOutsideServiceScope, "%s %s is a person's alone, and no service's scope reaches it", r.Method, r.URL.Path)
		return false
	}
	if h.videos == nil {
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsUnavailable, "this server runs no yt-dlp: it has none")
		return false
	}
	return true
}
