package reconcile

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// Worker makes a full pass as it starts, then a tick each interval give or
// take a fifth at random, and a pass at once whenever an edit arrives or
// YouTube refuses a read of a video. Passes never overlap.
//
// The jitter keeps the ticks from landing on a rhythm, the same as the reads
// of videos keep none.
type Worker struct {
	runner   *Runner
	interval time.Duration
	log      *slog.Logger
}

// NewWorker is a Worker making runner's passes, ticking every interval on
// average and logging each pass to log.
func NewWorker(runner *Runner, interval time.Duration, log *slog.Logger) *Worker {
	return &Worker{runner: runner, interval: interval, log: log}
}

// Run makes passes until ctx ends. A pass in progress when ctx ends stops
// before its next job, and is recorded as canceled before Run returns.
func (w *Worker) Run(ctx context.Context) {
	w.logged(ctx, w.runner.Run)
	timer := time.NewTimer(w.next())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.logged(ctx, w.runner.Tick)
			timer.Reset(w.next())
		case <-w.runner.edits.woken():
			w.logged(ctx, w.runner.Pass)
		case <-w.runner.tally.Refused():
			w.logged(ctx, w.runner.Pass)
		}
	}
}

// next is the wait before the next tick: the interval, give or take a fifth.
func (w *Worker) next() time.Duration {
	spread := int64(w.interval / 5)
	return w.interval - time.Duration(spread) + time.Duration(rand.Int64N(2*spread+1))
}

// logged makes a pass, logs it once recorded, and puts back each playlist an
// edit changed whose sync has to wait for YouTube to show a recent write, for
// a pass once it can.
func (w *Worker) logged(ctx context.Context, pass func(context.Context) (Report, error)) {
	report, err := pass(ctx)
	for _, id := range report.Retry {
		time.AfterFunc(youtube.ReadLag, func() { w.runner.edits.Edited(id) })
	}
	switch {
	case err != nil:
		w.log.Error("sync run not recorded", "err", err, "outcome", report.Outcome)
	case report.RunID != 0:
		w.log.Log(context.WithoutCancel(ctx), levelOf(report.Outcome), "sync run",
			"run_id", report.RunID,
			"outcome", report.Outcome,
			"playlists", report.Playlists,
			"deleted", report.PlaylistsDeleted,
			"skipped", report.PlaylistsSkipped,
			"deferred", report.PlaylistsDeferred,
			"items_added", report.ItemsAdded,
			"items_removed", report.ItemsRemoved,
			"writes", report.Writes,
			"requests", report.Requests,
			"units", report.Units,
			"write_units", report.WriteUnits,
			"probe_misses", report.ProbeMisses,
			"video_reads", report.VideoReads,
			"videos_enriched", report.VideosEnriched,
			"tracks_found", report.TracksFound,
			"videos_unreadable", report.VideosUnreadable,
			"rate_limited", report.RateLimited,
			"enrichment_paused", report.EnrichmentPaused,
			"failures", len(report.Failures),
		)
	}
}

// levelOf is the level a pass with outcome is logged at, so a filter on level
// finds every pass that did not sync every playlist it took up.
func levelOf(outcome string) slog.Level {
	switch outcome {
	case store.OutcomeOK:
		return slog.LevelInfo
	case store.OutcomeFailed:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
