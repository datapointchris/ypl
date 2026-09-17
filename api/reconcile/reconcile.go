// Package reconcile syncs the channel's playlists into the store. A run reads
// every playlist the channel owns and every item in each, and stores them as
// YouTube holds them. Every run leaves a sync_runs row saying how it ended and
// what it did.
//
// The API writes playlists too, and a read can predate a write YouTube has
// answered. So a run changes nothing a read could not yet show: it keeps the
// details of a playlist the API wrote after a read began, keeps a playlist the
// API created, and does not restore one the API deleted. The next run, whose
// reads follow the write, stores what YouTube holds.
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// ErrReadsExceedQuota is the failure of a run whose interval, at this run's
// read cost, makes more reads a day than the day's quota allows. Such a day's
// later runs draw YouTube's quota refusal.
var ErrReadsExceedQuota = errors.New("a day of runs at this interval reads more than the day's quota")

// DailyQuota is the units YouTube allows the Cloud project each Pacific day.
const DailyQuota = 10_000

// Channel is what a run reads YouTube through, as youtube.Channel does.
type Channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error)
	Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error)
	Requests() int64
	Units() int64
}

// Runner makes sync runs against one store and one channel, one at a time.
type Runner struct {
	store   *store.Store
	channel Channel
	// runsPerDay is how many runs a day the worker makes at its interval.
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
	ItemsAdded       int
	ItemsRemoved     int
	Requests         int64
	Units            int64
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
	run.date = youtube.QuotaDate(run.started)
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
	refused, err := run.store.Queries.CountQuotaSpentRuns(run.ctx, run.date)
	if err != nil {
		run.ended = err
		return
	}
	if refused > 0 {
		run.ended = fmt.Errorf("%w: a run on %s already drew the refusal", youtube.ErrQuotaSpent, run.date)
		return
	}

	listedAt := run.now()
	listed, err := run.channel.Playlists(run.ctx)
	if err != nil {
		run.ended = err
		return
	}
	if err := run.deleteUnlisted(listed); err != nil {
		run.ended = err
		return
	}
	for _, playlist := range listed {
		if err := run.storePlaylist(playlist, listedAt); err != nil {
			run.ended = err
			return
		}
	}

	units := run.channel.Units() - run.units0
	if run.runsPerDay*units > DailyQuota {
		run.report.Failures = append(run.report.Failures, Failure{
			Err: fmt.Errorf("%w: %d runs a day at %d units each", ErrReadsExceedQuota, run.runsPerDay, units),
		})
	}
}

// deleteUnlisted deletes each stored playlist absent from listed that a read by
// its id finds gone. A listing is not a snapshot, so a playlist absent from one
// is not known to be gone until that read says so.
func (run *run) deleteUnlisted(listed []youtube.Playlist) error {
	stored, err := run.store.Queries.ListPlaylistIDs(run.ctx)
	if err != nil {
		return err
	}
	for _, id := range stored {
		if slices.ContainsFunc(listed, func(p youtube.Playlist) bool { return string(p.ID) == id }) {
			continue
		}
		if err := run.ctx.Err(); err != nil {
			return err
		}
		readAt := run.now()
		_, err := run.channel.Playlist(run.ctx, youtube.PlaylistID(id))
		switch {
		case err == nil:
			// YouTube still has the playlist, so the listing missed it, and the
			// next run's listing is read instead.
		case errors.Is(err, youtube.ErrPlaylistNotFound):
			if err := run.deleteGone(id, readAt); err != nil {
				return err
			}
		case errors.Is(err, youtube.ErrQuotaSpent) || run.ctx.Err() != nil:
			return err
		default:
			run.report.PlaylistsSkipped++
			run.report.Failures = append(run.report.Failures, Failure{Playlist: youtube.PlaylistID(id), Err: err})
		}
	}
	return nil
}

// deleteGone deletes the stored playlist id, which a read sent at readAt found
// gone, unless the API wrote it later than that read can show.
func (run *run) deleteGone(id string, readAt time.Time) error {
	deleted := false
	err := run.store.InTx(run.ctx, func(tx *store.Tx) error {
		_, written, err := store.WriteNewerThanRead(run.ctx, tx.Queries, id, readAt)
		if err != nil || written {
			return err
		}
		deleted = true
		return tx.DeletePlaylist(run.ctx, id)
	})
	if err != nil {
		return err
	}
	if deleted {
		run.report.PlaylistsDeleted++
	}
	return nil
}

// storePlaylist reads one listed playlist's items and stores the playlist as
// YouTube holds it, in one transaction. listedAt is when the listing that named
// it was sent. A failure about this playlist alone is recorded and returns nil;
// the error is for one that ends the run.
func (run *run) storePlaylist(playlist youtube.Playlist, listedAt time.Time) error {
	if err := run.ctx.Err(); err != nil {
		return err
	}
	id := string(playlist.ID)
	itemsAt := run.now()
	items, err := run.channel.Items(run.ctx, playlist.ID)
	switch {
	case errors.Is(err, youtube.ErrPlaylistNotFound):
		// The playlist was deleted after the channel listed it, or created so
		// recently that YouTube does not yet list its items.
		return run.deleteGone(id, itemsAt)
	case err != nil:
		if errors.Is(err, youtube.ErrQuotaSpent) || run.ctx.Err() != nil {
			return err
		}
		run.report.PlaylistsSkipped++
		run.report.Failures = append(run.report.Failures, Failure{Playlist: playlist.ID, Err: err})
		return nil
	}

	var added, removed int
	stored := false
	err = run.store.InTx(run.ctx, func(tx *store.Tx) error {
		method, written, err := store.WriteNewerThanRead(run.ctx, tx.Queries, id, listedAt)
		switch {
		case err != nil:
			return err
		case written && method == youtube.MethodPlaylistsDelete:
			// The API deleted the playlist later than the listing can show.
			return nil
		case !written:
			err := tx.UpsertPlaylist(run.ctx, generated.UpsertPlaylistParams{
				PlaylistID: id, Title: playlist.Title, Description: playlist.Description, Privacy: playlist.Privacy,
			})
			if err != nil {
				return err
			}
		}
		// A playlist the API created or updated later than the listing can show
		// keeps the details the API stored.
		stored = true
		held, err := tx.ListPlaylistItems(run.ctx, id)
		if err != nil {
			return err
		}
		read := make([]store.PlaylistItem, len(items))
		for i, item := range items {
			read[i] = store.PlaylistItem{ItemID: string(item.ID), VideoID: string(item.VideoID)}
			if err := upsertVideo(run.ctx, tx, item); err != nil {
				return err
			}
			if !slices.ContainsFunc(held, func(row generated.ListPlaylistItemsRow) bool { return row.ItemID == string(item.ID) }) {
				added++
			}
		}
		for _, row := range held {
			if !slices.ContainsFunc(items, func(item youtube.Item) bool { return string(item.ID) == row.ItemID }) {
				removed++
			}
		}
		return tx.ReplacePlaylistItems(run.ctx, id, read)
	})
	if err != nil || !stored {
		return err
	}
	run.report.Playlists++
	run.report.ItemsAdded += added
	run.report.ItemsRemoved += removed
	return nil
}

func upsertVideo(ctx context.Context, tx *store.Tx, item youtube.Item) error {
	if item.Unavailable {
		return tx.UpsertUnavailableVideo(ctx, generated.UpsertUnavailableVideoParams{VideoID: string(item.VideoID), Title: item.Title})
	}
	return tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{
		VideoID: string(item.VideoID), Title: item.Title, ChannelTitle: item.ChannelTitle,
	})
}

// record writes the run and its failures, on a context the run's own ending
// does not cancel.
func (run *run) record() (Report, error) {
	rep := &run.report
	rep.Requests = run.channel.Requests() - run.requests0
	rep.Units = run.channel.Units() - run.units0
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
			StartedTs:        store.Timestamp(run.started),
			FinishedTs:       store.Timestamp(run.now()),
			QuotaDate:        run.date,
			Outcome:          rep.Outcome,
			Playlists:        int64(rep.Playlists),
			PlaylistsDeleted: int64(rep.PlaylistsDeleted),
			PlaylistsSkipped: int64(rep.PlaylistsSkipped),
			ItemsAdded:       int64(rep.ItemsAdded),
			ItemsRemoved:     int64(rep.ItemsRemoved),
			Requests:         rep.Requests,
			Units:            rep.Units,
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
