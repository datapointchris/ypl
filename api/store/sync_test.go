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

func inTx(t *testing.T, st *Store, fn func(ctx context.Context, tx *Tx) error) error {
	t.Helper()
	ctx := context.Background()
	return st.InTx(ctx, func(tx *Tx) error { return fn(ctx, tx) })
}

func TestOpenSeedsTheSyncOutcomes(t *testing.T) {
	st, _ := open(t)
	var n int
	if err := st.db.QueryRowContext(context.Background(), "SELECT count(*) FROM sync_outcomes").Scan(&n); err != nil {
		t.Fatalf("count sync outcomes: %v", err)
	}
	if n != len(syncOutcomes) {
		t.Fatalf("sync outcomes = %d, want %d", n, len(syncOutcomes))
	}
}

func TestReplacePlaylistItemsReplacesTheWholeOrder(t *testing.T) {
	st := withPlaylist(t)
	for _, order := range [][]string{{"a", "b", "c"}, {"c", "a"}} {
		if err := inTx(t, st, func(ctx context.Context, tx *Tx) error { return tx.ReplacePlaylistItems(ctx, "PLA", order) }); err != nil {
			t.Fatalf("replace with %v: %v", order, err)
		}
	}
	got, err := st.Queries.ListPlaylistVideoIDs(context.Background(), "PLA")
	if err != nil || !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("order = %v, %v, want [c a]", got, err)
	}
}

func TestReplaceBaseItemsReplacesTheWholeBase(t *testing.T) {
	st := withPlaylist(t)
	first := []BaseItem{{ItemID: "i1", VideoID: "a"}, {ItemID: "i2", VideoID: "b"}}
	second := []BaseItem{{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}
	for _, base := range [][]BaseItem{first, second} {
		if err := inTx(t, st, func(ctx context.Context, tx *Tx) error { return tx.ReplaceBaseItems(ctx, "PLA", base) }); err != nil {
			t.Fatalf("replace with %v: %v", base, err)
		}
	}
	rows, err := st.Queries.ListBaseItems(context.Background(), "PLA")
	if err != nil {
		t.Fatalf("list base: %v", err)
	}
	want := []generated.ListBaseItemsRow{{ItemID: "i2", VideoID: "b"}, {ItemID: "i3", VideoID: "a"}}
	if !slices.Equal(rows, want) {
		t.Fatalf("base = %v, want %v", rows, want)
	}
}

// A replacement naming a video the store does not hold rolls back, so the
// playlist keeps the order it had.
func TestAnOrderNamingAnUnknownVideoKeepsThePreviousOrder(t *testing.T) {
	st := withPlaylist(t)
	if err := inTx(t, st, func(ctx context.Context, tx *Tx) error { return tx.ReplacePlaylistItems(ctx, "PLA", []string{"a"}) }); err != nil {
		t.Fatalf("replace: %v", err)
	}
	err := inTx(t, st, func(ctx context.Context, tx *Tx) error {
		return tx.ReplacePlaylistItems(ctx, "PLA", []string{"b", "missing"})
	})
	if err == nil {
		t.Fatal("stored an order naming a video the store does not hold")
	}
	got, err := st.Queries.ListPlaylistVideoIDs(context.Background(), "PLA")
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("order after the failed replacement = %v, %v, want [a]", got, err)
	}
}

func TestDeletingAPlaylistDeletesItsItemsBaseAndRefusals(t *testing.T) {
	ctx := context.Background()
	st := withPlaylist(t)
	err := inTx(t, st, func(ctx context.Context, tx *Tx) error {
		if err := tx.ReplacePlaylistItems(ctx, "PLA", []string{"a", "b"}); err != nil {
			return err
		}
		return tx.ReplaceBaseItems(ctx, "PLA", []BaseItem{{ItemID: "i1", VideoID: "a"}})
	})
	if err != nil {
		t.Fatalf("store items and base: %v", err)
	}
	refusal := generated.UpsertPushRefusalParams{PlaylistID: "PLA", VideoID: "gone", RefusedTs: "2026-09-17T00:00:00Z", Reason: "videoNotFound"}
	if err := st.Queries.UpsertPushRefusal(ctx, refusal); err != nil {
		t.Fatalf("upsert refusal: %v", err)
	}

	if err := st.Queries.DeletePlaylist(ctx, "PLA"); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	for _, table := range []string{"playlist_items", "base_items", "push_refusals"} {
		var n int
		if err := st.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s holds %d rows after the playlist was deleted", table, n)
		}
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

func TestTheDaysWriteUnitsAndQuotaRefusalsCountOnlyThatDate(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	runs := []struct {
		date    string
		outcome string
		units   int64
	}{
		{"2026-09-16", OutcomeQuotaSpent, 900},
		{"2026-09-17", OutcomeOK, 150},
		{"2026-09-17", OutcomePartial, 50},
	}
	for _, run := range runs {
		_, err := st.Queries.InsertSyncRun(ctx, generated.InsertSyncRunParams{
			StartedTs: run.date + "T08:00:00Z", FinishedTs: run.date + "T08:01:00Z", QuotaDate: run.date,
			Outcome: run.outcome, WriteUnits: run.units,
		})
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}

	if units, err := st.Queries.SumWriteUnits(ctx, "2026-09-17"); err != nil || units != 200 {
		t.Errorf("write units on 2026-09-17 = %d, %v, want 200", units, err)
	}
	if units, err := st.Queries.SumWriteUnits(ctx, "2026-09-18"); err != nil || units != 0 {
		t.Errorf("write units on a date with no run = %d, %v, want 0", units, err)
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
