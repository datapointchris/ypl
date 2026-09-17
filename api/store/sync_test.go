package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"

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

func replaceItems(t *testing.T, st *Store, playlist string, items ...PlaylistItem) error {
	t.Helper()
	ctx := context.Background()
	return st.InTx(ctx, func(tx *Tx) error { return tx.ReplacePlaylistItems(ctx, playlist, items) })
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

func TestReplacePlaylistItemsReplacesTheWholePlaylist(t *testing.T) {
	st := withPlaylist(t)
	first := []PlaylistItem{{ItemID: "i1", VideoID: "a"}, {ItemID: "i2", VideoID: "b"}}
	second := []PlaylistItem{{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}
	for _, items := range [][]PlaylistItem{first, second} {
		if err := replaceItems(t, st, "PLA", items...); err != nil {
			t.Fatalf("replace with %v: %v", items, err)
		}
	}
	rows, err := st.Queries.ListPlaylistItems(context.Background(), "PLA")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	want := []generated.ListPlaylistItemsRow{{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}
	if !slices.Equal(rows, want) {
		t.Fatalf("items = %v, want %v", rows, want)
	}
}

// A replacement naming a video the store does not hold rolls back, so the
// playlist keeps the items it had.
func TestItemsNamingAnUnknownVideoKeepThePreviousItems(t *testing.T) {
	st := withPlaylist(t)
	if err := replaceItems(t, st, "PLA", PlaylistItem{ItemID: "i1", VideoID: "a"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := replaceItems(t, st, "PLA", PlaylistItem{ItemID: "i2", VideoID: "b"}, PlaylistItem{ItemID: "i3", VideoID: "missing"}); err == nil {
		t.Fatal("stored an item naming a video the store does not hold")
	}
	rows, err := st.Queries.ListPlaylistItems(context.Background(), "PLA")
	if err != nil || len(rows) != 1 || rows[0].ItemID != "i1" {
		t.Fatalf("items after the failed replacement = %v, %v, want i1 alone", rows, err)
	}
}

func TestDeletingAPlaylistDeletesItsItems(t *testing.T) {
	st := withPlaylist(t)
	if err := replaceItems(t, st, "PLA", PlaylistItem{ItemID: "i1", VideoID: "a"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := st.Queries.DeletePlaylist(context.Background(), "PLA"); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	if n := count(t, st, "playlist_items"); n != 0 {
		t.Fatalf("playlist_items holds %d rows after the playlist was deleted", n)
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
