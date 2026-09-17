package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/datapointchris/ypl/api/store/generated"
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

func replaceEntries(t *testing.T, st *Store, playlist string, entries ...Entry) error {
	t.Helper()
	ctx := context.Background()
	return st.InTx(ctx, func(tx *Tx) error { return tx.ReplaceEntries(ctx, playlist, entries) })
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
func TestReplaceEntriesReplacesTheWholeOrderAndKeepsEachEntrysID(t *testing.T) {
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

func TestReplaceBaseReplacesTheWholeBase(t *testing.T) {
	st := withPlaylist(t)
	ctx := context.Background()
	for _, items := range [][]BaseItem{{{ItemID: "i1", VideoID: "a"}, {ItemID: "i2", VideoID: "b"}}, {{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}} {
		if err := st.InTx(ctx, func(tx *Tx) error { return tx.ReplaceBase(ctx, "PLA", items) }); err != nil {
			t.Fatalf("replace the base with %v: %v", items, err)
		}
	}
	got, err := Base(ctx, st.Queries, "PLA")
	if want := []BaseItem{{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("base = %v, %v, want %v", got, err, want)
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
	state, err := st.Queries.GetPlaylistState(ctx, "PLA")
	if err != nil || state.Revision != 1 || state.Sort != SortManual || state.BaseState != BaseCurrent {
		t.Fatalf("a new playlist's state = %+v, %v, want revision 1, sorted manually, with a current base", state, err)
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
	if state, err := st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Revision != 1 || state.Sort != SortManual || state.BaseState != BaseCurrent {
		t.Fatalf("state = %+v, %v, want revision 1, sorted manually, with a current base", state, err)
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
	if err := st.Queries.ImportVideo(ctx, generated.ImportVideoParams{
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
