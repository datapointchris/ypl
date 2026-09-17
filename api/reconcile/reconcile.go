// Package reconcile syncs the channel's playlists with the store both ways. A
// run reads every playlist the channel owns and every item in each, merges each
// read into the server's order of the playlist against the playlist's base, and
// then pushes the server's order of every merged playlist back to YouTube, one
// write at a time. Every run leaves a sync_runs row saying how it ended and what
// it did.
//
// The API writes playlists too, and a read can predate a write YouTube has
// answered. So a run changes nothing a read could not yet show: it keeps the
// details of a playlist the API wrote after a read began, keeps a playlist the
// API created, does not restore one the API deleted, and leaves the items of a
// playlist whose items were written within the lag of its read for the next
// run. The next run, whose reads follow the write, stores what YouTube holds.
//
// A push write is recorded before it is sent, and settled with YouTube's answer
// in the transaction that applies the answer to the playlist's base and entries.
// What a push learns about YouTube is stored apart from what a read showed,
// beside what retires it:
//
//   - a write whose answer was lost is the playlist's unanswered write, until
//     the next merge reads what YouTube holds
//   - an item a write inserted or moved is placed in the base, where no read has
//     shown it, until the next merge's read replaces the base
//   - a playlist YouTube refused a position in is sorted automatically, until
//     an edit of its order tries positions again
//   - a write YouTube refused for any other reason holds the playlist's push for
//     the Pacific day, until its base or its order changes
//
// A push plans from its merge's read, so an edit on YouTube after the read and
// before a write lands moves where the write lands. The item the write placed
// is no evidence of a reorder on YouTube, so the next merge keeps the server's
// order unless YouTube also moved an item it placed itself.
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// ErrReadsExceedQuota is the failure of a run whose interval, at this run's
// read cost, makes more reads a day than the day's quota allows. Such a day's
// later runs draw YouTube's quota refusal.
var ErrReadsExceedQuota = errors.New("a day of runs at this interval reads more than the day's quota")

// ErrAbsenceNotConfirmed is the failure of a playlist whose read lacks an item
// that a read of the item by id finds. The playlist changed between the pages of
// its read, so the run leaves it for the next.
var ErrAbsenceNotConfirmed = errors.New("an item missing from the playlist's read is still on YouTube")

// ErrAllowanceSpent is the failure of a run that stopped pushing because the
// day's quota has no room for another write beside the reads of the day's
// remaining runs.
var ErrAllowanceSpent = errors.New("the day's quota has no room for another write beside the reads of the day's remaining runs")

// ErrPushHeld is the failure of a playlist whose push waits, because YouTube
// refused a write of it this Pacific day for a reason that is not the video or
// the playlist's sort, and neither the playlist's base nor its order has
// changed since, so the same write would be refused again.
var ErrPushHeld = errors.New("the push waits on a write YouTube refused today")

// DailyQuota is the units YouTube allows the Cloud project each Pacific day.
const DailyQuota = 10_000

// Channel is what a run reads and writes YouTube through, as youtube.Channel
// does.
type Channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error)
	Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error)
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
	// interval is the wait between runs, and runsPerDay how many runs a day the
	// worker makes at it.
	interval   time.Duration
	runsPerDay int64
	now        func() time.Time
}

// NewRunner is a Runner for runs every interval.
func NewRunner(st *store.Store, channel Channel, interval time.Duration) *Runner {
	return &Runner{
		store:      st,
		channel:    channel,
		interval:   interval,
		runsPerDay: int64((24*time.Hour + interval - 1) / interval),
		now:        time.Now,
	}
}

// Report is what one run did, as its sync_runs row records it.
type Report struct {
	RunID             int64
	Outcome           string
	Playlists         int
	PlaylistsDeleted  int
	PlaylistsSkipped  int
	PlaylistsDeferred int
	ItemsAdded        int
	ItemsRemoved      int
	Writes            int
	Requests          int64
	Units             int64
	WriteUnits        int64
	Failures          []Failure
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
	// merged is each playlist merged, in the order it was read, for the push.
	merged []merged
	// readUnits is what the run's reads cost.
	readUnits int64
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
		if err := run.mergePlaylist(playlist, listedAt); err != nil {
			run.ended = err
			return
		}
	}

	run.readUnits = run.channel.Units() - run.units0
	if run.runsPerDay*run.readUnits > DailyQuota {
		run.report.Failures = append(run.report.Failures, Failure{
			Err: fmt.Errorf("%w: %d runs a day at %d units each", ErrReadsExceedQuota, run.runsPerDay, run.readUnits),
		})
	}

	for _, playlist := range run.merged {
		err := run.push(playlist)
		if errors.Is(err, ErrAllowanceSpent) {
			run.report.Failures = append(run.report.Failures, Failure{Playlist: youtube.PlaylistID(playlist.id), Err: err})
			return
		}
		if err != nil {
			run.ended = err
			return
		}
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
			run.skip(youtube.PlaylistID(id), err)
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

// skip records a failure about the playlist alone, which the run passes over.
func (run *run) skip(playlist youtube.PlaylistID, err error) {
	run.report.PlaylistsSkipped++
	run.report.Failures = append(run.report.Failures, Failure{Playlist: playlist, Err: err})
}

// merged is a playlist a run merged, as the push starts from it: the revision
// the merge left, how YouTube orders it, what YouTube holds, the server's order
// with every entry's id, and the push write YouTube refused.
type merged struct {
	id             string
	revision       int64
	sort           merge.Sort
	read           []merge.Item
	entries        []merge.Entry
	refusedWriteID sql.NullInt64
}

// mergePlaylist reads one listed playlist's items and merges them into the
// store, in one transaction: its details as YouTube holds them, and its items
// into the server's order against its base. listedAt is when the listing that
// named it was sent. A failure about this playlist alone is recorded and
// returns nil; the error is for one that ends the run.
func (run *run) mergePlaylist(playlist youtube.Playlist, listedAt time.Time) error {
	if err := run.ctx.Err(); err != nil {
		return err
	}
	id := string(playlist.ID)
	readAt := run.now()
	items, err := run.channel.Items(run.ctx, playlist.ID)
	switch {
	case errors.Is(err, youtube.ErrPlaylistNotFound):
		// The playlist was deleted after the channel listed it, or created so
		// recently that YouTube does not yet list its items.
		return run.deleteGone(id, readAt)
	case errors.Is(err, youtube.ErrQuotaSpent) || (err != nil && run.ctx.Err() != nil):
		return err
	case err != nil:
		run.skip(playlist.ID, err)
		return nil
	}
	read := make([]merge.Item, len(items))
	for i, item := range items {
		read[i] = merge.Item{ID: string(item.ID), VideoID: string(item.VideoID)}
	}

	deferred, err := store.ItemWritesSince(run.ctx, run.store.Queries, id, readAt)
	if err != nil {
		return err
	}
	var base []store.BaseItem
	if !deferred {
		base, err = store.Base(run.ctx, run.store.Queries, id)
		if err != nil {
			return err
		}
		confirmed, err := run.confirmAbsences(playlist.ID, base, read)
		if err != nil || !confirmed {
			return err
		}
	}

	var result merge.Result
	var stored merged
	var outcome mergeOutcome
	err = run.store.InTx(run.ctx, func(tx *store.Tx) error {
		method, written, err := store.WriteNewerThanRead(run.ctx, tx.Queries, id, listedAt)
		switch {
		case err != nil:
			return err
		case written && method == youtube.MethodPlaylistsDelete:
			// The API deleted the playlist later than the listing can show.
			outcome = mergeSkipped
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
		for _, item := range items {
			if err := upsertVideo(run.ctx, tx, item); err != nil {
				return err
			}
		}
		if deferred {
			outcome = mergeDeferred
			return nil
		}
		stored, result, err = mergeInto(run.ctx, tx, id, base, read)
		if err != nil {
			return err
		}
		outcome = mergeStored
		return nil
	})
	if errors.Is(err, errBaseMoved) {
		run.skip(playlist.ID, err)
		return nil
	}
	if err != nil {
		return err
	}
	switch outcome {
	case mergeStored:
		run.merged = append(run.merged, stored)
		run.report.Playlists++
		run.report.ItemsAdded += result.Added
		run.report.ItemsRemoved += result.Removed
	case mergeDeferred:
		run.report.PlaylistsDeferred++
	}
	return nil
}

// mergeOutcome is what a merge of one playlist did.
type mergeOutcome int

const (
	// mergeSkipped stored nothing, since the API deleted the playlist.
	mergeSkipped mergeOutcome = iota
	// mergeDeferred stored the playlist's details and left its items for the
	// next run.
	mergeDeferred
	// mergeStored stored the playlist's details and merged its items.
	mergeStored
)

// errBaseMoved is the failure of a playlist whose base changed between the
// absences confirmed against it and the merge.
var errBaseMoved = errors.New("the playlist's base changed while its absences were confirmed")

// confirmAbsences reads by id each item of base that read lacks. It returns
// false, having recorded the playlist as skipped, when YouTube still has one or
// the read fails; the error is for a failure that ends the run.
func (run *run) confirmAbsences(playlist youtube.PlaylistID, base []store.BaseItem, read []merge.Item) (bool, error) {
	var absent []youtube.ItemID
	for _, item := range base {
		if !slices.ContainsFunc(read, func(r merge.Item) bool { return r.ID == item.ItemID }) {
			absent = append(absent, youtube.ItemID(item.ItemID))
		}
	}
	if len(absent) == 0 {
		return true, nil
	}
	still, err := run.channel.ExistingItems(run.ctx, absent)
	switch {
	case errors.Is(err, youtube.ErrQuotaSpent) || (err != nil && run.ctx.Err() != nil):
		return false, err
	case err != nil:
		run.skip(playlist, err)
		return false, nil
	case len(still) > 0:
		run.skip(playlist, fmt.Errorf("%w: %d of %d, the first %s", ErrAbsenceNotConfirmed, len(still), len(absent), still[0]))
		return false, nil
	}
	return true, nil
}

// mergeInto merges read into the playlist id inside tx, against the base whose
// absences were confirmed, and stores the result: the server's order, and read
// as the base, with no write left unanswered against it.
func mergeInto(ctx context.Context, tx *store.Tx, id string, confirmedBase []store.BaseItem, read []merge.Item) (merged, merge.Result, error) {
	state, err := tx.GetPlaylistState(ctx, id)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	base, err := store.Base(ctx, tx.Queries, id)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	if !slices.Equal(base, confirmedBase) {
		return merged{}, merge.Result{}, errBaseMoved
	}
	entries, err := store.Entries(ctx, tx.Queries, id)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	sort, err := mergeSort(state.Sort)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	unanswered, err := unansweredWrite(ctx, tx.Queries, state.UnansweredWriteID)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	result, err := merge.Merge(merge.Playlist{Base: mergeBase(base), Read: read, Entries: mergeEntries(entries), Unanswered: unanswered, Sort: sort})
	if err != nil {
		return merged{}, merge.Result{}, fmt.Errorf("merge playlist %s: %w", id, err)
	}
	revision, err := tx.ReplaceOrder(ctx, id, state.Revision, storeEntries(result.Entries))
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	if err := tx.ReplaceBase(ctx, id, storeBase(unplaced(read))); err != nil {
		return merged{}, merge.Result{}, err
	}
	if err := tx.SetUnansweredWrite(ctx, generated.SetUnansweredWriteParams{PlaylistID: id}); err != nil {
		return merged{}, merge.Result{}, err
	}
	after, err := tx.GetPlaylistState(ctx, id)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	withIDs, err := store.Entries(ctx, tx.Queries, id)
	if err != nil {
		return merged{}, merge.Result{}, err
	}
	return merged{id: id, revision: revision, sort: sort, read: read, entries: mergeEntries(withIDs), refusedWriteID: after.RefusedWriteID}, result, nil
}

// mergeSort is the sort a merge reads for a playlist the store holds as sorted
// stored.
func mergeSort(stored string) (merge.Sort, error) {
	switch stored {
	case store.SortManual:
		return merge.Manual, nil
	case store.SortAutomatic:
		return merge.Automatic, nil
	default:
		return "", fmt.Errorf("a playlist is sorted %q, which is not a sort a merge knows", stored)
	}
}

// unansweredWrite is the push write id names, as a merge reads it, and nil when
// id is not valid.
func unansweredWrite(ctx context.Context, q *generated.Queries, id sql.NullInt64) (*merge.Write, error) {
	if !id.Valid {
		return nil, nil
	}
	row, err := q.GetYouTubeWrite(ctx, id.Int64)
	if err != nil {
		return nil, fmt.Errorf("read the unanswered write %d: %w", id.Int64, err)
	}
	w := merge.Write{ItemID: row.ItemID.String, EntryID: row.EntryID.Int64, VideoID: row.VideoID.String, Position: row.Position.Int64}
	switch row.Method {
	case youtube.MethodPlaylistItemsInsert:
		w.Kind = merge.Append
		if row.Position.Valid {
			w.Kind = merge.Insert
		}
	case youtube.MethodPlaylistItemsUpdate:
		w.Kind = merge.Move
	case youtube.MethodPlaylistItemsDelete:
		w.Kind = merge.Delete
	default:
		return nil, fmt.Errorf("the unanswered write %d is a %s, which is not a write a push makes", row.WriteID, row.Method)
	}
	return &w, nil
}

func mergeBase(base []store.BaseItem) []merge.BaseItem {
	items := make([]merge.BaseItem, len(base))
	for i, item := range base {
		items[i] = merge.BaseItem{Item: merge.Item{ID: item.ItemID, VideoID: item.VideoID}, Placed: item.Placed}
	}
	return items
}

func storeBase(items []merge.BaseItem) []store.BaseItem {
	base := make([]store.BaseItem, len(items))
	for i, item := range items {
		base[i] = store.BaseItem{ItemID: item.ID, VideoID: item.VideoID, Placed: item.Placed}
	}
	return base
}

// unplaced is read as a base, where a read placed every item.
func unplaced(read []merge.Item) []merge.BaseItem {
	base := make([]merge.BaseItem, len(read))
	for i, item := range read {
		base[i] = merge.BaseItem{Item: item}
	}
	return base
}

func mergeEntries(entries []store.Entry) []merge.Entry {
	converted := make([]merge.Entry, len(entries))
	for i, entry := range entries {
		converted[i] = merge.Entry(entry)
	}
	return converted
}

func storeEntries(entries []merge.Entry) []store.Entry {
	converted := make([]store.Entry, len(entries))
	for i, entry := range entries {
		converted[i] = store.Entry(entry)
	}
	return converted
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
			StartedTs:         store.Timestamp(run.started),
			FinishedTs:        store.Timestamp(run.now()),
			QuotaDate:         run.date,
			Outcome:           rep.Outcome,
			Playlists:         int64(rep.Playlists),
			PlaylistsDeleted:  int64(rep.PlaylistsDeleted),
			PlaylistsSkipped:  int64(rep.PlaylistsSkipped),
			PlaylistsDeferred: int64(rep.PlaylistsDeferred),
			ItemsAdded:        int64(rep.ItemsAdded),
			ItemsRemoved:      int64(rep.ItemsRemoved),
			Requests:          rep.Requests,
			Units:             rep.Units,
			Writes:            int64(rep.Writes),
			WriteUnits:        rep.WriteUnits,
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
