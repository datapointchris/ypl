package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

type wireSyncRun struct {
	ID                int64             `json:"id"`
	StartedTs         string            `json:"started_ts"`
	FinishedTs        string            `json:"finished_ts"`
	QuotaDate         string            `json:"quota_date"`
	Outcome           string            `json:"outcome"`
	Playlists         int64             `json:"playlists"`
	PlaylistsDeleted  int64             `json:"playlists_deleted"`
	PlaylistsSkipped  int64             `json:"playlists_skipped"`
	PlaylistsDeferred int64             `json:"playlists_deferred"`
	ItemsAdded        int64             `json:"items_added"`
	ItemsRemoved      int64             `json:"items_removed"`
	Writes            int64             `json:"writes"`
	Requests          int64             `json:"requests"`
	Units             int64             `json:"units"`
	WriteUnits        int64             `json:"write_units"`
	VideoReads        int64             `json:"video_reads"`
	VideosEnriched    int64             `json:"videos_enriched"`
	TracksFound       int64             `json:"tracks_found"`
	VideosUnreadable  int64             `json:"videos_unreadable"`
	IsRateLimited     bool              `json:"is_rate_limited"`
	EnrichmentPaused  bool              `json:"enrichment_paused"`
	ProbeMisses       int64             `json:"probe_misses"`
	Failures          []wireSyncFailure `json:"failures"`
}

type wireSyncFailure struct {
	Stage      string  `json:"stage"`
	PlaylistID *string `json:"playlist_id"`
	VideoID    *string `json:"video_id"`
	Error      string  `json:"error"`
}

type wireStatus struct {
	Library   wireLibrary     `json:"library"`
	LastRun   *wireSyncRun    `json:"last_run"`
	LastOKRun *wireSyncRun    `json:"last_ok_run"`
	Sync      json.RawMessage `json:"sync"`
}

type wireLibrary struct {
	Playlists           int64 `json:"playlists"`
	Videos              int64 `json:"videos"`
	UnavailableVideos   int64 `json:"unavailable_videos"`
	EnrichedVideos      int64 `json:"enriched_videos"`
	VideosWithTracklist int64 `json:"videos_with_tracklist"`
	Tracks              int64 `json:"tracks"`
	Plays               int64 `json:"plays"`
}

// withRuns stores four runs: the first failed, the second ended ok, the third
// partial with a failure of PLA, one of the run as a whole and one of the read
// of the video v9, and the fourth failed, rate limited. Every run but the ok one
// carries a failure, so a page that read another run's failures would hold one.
// The nth run from 0 read 2n videos and enriched n of them with 10n tracks.
func (f *fixture) withRuns(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	runs := []struct {
		outcome  string
		failures []generated.InsertSyncFailureParams
	}{
		{store.OutcomeFailed, []generated.InsertSyncFailureParams{
			{Stage: store.StageSync, PlaylistID: sql.NullString{}, Error: "list playlists: token refused"},
		}},
		{store.OutcomeOK, nil},
		{store.OutcomePartial, []generated.InsertSyncFailureParams{
			{Stage: store.StageSync, PlaylistID: text("PLA"), Error: "read PLA: backend error"},
			{Stage: store.StageSync, PlaylistID: sql.NullString{}, Error: "list playlists: reads exceed the quota"},
			{Stage: store.StageEnrichment, VideoID: text("v9"), Error: "read video v9: yt-dlp: Unable to extract initial player response"},
			{Stage: store.StageEnrichment, Error: "store the enrichment of v8: disk I/O error"},
		}},
		{store.OutcomeFailed, []generated.InsertSyncFailureParams{
			{Stage: store.StageSync, PlaylistID: sql.NullString{}, Error: "list playlists: connection refused"},
		}},
	}
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		for i, r := range runs {
			id, err := tx.InsertSyncRun(ctx, generated.InsertSyncRunParams{
				StartedTs:        "2026-09-17T10:00:00Z",
				FinishedTs:       "2026-09-17T10:00:05Z",
				QuotaDate:        "2026-09-17",
				Outcome:          r.outcome,
				Playlists:        int64(36 + i),
				ItemsAdded:       int64(i),
				Requests:         68,
				Units:            68,
				VideoReads:       int64(2 * i),
				VideosEnriched:   int64(i),
				TracksFound:      int64(10 * i),
				VideosUnreadable: int64(i % 2),
				IsRateLimited:    i == len(runs)-1,
				EnrichmentPaused: i == 2,
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
	if len(first.Data) != 2 || !first.HasMore || first.Data[0].ID != 4 || first.Data[1].ID != 3 {
		t.Fatalf("first page = %+v, want runs 4 and 3 with more", first)
	}
	newest := first.Data[0]
	if newest.Outcome != store.OutcomeFailed || newest.Playlists != 39 || newest.ItemsAdded != 3 || newest.Requests != 68 ||
		newest.Units != 68 || newest.QuotaDate != "2026-09-17" || newest.StartedTs != "2026-09-17T10:00:00Z" || newest.FinishedTs != "2026-09-17T10:00:05Z" {
		t.Errorf("run 4 = %+v", newest)
	}
	if newest.VideoReads != 6 || newest.VideosEnriched != 3 || newest.TracksFound != 30 || newest.VideosUnreadable != 1 || !newest.IsRateLimited || first.Data[1].IsRateLimited {
		t.Errorf("run 4's enrichment = %+v and run 3 rate limited %v, want 6 reads, 3 videos, 30 tracks, 1 unreadable, rate limited, and run 3 not", newest, first.Data[1].IsRateLimited)
	}
	// Run 3 made no read because an earlier refusal still held, and run 4 is the
	// one whose own read drew a refusal. Neither carries the other's flag, and
	// run 3's failures are the sync's rather than the pause.
	if !first.Data[1].EnrichmentPaused || newest.EnrichmentPaused {
		t.Errorf("run 3 paused = %v and run 4 paused = %v, want the pause on run 3 alone", first.Data[1].EnrichmentPaused, newest.EnrichmentPaused)
	}
	partial := first.Data[1].Failures
	if len(partial) != 4 || partial[0].PlaylistID == nil || *partial[0].PlaylistID != "PLA" || partial[0].VideoID != nil ||
		partial[1].PlaylistID != nil || partial[1].VideoID != nil || !strings.Contains(partial[1].Error, "quota") ||
		partial[2].PlaylistID != nil || partial[2].VideoID == nil || *partial[2].VideoID != "v9" ||
		partial[3].PlaylistID != nil || partial[3].VideoID != nil {
		t.Errorf("run 3's failures = %+v, want PLA's, the sync's, v9's read, then the enrichment's", partial)
	}
	// The second and the fourth name no playlist and no video, so the stage is
	// the only thing that says which half of the run each one failed.
	if partial[1].Stage != store.StageSync || partial[3].Stage != store.StageEnrichment {
		t.Errorf("the run's two failures naming nothing are staged %q and %q, want %q and %q",
			partial[1].Stage, partial[3].Stage, store.StageSync, store.StageEnrichment)
	}
	if len(newest.Failures) != 1 {
		t.Errorf("run 4's failures = %+v, want its one", newest.Failures)
	}

	second := decode[wirePage[wireSyncRun]](t, f.get("/api/v1/sync/runs?limit=2&starting_after=3"), http.StatusOK)
	if len(second.Data) != 2 || second.HasMore || second.Data[0].ID != 2 || second.Data[1].ID != 1 {
		t.Fatalf("second page = %+v, want runs 2 and 1 and no more", second)
	}
	if second.Data[0].Failures == nil || len(second.Data[0].Failures) != 0 {
		t.Errorf("run 2's failures = %#v, want []", second.Data[0].Failures)
	}
	if len(second.Data[1].Failures) != 1 {
		t.Errorf("run 1's failures = %+v, want its one", second.Data[1].Failures)
	}
}

func TestSyncRunsDefaultToAPageOfTwenty(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for range 21 {
		run := generated.InsertSyncRunParams{StartedTs: "2026-09-17T10:00:00Z", FinishedTs: "2026-09-17T10:00:05Z", QuotaDate: "2026-09-17", Outcome: store.OutcomeOK}
		if _, err := f.st.Queries.InsertSyncRun(ctx, run); err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}
	got := decode[wirePage[wireSyncRun]](t, f.get("/api/v1/sync/runs"), http.StatusOK)
	if len(got.Data) != 20 || !got.HasMore {
		t.Errorf("runs = %d with has_more %v, want 20 of the 21 and more", len(got.Data), got.HasMore)
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
	refused(t, f.get("/api/v1/sync/runs?starting_after=99"), http.StatusBadRequest, wire.CodeUnknownReference)
	refused(t, f.get("/api/v1/sync/runs?starting_after=latest"), http.StatusBadRequest, wire.CodeInvalidParameter)
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
	want := wireLibrary{Playlists: 3, Videos: 5, UnavailableVideos: 1, EnrichedVideos: 1, VideosWithTracklist: 2, Tracks: 5, Plays: 5}
	if got.Library != want {
		t.Errorf("library = %+v, want %+v", got.Library, want)
	}
	if got.LastRun == nil || got.LastRun.ID != 4 || len(got.LastRun.Failures) != 1 {
		t.Errorf("last run = %+v, want run 4 with its failure", got.LastRun)
	}
	if got.LastOKRun == nil || got.LastOKRun.ID != 2 || got.LastOKRun.Failures == nil {
		t.Errorf("last ok run = %+v, want run 2 with []", got.LastOKRun)
	}
}
