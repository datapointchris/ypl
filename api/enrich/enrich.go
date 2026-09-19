// Package enrich reads a tracklist for each video the channel's playlists hold
// that has none: the video's chapters, or the timestamped lines of its
// description or of one of its top comments.
//
// A read goes through yt-dlp, signed in as nobody, and YouTube throttles an
// address that reads too much too fast. So reads come one at a time, at least
// Pace apart and up to twice that at random, and never in a burst. YouTube's
// rate limit stops reading for RateLimitPause. MaxConsecutiveFailures reads
// failing in a row stop it for FailingPause, whatever they failed with, since
// a refusal worded in a way this package does not recognize would otherwise be
// read into again and again.
//
// A read that stores no tracklist is recorded like one that failed, and the
// video is read again after a widening wait up to MaxAttempts times. A
// tracklist is usually posted as a comment some time after the video is, and
// reading reaches a video soon after it enters a playlist. A video YouTube
// answered that no signed-out read will ever return is not read again at all,
// and api/cmd/reset-enrichment is what puts one back in the queue.
//
// What the reads did is added to a Tally, which the sync takes into the record
// of each of its passes. A refusal is recorded at once, since a server that
// restarted before recording one would read straight back into it.
package enrich

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/tracklist"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// ErrReadsFailing is the failure recorded when MaxConsecutiveFailures reads in
// a row failed, which says something is wrong with the reading rather than with
// any one video.
var ErrReadsFailing = errors.New("enrichment paused after consecutive reads failed")

const (
	// RateLimitPause is how long enrichment makes no read after YouTube refuses
	// one. yt-dlp reports YouTube's rate limit as lasting up to an hour, and a
	// read sent into a refusal is what turns it into a block.
	RateLimitPause = 24 * time.Hour
	// FailingPause is how long enrichment makes no read after
	// MaxConsecutiveFailures reads in a row failed.
	FailingPause = time.Hour
	// idleWait is how long enrichment waits to look again when no video is due a
	// read.
	idleWait = 10 * time.Minute
	// readTimeout bounds one read, whose requests yt-dlp spaces a second apart.
	readTimeout = 2 * time.Minute
	// firstRetry is how long a video whose read stored no tracklist waits before
	// it is read again, doubling with each attempt up to lastRetry.
	firstRetry = 6 * time.Hour
	lastRetry  = 7 * 24 * time.Hour
	// MaxAttempts is how many times one video is read before enrichment stops
	// reading it. At the waits above that reaches about a fortnight past the
	// first read, which is long enough for a tracklist comment to be posted.
	MaxAttempts = 6
	// MaxConsecutiveFailures is how many reads in a row may fail before
	// enrichment pauses. A read sent into a refusal is what turns a pause into a
	// block, and only refusals worded the way ytdlp's markers spell them pause it
	// on their own.
	MaxConsecutiveFailures = 3
)

// verdict is what a read that stored no tracklist means for reading the video
// again.
type verdict int

const (
	// readAgain holds the video back until its retry time, and stops the reading
	// once it has been read MaxAttempts times.
	readAgain verdict = iota
	// holdForGood stops reading the video, since YouTube answered that no
	// signed-out session will ever read it.
	holdForGood
)

// Reader reads a video, as *ytdlp.Reader does.
type Reader interface {
	Video(ctx context.Context, id string) (ytdlp.Video, error)
}

// Limits bound how fast enrichment reads.
type Limits struct {
	// Pace is the least time between two reads. Each wait adds up to Pace again
	// at random, so the reads keep no rhythm.
	Pace time.Duration
}

// Enricher reads tracklists into one store through one reader.
type Enricher struct {
	store  *store.Store
	reader Reader
	limits Limits
	tally  *Tally
	// failedInARow is how many reads in a row have failed.
	failedInARow int
	now          func() time.Time
	// wait waits for d, or until ctx ends with ctx's error.
	wait func(ctx context.Context, d time.Duration) error
}

// New is an Enricher reading from st through reader within limits, adding what
// it does to tally.
func New(st *store.Store, reader Reader, limits Limits, tally *Tally) *Enricher {
	return &Enricher{store: st, reader: reader, limits: limits, tally: tally, now: time.Now, wait: sleep}
}

// Report is what reads did: the reads made, the videos a tracklist search was
// stored for and the tracks those held, the videos found closed to reading for
// good, whether YouTube refused a read for now, whether reading is paused
// because an earlier refusal still holds, and each read that failed in a way
// the reading is accountable for.
//
// Paused is not a failure. A pause is enrichment doing what a refusal asks of
// it, and recording it as a failure would make every pass for a day partial.
type Report struct {
	Reads       int
	Enriched    int
	Tracks      int
	Unreadable  int
	RateLimited bool
	Paused      bool
	Failures    []Failure
}

// Failure is a read that did not store a tracklist search. VideoID is empty for
// a failure of the reading itself.
type Failure struct {
	VideoID string
	Err     error
}

// Tally is what enrichment did since the sync last took it. It is safe for the
// reading and the sync to use at once.
type Tally struct {
	mu     sync.Mutex
	report Report
	paused bool
	// refused is signaled when a read is refused, so the sync records the
	// refusal without waiting for its next pass.
	refused chan struct{}
}

// NewTally is a Tally holding nothing.
func NewTally() *Tally {
	return &Tally{refused: make(chan struct{}, 1)}
}

// Take is what enrichment did since the last Take, and whether it is paused on
// a refusal now.
func (t *Tally) Take() Report {
	if t == nil {
		return Report{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	taken := t.report
	taken.Paused = taken.Paused || t.paused
	t.report = Report{}
	return taken
}

// Refused is signaled each time YouTube refuses a read for its rate limit.
func (t *Tally) Refused() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.refused
}

func (t *Tally) add(r Report) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.report.Reads += r.Reads
	t.report.Enriched += r.Enriched
	t.report.Tracks += r.Tracks
	t.report.Unreadable += r.Unreadable
	t.report.RateLimited = t.report.RateLimited || r.RateLimited
	t.report.Failures = append(t.report.Failures, r.Failures...)
	if r.RateLimited {
		select {
		case t.refused <- struct{}{}:
		default:
		}
	}
}

func (t *Tally) setPaused(paused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = paused
	// A pause that began and ended between two takes still happened.
	t.report.Paused = t.report.Paused || paused
}

// Run reads tracklists until ctx ends, one video at a time, and returns ctx's
// error. A failure of the store is recorded like a failed read and pauses the
// reading, rather than ending it.
func (e *Enricher) Run(ctx context.Context) error {
	for {
		if err := e.wait(ctx, e.step(ctx)); err != nil {
			return err
		}
	}
}

// step makes at most one read, adds what it did to the tally, and returns how
// long to wait before the next.
func (e *Enricher) step(ctx context.Context) time.Duration {
	now := e.now()
	until, err := e.pausedUntil(ctx, now)
	if err != nil {
		return e.failed(err)
	}
	if until.After(now) {
		e.tally.setPaused(true)
		return until.Sub(now)
	}
	e.tally.setPaused(false)
	ids, err := e.store.Queries.ListVideosToEnrich(ctx, generated.ListVideosToEnrichParams{
		Now:       sql.NullString{String: store.Timestamp(now), Valid: true},
		MaxVideos: 1,
	})
	switch {
	case ctx.Err() != nil:
		return 0
	case err != nil:
		return e.failed(fmt.Errorf("list the videos to enrich: %w", err))
	case len(ids) == 0:
		return idleWait
	}
	report, err := e.read(ctx, ids[0])
	if err == nil && report.Tracks > 0 {
		err = e.store.Rederive(ctx)
	}
	e.tally.add(report)
	switch {
	case ctx.Err() != nil:
		return 0
	case err != nil:
		return e.failed(err)
	case report.RateLimited:
		e.tally.setPaused(true)
		return RateLimitPause
	case e.failedInARow >= MaxConsecutiveFailures:
		e.tally.add(Report{Failures: []Failure{{Err: fmt.Errorf("%w: %d in a row", ErrReadsFailing, e.failedInARow)}}})
		e.failedInARow = 0
		return FailingPause
	}
	return e.limits.Pace + time.Duration(rand.Int64N(int64(e.limits.Pace)+1))
}

// failed records a failure of the reading itself and pauses it.
func (e *Enricher) failed(err error) time.Duration {
	e.tally.add(Report{Failures: []Failure{{Err: err}}})
	return FailingPause
}

// pausedUntil is when the pause on YouTube's latest refusal ends, which is in
// the past when no refusal holds.
func (e *Enricher) pausedUntil(ctx context.Context, now time.Time) (time.Time, error) {
	finished, err := e.store.Queries.LatestRateLimitedRunFinished(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read when YouTube last refused a read: %w", err)
	}
	if finished == "" {
		return now, nil
	}
	refused, err := time.Parse(time.RFC3339, finished)
	if err != nil {
		return time.Time{}, fmt.Errorf("read when YouTube last refused a read, %q: %w", finished, err)
	}
	return refused.Add(RateLimitPause), nil
}

// read reads the video id and stores what it finds. The error is a failure of
// the store, or ctx's once it ends.
func (e *Enricher) read(ctx context.Context, id string) (Report, error) {
	report := Report{Reads: 1}
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	video, err := e.reader.Video(readCtx, id)
	cancel()
	switch {
	case ctx.Err() != nil:
		return report, ctx.Err()
	case errors.Is(err, ytdlp.ErrRateLimited):
		report.RateLimited = true
		report.Failures = append(report.Failures, Failure{VideoID: id, Err: err})
	case errors.Is(err, ytdlp.ErrUnreadable):
		if err := e.recordFailure(ctx, id, err, holdForGood); err != nil {
			return report, err
		}
		report.Unreadable++
		e.failedInARow = 0
	case err != nil:
		if err := e.recordFailure(ctx, id, err, readAgain); err != nil {
			return report, err
		}
		report.Failures = append(report.Failures, Failure{VideoID: id, Err: err})
		e.failedInARow++
	default:
		tracks, err := e.storeVideo(ctx, video)
		if err != nil {
			return report, err
		}
		report.Enriched++
		report.Tracks += tracks
		e.failedInARow = 0
	}
	return report, nil
}

// storeVideo stores what the read of video reports and the tracklist it makes,
// replacing any tracks the video held, and clears the mark holding it back. A
// read that makes no tracklist leaves the video's tracks alone and marks it to
// be read again, since a video may hold tracks this package did not read and a
// tracklist may be posted after the video is. It returns how many tracks it
// stored.
func (e *Enricher) storeVideo(ctx context.Context, video ytdlp.Video) (int, error) {
	tracks := tracklist.Best(video.Chapters, video.DurationSeconds, video.Description, video.Comments)
	now := e.now()
	err := e.store.InTx(ctx, func(tx *store.Tx) error {
		updated, err := tx.SetVideoEnrichment(ctx, generated.SetVideoEnrichmentParams{
			VideoID:         video.ID,
			DurationSeconds: sql.NullInt64{Int64: video.DurationSeconds, Valid: video.DurationSeconds > 0},
			Description:     sql.NullString{String: video.Description, Valid: true},
			UploadDate:      sql.NullString{String: video.UploadDate, Valid: video.UploadDate != ""},
			EnrichedTs:      sql.NullString{String: store.Timestamp(now), Valid: true},
		})
		switch {
		case err != nil:
			return err
		case updated != 1:
			return fmt.Errorf("the store holds no video %s", video.ID)
		}
		if len(tracks) == 0 {
			return holdBack(ctx, tx, video.ID, noTracklist(video), readAgain, now)
		}
		rows := make([]generated.InsertTrackParams, len(tracks))
		for i, track := range tracks {
			rows[i] = generated.InsertTrackParams{
				Position:     track.Position,
				StartSeconds: sql.NullInt64{Int64: track.StartSeconds, Valid: true},
				EndSeconds:   sql.NullInt64{Int64: track.EndSeconds, Valid: track.HasEnd},
				Artist:       sql.NullString{String: track.Artist, Valid: track.Artist != ""},
				Title:        track.Title,
				RawText:      track.RawText,
				Source:       string(track.Source),
			}
		}
		if err := tx.ReplaceTracks(ctx, video.ID, rows); err != nil {
			return err
		}
		return tx.DeleteEnrichFailure(ctx, video.ID)
	})
	if err != nil {
		return 0, fmt.Errorf("store the enrichment of %s: %w", video.ID, err)
	}
	return len(tracks), nil
}

// noTracklist says what a read of video found instead of a tracklist, so a
// stored reason distinguishes a video that carries none from one whose comments
// were capped or switched off.
func noTracklist(video ytdlp.Video) error {
	switch {
	case video.Comments == nil:
		return errors.New("the read found no tracklist, and the video returned no comments")
	case video.CommentsCapped:
		return fmt.Errorf("the read found no tracklist in the video's description or its top %d comments, which is all of them it returned", len(video.Comments))
	}
	return fmt.Errorf("the read found no tracklist in the video's description or its %d comments", len(video.Comments))
}

// recordFailure records that the read of the video id did not store a tracklist
// because of cause, in its own transaction.
func (e *Enricher) recordFailure(ctx context.Context, id string, cause error, decided verdict) error {
	now := e.now()
	err := e.store.InTx(ctx, func(tx *store.Tx) error {
		return holdBack(ctx, tx, id, cause, decided, now)
	})
	if err != nil {
		return fmt.Errorf("record the failed read of %s: %w", id, err)
	}
	return nil
}

// holdBack holds the video id back from the queue, counting the read against
// the video's attempts. A video read again waits retryAfter its attempts so
// far, and one read MaxAttempts times or held for good waits for
// api/cmd/reset-enrichment.
func holdBack(ctx context.Context, tx *store.Tx, id string, cause error, decided verdict, now time.Time) error {
	attempts := int64(1)
	previous, err := tx.GetEnrichFailure(ctx, id)
	switch {
	case err == nil:
		attempts = previous.Attempts + 1
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	failure := generated.UpsertEnrichFailureParams{VideoID: id, AttemptedTs: store.Timestamp(now), Reason: cause.Error(), Attempts: attempts}
	if decided == readAgain && attempts < MaxAttempts {
		failure.RetryTs = sql.NullString{String: store.Timestamp(now.Add(retryAfter(attempts))), Valid: true}
	}
	return tx.UpsertEnrichFailure(ctx, failure)
}

// retryAfter is how long a video waits to be read again after its attempts-th
// read stored no tracklist.
func retryAfter(attempts int64) time.Duration {
	wait := firstRetry
	for range attempts - 1 {
		wait = min(wait*2, lastRetry)
	}
	return wait
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
