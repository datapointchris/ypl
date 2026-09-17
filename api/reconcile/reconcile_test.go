package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

func TestAFirstRunTakesEveryPlaylistAsYouTubeHoldsIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if report.Playlists != 2 || report.Writes != 0 || f.writes != 0 {
		t.Fatalf("report %+v after %d writes, want 2 playlists and no write", report, f.writes)
	}
	if got := order(t, st, "PLA"); got != "abc" {
		t.Errorf("PLA order = %q, want abc", got)
	}
	if got, want := base(t, st, "PLA"), f.baseOf("PLA"); !slices.Equal(got, want) {
		t.Errorf("PLA base = %v, want %v", got, want)
	}
	video, err := st.Queries.GetVideo(context.Background(), "d")
	if err != nil || video.Title != "Video d" {
		t.Errorf("video d = %+v, %v, want it stored with its title", video, err)
	}
}

func TestARunWithNothingChangedWritesNothing(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, _, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if report.Writes != 0 || report.PulledIn != 0 || report.PulledOut != 0 || f.writes != 0 {
		t.Fatalf("report %+v after %d writes, want nothing pulled or written", report, f.writes)
	}
}

func TestEditsOnYouTubeArriveHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	_ = f.DeleteItem(context.Background(), f.items["PLA"][1].ID)
	f.add("PLA", "g", 0)
	f.writes = 0

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if got := order(t, st, "PLA"); got != "gac" {
		t.Fatalf("order = %q, want gac", got)
	}
	if report.PulledIn != 1 || report.PulledOut != 1 || f.writes != 0 {
		t.Fatalf("report %+v after %d writes, want one pulled in, one pulled out, no write", report, f.writes)
	}
}

// Removing b, moving c to the front and inserting x is three writes.
func TestEditsHereArePushedWithTheFewestWrites(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "cax")

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if got := f.videos("PLA"); got != "cax" {
		t.Fatalf("YouTube holds %q, want cax", got)
	}
	if got, want := base(t, st, "PLA"), f.baseOf("PLA"); !slices.Equal(got, want) {
		t.Fatalf("base = %v, want what YouTube holds, %v", got, want)
	}
	if report.Writes != 3 || f.writes != 3 || report.WriteUnits != 3*youtube.WriteUnits {
		t.Fatalf("report %+v after %d writes, want 3 writes costing 150 units", report, f.writes)
	}

	next := mustRun(t, context.Background(), r)
	if next.Writes != 0 || order(t, st, "PLA") != "cax" {
		t.Fatalf("the next run wrote %d and holds %q, want nothing written and cax kept", next.Writes, order(t, st, "PLA"))
	}
}

func TestEditsOnBothSidesAreBothKept(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abcd"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.add("PLA", "g", 4)
	edit(t, st, "PLA", "acd")

	mustRun(t, context.Background(), r)
	if got := order(t, st, "PLA"); got != "acdg" {
		t.Fatalf("order = %q, want acdg", got)
	}
	if got := f.videos("PLA"); got != "acdg" {
		t.Fatalf("YouTube holds %q, want acdg", got)
	}
}

// A delete and an add between two page requests leave a read without an item
// YouTube still has.
func TestAnAbsenceYouTubeDoesNotConfirmIsNotMerged(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.hidden[f.items["PLA"][1].ID] = true

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomePartial)
	if report.PlaylistsSkipped != 1 || !errors.Is(report.Failures[0].Err, ErrAbsenceNotConfirmed) || report.Failures[0].Playlist != "PLA" {
		t.Fatalf("report %+v, want PLA skipped with ErrAbsenceNotConfirmed", report)
	}
	if got := order(t, st, "PLA"); got != "abc" {
		t.Fatalf("order = %q, want abc kept", got)
	}
	if f.writes != 0 {
		t.Fatalf("made %d writes, want none", f.writes)
	}
}

func TestAPlaylistGoneFromYouTubeIsDeletedHereAndOneStillThereIsKept(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d", "PLC": "e"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.playlists = slices.DeleteFunc(f.playlists, func(p youtube.Playlist) bool { return p.ID == "PLB" })
	delete(f.items, "PLB")
	f.unlisted["PLC"] = true

	report := mustRun(t, context.Background(), r)
	if report.PlaylistsDeleted != 1 {
		t.Fatalf("report %+v, want PLB deleted", report)
	}
	ids, err := st.Queries.ListPlaylistIDs(context.Background())
	if err != nil || !slices.Equal(ids, []string{"PLA", "PLC"}) {
		t.Fatalf("stored playlists %v, %v, want PLA and PLC", ids, err)
	}
}

func TestAPlaylistDeletedBetweenTheListAndItsReadIsDeletedHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.itemsErrors["PLB"] = fmt.Errorf("list items of playlist PLB: %w", youtube.ErrPlaylistNotFound)

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(context.Background(), "PLB"); !errors.Is(err, sql.ErrNoRows) || report.PlaylistsDeleted != 1 {
		t.Fatalf("PLB lookup = %v with report %+v, want it deleted", err, report)
	}
}

func TestAReadFailureSkipsOnlyThatPlaylist(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	f.itemsErrors["PLA"] = fmt.Errorf("%w: moved", youtube.ErrInconsistentRead)

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomePartial)
	if report.Playlists != 1 || order(t, st, "PLB") != "d" || order(t, st, "PLA") != "" {
		t.Fatalf("report %+v, want PLB reconciled and PLA not", report)
	}
	failures, err := st.Queries.ListSyncFailures(context.Background(), report.RunID)
	if err != nil || len(failures) != 1 || failures[0].PlaylistID.String != "PLA" {
		t.Fatalf("recorded failures %+v, %v, want one against PLA", failures, err)
	}
}

func TestAFailedListingIsAFailedRun(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	f.playlistsError = errors.New("connection reset")

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeFailed)
	failures, err := st.Queries.ListSyncFailures(context.Background(), report.RunID)
	if err != nil || len(failures) != 1 || failures[0].PlaylistID.Valid {
		t.Fatalf("recorded failures %+v, %v, want one naming no playlist", failures, err)
	}
}

func TestYouTubesQuotaRefusalEndsTheRunAndTheDay(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "cax")
	f.quota = f.units + 2 + youtube.WriteUnits // this run's reads and one write

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeQuotaSpent)
	if got, want := base(t, st, "PLA"), f.baseOf("PLA"); report.Writes != 1 || !slices.Equal(got, want) {
		t.Fatalf("report %+v with base %v, want 1 write and the base matching YouTube's %v", report, got, want)
	}

	requests := f.requests
	c.now = c.now.Add(time.Hour)
	later := mustRun(t, context.Background(), r)
	assertOutcome(t, later, store.OutcomeQuotaSpent)
	if f.requests != requests {
		t.Fatalf("a later run the same day made %d requests, want none", f.requests-requests)
	}

	f.quota = 0
	c.now = c.now.Add(24 * time.Hour)
	nextDay := mustRun(t, context.Background(), r)
	assertOutcome(t, nextDay, store.OutcomeOK)
	if got := f.videos("PLA"); got != "cax" {
		t.Fatalf("YouTube holds %q the next day, want cax", got)
	}
}

// With a reserve leaving 80 units a day for writes, one write goes a day and the
// rest wait for the next day, with no failure.
func TestTheWriteAllowanceLeavesTheReadsTheirReserve(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "cax")
	// A run reads 2 pages: the playlists and PLA's items.
	r.runsPerDay = (DailyQuota - 80) / 2

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if report.Writes != 1 {
		t.Fatalf("report %+v, want the one write the allowance covers", report)
	}
	sameDay := mustRun(t, context.Background(), r)
	if sameDay.Writes != 0 {
		t.Fatalf("a second run the same day wrote %d, want none", sameDay.Writes)
	}
	c.now = c.now.Add(24 * time.Hour)
	if nextDay := mustRun(t, context.Background(), r); nextDay.Writes != 1 {
		t.Fatalf("the next day's run wrote %d, want 1", nextDay.Writes)
	}
}

func TestAVideoYouTubeRefusesIsRecordedAndNotAskedForAgain(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.refusedVideos["x"] = youtube.ErrVideoRefused
	edit(t, st, "PLA", "axyb")

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if got := f.videos("PLA"); got != "ayb" {
		t.Fatalf("YouTube holds %q, want ayb with y placed after the refused x", got)
	}
	refused, err := st.Queries.ListPushRefusedVideoIDs(context.Background(), "PLA")
	if err != nil || !slices.Equal(refused, []string{"x"}) {
		t.Fatalf("refused videos %v, %v, want x", refused, err)
	}

	writes := f.writes
	mustRun(t, context.Background(), r)
	if f.writes != writes || order(t, st, "PLA") != "axyb" {
		t.Fatalf("the next run made %d writes holding %q, want none and axyb kept", f.writes-writes, order(t, st, "PLA"))
	}
}

func TestAPlaylistNotSortedManuallyIsAppendedToAndNotReordered(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	f.notSortedManually["PLA"] = true
	edit(t, st, "PLA", "xcba")

	mustRun(t, context.Background(), r)
	if got := f.videos("PLA"); got != "abcx" {
		t.Fatalf("YouTube holds %q, want abc with x appended", got)
	}
	playlist, err := st.Queries.GetPlaylist(context.Background(), "PLA")
	if err != nil || playlist.IsSortedManually {
		t.Fatalf("playlist %+v, %v, want it marked not sorted manually", playlist, err)
	}

	writes := f.writes
	mustRun(t, context.Background(), r)
	if f.writes != writes {
		t.Fatalf("the next run made %d writes, want no move tried again", f.writes-writes)
	}
}

// An append the reference says YouTube accepts is refused anyway, and the push
// stops rather than sending it again.
func TestAnAppendRefusedForItsSortOrderStopsThePush(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "abx")
	refusal := fmt.Errorf("append: %w", youtube.ErrManualSortRequired)
	f.refusedVideos["x"] = refusal
	f.notSortedManually["PLA"] = true

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomePartial)
	if f.writes != 2 {
		t.Fatalf("made %d writes, want the insert and one append before the push stops", f.writes)
	}
}

// YouTube deleted the item after the run read it, so the push's removal finds
// it gone and takes the removal as made.
func TestARemovalOfAnItemAlreadyGoneIsTakenAsMade(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "ac")
	f.beforeWrite = func(n int) {
		if n == 1 {
			f.items["PLA"] = slices.Delete(f.items["PLA"], 1, 2)
		}
	}

	report := mustRun(t, context.Background(), r)
	assertOutcome(t, report, store.OutcomeOK)
	if got, want := base(t, st, "PLA"), f.baseOf("PLA"); report.Writes != 0 || !slices.Equal(got, want) {
		t.Fatalf("report %+v with base %v, want no write counted and the base matching YouTube's %v", report, got, want)
	}
}

// The context ends before the second write, so YouTube holds the first and the
// base says so.
func TestACanceledRunLeavesTheBaseAsYouTubeHoldsIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "cax")
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeWrite = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	report := mustRun(t, ctx, r)
	assertOutcome(t, report, store.OutcomeCanceled)
	if got, want := base(t, st, "PLA"), f.baseOf("PLA"); !slices.Equal(got, want) {
		t.Fatalf("base = %v, want what YouTube holds, %v", got, want)
	}
	if got := order(t, st, "PLA"); got != "cax" {
		t.Fatalf("order = %q, want the server's cax kept for the next run", got)
	}
	run, err := st.Queries.GetSyncRun(context.Background(), report.RunID)
	if err != nil || run.Outcome != store.OutcomeCanceled {
		t.Fatalf("recorded run %+v, %v, want it canceled", run, err)
	}
}

func TestARunRecordsItsOwnRequestsAndUnitsAgainstThePacificDate(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	mustRun(t, context.Background(), r)
	edit(t, st, "PLA", "ab")
	// 05:00 UTC on the 18th is 22:00 Pacific on the 17th.
	c.now = time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)
	requests, units := f.requests, f.units

	report := mustRun(t, context.Background(), r)
	run, err := st.Queries.GetSyncRun(context.Background(), report.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.QuotaDate != "2026-09-17" {
		t.Errorf("quota date %s, want the Pacific date 2026-09-17", run.QuotaDate)
	}
	if run.Requests != f.requests-requests || run.ReadUnits+run.WriteUnits != f.units-units || run.WriteUnits != youtube.WriteUnits || run.Writes != 1 {
		t.Errorf("recorded %+v, want the run's own %d requests and %d units, 50 of them for its one write", run, f.requests-requests, f.units-units)
	}
}

func TestTheWorkerRunsAtOnceAndThenAfterEachInterval(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeList = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	done := make(chan struct{})

	go func() {
		NewWorker(r, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop after its context ended")
	}
	var outcomes []string
	for id := int64(1); id <= 3; id++ {
		run, err := st.Queries.GetSyncRun(context.Background(), id)
		if err != nil {
			t.Fatalf("run %d: %v", id, err)
		}
		outcomes = append(outcomes, run.Outcome)
	}
	if !slices.Equal(outcomes, []string{store.OutcomeOK, store.OutcomeOK, store.OutcomeCanceled}) {
		t.Fatalf("outcomes %v, want two runs ok and the third canceled", outcomes)
	}
}
