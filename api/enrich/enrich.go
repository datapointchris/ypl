// Package enrich reads a tracklist for each video the channel's playlists hold
// that has none: the video's chapters, or the timestamped lines of its
// description or of one of its top comments.
//
// A read goes through yt-dlp, signed in as nobody, and YouTube throttles an
// address that reads too much too fast. So a run reads a bounded number of
// videos with a paced, jittered gap between reads, within a wall-clock budget,
// stops at YouTube's rate limit, and makes no read for RateLimitPause after
// one. A run also stops after MaxConsecutiveFailures reads fail, whatever they
// failed with, since a refusal worded in a way this package does not recognize
// would otherwise spend a whole batch against it.
//
// A read that stores no tracklist is recorded like one that failed, and the
// video is read again after a widening wait up to MaxAttempts times. A
// tracklist is usually posted as a comment some time after the video is, and
// the queue reaches a video within a run of it entering a playlist. A video
// YouTube answered that no signed-out read will ever return is not read again
// at all, and api/cmd/reset-enrichment is what puts one back in the queue.
package enrich

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/tracklist"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// ErrReadsFailing ends a run whose reads failed MaxConsecutiveFailures times
// over, which says something is wrong with the reading rather than with any one
// video.
var ErrReadsFailing = errors.New("enrichment stopped after consecutive reads failed")

// ErrBudgetSpent ends a run whose reads took its whole budget, which happens
// when reads are reaching their timeout rather than answering.
var ErrBudgetSpent = errors.New("enrichment stopped after spending its budget")

const (
	// RateLimitPause is how long enrichment makes no read after YouTube refuses
	// one. yt-dlp reports YouTube's rate limit as lasting up to an hour, and a
	// read sent into a refusal is what turns it into a block.
	RateLimitPause = 24 * time.Hour
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
	// MaxConsecutiveFailures is how many reads in a row may fail before a run
	// stops reading. A read sent into a refusal is what turns a pause into a
	// block, and only refusals worded the way ytdlp's markers spell them stop a
	// run on their own.
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

// Limits bound what one run's enrichment does.
type Limits struct {
	// Pace is the least time between two reads. A run waits up to half as long
	// again on top of it at random, so its reads keep no rhythm.
	Pace time.Duration
	// Batch is the most videos a run reads.
	Batch int
	// Budget is the longest a run spends reading. It is what keeps enrichment
	// from setting the period of the sync it runs inside, since a run whose
	// reads reach their timeout takes far longer than its pace predicts.
	Budget time.Duration
}

// Enricher reads tracklists into one store through one reader.
type Enricher struct {
	store  *store.Store
	reader Reader
	limits Limits
	// jitter is the most added to a wait between reads at random.
	jitter time.Duration
	now    func() time.Time
	// wait waits for d, or until ctx ends with ctx's error.
	wait func(ctx context.Context, d time.Duration) error
}

// New is an Enricher reading from st through reader within limits. reader may
// be nil where limits.Batch is 0, since a run then makes no read.
func New(st *store.Store, reader Reader, limits Limits) *Enricher {
	return &Enricher{store: st, reader: reader, limits: limits, jitter: limits.Pace / 2, now: time.Now, wait: sleep}
}

// Report is what one run's enrichment did: the reads it made, the videos it
// stored a tracklist search for and the tracks those held, the videos it found
// closed to reading for good, whether YouTube refused its reads for now,
// whether it made no read because an earlier refusal still holds, and each read
// that failed in a way the run is accountable for.
//
// Paused is not a failure. A pause is enrichment doing what a refusal asks of
// it, and a run that records it as a failure makes every run for a day partial.
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
// a failure of the whole run's enrichment.
type Failure struct {
	VideoID string
	Err     error
}

// Run reads the videos waiting for a tracklist, newest in a playlist first, up
// to the batch and within the budget. The error is a failure of the store, or
// ctx's once it ends.
func (e *Enricher) Run(ctx context.Context) (Report, error) {
	var report Report
	if e.limits.Batch <= 0 {
		return report, nil
	}
	began := e.now()
	refused, err := e.store.Queries.CountRateLimitedRunsSince(ctx, store.Timestamp(began.Add(-RateLimitPause)))
	if err != nil {
		return report, fmt.Errorf("count the runs YouTube refused reads to: %w", err)
	}
	if refused > 0 {
		report.Paused = true
		return report, nil
	}
	ids, err := e.store.Queries.ListVideosToEnrich(ctx, generated.ListVideosToEnrichParams{
		Now:       sql.NullString{String: store.Timestamp(began), Valid: true},
		MaxVideos: int64(e.limits.Batch),
	})
	if err != nil {
		return report, fmt.Errorf("list the videos to enrich: %w", err)
	}
	failedInARow := 0
	for i, id := range ids {
		if i > 0 {
			if err := e.wait(ctx, e.limits.Pace+time.Duration(rand.Int64N(int64(e.jitter)+1))); err != nil {
				return report, err
			}
		}
		if spent := e.now().Sub(began); spent >= e.limits.Budget {
			report.Failures = append(report.Failures, Failure{
				Err: fmt.Errorf("%w of %v after %d of %d videos", ErrBudgetSpent, e.limits.Budget, i, len(ids)),
			})
			return report, nil
		}
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		video, err := e.reader.Video(readCtx, id)
		cancel()
		report.Reads++
		switch {
		case ctx.Err() != nil:
			return report, ctx.Err()
		case errors.Is(err, ytdlp.ErrRateLimited):
			report.RateLimited = true
			report.Failures = append(report.Failures, Failure{VideoID: id, Err: err})
			return report, nil
		case errors.Is(err, ytdlp.ErrUnreadable):
			if err := e.recordFailure(ctx, id, err, holdForGood); err != nil {
				return report, err
			}
			report.Unreadable++
			failedInARow = 0
		case err != nil:
			if err := e.recordFailure(ctx, id, err, readAgain); err != nil {
				return report, err
			}
			report.Failures = append(report.Failures, Failure{VideoID: id, Err: err})
			failedInARow++
			if failedInARow >= MaxConsecutiveFailures {
				report.Failures = append(report.Failures, Failure{
					Err: fmt.Errorf("%w: %d in a row", ErrReadsFailing, failedInARow),
				})
				return report, nil
			}
		default:
			tracks, err := e.storeVideo(ctx, video)
			if err != nil {
				return report, err
			}
			report.Enriched++
			report.Tracks += tracks
			failedInARow = 0
		}
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
