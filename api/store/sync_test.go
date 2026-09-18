package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// withPlaylist is a store holding the playlist PLA and the videos a, b and c.
func withPlaylist(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, _ := open(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := st.Queries.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: id, Title: "Video " + id, ChannelTitle: "Channel"}); err != nil {
			t.Fatalf("upsert video %s: %v", id, err)
		}
	}
	playlist := generated.UpsertPlaylistParams{PlaylistID: "PLA", Title: "A", Description: "", Privacy: "private"}
	if err := st.Queries.UpsertPlaylist(ctx, playlist); err != nil {
		t.Fatalf("upsert playlist: %v", err)
	}
	return st
}

// replaceEntries sets the playlist's order to entries at the revision it holds.
func replaceEntries(t *testing.T, st *Store, playlist string, entries ...Entry) error {
	t.Helper()
	ctx := context.Background()
	return st.InTx(ctx, func(tx *Tx) error {
		state, err := tx.GetPlaylistState(ctx, playlist)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceOrder(ctx, playlist, state.Revision, entries)
		return err
	})
}

// refuseWrite records a refused push write to the playlist and holds its push
// on it, returning the write's id.
func refuseWrite(t *testing.T, st *Store, playlist string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := st.InTx(ctx, func(tx *Tx) error {
		var err error
		if id, err = tx.BeginWrite(ctx, Write{Method: youtube.MethodPlaylistItemsDelete, PlaylistID: playlist, ItemID: "i1", SentAt: sent}); err != nil {
			return err
		}
		if err := tx.SettleWrite(ctx, Settlement{WriteID: id, PlaylistID: playlist, Outcome: WriteRefused, SettledAt: sent, Requests: 1, Units: 50, Err: errors.New("forbidden")}); err != nil {
			return err
		}
		return tx.SetRefusedWrite(ctx, generated.SetRefusedWriteParams{PlaylistID: playlist, RefusedWriteID: sql.NullInt64{Int64: id, Valid: true}})
	})
	if err != nil {
		t.Fatalf("refuse a write to %s: %v", playlist, err)
	}
	return id
}

func state(t *testing.T, st *Store, playlist string) generated.GetPlaylistStateRow {
	t.Helper()
	row, err := st.Queries.GetPlaylistState(context.Background(), playlist)
	if err != nil {
		t.Fatalf("read the state of %s: %v", playlist, err)
	}
	return row
}

func entries(t *testing.T, st *Store, playlist string) []Entry {
	t.Helper()
	held, err := Entries(context.Background(), st.Queries, playlist)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return held
}

func count(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOpenSeedsTheSyncOutcomesAndPlaylistPrivacies(t *testing.T) {
	st, _ := open(t)
	if n := count(t, st, "sync_outcomes"); n != len(syncOutcomes) {
		t.Errorf("sync outcomes = %d, want %d", n, len(syncOutcomes))
	}
	if n := count(t, st, "playlist_privacies"); n != len(playlistPrivacies) {
		t.Errorf("playlist privacies = %d, want %d", n, len(playlistPrivacies))
	}
}

// A replacement keeps the id of each entry it names, and gives a new entry the
// next id.
func TestReplaceOrderReplacesTheWholeOrderAndKeepsEachEntrysID(t *testing.T) {
	st := withPlaylist(t)
	if err := replaceEntries(t, st, "PLA", Entry{VideoID: "a", ItemID: "i1"}, Entry{VideoID: "b"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	first := entries(t, st, "PLA")
	if len(first) != 2 || first[0].ID == 0 || first[1].ID == first[0].ID || first[0].ItemID != "i1" || first[1].ItemID != "" {
		t.Fatalf("entries = %+v, want a with item i1 and a pending b, each with an id", first)
	}

	second := []Entry{{ID: first[1].ID, VideoID: "b", ItemID: "i2"}, {VideoID: "a"}}
	if err := replaceEntries(t, st, "PLA", second...); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := entries(t, st, "PLA")
	if len(got) != 2 || got[0] != second[0] || got[1].VideoID != "a" || got[1].ItemID != "" || got[1].ID == 0 || got[1].ID == first[1].ID {
		t.Fatalf("entries = %+v, want b keeping its id with item i2, then a pending a with a new id", got)
	}
}

// A replacement naming a video the store does not hold, or an item another
// entry holds, rolls back, so the playlist keeps the order it had.
func TestAnEntryTheStoreCannotHoldKeepsThePreviousOrder(t *testing.T) {
	st := withPlaylist(t)
	if err := replaceEntries(t, st, "PLA", Entry{VideoID: "a", ItemID: "i1"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	for name, replacement := range map[string][]Entry{
		"an unknown video": {{VideoID: "b", ItemID: "i2"}, {VideoID: "missing"}},
		"one item twice":   {{VideoID: "b", ItemID: "i2"}, {VideoID: "c", ItemID: "i2"}},
	} {
		if err := replaceEntries(t, st, "PLA", replacement...); err == nil {
			t.Errorf("stored entries holding %s", name)
		}
	}
	if got := entries(t, st, "PLA"); len(got) != 1 || got[0].ItemID != "i1" {
		t.Fatalf("entries after the failed replacements = %+v, want i1 alone", got)
	}
}

// The revision counts a change to the order of the videos. An entry taking the
// item a push made leaves it, and a change made at an older revision changes
// nothing.
func TestReplaceOrderCountsAChangeToTheOrderOfTheVideos(t *testing.T) {
	st := withPlaylist(t)
	ctx := context.Background()
	replace := func(revision int64, entries ...Entry) (int64, error) {
		var left int64
		err := st.InTx(ctx, func(tx *Tx) error {
			var err error
			left, err = tx.ReplaceOrder(ctx, "PLA", revision, entries)
			return err
		})
		return left, err
	}
	if revision, err := replace(1, Entry{VideoID: "a", ItemID: "i1"}, Entry{VideoID: "b"}); err != nil || revision != 2 {
		t.Fatalf("a new order = revision %d, %v, want 2", revision, err)
	}
	held := entries(t, st, "PLA")
	held[1].ItemID = "i2"
	if revision, err := replace(2, held...); err != nil || revision != 2 {
		t.Fatalf("an entry taking its item = revision %d, %v, want 2 kept", revision, err)
	}
	if revision, err := replace(2, held...); err != nil || revision != 2 {
		t.Fatalf("the same order = revision %d, %v, want 2 kept", revision, err)
	}
	if _, err := replace(1, held[1], held[0]); !errors.Is(err, ErrRevisionMoved) {
		t.Fatalf("a change of the videos' order at revision 1 = %v, want ErrRevisionMoved", err)
	}
	if _, err := replace(1, Entry{ID: held[0].ID, VideoID: "a", ItemID: "i9"}, held[1]); !errors.Is(err, ErrRevisionMoved) {
		t.Fatalf("a change of an entry's item at revision 1 = %v, want ErrRevisionMoved", err)
	}
	if got := entries(t, st, "PLA"); !slices.Equal(got, held) || state(t, st, "PLA").Revision != 2 {
		t.Fatalf("after the stale change the order is %+v at %d, want %+v at 2", got, state(t, st, "PLA").Revision, held)
	}
}

func TestReplaceBaseReplacesTheWholeBase(t *testing.T) {
	st := withPlaylist(t)
	ctx := context.Background()
	for _, items := range [][]BaseItem{{{ItemID: "i1", VideoID: "a"}, {ItemID: "i2", VideoID: "b"}}, {{ItemID: "i2", VideoID: "b", Placed: true}, {ItemID: "i3", VideoID: "a"}}} {
		if err := st.InTx(ctx, func(tx *Tx) error { return tx.ReplaceBase(ctx, "PLA", items) }); err != nil {
			t.Fatalf("replace the base with %v: %v", items, err)
		}
	}
	got, err := Base(ctx, st.Queries, "PLA")
	if want := []BaseItem{{ItemID: "i2", VideoID: "b", Placed: true}, {ItemID: "i3", VideoID: "a"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("base = %v, %v, want %v", got, err, want)
	}
}

// A push write YouTube refused holds until the base's items or the videos'
// order change. The same items with other placements, and the same order, keep
// it.
func TestAChangeToTheBaseOrTheOrderReleasesTheRefusedWrite(t *testing.T) {
	ctx := context.Background()
	base := []BaseItem{{ItemID: "i1", VideoID: "a"}, {ItemID: "i2", VideoID: "b"}}
	// Each change is made to the order held, at the revision it is at.
	cases := []struct {
		name     string
		change   func(tx *Tx, order []Entry, revision int64) error
		released bool
	}{
		{"the same base", func(tx *Tx, _ []Entry, _ int64) error { return tx.ReplaceBase(ctx, "PLA", base) }, false},
		{"the same items placed", func(tx *Tx, _ []Entry, _ int64) error {
			return tx.ReplaceBase(ctx, "PLA", []BaseItem{{ItemID: "i1", VideoID: "a", Placed: true}, {ItemID: "i2", VideoID: "b"}})
		}, false},
		{"another item", func(tx *Tx, _ []Entry, _ int64) error {
			return tx.ReplaceBase(ctx, "PLA", []BaseItem{{ItemID: "i1", VideoID: "a"}})
		}, true},
		{"the same order", func(tx *Tx, order []Entry, revision int64) error {
			_, err := tx.ReplaceOrder(ctx, "PLA", revision, order)
			return err
		}, false},
		{"another order", func(tx *Tx, order []Entry, revision int64) error {
			_, err := tx.ReplaceOrder(ctx, "PLA", revision, []Entry{order[1], order[0]})
			return err
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := withPlaylist(t)
			if err := st.InTx(ctx, func(tx *Tx) error { return tx.ReplaceBase(ctx, "PLA", base) }); err != nil {
				t.Fatalf("replace the base: %v", err)
			}
			if err := replaceEntries(t, st, "PLA", Entry{VideoID: "a", ItemID: "i1"}, Entry{VideoID: "b", ItemID: "i2"}); err != nil {
				t.Fatalf("replace the order: %v", err)
			}
			refused := refuseWrite(t, st, "PLA")
			order, revision := entries(t, st, "PLA"), state(t, st, "PLA").Revision
			if err := st.InTx(ctx, func(tx *Tx) error { return c.change(tx, order, revision) }); err != nil {
				t.Fatalf("change: %v", err)
			}
			held := state(t, st, "PLA").RefusedWriteID
			if c.released && held.Valid || !c.released && held.Int64 != refused {
				t.Fatalf("refused write = %+v after the change, want write %d released %v", held, refused, c.released)
			}
		})
	}
}

func TestDeletingAPlaylistDeletesItsEntriesAndBase(t *testing.T) {
	st := withPlaylist(t)
	ctx := context.Background()
	if err := replaceEntries(t, st, "PLA", Entry{VideoID: "a", ItemID: "i1"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := st.InTx(ctx, func(tx *Tx) error { return tx.ReplaceBase(ctx, "PLA", []BaseItem{{ItemID: "i1", VideoID: "a"}}) }); err != nil {
		t.Fatalf("replace the base: %v", err)
	}
	if err := st.Queries.DeletePlaylist(ctx, "PLA"); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	if n, m := count(t, st, "playlist_entries"), count(t, st, "base_items"); n != 0 || m != 0 {
		t.Fatalf("%d entries and %d base items after the playlist was deleted, want none", n, m)
	}
}

func TestARevisionCountsOnlyFromTheRevisionHeld(t *testing.T) {
	st := withPlaylist(t)
	ctx := context.Background()
	held, err := st.Queries.GetPlaylistState(ctx, "PLA")
	if err != nil || held.Revision != 1 || held.Sort != SortManual || held.UnansweredWriteID.Valid || held.RefusedWriteID.Valid {
		t.Fatalf("a new playlist's state = %+v, %v, want revision 1, sorted manually, with no write unanswered or refused", held, err)
	}
	bump := generated.BumpRevisionParams{PlaylistID: "PLA", Revision: 1}
	if n, err := st.Queries.BumpRevision(ctx, bump); err != nil || n != 1 {
		t.Fatalf("BumpRevision from 1 = %d, %v, want 1 row", n, err)
	}
	if n, err := st.Queries.BumpRevision(ctx, bump); err != nil || n != 0 {
		t.Fatalf("BumpRevision from 1 again = %d, %v, want no row, since the playlist is at 2", n, err)
	}
}

// A database whose playlists were stored before the server kept its own order
// takes each playlist's items as both its entries and its base.
func TestMigratingKeepsEachPlaylistsItemsAsItsEntriesAndBase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "api.db")
	db, err := sql.Open("sqlite", URI(path, "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO playlist_privacies (privacy, label, description) VALUES ('private', 'Private', '')`,
		`INSERT INTO playlists (playlist_id, title, description, privacy) VALUES ('PLA', 'A', '', 'private')`,
		`INSERT INTO videos (video_id, title, channel_title, is_unavailable) VALUES ('a', 'A', 'C', 0), ('b', 'B', 'C', 0)`,
		`INSERT INTO playlist_items (item_id, playlist_id, position, video_id) VALUES ('i2', 'PLA', 1, 'a'), ('i1', 'PLA', 0, 'b')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	_ = db.Close()

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	held := entries(t, st, "PLA")
	if len(held) != 2 || held[0].ItemID != "i1" || held[0].VideoID != "b" || held[1].ItemID != "i2" || held[1].VideoID != "a" {
		t.Fatalf("entries = %+v, want i1 holding b, then i2 holding a", held)
	}
	base, err := Base(ctx, st.Queries, "PLA")
	if want := []BaseItem{{ItemID: "i1", VideoID: "b"}, {ItemID: "i2", VideoID: "a"}}; err != nil || !slices.Equal(base, want) {
		t.Fatalf("base = %v, %v, want %v", base, err, want)
	}
	if held := state(t, st, "PLA"); held.Revision != 1 || held.Sort != SortManual || held.UnansweredWriteID.Valid || held.RefusedWriteID.Valid {
		t.Fatalf("state = %+v, want revision 1, sorted manually, with no write unanswered or refused", held)
	}
}

func TestAPlaylistWithAnUnknownPrivacyIsRefused(t *testing.T) {
	st, _ := open(t)
	err := st.Queries.UpsertPlaylist(context.Background(), generated.UpsertPlaylistParams{PlaylistID: "PLA", Title: "A", Privacy: "someFuturePrivacy"})
	if err == nil {
		t.Fatal("stored a playlist with a privacy the vocabulary does not hold")
	}
}

func TestAReadOfAnUnavailableVideoKeepsItsStoredTitle(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.SeedVideo(ctx, generated.SeedVideoParams{
		VideoID: "v1", Title: "A Mix", ChannelTitle: "A Channel", Description: sql.NullString{String: "tracklist", Valid: true},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	if err := st.Queries.UpsertUnavailableVideo(ctx, generated.UpsertUnavailableVideoParams{VideoID: "v1", Title: "Private video"}); err != nil {
		t.Fatalf("upsert unavailable: %v", err)
	}
	got, err := st.Queries.GetVideo(ctx, "v1")
	if err != nil || got.Title != "A Mix" || got.ChannelTitle != "A Channel" || !got.IsUnavailable {
		t.Fatalf("video %+v, %v, want A Mix by A Channel marked unavailable", got, err)
	}

	if err := st.Queries.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: "v1", Title: "A Mix, Renamed", ChannelTitle: "A Channel"}); err != nil {
		t.Fatalf("upsert available: %v", err)
	}
	got, err = st.Queries.GetVideo(ctx, "v1")
	if err != nil || got.Title != "A Mix, Renamed" || got.IsUnavailable || got.Description.String != "tracklist" {
		t.Fatalf("video %+v, %v, want the new title, available, with its description kept", got, err)
	}
}

func TestAnUnavailableVideoTheStoreHasNeverSeenTakesTheReadTitle(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.UpsertUnavailableVideo(ctx, generated.UpsertUnavailableVideoParams{VideoID: "v1", Title: "Deleted video"}); err != nil {
		t.Fatalf("upsert unavailable: %v", err)
	}
	got, err := st.Queries.GetVideo(ctx, "v1")
	if err != nil || got.Title != "Deleted video" || got.ChannelTitle != "" || !got.IsUnavailable {
		t.Fatalf("video %+v, %v, want Deleted video with no channel, unavailable", got, err)
	}
}

func TestQuotaRefusalsCountOnlyTheirDate(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	runs := []struct {
		date    string
		outcome string
	}{
		{"2026-09-16", OutcomeQuotaSpent},
		{"2026-09-17", OutcomeOK},
		{"2026-09-17", OutcomePartial},
	}
	for _, run := range runs {
		_, err := st.Queries.InsertSyncRun(ctx, generated.InsertSyncRunParams{
			StartedTs: run.date + "T08:00:00Z", FinishedTs: run.date + "T08:01:00Z", QuotaDate: run.date, Outcome: run.outcome,
		})
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}
	if spent, err := st.Queries.CountQuotaSpentRuns(ctx, "2026-09-17"); err != nil || spent != 0 {
		t.Errorf("quota refusals on 2026-09-17 = %d, %v, want 0", spent, err)
	}
	if spent, err := st.Queries.CountQuotaSpentRuns(ctx, "2026-09-16"); err != nil || spent != 1 {
		t.Errorf("quota refusals on 2026-09-16 = %d, %v, want 1", spent, err)
	}
}

func TestARunWithAnUnknownOutcomeIsRefused(t *testing.T) {
	st, _ := open(t)
	_, err := st.Queries.InsertSyncRun(context.Background(), generated.InsertSyncRunParams{
		StartedTs: "2026-09-17T08:00:00Z", FinishedTs: "2026-09-17T08:01:00Z", QuotaDate: "2026-09-17", Outcome: "unseeded",
	})
	if err == nil {
		t.Fatal("stored a run with an outcome the vocabulary does not hold")
	}
}
