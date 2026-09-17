package reconcile

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeChannel is a channel's playlists held in memory, answering each call as
// youtube.Channel does: an insert takes a position up to the item count and a
// move one less, a gone item is ErrItemNotFound, and the refusals Channel names
// come back wrapped in their sentinels. Each list call costs 1 unit a page of 50
// and each write 50.
type fakeChannel struct {
	playlists []youtube.Playlist
	items     map[youtube.PlaylistID][]youtube.Item
	// unlisted holds playlists Playlists leaves out while ExistingPlaylists still
	// finds them.
	unlisted map[youtube.PlaylistID]bool
	// hidden holds items Items leaves out while ExistingItems still finds them,
	// as a delete and an add between two page requests leave a read.
	hidden map[youtube.ItemID]bool
	// refusedVideos is the refusal an insert of each video draws.
	refusedVideos map[youtube.VideoID]error
	// notSortedManually holds the playlists that refuse a write naming a position.
	notSortedManually map[youtube.PlaylistID]bool
	// itemsErrors is the error Items of a playlist returns.
	itemsErrors map[youtube.PlaylistID]error
	// playlistsError is the error Playlists returns.
	playlistsError error
	// quota is how many units are served before every request is refused with
	// ErrQuotaSpent. Zero serves without limit.
	quota int64
	// beforeWrite runs before each write, with the write's number counting from 1.
	beforeWrite func(n int)
	// beforeList runs before each Playlists call, with its number counting from 1.
	beforeList func(n int)

	writes, lists   int
	requests, units int64
	nextItem        int
}

// newFakeChannel holds a playlist per entry of playlists, named by id and holding
// one item per character of the string.
func newFakeChannel(playlists map[youtube.PlaylistID]string) *fakeChannel {
	f := &fakeChannel{
		items:             map[youtube.PlaylistID][]youtube.Item{},
		unlisted:          map[youtube.PlaylistID]bool{},
		hidden:            map[youtube.ItemID]bool{},
		refusedVideos:     map[youtube.VideoID]error{},
		notSortedManually: map[youtube.PlaylistID]bool{},
		itemsErrors:       map[youtube.PlaylistID]error{},
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

func (f *fakeChannel) add(playlist youtube.PlaylistID, video youtube.VideoID, position int) youtube.ItemID {
	f.nextItem++
	item := youtube.Item{
		ID:           youtube.ItemID(fmt.Sprintf("%s-i%03d", playlist, f.nextItem)),
		PlaylistID:   playlist,
		VideoID:      video,
		Title:        "Video " + string(video),
		ChannelTitle: "Channel",
	}
	f.items[playlist] = slices.Insert(f.items[playlist], position, item)
	return item.ID
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

func (f *fakeChannel) write(ctx context.Context) error {
	f.writes++
	if f.beforeWrite != nil {
		f.beforeWrite(f.writes)
	}
	return f.charge(ctx, youtube.WriteUnits)
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
	return slices.DeleteFunc(slices.Clone(f.playlists), func(p youtube.Playlist) bool { return f.unlisted[p.ID] }), nil
}

func (f *fakeChannel) Items(ctx context.Context, playlist youtube.PlaylistID) ([]youtube.Item, error) {
	held, ok := f.items[playlist]
	if err := f.charge(ctx, pages(len(held))); err != nil {
		return nil, err
	}
	if err := f.itemsErrors[playlist]; err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("list items of playlist %s: %w", playlist, youtube.ErrPlaylistNotFound)
	}
	var read []youtube.Item
	for i, item := range held {
		item.Position = int64(i)
		if !f.hidden[item.ID] {
			read = append(read, item)
		}
	}
	return read, nil
}

func (f *fakeChannel) ExistingPlaylists(ctx context.Context, ids []youtube.PlaylistID) ([]youtube.PlaylistID, error) {
	if len(ids) > 0 {
		if err := f.charge(ctx, pages(len(ids))); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(slices.Clone(ids), func(id youtube.PlaylistID) bool {
		return !slices.ContainsFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == id })
	}), nil
}

func (f *fakeChannel) ExistingItems(ctx context.Context, ids []youtube.ItemID) ([]youtube.ItemID, error) {
	if len(ids) > 0 {
		if err := f.charge(ctx, pages(len(ids))); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(slices.Clone(ids), func(id youtube.ItemID) bool {
		_, _, found := f.find(id)
		return !found
	}), nil
}

func (f *fakeChannel) find(id youtube.ItemID) (youtube.PlaylistID, int, bool) {
	for playlist, items := range f.items {
		if at := slices.IndexFunc(items, func(it youtube.Item) bool { return it.ID == id }); at >= 0 {
			return playlist, at, true
		}
	}
	return "", 0, false
}

func (f *fakeChannel) insert(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int, positioned bool) (youtube.ItemID, error) {
	if err := f.write(ctx); err != nil {
		return "", err
	}
	held, ok := f.items[playlist]
	switch {
	case !ok:
		return "", fmt.Errorf("insert: %w", youtube.ErrPlaylistNotFound)
	case f.refusedVideos[video] != nil:
		return "", fmt.Errorf("insert %s: %w", video, f.refusedVideos[video])
	case positioned && f.notSortedManually[playlist]:
		return "", fmt.Errorf("insert: %w", youtube.ErrManualSortRequired)
	case position < 0 || position > len(held):
		return "", fmt.Errorf("insert at %d into %d items: badRequest", position, len(held))
	}
	return f.add(playlist, video, position), nil
}

func (f *fakeChannel) InsertItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error) {
	return f.insert(ctx, playlist, video, int(position), true)
}

func (f *fakeChannel) AppendItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID) (youtube.ItemID, error) {
	return f.insert(ctx, playlist, video, len(f.items[playlist]), false)
}

func (f *fakeChannel) MoveItem(ctx context.Context, item youtube.Item, position int64) error {
	if err := f.write(ctx); err != nil {
		return err
	}
	playlist, at, found := f.find(item.ID)
	switch {
	case f.notSortedManually[item.PlaylistID]:
		return fmt.Errorf("move: %w", youtube.ErrManualSortRequired)
	case !found:
		return fmt.Errorf("move %s: %w", item.ID, youtube.ErrItemNotFound)
	case playlist != item.PlaylistID || position < 0 || int(position) >= len(f.items[playlist]):
		return fmt.Errorf("move %s to %d: invalidPlaylistItemPosition", item.ID, position)
	}
	held := f.items[playlist][at]
	f.items[playlist] = slices.Insert(slices.Delete(f.items[playlist], at, at+1), int(position), held)
	return nil
}

func (f *fakeChannel) DeleteItem(ctx context.Context, id youtube.ItemID) error {
	if err := f.write(ctx); err != nil {
		return err
	}
	playlist, at, found := f.find(id)
	if !found {
		return fmt.Errorf("delete %s: %w", id, youtube.ErrItemNotFound)
	}
	f.items[playlist] = slices.Delete(f.items[playlist], at, at+1)
	return nil
}

func (f *fakeChannel) Requests() int64 { return f.requests }

func (f *fakeChannel) Units() int64 { return f.units }

// videos is the playlist's videos in order, one character each.
func (f *fakeChannel) videos(playlist youtube.PlaylistID) string {
	var b strings.Builder
	for _, item := range f.items[playlist] {
		b.WriteString(string(item.VideoID))
	}
	return b.String()
}

// baseOf is the playlist as a base: its item ids and videos, in order.
func (f *fakeChannel) baseOf(playlist youtube.PlaylistID) []generated.ListBaseItemsRow {
	rows := []generated.ListBaseItemsRow{}
	for _, item := range f.items[playlist] {
		rows = append(rows, generated.ListBaseItemsRow{ItemID: string(item.ID), VideoID: string(item.VideoID)})
	}
	return rows
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

// mustRun makes a run and fails t if it could not be recorded.
func mustRun(t *testing.T, ctx context.Context, r *Runner) Report {
	t.Helper()
	report, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// order is the server's order of the playlist, one character a video.
func order(t *testing.T, st *store.Store, playlist youtube.PlaylistID) string {
	t.Helper()
	ids, err := st.Queries.ListPlaylistVideoIDs(context.Background(), string(playlist))
	if err != nil {
		t.Fatalf("list order: %v", err)
	}
	return strings.Join(ids, "")
}

func base(t *testing.T, st *store.Store, playlist youtube.PlaylistID) []generated.ListBaseItemsRow {
	t.Helper()
	rows, err := st.Queries.ListBaseItems(context.Background(), string(playlist))
	if err != nil {
		t.Fatalf("list base: %v", err)
	}
	if rows == nil {
		rows = []generated.ListBaseItemsRow{}
	}
	return rows
}

// edit sets the server's order of the playlist to videos, one character each,
// storing any video it does not hold yet.
func edit(t *testing.T, st *store.Store, playlist youtube.PlaylistID, videos string) {
	t.Helper()
	ctx := context.Background()
	ids := strings.Split(videos, "")[:len(videos)]
	err := st.InTx(ctx, func(tx *store.Tx) error {
		for _, id := range ids {
			if _, err := tx.GetVideo(ctx, id); err == nil {
				continue
			}
			if err := tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: id, Title: "Video " + id, ChannelTitle: "Channel"}); err != nil {
				return err
			}
		}
		return tx.ReplacePlaylistItems(ctx, string(playlist), ids)
	})
	if err != nil {
		t.Fatalf("edit %s: %v", playlist, err)
	}
}

func assertOutcome(t *testing.T, report Report, want string) {
	t.Helper()
	if report.Outcome != want {
		var failures []string
		for _, f := range report.Failures {
			failures = append(failures, fmt.Sprintf("%s: %v", f.Playlist, f.Err))
		}
		t.Fatalf("outcome %s with failures %v, want %s", report.Outcome, failures, want)
	}
}
