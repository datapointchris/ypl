package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Class is who a read is for. When the door frees, the waiting read of the
// lowest Class goes next, and reads of one Class go in the order they arrived.
type Class int

const (
	// Person is a read a person asked for and waits on.
	Person Class = iota
	// Service is a read another program asked for: a video's captions or audio.
	Service
	// Enrichment is the server reading its own library for tracklists, which
	// keeps its own slower pace on top of the door's.
	Enrichment
	classes
)

// String names the class as a log line and a refusal word it.
func (c Class) String() string {
	switch c {
	case Person:
		return "person"
	case Service:
		return "service"
	case Enrichment:
		return "enrichment"
	}
	return fmt.Sprintf("class %d", int(c))
}

const (
	// Gap is the least time between one read ending and the next starting.
	// Each gap adds up to Jitter more at random, so reads keep no rhythm.
	Gap    = 8 * time.Second
	Jitter = 8 * time.Second
	// RefusedPause is how long the door opens to nothing after YouTube refuses
	// the address. YouTube's rate limit can outlast an hour, and a read sent
	// into a refusal is what turns it into a block.
	RefusedPause = 24 * time.Hour
	// FailingPause is how long the door opens to nothing after reads of
	// MaxFailuresInARow different subjects failed in a row, whatever they
	// failed with. A refusal worded in a way this package does not recognize
	// would otherwise be read into again and again. A subject failing again
	// adds nothing, so one caller asking for one unreadable video again and
	// again does not pause every other caller.
	FailingPause      = time.Hour
	MaxFailuresInARow = 3
	// MostWait is how long a person's or a service's read waits for its turn
	// before the door answers ErrBusy. A read of audio holds the door for many
	// minutes, and a caller told to come back can do other work meanwhile.
	// Enrichment waits however long it takes.
	MostWait = 2 * time.Minute
)

// ErrBusy is a read whose turn did not come within MostWait.
var ErrBusy = errors.New("the server's yt-dlp stayed busy with other reads")

// ErrClosed is a read asked for, or still waiting, once the door has closed
// for the server to stop.
var ErrClosed = errors.New("the server is shutting down, so it begins no read of YouTube")

// PauseCause says why the door paused.
type PauseCause string

const (
	// PausedRefused is YouTube refusing the address.
	PausedRefused PauseCause = "refused"
	// PausedFailing is MaxFailuresInARow reads in a row failing.
	PausedFailing PauseCause = "failing"
)

// Pause is the door opening to nothing from At until Until, for Cause, after
// the read that failed with Error.
type Pause struct {
	At    time.Time
	Until time.Time
	Cause PauseCause
	Error string
}

// ErrPaused is every read the door turns away while a pause holds, and every
// read waiting when one begins. The error wrapping it is a *PausedError.
var ErrPaused = errors.New("reads of YouTube are paused")

// PausedError is a read turned away by the pause it carries.
type PausedError struct {
	Pause Pause
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("%v until %s, after %s reads: %s", ErrPaused, e.Pause.Until.UTC().Format(time.RFC3339), e.Pause.Cause, e.Pause.Error)
}

func (e *PausedError) Is(target error) bool { return target == ErrPaused }

// pauseBegun is the refusal of a read by YouTube, which began pause for every
// read. It is no ErrPaused, since the read was made.
type pauseBegun struct {
	pause Pause
	err   error
}

func (e *pauseBegun) Error() string { return e.err.Error() }

func (e *pauseBegun) Unwrap() error { return e.err }

// PauseOf is the pause the read that failed with err met: the one that turned
// it away, or the one YouTube's refusal of it began. ok is false where no pause
// touched the read.
func PauseOf(err error) (Pause, bool) {
	var paused *PausedError
	if errors.As(err, &paused) {
		return paused.Pause, true
	}
	var begun *pauseBegun
	if errors.As(err, &begun) {
		return begun.pause, true
	}
	return Pause{}, false
}

// Pauses keeps the door's pauses across a restart: the latest one, which the
// door takes up again as it opens, and each new one.
type Pauses interface {
	LatestPause(ctx context.Context) (Pause, bool, error)
	RecordPause(ctx context.Context, pause Pause) error
}

// recordLimit bounds the store write of a new pause.
const recordLimit = 5 * time.Second

// Door is the one way this server runs yt-dlp. It runs one process at a time,
// and starts each read at least Gap, plus up to Jitter, after the last ended.
// A refusal of the address, or failures in a row of MaxFailuresInARow
// different subjects, pause it for every Class.
type Door struct {
	binary   Binary
	pauses   Pauses
	log      *slog.Logger
	now      func() time.Time
	gap      func() time.Duration
	mostWait time.Duration
	// stopped is done once Close is called, which cuts short every read that
	// does not outlive it.
	stopped context.Context
	stop    context.CancelFunc

	mu sync.Mutex
	// busy is set while a read holds the door, and closed once Close is called.
	busy   bool
	closed bool
	// nextStart is when the door next lets a read in.
	nextStart time.Time
	waiting   [classes][]*turn
	paused    Pause
	// failedSubjects is the subject of each read in the current run of
	// failures, each once.
	failedSubjects []string
}

// readTerms is how one read goes through the door.
type readTerms struct {
	class Class
	// subject is what the read is of, such as a video id. A read failing on a
	// subject already in the run of failures adds none.
	subject string
	limit   time.Duration
	// outlivesClose is a read that Close lets run to its end.
	outlivesClose bool
}

// DrainBound is the longest a read runs on once Close is called. That is the
// history read, which Close lets finish so the jar YouTube rotated during it
// is kept. Every other read is cut short, and its process is gone within
// waitDelay.
const DrainBound = max(HistoryLimit, waitDelay)

// turn is one read waiting at the door. wake is signaled whenever what it waits
// on may have changed: the door freed, a read ahead of it arrived or left, or a
// pause began.
type turn struct {
	class Class
	wake  chan struct{}
	// granted is set once the turn holds the door, and refused once a pause
	// turned it away. Both are read and written under the door's mutex.
	granted bool
	refused error
}

// NewDoor is a door running binary, taking up the latest pause pauses holds and
// recording each new one there, and logging each pause to log. The first read
// waits a gap, since the last read before a restart may have ended a moment
// ago.
func NewDoor(ctx context.Context, binary Binary, pauses Pauses, log *slog.Logger) (*Door, error) {
	d := &Door{binary: binary, pauses: pauses, log: log, now: time.Now, gap: gap, mostWait: MostWait}
	d.stopped, d.stop = context.WithCancel(context.Background())
	d.nextStart = d.now().Add(d.gap())
	latest, ok, err := pauses.LatestPause(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the latest pause of YouTube reads: %w", err)
	}
	if ok {
		d.paused = latest
	}
	return d, nil
}

// gap is Gap and up to Jitter more at random.
func gap() time.Duration {
	return Gap + time.Duration(rand.Int64N(int64(Jitter)+1))
}

// PausedUntil is the pause holding now, and false when none does.
func (d *Door) PausedUntil() (Pause, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused, d.now().Before(d.paused.Until)
}

// Close turns away every read waiting at the door and every read asked for
// after, with ErrClosed. It cuts short the read holding the door, which then
// answers ErrClosed too, unless that read outlives a close. The history read
// does.
func (d *Door) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.refuseAllLocked(ErrClosed)
	d.stop()
}

// enter waits for the door, for a read of class, and returns the function that
// lets it go with what the read was of and how it ended, which leave describes.
// It returns a *PausedError while a pause
// holds or once one begins, ErrBusy once a person or a service has waited
// mostWait, ErrClosed once the door closes, and ctx's error once ctx ends.
func (d *Door) enter(ctx context.Context, class Class) (func(subject string, outcome error) (Pause, bool), error) {
	t := &turn{class: class, wake: make(chan struct{}, 1)}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ErrClosed
	}
	if err := d.pausedLocked(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.waiting[class] = append(d.waiting[class], t)
	d.wakeAllLocked()
	d.mu.Unlock()

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var givenUp <-chan time.Time
	if class != Enrichment {
		most := time.NewTimer(d.mostWait)
		defer most.Stop()
		givenUp = most.C
	}
	leaveQueue := func(err error) (func(string, error) (Pause, bool), error) {
		d.mu.Lock()
		d.removeLocked(t)
		d.wakeAllLocked()
		d.mu.Unlock()
		return nil, err
	}
	for {
		d.mu.Lock()
		if t.refused != nil {
			d.mu.Unlock()
			return nil, t.refused
		}
		var due <-chan time.Time
		if !d.busy && d.nextLocked() == t {
			wait := d.nextStart.Sub(d.now())
			if wait <= 0 {
				d.removeLocked(t)
				d.busy, t.granted = true, true
				d.mu.Unlock()
				return func(subject string, outcome error) (Pause, bool) { return d.leave(class, subject, outcome) }, nil
			}
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			due = timer.C
		}
		d.mu.Unlock()
		select {
		case <-t.wake:
		case <-due:
		case <-givenUp:
			return leaveQueue(fmt.Errorf("%w: no turn came within %v", ErrBusy, d.mostWait))
		case <-ctx.Done():
			return leaveQueue(ctx.Err())
		}
	}
}

// leave frees the door after a read of class and subject that ended with
// outcome, and pauses it where outcome calls for that. It returns the pause
// outcome began, and false where it began none.
func (d *Door) leave(class Class, subject string, outcome error) (Pause, bool) {
	d.mu.Lock()
	now := d.now()
	d.busy = false
	d.nextStart = now.Add(d.gap())
	var begun *Pause
	switch counted := counts(outcome); {
	case errors.Is(outcome, ErrRateLimited):
		begun = &Pause{At: now, Until: now.Add(RefusedPause), Cause: PausedRefused, Error: outcome.Error()}
	case counted < 0:
		d.failedSubjects = nil
	case counted > 0 && !slices.Contains(d.failedSubjects, subject):
		d.failedSubjects = append(d.failedSubjects, subject)
		if len(d.failedSubjects) >= MaxFailuresInARow {
			begun = &Pause{At: now, Until: now.Add(FailingPause), Cause: PausedFailing, Error: fmt.Sprintf("reads of %d different subjects failed in a row, the last: %v", len(d.failedSubjects), outcome)}
		}
	}
	if begun != nil {
		d.failedSubjects = nil
		d.paused = *begun
		d.refuseAllLocked(&PausedError{Pause: *begun})
	}
	d.wakeAllLocked()
	d.mu.Unlock()

	if begun == nil {
		return Pause{}, false
	}
	d.log.Warn("reads of YouTube paused", "until", begun.Until.UTC().Format(time.RFC3339), "cause", begun.Cause, "class", class.String(), "err", outcome)
	ctx, cancel := context.WithTimeout(context.Background(), recordLimit)
	defer cancel()
	if err := d.pauses.RecordPause(ctx, *begun); err != nil {
		d.log.Error("the pause of YouTube reads was not recorded, so a restart reads again before it ends", "until", begun.Until.UTC().Format(time.RFC3339), "err", err)
	}
	return *begun, true
}

// counts is how outcome moves the failures in a row: -1 clears them, since
// YouTube answered; 0 leaves them, since the read ended for its caller rather
// than for YouTube; and 1 adds one.
func counts(outcome error) int {
	switch {
	case outcome == nil, errors.Is(outcome, ErrUnreadable), errors.Is(outcome, ErrNotStarted),
		errors.Is(outcome, ErrSignedOut), errors.Is(outcome, ErrTooLarge):
		return -1
	case errors.Is(outcome, errCallerLeft):
		return 0
	}
	return 1
}

// errCallerLeft is a read cut short because its caller's context ended, which
// says nothing about YouTube.
var errCallerLeft = errors.New("the caller stopped waiting for the read")

// pausedLocked is the refusal of a read while a pause holds, and nil when none
// does.
func (d *Door) pausedLocked() error {
	if d.now().Before(d.paused.Until) {
		return &PausedError{Pause: d.paused}
	}
	return nil
}

// nextLocked is the next turn in: the earliest of the lowest class waiting.
func (d *Door) nextLocked() *turn {
	for c := range d.waiting {
		if len(d.waiting[c]) > 0 {
			return d.waiting[c][0]
		}
	}
	return nil
}

// refuseAllLocked turns every waiting read away with err. Each is woken before
// the queue empties, because wakeAllLocked reaches only queued turns.
func (d *Door) refuseAllLocked(err error) {
	for c := range d.waiting {
		for _, t := range d.waiting[c] {
			t.refused = err
		}
	}
	d.wakeAllLocked()
	d.waiting = [classes][]*turn{}
}

func (d *Door) removeLocked(t *turn) {
	d.waiting[t.class] = slices.DeleteFunc(d.waiting[t.class], func(w *turn) bool { return w == t })
}

// wakeAllLocked tells every waiting turn to look again. Few reads wait at once,
// so waking each beats tracking which one has to.
func (d *Door) wakeAllLocked() {
	for c := range d.waiting {
		for _, t := range d.waiting[c] {
			select {
			case t.wake <- struct{}{}:
			default:
			}
		}
	}
}

// through runs read as one turn at the door on terms, and lets the door go
// with how it ended. A read cut short because ctx ended is ctx's error. One cut
// short by Close is ErrClosed, and neither counts as a failure. One cut short
// by its limit fails like any other read. A refusal of the address carries the
// pause it began, which PauseOf reads.
func through[T any](ctx context.Context, d *Door, terms readTerms, read func(context.Context) (T, error)) (T, error) {
	var none T
	release, err := d.enter(ctx, terms.class)
	if err != nil {
		return none, err
	}
	readCtx, cancel := context.WithTimeout(ctx, terms.limit)
	defer cancel()
	if !terms.outlivesClose {
		defer context.AfterFunc(d.stopped, cancel)()
	}
	got, err := read(readCtx)
	switch {
	case ctx.Err() != nil:
		release(terms.subject, errCallerLeft)
		return none, ctx.Err()
	case err != nil && !terms.outlivesClose && d.stopped.Err() != nil:
		release(terms.subject, errCallerLeft)
		return none, fmt.Errorf("%w, and stopped the read under way", ErrClosed)
	case err != nil && readCtx.Err() != nil:
		err = fmt.Errorf("the read ran past %v: %w", terms.limit, err)
	}
	if pause, begun := release(terms.subject, err); begun && errors.Is(err, ErrRateLimited) {
		err = &pauseBegun{pause: pause, err: err}
	}
	return got, err
}
