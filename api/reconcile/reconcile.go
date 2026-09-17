// Package reconcile syncs the channel's playlists with the store. A run reads
// every playlist from YouTube, merges each with the server's copy, and pushes
// the server's edits back within the day's write allowance. Every run leaves a
// sync_runs row saying how it ended and what it did.
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
	_ "time/tzdata" // the quota day is Pacific, whatever zone the host is in

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// ErrAbsenceNotConfirmed is the refusal to reconcile a playlist whose read left
// out an item YouTube still has when asked for it by id. A delete and an add
// between two page requests leave a read like that, and merging it would delete
// the item from the server.
var ErrAbsenceNotConfirmed = errors.New("an item missing from the playlist's read still exists")

// DailyQuota is the units YouTube allows the Cloud project each Pacific day.
const DailyQuota = 10_000

// Channel is what a run reads and edits YouTube through, as youtube.Channel
// does.
type Channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error)
	ExistingPlaylists(ctx context.Context, ids []youtube.PlaylistID) ([]youtube.PlaylistID, error)
	ExistingItems(ctx context.Context, ids []youtube.ItemID) ([]youtube.ItemID, error)
	InsertItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error)
	AppendItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID) (youtube.ItemID, error)
	MoveItem(ctx context.Context, item youtube.Item, position int64) error
	DeleteItem(ctx context.Context, id youtube.ItemID) error
	Requests() int64
	Units() int64
}

// Runner makes sync runs against one store and one channel, one at a time.
type Runner struct {
	store   *store.Store
	channel Channel
	// runsPerDay is how many runs a day the worker makes, and sizes the reserve
	// of units a day's writes leave for their reads.
	runsPerDay int64
	now        func() time.Time
}

// NewRunner is a Runner for runs every interval.
func NewRunner(st *store.Store, channel Channel, interval time.Duration) *Runner {
	return &Runner{
		store:      st,
		channel:    channel,
		runsPerDay: int64((24*time.Hour + interval - 1) / interval),
		now:        time.Now,
	}
}

// Report is what one run did, as its sync_runs row records it.
type Report struct {
	RunID            int64
	Outcome          string
	Playlists        int
	PlaylistsDeleted int
	PlaylistsSkipped int
	PulledIn         int
	PulledOut        int
	Writes           int
	Requests         int64
	ReadUnits        int64
	WriteUnits       int64
	Failures         []Failure
}

// Failure is one thing that went wrong in a run. Playlist is empty for a failure
// of the whole run.
type Failure struct {
	Playlist youtube.PlaylistID
	Err      error
}

// Run makes one run and records it. The error is only for a run that could not
// be recorded: whatever went wrong inside the run is in the report.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	run := &run{
		Runner:    r,
		ctx:       ctx,
		started:   r.now(),
		requests0: r.channel.Requests(),
		units0:    r.channel.Units(),
	}
	run.date = quotaDate(run.started)
	run.execute()
	return run.record()
}

// run is the state of one run while it is made.
type run struct {
	*Runner
	ctx       context.Context
	started   time.Time
	date      string
	requests0 int64
	units0    int64
	report    Report
	// ended is why the run stopped before its end: YouTube's quota refusal, the
	// context ending, or a failure of the whole run.
	ended error
}

func (run *run) execute() {
	q := run.store.Queries
	refused, err := q.CountQuotaSpentRuns(run.ctx, run.date)
	if err != nil {
		run.end(err)
		return
	}
	if refused > 0 {
		run.end(fmt.Errorf("%w: a run on %s already drew the refusal", youtube.ErrQuotaSpent, run.date))
		return
	}
	writeSpent, err := q.SumWriteUnits(run.ctx, run.date)
	if err != nil {
		run.end(err)
		return
	}

	listed, err := run.channel.Playlists(run.ctx)
	if err != nil {
		run.end(err)
		return
	}
	if err := run.deleteGone(listed); err != nil {
		run.end(err)
		return
	}
	var reconciled []youtube.PlaylistID
	for _, playlist := range listed {
		ok, err := run.reconcile(playlist)
		if err != nil {
			run.end(err)
			return
		}
		if ok {
			reconciled = append(reconciled, playlist.ID)
		}
	}

	allowance := DailyQuota - run.runsPerDay*run.readUnits() - writeSpent
	for _, id := range reconciled {
		if err := run.push(id, &allowance); err != nil {
			run.end(err)
			return
		}
	}
}

// deleteGone deletes each stored playlist absent from listed that YouTube no
// longer returns by id.
func (run *run) deleteGone(listed []youtube.Playlist) error {
	stored, err := run.store.Queries.ListPlaylistIDs(run.ctx)
	if err != nil {
		return err
	}
	var absent []youtube.PlaylistID
	for _, id := range stored {
		if !slices.ContainsFunc(listed, func(p youtube.Playlist) bool { return string(p.ID) == id }) {
			absent = append(absent, youtube.PlaylistID(id))
		}
	}
	if len(absent) == 0 {
		return nil
	}
	existing, err := run.channel.ExistingPlaylists(run.ctx, absent)
	if err != nil {
		return err
	}
	for _, id := range absent {
		if slices.Contains(existing, id) {
			continue
		}
		if err := run.store.Queries.DeletePlaylist(run.ctx, string(id)); err != nil {
			return err
		}
		run.report.PlaylistsDeleted++
	}
	return nil
}

// reconcile merges one listed playlist with the store, and reports whether it
// was merged. A failure about this playlist alone is recorded and returns
// false; the error is for one that ends the run.
func (run *run) reconcile(playlist youtube.Playlist) (bool, error) {
	if err := run.ctx.Err(); err != nil {
		return false, err
	}
	q := run.store.Queries
	id := string(playlist.ID)
	err := q.UpsertPlaylist(run.ctx, generated.UpsertPlaylistParams{
		PlaylistID: id, Title: playlist.Title, Description: playlist.Description, Privacy: playlist.Privacy,
	})
	if err != nil {
		return false, err
	}

	items, err := run.channel.Items(run.ctx, playlist.ID)
	switch {
	case errors.Is(err, youtube.ErrPlaylistNotFound):
		if err := q.DeletePlaylist(run.ctx, id); err != nil {
			return false, err
		}
		run.report.PlaylistsDeleted++
		return false, nil
	case err != nil:
		return false, run.skip(playlist.ID, err)
	}

	base, err := q.ListBaseItems(run.ctx, id)
	if err != nil {
		return false, err
	}
	local, err := q.ListPlaylistVideoIDs(run.ctx, id)
	if err != nil {
		return false, err
	}
	var absent []youtube.ItemID
	for _, row := range base {
		if !slices.ContainsFunc(items, func(it youtube.Item) bool { return string(it.ID) == row.ItemID }) {
			absent = append(absent, youtube.ItemID(row.ItemID))
		}
	}
	if len(absent) > 0 {
		still, err := run.channel.ExistingItems(run.ctx, absent)
		if err != nil {
			return false, run.skip(playlist.ID, err)
		}
		if len(still) > 0 {
			return false, run.skip(playlist.ID, fmt.Errorf("%w: %d of %d", ErrAbsenceNotConfirmed, len(still), len(absent)))
		}
	}

	remote := make([]string, len(items))
	read := make([]store.BaseItem, len(items))
	for i, item := range items {
		remote[i] = string(item.VideoID)
		read[i] = store.BaseItem{ItemID: string(item.ID), VideoID: string(item.VideoID)}
	}
	result := merge.Merge(baseVideoIDs(base), remote, local)
	err = run.store.InTx(run.ctx, func(tx *store.Tx) error {
		for _, item := range items {
			if err := upsertVideo(run.ctx, tx, item); err != nil {
				return err
			}
		}
		if err := tx.ReplacePlaylistItems(run.ctx, id, result.Order); err != nil {
			return err
		}
		return tx.ReplaceBaseItems(run.ctx, id, read)
	})
	if err != nil {
		return false, err
	}
	run.report.Playlists++
	run.report.PulledIn += len(result.PulledIn)
	run.report.PulledOut += len(result.PulledOut)
	return true, nil
}

func upsertVideo(ctx context.Context, tx *store.Tx, item youtube.Item) error {
	if item.Unavailable {
		return tx.UpsertUnavailableVideo(ctx, generated.UpsertUnavailableVideoParams{VideoID: string(item.VideoID), Title: item.Title})
	}
	return tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{
		VideoID: string(item.VideoID), Title: item.Title, ChannelTitle: item.ChannelTitle,
	})
}

// skip records err against the playlist, unless it ends the run, which it
// returns.
func (run *run) skip(playlist youtube.PlaylistID, err error) error {
	if endsRun(run.ctx, err) {
		return err
	}
	run.report.PlaylistsSkipped++
	run.report.Failures = append(run.report.Failures, Failure{Playlist: playlist, Err: err})
	return nil
}

// endsRun is whether err stops the whole run rather than one playlist's part in
// it: YouTube's quota refusal, or the run's context ending.
func endsRun(ctx context.Context, err error) bool {
	return errors.Is(err, youtube.ErrQuotaSpent) || ctx.Err() != nil
}

// end stops the run on err.
func (run *run) end(err error) {
	run.ended = err
}

func (run *run) readUnits() int64 {
	return run.channel.Units() - run.units0 - run.report.WriteUnits
}

// record writes the run and its failures, on a context the run's own ending
// does not cancel.
func (run *run) record() (Report, error) {
	rep := &run.report
	rep.Requests = run.channel.Requests() - run.requests0
	rep.ReadUnits = run.readUnits()
	switch {
	case errors.Is(run.ended, youtube.ErrQuotaSpent):
		rep.Outcome = store.OutcomeQuotaSpent
	case run.ended != nil && run.ctx.Err() != nil:
		rep.Outcome = store.OutcomeCanceled
	case run.ended != nil:
		rep.Outcome = store.OutcomeFailed
		rep.Failures = append(rep.Failures, Failure{Err: run.ended})
	case len(rep.Failures) > 0:
		rep.Outcome = store.OutcomePartial
	default:
		rep.Outcome = store.OutcomeOK
	}

	ctx := context.WithoutCancel(run.ctx)
	err := run.store.InTx(ctx, func(tx *store.Tx) error {
		id, err := tx.InsertSyncRun(ctx, generated.InsertSyncRunParams{
			StartedTs:        run.started.UTC().Format(time.RFC3339),
			FinishedTs:       run.now().UTC().Format(time.RFC3339),
			QuotaDate:        run.date,
			Outcome:          rep.Outcome,
			Playlists:        int64(rep.Playlists),
			PlaylistsDeleted: int64(rep.PlaylistsDeleted),
			PlaylistsSkipped: int64(rep.PlaylistsSkipped),
			PulledIn:         int64(rep.PulledIn),
			PulledOut:        int64(rep.PulledOut),
			Writes:           int64(rep.Writes),
			Requests:         rep.Requests,
			ReadUnits:        rep.ReadUnits,
			WriteUnits:       rep.WriteUnits,
		})
		if err != nil {
			return err
		}
		rep.RunID = id
		for _, failure := range rep.Failures {
			playlist := sql.NullString{String: string(failure.Playlist), Valid: failure.Playlist != ""}
			if err := tx.InsertSyncFailure(ctx, generated.InsertSyncFailureParams{RunID: id, PlaylistID: playlist, Error: failure.Err.Error()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return *rep, fmt.Errorf("record the sync run: %w", err)
	}
	return *rep, nil
}

func baseVideoIDs(base []generated.ListBaseItemsRow) []string {
	ids := make([]string, len(base))
	for i, row := range base {
		ids[i] = row.VideoID
	}
	return ids
}

// pacific is the zone YouTube's daily quota resets in.
var pacific = mustLoadLocation("America/Los_Angeles")

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("load %s from the embedded zone database: %v", name, err))
	}
	return location
}

// quotaDate is the Pacific date t's units count against.
func quotaDate(t time.Time) string {
	return t.In(pacific).Format(time.DateOnly)
}
