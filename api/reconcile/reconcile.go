// Package reconcile syncs the channel's playlists with the store both ways, in
// passes. A pass works through a queue of jobs, the earliest priority first:
// a push of each playlist an edit changed, a probe listing every playlist and a
// sync of each whose count moved, a push that stopped before it finished, a
// sweep reading the playlist read longest ago, and the lengths of new videos.
// A sync reads one playlist, merges the read into the server's order against
// the playlist's base, and pushes the server's order back, one write at a time.
// Every pass that did anything leaves a sync_runs row saying how it ended and
// what it did, what enrichment read since the pass before included.
//
// The API writes playlists too, and a read can predate a write YouTube has
// answered. So a pass changes nothing a read could not yet show: it keeps the
// details of a playlist the API wrote after a read began, keeps a playlist the
// API created, does not restore one the API deleted, and leaves the items of a
// playlist whose items were written within the lag of its read for a later
// pass, whose reads follow the write.
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

	"github.com/datapointchris/ypl/api/enrich"
	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// ErrAbsenceNotConfirmed is the failure of a playlist whose read lacks an item
// that a read of the item by id finds. The playlist changed between the pages of
// its read, so the pass leaves it for a later one.
var ErrAbsenceNotConfirmed = errors.New("an item missing from the playlist's read is still on YouTube")

// ErrAllowanceSpent is the failure of a push that stopped because the day's
// quota has no room for another write of its priority beside the reads of the
// day's remaining passes.
var ErrAllowanceSpent = errors.New("the day's quota has no room for another write beside the reads of the day's remaining passes")

// ErrPushHeld is the failure of a playlist whose push waits, because YouTube
// refused a write of it this Pacific day for a reason that is not the video or
// the playlist's sort, and neither the playlist's base nor its order has
// changed since, so the same write would be refused again.
var ErrPushHeld = errors.New("the push waits on a write YouTube refused today")

// EditReserve is the units no job but an edit's push spends, so a long push
// running in the background leaves room for the next edit.
const EditReserve = 1_000

// Channel is what a pass reads and writes YouTube through, as youtube.Channel
// does.
type Channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error)
	Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error)
	ExistingItems(ctx context.Context, ids []youtube.ItemID) ([]youtube.ItemID, error)
	Videos(ctx context.Context, ids []youtube.VideoID) ([]youtube.Video, error)
	InsertItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error)
	AppendItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID) (youtube.ItemID, error)
	MoveItem(ctx context.Context, item youtube.Item, position int64) error
	DeleteItem(ctx context.Context, id youtube.ItemID) error
	Requests() int64
	Units() int64
}

// Runner makes passes against one store and one channel, one at a time.
type Runner struct {
	store   *store.Store
	channel Channel
	// tally is what enrichment read since the last pass, which each pass
	// records.
	tally *enrich.Tally
	// edits is the playlists an edit changed and no pass has taken up yet.
	edits *Edits
	// interval is the mean wait between two ticks, which the quota guard counts
	// the day's remaining probes and sweeps by.
	interval time.Duration
	// lengthsDue is when a tick next reads the lengths of videos. A live or
	// upcoming stream has none to give and is asked for at each read, so the
	// reads are hourly rather than every tick.
	lengthsDue time.Time
	now        func() time.Time
}

// lengthsEvery is how often a tick reads the lengths of new videos.
const lengthsEvery = time.Hour

// NewRunner is a Runner ticking every interval on average, recording what
// tally holds with each pass, and pushing what edits holds first. tally and
// edits may each be nil.
func NewRunner(st *store.Store, channel Channel, tally *enrich.Tally, edits *Edits, interval time.Duration) *Runner {
	return &Runner{store: st, channel: channel, tally: tally, edits: edits, interval: interval, now: time.Now}
}

// Report is what one pass did, as its sync_runs row records it. VideoReads,
// VideosEnriched, TracksFound, VideosUnreadable, RateLimited and
// EnrichmentPaused are what enrichment did since the pass before, as
// enrich.Report names them. ProbeMisses counts the playlists its sweep found
// changed on YouTube that the probe had not flagged, and Retry the playlists an
// edit changed whose sync has to wait for YouTube to show a recent write.
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
	VideoReads        int
	VideosEnriched    int
	TracksFound       int
	VideosUnreadable  int
	RateLimited       bool
	EnrichmentPaused  bool
	ProbeMisses       int
	Failures          []Failure
	Retry             []string
}

// Failure is one thing that went wrong in a pass. Stage is which half of the
// work it happened in, Playlist the playlist a failure of the sync is about,
// and Video the video a failure of enrichment is about. A failure of a whole
// stage names neither, which is why it names the stage.
type Failure struct {
	Stage    string
	Playlist youtube.PlaylistID
	Video    youtube.VideoID
	Err      error
}

// Run makes a full pass: every playlist the channel lists is read and synced,
// whatever its count, and the lengths of new videos are read. The worker makes
// one as it starts, which is what the probes after it compare their counts
// with. The error is only for a pass that could not be recorded: whatever went
// wrong inside it is in the report.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	r.lengthsDue = r.now().Add(lengthsEvery)
	return r.pass(ctx, []Job{
		{Kind: KindProbe, Priority: PriorityChanged, Full: true},
		{Kind: KindLengths, Priority: PriorityLengths},
	})
}

// Tick makes the pass the worker makes each interval: the probe, the sweep of
// the playlist read longest ago, every push that stopped before it finished,
// and, once lengthsEvery has passed, the lengths of new videos.
func (r *Runner) Tick(ctx context.Context) (Report, error) {
	jobs := []Job{{Kind: KindProbe, Priority: PriorityChanged}}
	if now := r.now(); !now.Before(r.lengthsDue) {
		r.lengthsDue = now.Add(lengthsEvery)
		jobs = append(jobs, Job{Kind: KindLengths, Priority: PriorityLengths})
	}
	pending, err := r.readPending(ctx)
	if err != nil {
		return r.failedPass(ctx, err)
	}
	for _, p := range pending {
		if !p.Held {
			jobs = append(jobs, Job{Kind: KindSync, Priority: PriorityUnfinished, Playlist: p.PlaylistID})
		}
	}
	return r.pass(ctx, jobs)
}

// Pass makes a pass of whatever is waiting: the playlists edits changed, and
// what enrichment did since the last pass, which a refusal of its reads wants
// recorded at once.
func (r *Runner) Pass(ctx context.Context) (Report, error) {
	return r.pass(ctx, nil)
}

func (r *Runner) pass(ctx context.Context, jobs []Job) (Report, error) {
	run := r.begin(ctx)
	run.execute(jobs)
	return run.record()
}

// failedPass records a pass that failed before its first job.
func (r *Runner) failedPass(ctx context.Context, err error) (Report, error) {
	run := r.begin(ctx)
	run.ended = err
	return run.record()
}

func (r *Runner) begin(ctx context.Context) *run {
	started := r.now()
	return &run{
		Runner:    r,
		ctx:       ctx,
		started:   started,
		date:      youtube.QuotaDate(started),
		stage:     store.StageSync,
		requests0: r.channel.Requests(),
		units0:    r.channel.Units(),
		listed:    map[string]youtube.Playlist{},
		synced:    map[string]bool{},
	}
}

// run is the state of one pass while it is made.
type run struct {
	*Runner
	ctx       context.Context
	started   time.Time
	date      string
	requests0 int64
	units0    int64
	report    Report
	// listed is each playlist the pass's probe listed, and listedAt when the
	// listing was sent. A sync of a playlist no probe listed keeps the details
	// the store holds.
	listed   map[string]youtube.Playlist
	listedAt time.Time
	// synced is each playlist the pass has synced, which its sweep passes over.
	synced map[string]bool
	// perTick is what one tick's probe and sweep read, once the pass has asked.
	perTick int64
	// idle is a pass that sent nothing, since a pass before it drew the day's
	// quota refusal.
	idle bool
	// ended is why the pass stopped before its end: YouTube's quota refusal, the
	// context ending, or a failure of the whole pass.
	ended error
	// stage is which half of the work is happening, which every failure it
	// records names.
	stage string
}

func (run *run) execute(jobs []Job) {
	refused, err := run.store.Queries.CountQuotaSpentRuns(run.ctx, run.date)
	if err != nil {
		run.ended = err
		return
	}
	if refused > 0 {
		// A pass that drew the day's refusal is recorded, and those after it
		// send nothing until the quota resets. A playlist an edit changed
		// meanwhile is found again by the first tick after it.
		run.edits.take()
		run.idle = true
		run.ended = fmt.Errorf("%w: a pass on %s already drew the refusal", youtube.ErrQuotaSpent, run.date)
		return
	}
	q := &queue{}
	for _, j := range jobs {
		q.add(j)
	}
	for {
		for _, id := range run.edits.take() {
			q.add(Job{Kind: KindSync, Priority: PriorityEdit, Playlist: id})
		}
		job, ok := q.next()
		if !ok {
			return
		}
		if err := run.do(job, q); err != nil {
			run.ended = err
			return
		}
	}
}

// do makes one job. The error is one that ends the pass.
func (run *run) do(job Job, q *queue) error {
	if err := run.ctx.Err(); err != nil {
		return err
	}
	switch job.Kind {
	case KindProbe:
		return run.probe(job.Full, q)
	case KindSync:
		return run.sync(job)
	case KindLengths:
		return run.lengths()
	}
	return fmt.Errorf("a job of kind %d, which a pass does not make", job.Kind)
}

// probe lists every playlist the channel owns and queues a sync of each whose
// count moved since its last read, of each the store has never read, and of
// every one when full. A pass that is not full also queues the sweep: the
// playlist read longest ago that nothing else queued. A failed listing ends
// the pass.
func (run *run) probe(full bool, q *queue) error {
	listedAt := run.now()
	listed, err := run.channel.Playlists(run.ctx)
	if err != nil {
		return err
	}
	run.listedAt = listedAt
	for _, playlist := range listed {
		run.listed[string(playlist.ID)] = playlist
	}
	if err := run.deleteUnlisted(listed); err != nil {
		return err
	}
	reads, err := run.store.Queries.ListPlaylistReads(run.ctx)
	if err != nil {
		return err
	}
	counted := map[string]sql.NullInt64{}
	for _, read := range reads {
		counted[read.PlaylistID] = read.ReadItemCount
	}
	for _, playlist := range listed {
		count, known := counted[string(playlist.ID)]
		if full || !known || !count.Valid || count.Int64 != playlist.ItemCount {
			q.add(Job{Kind: KindSync, Priority: PriorityChanged, Playlist: string(playlist.ID)})
			continue
		}
		if err := run.storeDetails(playlist); err != nil {
			return err
		}
	}
	if full {
		return nil
	}
	for _, read := range reads {
		_, isListed := run.listed[read.PlaylistID]
		if isListed && !q.holds(read.PlaylistID) && !run.synced[read.PlaylistID] {
			q.add(Job{Kind: KindSync, Priority: PrioritySweep, Playlist: read.PlaylistID})
			break
		}
	}
	return nil
}

// storeDetails stores a listed playlist's title, description and privacy,
// unless the API wrote the playlist later than the listing can show.
func (run *run) storeDetails(playlist youtube.Playlist) error {
	return run.store.InTx(run.ctx, func(tx *store.Tx) error {
		_, written, err := store.WriteNewerThanRead(run.ctx, tx.Queries, string(playlist.ID), run.listedAt)
		if err != nil || written {
			return err
		}
		return tx.UpsertPlaylist(run.ctx, generated.UpsertPlaylistParams{
			PlaylistID: string(playlist.ID), Title: playlist.Title, Description: playlist.Description, Privacy: playlist.Privacy,
		})
	})
}

// sync reads one playlist, merges it and pushes it. A push that stopped before
// it finished is not read until the day's quota has room for one of its
// writes, since a read it cannot act on is spent for nothing. A playlist an
// edit changed whose read cannot yet show a recent write is retried once it
// can. The error is one that ends the pass.
//
// The read is stamped as taken up whatever came of it, so the sweep moves past
// a playlist whose reads keep failing rather than taking it every tick.
func (run *run) sync(job Job) error {
	if job.Priority == PriorityUnfinished {
		room, err := run.allows(job.Priority, youtube.WriteUnits)
		if err != nil || !room {
			return err
		}
	}
	run.synced[job.Playlist] = true
	takenAt := run.now()
	m, result, outcome, err := run.mergePlaylist(job.Playlist)
	if err != nil {
		return err
	}
	if err := run.store.Queries.SetPlaylistReadTs(run.ctx, generated.SetPlaylistReadTsParams{
		PlaylistID: job.Playlist, ReadTs: sql.NullString{String: store.Timestamp(takenAt), Valid: true},
	}); err != nil {
		return err
	}
	switch outcome {
	case mergeStored:
		if job.Priority == PrioritySweep && result.Added+result.Removed > 0 {
			run.report.ProbeMisses++
		}
		err := run.push(m, job.Priority)
		if errors.Is(err, ErrAllowanceSpent) {
			run.report.Failures = append(run.report.Failures, Failure{Stage: run.stage, Playlist: youtube.PlaylistID(m.id), Err: err})
			return nil
		}
		return err
	case mergeDeferred, mergeNotYet:
		if job.Priority == PriorityEdit {
			run.report.Retry = append(run.report.Retry, job.Playlist)
		}
	}
	return nil
}

// lengthsPerPass is the most videos a pass reads the length of. The Data API
// reads 50 a unit, so the most a pass spends on lengths is 20 units.
const lengthsPerPass = 1000

// lengths reads the length of each video a playlist holds that the store holds
// none for, from the Data API, when the day's quota has room beside the
// reserves. A length otherwise arrives with the video's tracklist read, which
// paces through the library, and until then a filter or an order by length
// passes the video over.
//
// Once the library is measured it reads the videos new since the last pass and
// those YouTube reports no length for, live and upcoming streams, at a unit per
// 50. A failed read costs the pass nothing it came for, so it is a failure of
// the pass rather than its end, unless it is the day's quota or the pass itself
// was stopped.
func (run *run) lengths() error {
	stored, err := run.store.Queries.ListVideosWithoutLength(run.ctx, lengthsPerPass)
	if err != nil || len(stored) == 0 {
		return err
	}
	room, err := run.allows(PriorityLengths, int64(len(stored)+49)/50)
	if err != nil || !room {
		return err
	}
	ids := make([]youtube.VideoID, len(stored))
	for i, id := range stored {
		ids[i] = youtube.VideoID(id)
	}
	videos, err := run.channel.Videos(run.ctx, ids)
	switch {
	case errors.Is(err, youtube.ErrQuotaSpent) || (err != nil && run.ctx.Err() != nil):
		return err
	case err != nil:
		run.report.Failures = append(run.report.Failures, Failure{Stage: run.stage, Err: fmt.Errorf("read the lengths of %d videos: %w", len(ids), err)})
		return nil
	}
	for _, video := range videos {
		if video.DurationSeconds == nil {
			continue
		}
		if err := run.store.Queries.SetVideoLength(run.ctx, generated.SetVideoLengthParams{
			VideoID: string(video.ID), DurationSeconds: sql.NullInt64{Int64: *video.DurationSeconds, Valid: true},
		}); err != nil {
			return err
		}
	}
	return nil
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
			// YouTube still has the playlist, so the listing missed it, and a
			// later pass's listing is read instead.
		case errors.Is(err, youtube.ErrPlaylistNotFound):
			if _, err := run.deleteGone(id, readAt); err != nil {
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
// gone, unless the API wrote it later than that read can show. It returns
// whether it deleted it.
func (run *run) deleteGone(id string, readAt time.Time) (bool, error) {
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
		return false, err
	}
	if deleted {
		run.report.PlaylistsDeleted++
	}
	return deleted, nil
}

// skip records a failure about the playlist alone, which the pass passes over.
func (run *run) skip(playlist youtube.PlaylistID, err error) {
	run.report.PlaylistsSkipped++
	run.report.Failures = append(run.report.Failures, Failure{Stage: run.stage, Playlist: playlist, Err: err})
}

// merged is a playlist a pass merged, as the push starts from it: the revision
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

// mergeOutcome is what a merge of one playlist did.
type mergeOutcome int

const (
	// mergeSkipped stored nothing: the API deleted the playlist, YouTube has
	// deleted it, or the read failed in a way recorded as the playlist's
	// failure.
	mergeSkipped mergeOutcome = iota
	// mergeDeferred stored the playlist's details and left its items for a
	// later pass, since the API wrote them within the lag of the read.
	mergeDeferred
	// mergeNotYet stored nothing, since YouTube does not yet show a playlist
	// the API created or changed within the lag of the read.
	mergeNotYet
	// mergeStored stored the playlist's details and merged its items.
	mergeStored
)

// mergePlaylist reads one playlist's items and merges them into the store, in
// one transaction: its details as the pass's listing gave them, and its items
// into the server's order against its base. A failure about this playlist
// alone is recorded; the error is for one that ends the pass.
func (run *run) mergePlaylist(id string) (merged, merge.Result, mergeOutcome, error) {
	listing, isListed := run.listed[id]
	if !isListed {
		_, err := run.store.Queries.GetPlaylistState(run.ctx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The API deleted the playlist after it was queued.
			return merged{}, merge.Result{}, mergeSkipped, nil
		case err != nil:
			return merged{}, merge.Result{}, mergeSkipped, err
		}
	}
	readAt := run.now()
	items, err := run.channel.Items(run.ctx, youtube.PlaylistID(id))
	switch {
	case errors.Is(err, youtube.ErrPlaylistNotFound):
		// The playlist was deleted after it was listed or queued, or created so
		// recently that YouTube does not yet list its items.
		deleted, err := run.deleteGone(id, readAt)
		if err != nil || deleted {
			return merged{}, merge.Result{}, mergeSkipped, err
		}
		return merged{}, merge.Result{}, mergeNotYet, nil
	case errors.Is(err, youtube.ErrQuotaSpent) || (err != nil && run.ctx.Err() != nil):
		return merged{}, merge.Result{}, mergeSkipped, err
	case err != nil:
		run.skip(youtube.PlaylistID(id), err)
		return merged{}, merge.Result{}, mergeSkipped, nil
	}
	read := make([]merge.Item, len(items))
	for i, item := range items {
		read[i] = merge.Item{ID: string(item.ID), VideoID: string(item.VideoID)}
	}

	deferred, err := store.ItemWritesSince(run.ctx, run.store.Queries, id, readAt)
	if err != nil {
		return merged{}, merge.Result{}, mergeSkipped, err
	}
	var base []store.BaseItem
	if !deferred {
		base, err = store.Base(run.ctx, run.store.Queries, id)
		if err != nil {
			return merged{}, merge.Result{}, mergeSkipped, err
		}
		confirmed, err := run.confirmAbsences(youtube.PlaylistID(id), base, read)
		if err != nil || !confirmed {
			return merged{}, merge.Result{}, mergeSkipped, err
		}
	}

	var result merge.Result
	var stored merged
	outcome := mergeSkipped
	err = run.store.InTx(run.ctx, func(tx *store.Tx) error {
		if isListed {
			method, written, err := store.WriteNewerThanRead(run.ctx, tx.Queries, id, run.listedAt)
			switch {
			case err != nil:
				return err
			case written && method == youtube.MethodPlaylistsDelete:
				// The API deleted the playlist later than the listing can show.
				return nil
			case !written:
				// A playlist the API created or updated later than the listing
				// can show keeps the details the API stored.
				err := tx.UpsertPlaylist(run.ctx, generated.UpsertPlaylistParams{
					PlaylistID: id, Title: listing.Title, Description: listing.Description, Privacy: listing.Privacy,
				})
				if err != nil {
					return err
				}
			}
		}
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
		if isListed {
			if err := tx.SetPlaylistReadCount(run.ctx, generated.SetPlaylistReadCountParams{
				PlaylistID: id, ReadItemCount: sql.NullInt64{Int64: listing.ItemCount, Valid: true},
			}); err != nil {
				return err
			}
		}
		outcome = mergeStored
		return nil
	})
	if errors.Is(err, errBaseMoved) {
		run.skip(youtube.PlaylistID(id), err)
		return merged{}, merge.Result{}, mergeSkipped, nil
	}
	if err != nil {
		return merged{}, merge.Result{}, mergeSkipped, err
	}
	switch outcome {
	case mergeStored:
		run.report.Playlists++
		run.report.ItemsAdded += result.Added
		run.report.ItemsRemoved += result.Removed
	case mergeDeferred:
		run.report.PlaylistsDeferred++
	}
	return stored, result, outcome, nil
}

// errBaseMoved is the failure of a playlist whose base changed between the
// absences confirmed against it and the merge.
var errBaseMoved = errors.New("the playlist's base changed while its absences were confirmed")

// confirmAbsences reads by id each item of base that read lacks. It returns
// false, having recorded the playlist as skipped, when YouTube still has one or
// the read fails; the error is for a failure that ends the pass.
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

// record writes the pass and its failures, with what enrichment did since the
// pass before, on a context the pass's own ending does not cancel. A pass that
// sent nothing and failed at nothing, or sent nothing because the day's quota
// is spent, writes no row unless enrichment has something to record.
func (run *run) record() (Report, error) {
	rep := &run.report
	rep.Requests = run.channel.Requests() - run.requests0
	rep.Units = run.channel.Units() - run.units0
	read := run.tally.Take()
	rep.VideoReads = read.Reads
	rep.VideosEnriched = read.Enriched
	rep.TracksFound = read.Tracks
	rep.VideosUnreadable = read.Unreadable
	rep.RateLimited = read.RateLimited
	rep.EnrichmentPaused = read.Paused
	for _, failure := range read.Failures {
		rep.Failures = append(rep.Failures, Failure{Stage: store.StageEnrichment, Video: youtube.VideoID(failure.VideoID), Err: failure.Err})
	}
	switch {
	case errors.Is(run.ended, youtube.ErrQuotaSpent):
		rep.Outcome = store.OutcomeQuotaSpent
	case run.ended != nil && run.ctx.Err() != nil:
		rep.Outcome = store.OutcomeCanceled
	case run.ended != nil:
		rep.Outcome = store.OutcomeFailed
		rep.Failures = append(rep.Failures, Failure{Stage: run.stage, Err: run.ended})
	case len(rep.Failures) > 0:
		rep.Outcome = store.OutcomePartial
	default:
		rep.Outcome = store.OutcomeOK
	}
	quiet := rep.Requests == 0 && len(rep.Failures) == 0 && rep.Outcome == store.OutcomeOK
	if read.Reads == 0 && len(read.Failures) == 0 && (quiet || run.idle) {
		return *rep, nil
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
			VideoReads:        int64(rep.VideoReads),
			VideosEnriched:    int64(rep.VideosEnriched),
			TracksFound:       int64(rep.TracksFound),
			VideosUnreadable:  int64(rep.VideosUnreadable),
			IsRateLimited:     rep.RateLimited,
			EnrichmentPaused:  rep.EnrichmentPaused,
			ProbeMisses:       int64(rep.ProbeMisses),
		})
		if err != nil {
			return err
		}
		rep.RunID = id
		for _, failure := range rep.Failures {
			err := tx.InsertSyncFailure(ctx, generated.InsertSyncFailureParams{
				RunID:      id,
				PlaylistID: sql.NullString{String: string(failure.Playlist), Valid: failure.Playlist != ""},
				VideoID:    sql.NullString{String: string(failure.Video), Valid: failure.Video != ""},
				Error:      failure.Err.Error(),
				Stage:      failure.Stage,
			})
			if err != nil {
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
