package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

type wireSyncRun struct {
	ID               int64             `json:"id"`
	StartedTs        string            `json:"started_ts"`
	FinishedTs       string            `json:"finished_ts"`
	QuotaDate        string            `json:"quota_date"`
	Outcome          string            `json:"outcome"`
	Playlists        int64             `json:"playlists"`
	PlaylistsDeleted int64             `json:"playlists_deleted"`
	PlaylistsSkipped int64             `json:"playlists_skipped"`
	ItemsAdded       int64             `json:"items_added"`
	ItemsRemoved     int64             `json:"items_removed"`
	Requests         int64             `json:"requests"`
	Units            int64             `json:"units"`
	Failures         []wireSyncFailure `json:"failures"`
}

type wireSyncFailure struct {
	PlaylistID *string `json:"playlist_id"`
	Error      string  `json:"error"`
}

type wireStatus struct {
	Library   wireLibrary  `json:"library"`
	LastRun   *wireSyncRun `json:"last_run"`
	LastOKRun *wireSyncRun `json:"last_ok_run"`
}

type wireLibrary struct {
	Playlists         int64 `json:"playlists"`
	Videos            int64 `json:"videos"`
	UnavailableVideos int64 `json:"unavailable_videos"`
	EnrichedVideos    int64 `json:"enriched_videos"`
	Tracks            int64 `json:"tracks"`
	Plays             int64 `json:"plays"`
}

// withRuns stores three runs: the first ended ok, the second partial with a
// failure of PLA and one of the run as a whole, and the third failed.
func (f *fixture) withRuns(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	runs := []struct {
		outcome  string
		failures []generated.InsertSyncFailureParams
	}{
		{store.OutcomeOK, nil},
		{store.OutcomePartial, []generated.InsertSyncFailureParams{
			{PlaylistID: text("PLA"), Error: "read PLA: backend error"},
			{PlaylistID: sql.NullString{}, Error: "list playlists: reads exceed the quota"},
		}},
		{store.OutcomeFailed, []generated.InsertSyncFailureParams{
			{PlaylistID: sql.NullString{}, Error: "list playlists: connection refused"},
		}},
	}
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		for i, r := range runs {
			id, err := tx.InsertSyncRun(ctx, generated.InsertSyncRunParams{
				StartedTs:  "2026-09-17T10:00:00Z",
				FinishedTs: "2026-09-17T10:00:05Z",
				QuotaDate:  "2026-09-17",
				Outcome:    r.outcome,
				Playlists:  int64(36 + i),
				ItemsAdded: int64(i),
				Requests:   68,
				Units:      68,
			})
			if err != nil {
				return err
			}
			for _, failure := range r.failures {
				failure.RunID = id
				if err := tx.InsertSyncFailure(ctx, failure); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store the runs: %v", err)
	}
}

func TestSyncRunsPageNewestFirstWithTheirFailures(t *testing.T) {
	f := newFixture(t)
	f.withRuns(t)

	first := decode[wirePage[wireSyncRun]](t, f.get("/api/v1/sync/runs?limit=2"), http.StatusOK)
	if len(first.Data) != 2 || !first.HasMore || first.Data[0].ID != 3 || first.Data[1].ID != 2 {
		t.Fatalf("first page = %+v, want runs 3 and 2 with more", first)
	}
	newest := first.Data[0]
	if newest.Outcome != store.OutcomeFailed || newest.Playlists != 38 || newest.ItemsAdded != 2 || newest.Requests != 68 ||
		newest.Units != 68 || newest.QuotaDate != "2026-09-17" || newest.StartedTs != "2026-09-17T10:00:00Z" || newest.FinishedTs != "2026-09-17T10:00:05Z" {
		t.Errorf("run 3 = %+v", newest)
	}
	partial := first.Data[1].Failures
	if len(partial) != 2 || partial[0].PlaylistID == nil || *partial[0].PlaylistID != "PLA" || partial[1].PlaylistID != nil ||
		!strings.Contains(partial[1].Error, "quota") {
		t.Errorf("run 2's failures = %+v, want PLA's then the run's", partial)
	}
	if len(newest.Failures) != 1 {
		t.Errorf("run 3's failures = %+v, want its one", newest.Failures)
	}

	second := decode[wirePage[wireSyncRun]](t, f.get("/api/v1/sync/runs?limit=2&starting_after=2"), http.StatusOK)
	if len(second.Data) != 1 || second.HasMore || second.Data[0].ID != 1 {
		t.Fatalf("second page = %+v, want run 1 and no more", second)
	}
	if second.Data[0].Failures == nil || len(second.Data[0].Failures) != 0 {
		t.Errorf("run 1's failures = %#v, want []", second.Data[0].Failures)
	}
}

func TestNoSyncRunsPageAsAnEmptyList(t *testing.T) {
	f := newFixture(t)
	got := decode[wirePage[wireSyncRun]](t, f.get("/api/v1/sync/runs"), http.StatusOK)
	if got.Data == nil || got.HasMore {
		t.Errorf("no runs = %+v, want [] and no more", got)
	}
}

func TestASyncRunsPageStartingAfterNoStoredRunIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withRuns(t)
	for _, after := range []string{"99", "latest"} {
		refused(t, f.get("/api/v1/sync/runs?starting_after="+after), http.StatusBadRequest)
	}
}

func TestAFailureOfARunNotReadIsRefused(t *testing.T) {
	runs := []syncRun{{ID: 2}, {ID: 1}}
	failures := []generated.SyncFailure{{SyncFailureID: 1, RunID: 3, Error: "stray"}}
	if err := attachFailures(runs, failures); err == nil {
		t.Fatal("attachFailures dropped a failure of a run it was not given")
	}
}

func TestStatusOfAnEmptyStoreCountsNothingAndHasNoRuns(t *testing.T) {
	f := newFixture(t)
	got := decode[wireStatus](t, f.get("/api/v1/status"), http.StatusOK)
	if got.Library != (wireLibrary{}) || got.LastRun != nil || got.LastOKRun != nil {
		t.Errorf("status = %+v, want zero counts and null runs", got)
	}
}

func TestStatusCountsTheLibraryAndNamesTheLatestRunAndLatestOKRun(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)
	f.withRuns(t)

	got := decode[wireStatus](t, f.get("/api/v1/status"), http.StatusOK)
	want := wireLibrary{Playlists: 3, Videos: 5, UnavailableVideos: 1, EnrichedVideos: 1, Tracks: 5, Plays: 5}
	if got.Library != want {
		t.Errorf("library = %+v, want %+v", got.Library, want)
	}
	if got.LastRun == nil || got.LastRun.ID != 3 || len(got.LastRun.Failures) != 1 {
		t.Errorf("last run = %+v, want run 3 with its failure", got.LastRun)
	}
	if got.LastOKRun == nil || got.LastOKRun.ID != 1 || got.LastOKRun.Failures == nil {
		t.Errorf("last ok run = %+v, want run 1 with []", got.LastOKRun)
	}
}
