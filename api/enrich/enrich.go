// Package enrich reads a tracklist for each video the channel's playlists hold
// that enrichment has not read: the video's chapters, or the timestamped lines
// of its description or of one of its top comments.
//
// A read goes through yt-dlp, signed in as nobody, and YouTube throttles an
// address that reads too much too fast. So a run reads a bounded number of
// videos with a paced, jittered gap between reads, stops at YouTube's rate
// limit, and makes no read for RateLimitPause after one. A video whose read
// fails is not read again until its failure's retry time, and never when
// YouTube answered that no signed-out read will return it.
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

// ErrPaused is the failure of a run that read no video because YouTube refused
// a read of this address within RateLimitPause.
var ErrPaused = errors.New("enrichment is paused after YouTube refused a read from this address")

const (
	// RateLimitPause is how long enrichment makes no read after YouTube refuses
	// one. yt-dlp reports YouTube's rate limit as lasting up to an hour, and a
	// read sent into a refusal is what turns it into a block.
	RateLimitPause = 24 * time.Hour
	// readTimeout bounds one read, whose requests yt-dlp spaces a second apart.
	readTimeout = 2 * time.Minute
	// firstRetry is how long a video whose read failed waits before it is read
	// again, doubling with each failure up to lastRetry.
	firstRetry = 6 * time.Hour
	lastRetry  = 7 * 24 * time.Hour
)

// Reader reads a video, as *ytdlp.Reader does.
type Reader interface {
	Video(ctx context.Context, id string) (ytdlp.Video, error)
}

// Enricher reads tracklists into one store through one reader.
type Enricher struct {
	store  *store.Store
	reader Reader
	// pace is the least time between two reads, jitter the most added to it at
	// random so reads keep no rhythm, and batch the most videos a run reads.
	pace, jitter time.Duration
	batch        int
	now          func() time.Time
	// wait waits for d, or until ctx ends with ctx's error.
	wait func(ctx context.Context, d time.Duration) error
}

// New is an Enricher reading at most batch videos a run from st through reader,
// pace or up to half as long again apart.
func New(st *store.Store, reader Reader, pace time.Duration, batch int) *Enricher {
	return &Enricher{store: st, reader: reader, pace: pace, jitter: pace / 2, batch: batch, now: time.Now, wait: sleep}
}

// Report is what one run's enrichment did: the reads it made, the videos it
// stored a tracklist search for and the tracks those held, the videos it found
// closed to reading for good, whether YouTube refused its reads for now, and
// each video whose read failed otherwise.
type Report struct {
	Reads       int
	Enriched    int
	Tracks      int
	Unreadable  int
	RateLimited bool
	Failures    []Failure
}

// Failure is a read that did not store a tracklist search. VideoID is empty for
// a failure of the whole run's enrichment.
type Failure struct {
	VideoID string
	Err     error
}

// Run reads the videos waiting for enrichment, newest in a playlist first, up to
// the batch. The error is a failure of the store, or ctx's once it ends.
func (e *Enricher) Run(ctx context.Context) (Report, error) {
	var report Report
	now := e.now()
	refused, err := e.store.Queries.CountRateLimitedRunsSince(ctx, store.Timestamp(now.Add(-RateLimitPause)))
	if err != nil {
		return report, fmt.Errorf("count the runs YouTube refused reads to: %w", err)
	}
	if refused > 0 {
		report.Failures = append(report.Failures, Failure{Err: fmt.Errorf("%w within %v", ErrPaused, RateLimitPause)})
		return report, nil
	}
	ids, err := e.store.Queries.ListVideosToEnrich(ctx, generated.ListVideosToEnrichParams{
		Now:       sql.NullString{String: store.Timestamp(now), Valid: true},
		MaxVideos: int64(e.batch),
	})
	if err != nil {
		return report, fmt.Errorf("list the videos to enrich: %w", err)
	}
	for i, id := range ids {
		if i > 0 {
			if err := e.wait(ctx, e.pace+time.Duration(rand.Int64N(int64(e.jitter)+1))); err != nil {
				return report, err
			}
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
			if err := e.recordFailure(ctx, id, err, false); err != nil {
				return report, err
			}
			report.Unreadable++
		case err != nil:
			if err := e.recordFailure(ctx, id, err, true); err != nil {
				return report, err
			}
			report.Failures = append(report.Failures, Failure{VideoID: id, Err: err})
		default:
			tracks, err := e.storeVideo(ctx, video)
			if err != nil {
				return report, err
			}
			report.Enriched++
			report.Tracks += tracks
		}
	}
	return report, nil
}

// storeVideo stores what the read of video reports and the tracklist it makes,
// replacing any tracks the video held, and clears a failure of its read. It
// returns how many tracks it stored.
func (e *Enricher) storeVideo(ctx context.Context, video ytdlp.Video) (int, error) {
	tracks := tracklist.Best(video.Chapters, video.Description, video.Comments)
	stored := 0
	err := e.store.InTx(ctx, func(tx *store.Tx) error {
		updated, err := tx.SetVideoEnrichment(ctx, generated.SetVideoEnrichmentParams{
			VideoID:         video.ID,
			DurationSeconds: sql.NullInt64{Int64: video.DurationSeconds, Valid: video.DurationSeconds > 0},
			Description:     sql.NullString{String: video.Description, Valid: true},
			UploadDate:      sql.NullString{String: video.UploadDate, Valid: video.UploadDate != ""},
			EnrichedTs:      sql.NullString{String: store.Timestamp(e.now()), Valid: true},
		})
		switch {
		case err != nil:
			return err
		case updated != 1:
			return fmt.Errorf("the store holds no video %s", video.ID)
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
				Source:       track.Source,
			}
		}
		if err := tx.ReplaceTracks(ctx, video.ID, rows); err != nil {
			return err
		}
		stored = len(rows)
		return tx.DeleteEnrichFailure(ctx, video.ID)
	})
	if err != nil {
		return 0, fmt.Errorf("store the enrichment of %s: %w", video.ID, err)
	}
	return stored, nil
}

// recordFailure records that the read of the video id failed with cause. A
// failure retried waits firstRetry, doubled for each failure the video had,
// up to lastRetry, and one not retried holds the video back for good.
func (e *Enricher) recordFailure(ctx context.Context, id string, cause error, retried bool) error {
	now := e.now()
	err := e.store.InTx(ctx, func(tx *store.Tx) error {
		attempts := int64(1)
		previous, err := tx.GetEnrichFailure(ctx, id)
		switch {
		case err == nil:
			attempts = previous.Attempts + 1
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		failure := generated.UpsertEnrichFailureParams{VideoID: id, AttemptedTs: store.Timestamp(now), Reason: cause.Error(), Attempts: attempts}
		if retried {
			failure.RetryTs = sql.NullString{String: store.Timestamp(now.Add(retryAfter(attempts))), Valid: true}
		}
		return tx.UpsertEnrichFailure(ctx, failure)
	})
	if err != nil {
		return fmt.Errorf("record the failed read of %s: %w", id, err)
	}
	return nil
}

// retryAfter is how long a video waits to be read again after its attempts-th
// failed read.
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
