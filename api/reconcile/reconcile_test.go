package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

func TestARunStoresEveryPlaylistAsYouTubeHoldsIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	if report.Playlists != 2 || report.ItemsAdded != 4 || report.ItemsRemoved != 0 {
		t.Fatalf("report %+v, want 2 playlists and 4 items added", report)
	}
	videos, ids := stored(t, st, "PLA")
	if videos != "abc" || !slices.Equal(ids, f.itemIDs("PLA")) {
		t.Fatalf("PLA holds %q with ids %v, want abc with YouTube's ids %v", videos, ids, f.itemIDs("PLA"))
	}
	playlist, err := st.Queries.GetPlaylist(context.Background(), "PLA")
	if err != nil || playlist.Title != "Playlist PLA" || playlist.Privacy != "private" {
		t.Fatalf("playlist %+v, %v, want it stored as YouTube reports it", playlist, err)
	}
	video, err := st.Queries.GetVideo(context.Background(), "d")
	if err != nil || video.Title != "Video d" {
		t.Fatalf("video d = %+v, %v, want it stored with its title", video, err)
	}
}

func TestARunWithNothingChangedAddsAndRemovesNothing(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, _, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	if report.Playlists != 1 || report.ItemsAdded != 0 || report.ItemsRemoved != 0 {
		t.Fatalf("report %+v, want the playlist stored with nothing added or removed", report)
	}
}

func TestEditsOnYouTubeArriveHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.remove("PLA", 1)
	f.add("PLA", "g", 0)

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	if videos, ids := stored(t, st, "PLA"); videos != "gac" || !slices.Equal(ids, f.itemIDs("PLA")) {
		t.Fatalf("PLA holds %q with ids %v, want gac with YouTube's ids", videos, ids)
	}
	if report.ItemsAdded != 1 || report.ItemsRemoved != 1 {
		t.Fatalf("report %+v, want one item added and one removed", report)
	}
}

// A second copy of a video is its own item, so adding one counts one item.
func TestASecondCopyOfAVideoIsItsOwnItem(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.add("PLA", "a", 0)

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	if videos, _ := stored(t, st, "PLA"); videos != "aab" || report.ItemsAdded != 1 || report.ItemsRemoved != 0 {
		t.Fatalf("PLA holds %q with report %+v, want aab with one item added", videos, report)
	}
}

func TestAPlaylistTheChannelNoLongerListsIsDeletedHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.deletePlaylist("PLB")

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	ids, err := st.Queries.ListPlaylistIDs(context.Background())
	if err != nil || !slices.Equal(ids, []string{"PLA"}) || report.PlaylistsDeleted != 1 {
		t.Fatalf("stored playlists %v, %v with report %+v, want PLA alone and one deleted", ids, err, report)
	}
}

func TestAPlaylistDeletedBetweenTheListAndItsReadIsDeletedHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.itemsErrors["PLB"] = fmt.Errorf("list items of playlist PLB: %w", youtube.ErrPlaylistNotFound)

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	if _, err := st.Queries.GetPlaylist(context.Background(), "PLB"); !errors.Is(err, sql.ErrNoRows) || report.PlaylistsDeleted != 1 {
		t.Fatalf("PLB lookup = %v with report %+v, want it deleted", err, report)
	}
}

// The playlist that could not be read keeps what the store held for it.
func TestAReadFailureSkipsOnlyThatPlaylist(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.remove("PLA", 0)
	f.add("PLB", "e", 1)
	f.itemsErrors["PLA"] = fmt.Errorf("%w: moved", youtube.ErrInconsistentRead)

	report := mustRun(t, context.Background(), r, store.OutcomePartial)
	if videos, _ := stored(t, st, "PLA"); videos != "abc" || report.PlaylistsSkipped != 1 {
		t.Fatalf("PLA holds %q with report %+v, want abc kept and PLA skipped", videos, report)
	}
	if videos, _ := stored(t, st, "PLB"); videos != "de" {
		t.Fatalf("PLB holds %q, want de", videos)
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

	report := mustRun(t, context.Background(), r, store.OutcomeFailed)
	failures, err := st.Queries.ListSyncFailures(context.Background(), report.RunID)
	if err != nil || len(failures) != 1 || failures[0].PlaylistID.Valid {
		t.Fatalf("recorded failures %+v, %v, want one naming no playlist", failures, err)
	}
}

func TestYouTubesQuotaRefusalEndsTheRunAndTheDay(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, _, c := newRunner(t, f)
	f.quota = 2 // the listing and PLA's items

	mustRun(t, context.Background(), r, store.OutcomeQuotaSpent)

	requests := f.requests
	c.now = c.now.Add(time.Hour)
	mustRun(t, context.Background(), r, store.OutcomeQuotaSpent)
	if f.requests != requests {
		t.Fatalf("a later run the same day made %d requests, want none", f.requests-requests)
	}

	f.quota = 0
	c.now = c.now.Add(24 * time.Hour)
	if report := mustRun(t, context.Background(), r, store.OutcomeOK); report.Playlists != 2 {
		t.Fatalf("the next day's run stored %d playlists, want 2", report.Playlists)
	}
}

// The run is canceled while it reads PLB, so PLA is stored whole and PLB keeps
// what the store held.
func TestACanceledRunLeavesEachPlaylistWhole(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.add("PLA", "x", 3)
	f.add("PLB", "y", 1)
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeItems = func(n int) {
		if n == 4 {
			cancel()
		}
	}

	report := mustRun(t, ctx, r, store.OutcomeCanceled)
	if videos, _ := stored(t, st, "PLA"); videos != "abcx" {
		t.Fatalf("PLA holds %q, want abcx", videos)
	}
	if videos, _ := stored(t, st, "PLB"); videos != "d" {
		t.Fatalf("PLB holds %q, want d kept", videos)
	}
	run, err := st.Queries.GetSyncRun(context.Background(), report.RunID)
	if err != nil || run.Outcome != store.OutcomeCanceled {
		t.Fatalf("recorded run %+v, %v, want it canceled", run, err)
	}
}

func TestARunRecordsItsOwnRequestsAndUnitsAgainstThePacificDate(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, c := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	// 05:00 UTC on the 18th is 22:00 Pacific on the 17th.
	c.now = time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)
	requests, units := f.requests, f.units

	report := mustRun(t, context.Background(), r, store.OutcomeOK)
	run, err := st.Queries.GetSyncRun(context.Background(), report.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.QuotaDate != "2026-09-17" {
		t.Errorf("quota date %s, want the Pacific date 2026-09-17", run.QuotaDate)
	}
	if run.Requests != f.requests-requests || run.Units != f.units-units || run.Units != 3 {
		t.Errorf("recorded %+v, want the run's own 3 requests and 3 units", run)
	}
}

// The fake reads 2 units a run. Hourly runs read 48 a day, and runs every 8
// seconds read 21,600, past the day's quota.
func TestAnIntervalWhoseRunsOutreadTheQuotaIsReported(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		outcome  string
	}{
		{time.Hour, store.OutcomeOK},
		{8 * time.Second, store.OutcomePartial},
	} {
		t.Run(c.interval.String(), func(t *testing.T) {
			f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
			st, err := store.Open(context.Background(), t.TempDir()+"/api.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })

			report := mustRun(t, context.Background(), NewRunner(st, f, c.interval), c.outcome)
			exceeds := slices.ContainsFunc(report.Failures, func(f Failure) bool { return errors.Is(f.Err, ErrReadsExceedQuota) })
			if exceeds != (c.outcome == store.OutcomePartial) || report.Playlists != 1 {
				t.Fatalf("report %+v, want the playlist stored and ErrReadsExceedQuota reported only when the day's reads pass the quota", report)
			}
		})
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
	log, _ := captured()
	done := make(chan struct{})

	go func() {
		NewWorker(r, time.Millisecond, log).Run(ctx)
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

// A run that did not store every playlist is logged above Info, so a filter on
// level finds it.
func TestTheWorkerLogsEachRunAtALevelItsOutcomeSets(t *testing.T) {
	cases := []struct {
		breakRun func(*fakeChannel)
		level    string
	}{
		{func(*fakeChannel) {}, "INFO"},
		{func(f *fakeChannel) { f.itemsErrors["PLA"] = youtube.ErrInconsistentRead }, "WARN"},
		{func(f *fakeChannel) { f.playlistsError = errors.New("token revoked") }, "ERROR"},
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
			c.breakRun(f)
			r, _, _ := newRunner(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			f.beforeList = func(n int) {
				if n == 2 {
					cancel()
				}
			}
			log, logged := captured()

			NewWorker(r, time.Millisecond, log).Run(ctx)
			first, _, _ := strings.Cut(logged.String(), "\n")
			var line map[string]any
			if err := json.Unmarshal([]byte(first), &line); err != nil {
				t.Fatalf("decode %q: %v", first, err)
			}
			if line["msg"] != "sync run" || line["level"] != c.level {
				t.Fatalf("first log line %v, want a sync run at %s", line, c.level)
			}
		})
	}
}
