package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// apiWrote records a write the API made to playlist with method, applied and
// settled at settledAt, and makes change to the store in the transaction that
// settles it, as the API does.
func apiWrote(t *testing.T, st *store.Store, method string, playlist youtube.PlaylistID, settledAt time.Time, change func(context.Context, *store.Tx) error) {
	t.Helper()
	ctx := context.Background()
	id, err := st.BeginWrite(ctx, method, string(playlist), settledAt)
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	err = st.InTx(ctx, func(tx *store.Tx) error {
		settlement := store.Settlement{WriteID: id, PlaylistID: string(playlist), Outcome: store.WriteApplied, SettledAt: settledAt, Requests: 1, Units: 50}
		if err := tx.SettleWrite(ctx, settlement); err != nil {
			return err
		}
		return change(ctx, tx)
	})
	if err != nil {
		t.Fatalf("record the API's %s of %s: %v", method, playlist, err)
	}
}

func renamed(title string) func(context.Context, *store.Tx) error {
	return func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.UpdatePlaylistDetails(ctx, generated.UpdatePlaylistDetailsParams{PlaylistID: "PLA", Title: title, Description: "New"})
		return err
	}
}

func created(id youtube.PlaylistID) func(context.Context, *store.Tx) error {
	return func(ctx context.Context, tx *store.Tx) error {
		return tx.UpsertPlaylist(ctx, generated.UpsertPlaylistParams{PlaylistID: string(id), Title: "New", Privacy: "private"})
	}
}

func deleted(id youtube.PlaylistID) func(context.Context, *store.Tx) error {
	return func(ctx context.Context, tx *store.Tx) error { return tx.DeletePlaylist(ctx, string(id)) }
}

// The API renames PLA while the run reads PLA's items, after a listing that
// carries the old title.
func TestARunKeepsTheDetailsOfARenameMadeAfterItsListing(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.add("PLA", "d", 3)
	written := false
	f.beforeItems = func(int) {
		if !written {
			apiWrote(t, st, youtube.MethodPlaylistsUpdate, "PLA", c.now, renamed("Renamed"))
			written = true
		}
	}

	report := mustRun(t, ctx, r, store.OutcomeOK)
	playlist, err := st.Queries.GetPlaylist(ctx, "PLA")
	if err != nil || playlist.Title != "Renamed" || playlist.Description != "New" || report.Playlists != 1 {
		t.Fatalf("PLA = %+v, %v with report %+v, want the API's rename kept and the playlist stored", playlist, err, report)
	}
	if videos, _ := stored(t, st, "PLA"); videos != "abcd" {
		t.Fatalf("PLA holds %q, want the items the run read, abcd", videos)
	}

	c.now = c.now.Add(youtube.ReadLag + time.Second)
	f.playlists[0].Title = "Renamed on YouTube"
	mustRun(t, ctx, r, store.OutcomeOK)
	if playlist, err := st.Queries.GetPlaylist(ctx, "PLA"); err != nil || playlist.Title != "Renamed on YouTube" {
		t.Fatalf("PLA = %+v, %v after the read lag, want the title YouTube holds", playlist, err)
	}
}

// YouTube does not show a playlist to any read in the seconds after its create,
// and then lists it before it lists its items.
func TestARunKeepsAPlaylistTheAPICreatedBeforeYouTubeShowsIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.playlists = append(f.playlists, youtube.Playlist{ID: "PLnew", Title: "New", Privacy: "private"})
	f.items["PLnew"] = []youtube.Item{}
	f.unseen["PLnew"] = true
	apiWrote(t, st, youtube.MethodPlaylistsInsert, "PLnew", c.now, created("PLnew"))

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(ctx, "PLnew"); err != nil || report.PlaylistsDeleted != 0 || f.byIDReads != 1 {
		t.Fatalf("PLnew lookup = %v with report %+v after %d reads by id, want it kept after one", err, report, f.byIDReads)
	}

	delete(f.unseen, "PLnew")
	f.itemsErrors["PLnew"] = fmt.Errorf("list items of playlist PLnew: %w", youtube.ErrPlaylistNotFound)
	report = mustRun(t, ctx, r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(ctx, "PLnew"); err != nil || report.PlaylistsDeleted != 0 {
		t.Fatalf("PLnew lookup = %v with report %+v, want it kept while its items are unseen", err, report)
	}
}

// The API deletes PLA while the run reads its items, and YouTube still answers
// the listing and the item read with PLA.
func TestARunDoesNotRestoreAPlaylistTheAPIDeletedAfterItsListing(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	written := false
	f.beforeItems = func(int) {
		if !written {
			apiWrote(t, st, youtube.MethodPlaylistsDelete, "PLA", c.now, deleted("PLA"))
			written = true
		}
	}

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(ctx, "PLA"); !errors.Is(err, sql.ErrNoRows) || report.Playlists != 1 {
		t.Fatalf("PLA lookup = %v with report %+v, want it still deleted and PLB alone stored", err, report)
	}
}

// A listing is not a snapshot, so a playlist missing only from it is kept.
func TestAPlaylistMissingOnlyFromTheListingIsKept(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)

	f.unlisted["PLB"] = true
	report := mustRun(t, ctx, r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(ctx, "PLB"); err != nil || report.PlaylistsDeleted != 0 || f.byIDReads != 1 {
		t.Fatalf("PLB lookup = %v with report %+v after %d reads by id, want it kept once its read by id finds it", err, report, f.byIDReads)
	}

	f.byIDErrors["PLB"] = errors.New("connection reset")
	report = mustRun(t, ctx, r, store.OutcomePartial)
	if _, err := st.Queries.GetPlaylist(ctx, "PLB"); err != nil || report.PlaylistsSkipped != 1 || report.Failures[0].Playlist != "PLB" {
		t.Fatalf("PLB lookup = %v with report %+v, want it kept and skipped when its read by id fails", err, report)
	}

	f.byIDErrors["PLB"] = fmt.Errorf("read playlist PLB: %w", youtube.ErrQuotaSpent)
	mustRun(t, ctx, r, store.OutcomeQuotaSpent)
}

func TestAPlaylistGoneFromEveryReadIsDeleted(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.deletePlaylist("PLB")

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(ctx, "PLB"); !errors.Is(err, sql.ErrNoRows) || report.PlaylistsDeleted != 1 || f.byIDReads != 1 {
		t.Fatalf("PLB lookup = %v with report %+v after %d reads by id, want it deleted after one", err, report, f.byIDReads)
	}
}

// Once the read lag has passed since a write, a read shows it, so the write no
// longer holds back what the run stores.
func TestAWriteOlderThanTheReadLagDoesNotHoldBackARun(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	before := c.now.Add(-youtube.ReadLag - time.Second)
	apiWrote(t, st, youtube.MethodPlaylistsUpdate, "PLA", before, renamed("Renamed"))
	apiWrote(t, st, youtube.MethodPlaylistsInsert, "PLold", before, created("PLold"))

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if playlist, err := st.Queries.GetPlaylist(ctx, "PLA"); err != nil || playlist.Title != "Playlist PLA" {
		t.Fatalf("PLA = %+v, %v, want the title YouTube holds", playlist, err)
	}
	if _, err := st.Queries.GetPlaylist(ctx, "PLold"); !errors.Is(err, sql.ErrNoRows) || report.PlaylistsDeleted != 1 {
		t.Fatalf("PLold lookup = %v with report %+v, want it deleted", err, report)
	}
}
