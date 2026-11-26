package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RunLimit bounds one person's run. A playlist read whole, with the door's
// sleeps before each of its videos, fits inside it.
const RunLimit = 2 * time.Hour

// unreadLimit bounds a run with no input, such as --version or --help, which
// asks YouTube nothing.
const unreadLimit = 30 * time.Second

// stderrKept bounds how much of a failed run's stderr is read for why it
// failed. yt-dlp writes the error that ended a run last.
const stderrKept = 1 << 20

// DiskFloor is the least free room a run leaves on the disk it writes to. The
// server's store may share that disk, and a write it cannot make fails a play,
// a sync or the record of a pause.
const DiskFloor = 1 << 30

// diskEvery is how often a run's disk is read for its free room.
const diskEvery = time.Second

// ErrDiskLow is a run stopped once the disk it writes to fell below DiskFloor.
var ErrDiskLow = errors.New("the run stopped, since the server's disk had too little room left")

// ErrRunCutShort is a run that began, and whose caller stopped waiting before
// it ended. It wraps the caller's own error.
var ErrRunCutShort = errors.New("the run began, and its caller stopped waiting before it ended")

// Run is a person's yt-dlp run as it ended: its exit code, what it printed,
// and the files it wrote. Close removes them.
type Run struct {
	ExitCode int
	dir      string
}

// RunFile is one file a run wrote: its path under the run's directory, with
// slashes, and when it was last written.
type RunFile struct {
	Path     string
	Size     int64
	Modified time.Time
}

// StdoutPath holds what the run printed to stdout.
func (r *Run) StdoutPath() string { return filepath.Join(r.dir, "stdout") }

// StderrPath holds what the run printed to stderr.
func (r *Run) StderrPath() string { return filepath.Join(r.dir, "stderr") }

// FilesDir is where yt-dlp wrote the run's files, its working directory.
func (r *Run) FilesDir() string { return filepath.Join(r.dir, "files") }

// Close removes the run's streams and files.
func (r *Run) Close() error { return os.RemoveAll(r.dir) }

// Files lists each regular file under FilesDir, in walk order. A link that
// yt-dlp left is no file of the run's, so it is skipped.
func (r *Run) Files() ([]RunFile, error) {
	var files []RunFile
	err := filepath.WalkDir(r.FilesDir(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.FilesDir(), path)
		if err != nil {
			return err
		}
		files = append(files, RunFile{Path: filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	return files, err
}

// Size is the bytes of all the run answers with: its two streams and its
// files.
func (r *Run) Size() (int64, error) {
	var total int64
	for _, path := range []string{r.StdoutPath(), r.StderrPath()} {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	files, err := r.Files()
	for _, file := range files {
		total += file.Size
	}
	return total, err
}

// Run runs a person's yt-dlp for class with args, through the door, in a
// directory of its own. Every run passes the door's sleeps, or the person's
// where those are longer, reads no config file, loads no plugin and runs
// YouTube's extractors alone. Under maxBytes, where it is not 0, no download
// is larger than maxBytes.
//
// A run that yt-dlp ended, whatever its exit code, is answered as it ended:
// the exit code is the person's to read, and it moves the runs' own failures
// in a row as a read's error would. A run with no input asks YouTube nothing,
// so it does not wait at the door. The error wraps ErrRateLimited where
// YouTube refused the address, which stops the run at once, and is the door's
// own refusal where no turn came. It wraps ErrRunCutShort where ctx ended once
// the run had begun, and ErrDiskLow where the disk filled.
func (d *Door) Run(ctx context.Context, class Class, args RunArguments, maxBytes int64) (*Run, error) {
	argv := args.argv(d.binary.sleeps, maxBytes)
	if len(args.inputs) == 0 {
		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		readCtx, cancel := context.WithTimeout(ctx, unreadLimit)
		defer cancel()
		return d.binary.passThrough(readCtx, argv, d.diskFloor)
	}
	// made is the run the read made, which through drops where the caller left
	// or the door closed as it ended, and which is then removed here.
	var made *Run
	began := false
	terms := readTerms{class: class, subject: strings.Join(args.inputs, " "), limit: RunLimit, run: true}
	run, err := through(ctx, d, terms, func(ctx context.Context) (*Run, error) {
		began = true
		run, err := d.binary.passThrough(ctx, argv, d.diskFloor)
		if err != nil {
			return nil, err
		}
		made = run
		verdict := run.verdict()
		return run, verdict
	})
	switch {
	case run == nil && made != nil:
		_ = made.Close()
	case errors.Is(err, ErrRateLimited):
		if run != nil {
			_ = run.Close()
		}
		return nil, err
	case run != nil:
		return run, nil
	}
	if began && ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %w", ErrRunCutShort, err)
	}
	return nil, err
}

// argv is what a run passes yt-dlp: the person's options, then what every
// read passes with each sleep at least sleeps, then runOnly, the smaller of the
// person's --max-filesize and maxBytes, and the inputs after --. What ypl adds
// comes after the person's options, so yt-dlp keeps ypl's where both name one.
func (a RunArguments) argv(sleeps Sleeps, maxBytes int64) []string {
	args := append([]string{}, a.options...)
	args = append(args, paced(sleeps.longerOf(a.sleeps))...)
	args = append(args, runOnly...)
	most := a.maxFilesize
	if maxBytes > 0 && (most == 0 || most > maxBytes) {
		most = maxBytes
	}
	if most > 0 {
		args = append(args, "--max-filesize="+strconv.FormatInt(most, 10))
	}
	if len(a.inputs) > 0 {
		args = append(append(args, "--"), a.inputs...)
	}
	return args
}

// verdict is what the run's end says of YouTube's answer, as a read's error
// would: nil for a run that ended well, or stopped at --max-downloads or
// --break-on-existing; errNoVerdict for yt-dlp refusing its arguments; and
// refusal's reading of its stderr otherwise.
func (r *Run) verdict() error {
	switch r.ExitCode {
	case 0, 101:
		return nil
	case 2:
		return errNoVerdict
	}
	stderr, err := os.Open(r.StderrPath())
	if err != nil {
		return err
	}
	defer func() { _ = stderr.Close() }()
	if info, err := stderr.Stat(); err == nil && info.Size() > stderrKept {
		if _, err := stderr.Seek(-stderrKept, io.SeekEnd); err != nil {
			return err
		}
	}
	said, err := io.ReadAll(stderr)
	if err != nil {
		return err
	}
	return refusal(string(said), fmt.Errorf("yt-dlp exited %d", r.ExitCode))
}

// passThrough runs argv in a new directory, writing yt-dlp's two streams
// beside the directory it works in. It stops yt-dlp at the first error line
// saying YouTube refused the address, since reading on is what turns a pause
// into a block. It stops it too once the disk holds less than floor bytes
// free.
func (b Binary) passThrough(ctx context.Context, argv []string, floor int64) (run *Run, err error) {
	dir, err := os.MkdirTemp("", "ypl-run-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if run == nil {
			_ = os.RemoveAll(dir)
		}
	}()
	made := &Run{dir: dir}
	if err := os.Mkdir(made.FilesDir(), 0o700); err != nil {
		return nil, err
	}
	stdout, err := os.Create(made.StdoutPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.Create(made.StderrPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = stderr.Close() }()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	watch := &refusalWatch{stop: stop}
	cmd := exec.CommandContext(runCtx, b.path, argv...)
	cmd.Dir = made.FilesDir()
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(stderr, watch)
	cmd.WaitDelay = waitDelay
	diskLow := make(chan error, 1)
	var watching sync.WaitGroup
	watching.Go(func() { watchDisk(runCtx, dir, floor, stop, diskLow) })
	ran := cmd.Run()
	stop()
	watching.Wait()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case len(diskLow) > 0:
		return nil, <-diskLow
	case watch.refused != nil:
		return nil, watch.refused
	case errors.As(ran, &exit):
		made.ExitCode = exit.ExitCode()
	case ran != nil:
		return nil, ran
	}
	return made, nil
}

// watchDisk reads the free room on dir's disk at once and every diskEvery
// until ctx ends. Once it falls below floor, it sends ErrDiskLow to low and
// stops the run.
func watchDisk(ctx context.Context, dir string, floor int64, stop context.CancelFunc, low chan<- error) {
	tick := time.NewTicker(diskEvery)
	defer tick.Stop()
	for {
		if free, err := freeBytes(dir); err == nil && free < floor {
			low <- fmt.Errorf("%w: %d bytes free, under the %d it keeps", ErrDiskLow, free, floor)
			stop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// freeBytes is the room left to an unprivileged writer on the disk that holds
// path.
func freeBytes(path string) (int64, error) {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(path, &disk); err != nil {
		return 0, err
	}
	return int64(disk.Bavail) * disk.Bsize, nil
}

// refusalWatch reads yt-dlp's stderr a line at a time as it is written, and
// stops the run at the first error line carrying one of rateLimitMarkers.
type refusalWatch struct {
	stop    context.CancelFunc
	partial []byte
	refused error
}

// lineKept bounds the partial line held until its end arrives.
const lineKept = 64 << 10

func (w *refusalWatch) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		end := bytes.IndexAny(w.partial, "\r\n")
		if end < 0 {
			break
		}
		w.check(string(w.partial[:end]))
		w.partial = w.partial[end+1:]
	}
	if len(w.partial) > lineKept {
		w.partial = w.partial[len(w.partial)-lineKept:]
	}
	return len(p), nil
}

func (w *refusalWatch) check(line string) {
	line = strings.TrimSpace(line)
	if w.refused != nil || !strings.HasPrefix(line, "ERROR:") || !containsAny(normalized(line), rateLimitMarkers) {
		return
	}
	w.refused = fmt.Errorf("%w: %s", ErrRateLimited, line)
	w.stop()
}
