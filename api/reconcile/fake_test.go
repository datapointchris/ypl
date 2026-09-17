package reconcile

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeChannel is a channel's playlists held in memory, read as youtube.Channel
// reads them: each list call costs 1 unit a page of 50, a read by id costs 1,
// and a playlist that is gone answers ErrPlaylistNotFound.
type fakeChannel struct {
	playlists []youtube.Playlist
	items     map[youtube.PlaylistID][]youtube.Item
	// unseen holds each playlist every read passes over, as a read sent within
	// seconds of the playlist's create does, and unlisted each one only the
	// listing passes over.
	unseen   map[youtube.PlaylistID]bool
	unlisted map[youtube.PlaylistID]bool
	// itemsErrors is the error Items of a playlist returns, and byIDErrors the
	// error Playlist does.
	itemsErrors map[youtube.PlaylistID]error
	byIDErrors  map[youtube.PlaylistID]error
	// playlistsError is the error Playlists returns.
	playlistsError error
	// quota is how many units are served before every request is refused with
	// ErrQuotaSpent. Zero serves without limit.
	quota int64
	// beforeList runs before each Playlists call, beforeItems before each Items
	// call, and beforeWrite before each item write, each with its number
	// counting from 1.
	beforeList  func(n int)
	beforeItems func(n int)
	beforeWrite func(n int)
	// hidden holds each item a read of its playlist passes over, as a read
	// spanning pages with an edit between them can, while a read by id finds it.
	hidden map[youtube.ItemID]bool
	// automatic holds each playlist YouTube orders itself, which refuses a write
	// naming a position and puts an appended video first, as a playlist sorted
	// newest first does. Where YouTube's own sorts put one was not measured, so
	// the fake puts it where the push's own record of the append does not.
	// refusedVideos holds each video YouTube will not add.
	automatic     map[youtube.PlaylistID]bool
	refusedVideos map[youtube.VideoID]bool
	// videoTitles is each public video a read of videos by id returns, and its
	// title.
	videoTitles map[youtube.VideoID]string
	// faults is what the item write numbered n, counting from 1, does in place
	// of answering.
	faults map[int]fault

	lists, itemReads, byIDReads, writes int
	requests, units                     int64
	nextItem                            int
}

// fault is an item write that returns err, after it is applied when applied is
// true, as a write whose answer is lost is, and before it is otherwise.
type fault struct {
	applied bool
	err     error
}

// refusedAs is YouTube's refusal as youtube.Channel returns it, carrying named
// when it is not nil.
func refusedAs(named error) error {
	if named == nil {
		return fmt.Errorf("googleapi: Error 400: %w", youtube.ErrRefused)
	}
	return fmt.Errorf("%w: %w", named, youtube.ErrRefused)
}

// newFakeChannel holds a playlist per entry of playlists, named by id and holding
// one item per character of the string.
func newFakeChannel(playlists map[youtube.PlaylistID]string) *fakeChannel {
	f := &fakeChannel{
		items:         map[youtube.PlaylistID][]youtube.Item{},
		unseen:        map[youtube.PlaylistID]bool{},
		unlisted:      map[youtube.PlaylistID]bool{},
		itemsErrors:   map[youtube.PlaylistID]error{},
		byIDErrors:    map[youtube.PlaylistID]error{},
		hidden:        map[youtube.ItemID]bool{},
		automatic:     map[youtube.PlaylistID]bool{},
		refusedVideos: map[youtube.VideoID]bool{},
		videoTitles:   map[youtube.VideoID]string{},
		faults:        map[int]fault{},
	}
	for _, id := range slices.Sorted(maps.Keys(playlists)) {
		f.playlists = append(f.playlists, youtube.Playlist{ID: id, Title: "Playlist " + string(id), Privacy: "private"})
		f.items[id] = []youtube.Item{}
		for _, video := range strings.Split(playlists[id], "")[:len(playlists[id])] {
			f.add(id, youtube.VideoID(video), len(f.items[id]))
		}
	}
	return f
}

func (f *fakeChannel) add(playlist youtube.PlaylistID, video youtube.VideoID, position int) {
	f.nextItem++
	item := youtube.Item{
		ID:           youtube.ItemID(fmt.Sprintf("%s-i%03d", playlist, f.nextItem)),
		PlaylistID:   playlist,
		VideoID:      video,
		Title:        "Video " + string(video),
		ChannelTitle: "Channel",
	}
	f.items[playlist] = slices.Insert(f.items[playlist], position, item)
}

// remove deletes the playlist's item at position.
func (f *fakeChannel) remove(playlist youtube.PlaylistID, position int) {
	f.items[playlist] = slices.Delete(f.items[playlist], position, position+1)
}

// charge counts a request, as youtube.Channel counts every attempt, and its
// units, or refuses it as the client would once ctx has ended or YouTube would
// once the quota is spent.
func (f *fakeChannel) charge(ctx context.Context, units int64) error {
	f.requests++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.quota > 0 && f.units+units > f.quota {
		return fmt.Errorf("refused: %w", youtube.ErrQuotaSpent)
	}
	f.units += units
	return nil
}

func pages(n int) int64 {
	return int64(max(1, (n+49)/50))
}

func (f *fakeChannel) Playlists(ctx context.Context) ([]youtube.Playlist, error) {
	f.lists++
	if f.beforeList != nil {
		f.beforeList(f.lists)
	}
	if err := f.charge(ctx, pages(len(f.playlists))); err != nil {
		return nil, err
	}
	if f.playlistsError != nil {
		return nil, f.playlistsError
	}
	return slices.DeleteFunc(slices.Clone(f.playlists), func(p youtube.Playlist) bool { return f.unseen[p.ID] || f.unlisted[p.ID] }), nil
}

func (f *fakeChannel) Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error) {
	f.byIDReads++
	if err := f.charge(ctx, 1); err != nil {
		return youtube.Playlist{}, err
	}
	if err := f.byIDErrors[id]; err != nil {
		return youtube.Playlist{}, err
	}
	index := slices.IndexFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == id })
	if index < 0 || f.unseen[id] {
		return youtube.Playlist{}, fmt.Errorf("read playlist %s: %w", id, youtube.ErrPlaylistNotFound)
	}
	return f.playlists[index], nil
}

func (f *fakeChannel) Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error) {
	f.itemReads++
	if f.beforeItems != nil {
		f.beforeItems(f.itemReads)
	}
	held, ok := f.items[playlist]
	if err := f.charge(ctx, pages(len(held))); err != nil {
		return nil, err
	}
	if err := f.itemsErrors[playlist]; err != nil {
		return nil, err
	}
	if !ok || f.unseen[playlist] {
		return nil, fmt.Errorf("list items of playlist %s: %w", playlist, youtube.ErrPlaylistNotFound)
	}
	read := []youtube.Item{}
	for _, item := range held {
		if f.hidden[item.ID] {
			continue
		}
		item.Position = int64(len(read))
		read = append(read, item)
	}
	return read, nil
}

func (f *fakeChannel) ExistingItems(ctx context.Context, ids []youtube.ItemID) ([]youtube.ItemID, error) {
	if err := f.charge(ctx, pages(len(ids))); err != nil {
		return nil, err
	}
	var found []youtube.ItemID
	for _, id := range ids {
		if _, index := f.findItem(id); index >= 0 {
			found = append(found, id)
		}
	}
	return found, nil
}

// findItem is the playlist holding the item id and its index there, or an index
// of -1.
func (f *fakeChannel) findItem(id youtube.ItemID) (youtube.PlaylistID, int) {
	for playlist, items := range f.items {
		if index := slices.IndexFunc(items, func(item youtube.Item) bool { return item.ID == id }); index >= 0 {
			return playlist, index
		}
	}
	return "", -1
}

// write counts an item write at its price and returns its fault, or the
// refusal of the request itself.
func (f *fakeChannel) write(ctx context.Context) (fault, error) {
	f.writes++
	if f.beforeWrite != nil {
		f.beforeWrite(f.writes)
	}
	if err := f.charge(ctx, youtube.WriteUnits); err != nil {
		return fault{}, err
	}
	failure := f.faults[f.writes]
	if failure.err != nil && !failure.applied {
		return failure, failure.err
	}
	return failure, nil
}

// InsertItem, AppendItem, MoveItem and DeleteItem make item writes as the Data
// API was measured to: an insert names a position from 0 to the item count, a
// move takes the item out and puts it at exactly the position it names, from 0
// to one less than the count, and a write naming an item that is gone is
// ErrItemNotFound.
func (f *fakeChannel) InsertItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error) {
	failure, err := f.write(ctx)
	if err != nil {
		return "", err
	}
	held, ok := f.items[playlist]
	switch {
	case !ok:
		return "", refusedAs(youtube.ErrPlaylistNotFound)
	case f.refusedVideos[video]:
		return "", refusedAs(youtube.ErrVideoRefused)
	case f.automatic[playlist]:
		return "", refusedAs(youtube.ErrManualSortRequired)
	case position < 0 || int(position) > len(held):
		return "", refusedAs(nil)
	}
	f.add(playlist, video, int(position))
	if failure.err != nil {
		return "", failure.err
	}
	return f.items[playlist][position].ID, nil
}

func (f *fakeChannel) AppendItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID) (youtube.ItemID, error) {
	failure, err := f.write(ctx)
	if err != nil {
		return "", err
	}
	held, ok := f.items[playlist]
	switch {
	case !ok:
		return "", refusedAs(youtube.ErrPlaylistNotFound)
	case f.refusedVideos[video]:
		return "", refusedAs(youtube.ErrVideoRefused)
	}
	at := len(held)
	if f.automatic[playlist] {
		at = 0
	}
	f.add(playlist, video, at)
	if failure.err != nil {
		return "", failure.err
	}
	return f.items[playlist][at].ID, nil
}

func (f *fakeChannel) MoveItem(ctx context.Context, item youtube.Item, position int64) error {
	failure, err := f.write(ctx)
	if err != nil {
		return err
	}
	playlist, index := f.findItem(item.ID)
	switch {
	case index < 0:
		return refusedAs(youtube.ErrItemNotFound)
	case playlist != item.PlaylistID || f.items[playlist][index].VideoID != item.VideoID:
		return refusedAs(nil)
	case f.automatic[playlist]:
		return refusedAs(youtube.ErrManualSortRequired)
	case position < 0 || int(position) >= len(f.items[playlist]):
		return refusedAs(nil)
	}
	moved := f.items[playlist][index]
	f.items[playlist] = slices.Insert(slices.Delete(f.items[playlist], index, index+1), int(position), moved)
	return failure.err
}

func (f *fakeChannel) DeleteItem(ctx context.Context, id youtube.ItemID) error {
	failure, err := f.write(ctx)
	if err != nil {
		return err
	}
	playlist, index := f.findItem(id)
	if index < 0 {
		return refusedAs(youtube.ErrItemNotFound)
	}
	f.remove(playlist, index)
	return failure.err
}

// Videos reads each video the fake has in videoTitles, at 1 unit a request of
// 50, as a read of public videos by id does.
func (f *fakeChannel) Videos(ctx context.Context, ids []youtube.VideoID) ([]youtube.Video, error) {
	if err := f.charge(ctx, pages(len(ids))); err != nil {
		return nil, err
	}
	var videos []youtube.Video
	for _, id := range ids {
		if title, ok := f.videoTitles[id]; ok {
			videos = append(videos, youtube.Video{ID: id, Title: title, ChannelTitle: "Channel", Privacy: "public"})
		}
	}
	return videos, nil
}

// videos is the playlist's videos on the channel, one character each.
func (f *fakeChannel) videos(playlist youtube.PlaylistID) string {
	var videos strings.Builder
	for _, item := range f.items[playlist] {
		videos.WriteString(string(item.VideoID))
	}
	return videos.String()
}

// CreatePlaylist, UpdatePlaylist and DeletePlaylist make the API's writes on the
// channel, at 50 units each, so a test can serve the API over it.
func (f *fakeChannel) CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.Playlist, error) {
	if err := f.charge(ctx, 50); err != nil {
		return youtube.Playlist{}, err
	}
	created := youtube.Playlist{ID: youtube.PlaylistID(fmt.Sprintf("PLnew%d", len(f.playlists))), Title: details.Title, Description: details.Description, Privacy: "private"}
	f.playlists = append(f.playlists, created)
	f.items[created.ID] = []youtube.Item{}
	return created, nil
}

func (f *fakeChannel) UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) (youtube.PlaylistDetails, error) {
	if err := f.charge(ctx, 50); err != nil {
		return youtube.PlaylistDetails{}, err
	}
	if index := slices.IndexFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == id }); index >= 0 {
		f.playlists[index].Title, f.playlists[index].Description = details.Title, details.Description
	}
	return details, nil
}

func (f *fakeChannel) DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error {
	if err := f.charge(ctx, 50); err != nil {
		return err
	}
	if !slices.ContainsFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == id }) {
		return fmt.Errorf("delete playlist %s: %w", id, youtube.ErrPlaylistNotFound)
	}
	f.deletePlaylist(id)
	return nil
}

func (f *fakeChannel) Requests() int64 { return f.requests }

func (f *fakeChannel) Units() int64 { return f.units }

// deletePlaylist removes the playlist from the channel.
func (f *fakeChannel) deletePlaylist(id youtube.PlaylistID) {
	f.playlists = slices.DeleteFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == id })
	delete(f.items, id)
}

// clock is a time a test moves by hand.
type clock struct {
	now time.Time
}

func (c *clock) Now() time.Time { return c.now }

// start is 08:00 in the morning, Pacific, on 2026-09-17.
var start = time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)

// newRunner is a store in a temporary directory and a runner over it and f,
// running hourly on the clock it returns.
func newRunner(t *testing.T, f *fakeChannel) (*Runner, *store.Store, *clock) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &clock{now: start}
	r := NewRunner(st, f, time.Hour)
	r.now = c.Now
	return r, st, c
}

// mustRun makes a run, fails t if it could not be recorded, and fails t unless
// it ended with outcome.
func mustRun(t *testing.T, ctx context.Context, r *Runner, outcome string) Report {
	t.Helper()
	report, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != outcome {
		var failures []string
		for _, f := range report.Failures {
			failures = append(failures, fmt.Sprintf("%s: %v", f.Playlist, f.Err))
		}
		t.Fatalf("outcome %s with failures %v, want %s", report.Outcome, failures, outcome)
	}
	return report
}

// stored is the server's order of the playlist: its videos, one character each,
// and the item id of each entry, empty for one not yet pushed.
func stored(t *testing.T, st *store.Store, playlist youtube.PlaylistID) (string, []string) {
	t.Helper()
	entries, err := store.Entries(context.Background(), st.Queries, string(playlist))
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	var videos strings.Builder
	ids := []string{}
	for _, entry := range entries {
		videos.WriteString(entry.VideoID)
		ids = append(ids, entry.ItemID)
	}
	return videos.String(), ids
}

// itemIDs is the playlist's item ids on the channel, in order.
func (f *fakeChannel) itemIDs(playlist youtube.PlaylistID) []string {
	ids := []string{}
	for _, item := range f.items[playlist] {
		ids = append(ids, string(item.ID))
	}
	return ids
}

// captured is a logger writing JSON lines into the buffer it returns.
func captured() (*slog.Logger, *bytes.Buffer) {
	var logged bytes.Buffer
	return slog.New(slog.NewJSONHandler(&logged, nil)), &logged
}
