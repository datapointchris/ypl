package reconcile

import (
	"context"
	"log/slog"
	"time"
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

// Run makes runs until ctx ends. A run in progress when ctx ends stops between
// writes and is recorded as canceled before Run returns.
func (w *Worker) Run(ctx context.Context) {
	for {
		report, err := w.runner.Run(ctx)
		if err != nil {
			w.log.Error("sync run not recorded", "err", err, "outcome", report.Outcome)
		} else {
			w.log.Info("sync run",
				"run_id", report.RunID,
				"outcome", report.Outcome,
				"playlists", report.Playlists,
				"deleted", report.PlaylistsDeleted,
				"skipped", report.PlaylistsSkipped,
				"pulled_in", report.PulledIn,
				"pulled_out", report.PulledOut,
				"writes", report.Writes,
				"requests", report.Requests,
				"read_units", report.ReadUnits,
				"write_units", report.WriteUnits,
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
