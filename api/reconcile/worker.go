package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/datapointchris/ypl/api/store"
)

// Worker makes a run as it starts and another each interval after the last one
// ends, until its context ends. Runs never overlap.
type Worker struct {
	runner   *Runner
	interval time.Duration
	log      *slog.Logger
}

// NewWorker is a Worker making runner's runs every interval, logging each to
// log.
func NewWorker(runner *Runner, interval time.Duration, log *slog.Logger) *Worker {
	return &Worker{runner: runner, interval: interval, log: log}
}

// Run makes runs until ctx ends. A run in progress when ctx ends stops before
// its next playlist, and is recorded as canceled before Run returns.
func (w *Worker) Run(ctx context.Context) {
	for {
		report, err := w.runner.Run(ctx)
		if err != nil {
			w.log.Error("sync run not recorded", "err", err, "outcome", report.Outcome)
		} else {
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
				"video_reads", report.VideoReads,
				"videos_enriched", report.VideosEnriched,
				"tracks_found", report.TracksFound,
				"videos_unreadable", report.VideosUnreadable,
				"rate_limited", report.RateLimited,
				"failures", len(report.Failures),
			)
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// levelOf is the level a run with outcome is logged at, so a filter on level
// finds every run that did not sync every playlist.
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
