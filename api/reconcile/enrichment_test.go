package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/datapointchris/ypl/api/enrich"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// A run records what its enrichment read and stored, and a failed read of one
// video as a failure naming the video.
func TestARunRecordsWhatItsEnrichmentDid(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	failed := errors.New("yt-dlp: Unable to extract initial player response")
	r.enricher = &fakeEnricher{report: enrich.Report{
		Reads: 4, Enriched: 2, Tracks: 12, Unreadable: 1, RateLimited: true,
		Failures: []enrich.Failure{{VideoID: "v9", Err: failed}},
	}}

	report := mustRun(t, context.Background(), r, store.OutcomePartial)
	ctx := context.Background()
	row, err := st.Queries.GetSyncRun(ctx, report.RunID)
	if err != nil || row.VideoReads != 4 || row.VideosEnriched != 2 || row.TracksFound != 12 || row.VideosUnreadable != 1 || !row.IsRateLimited || row.Playlists != 1 {
		t.Fatalf("run %+v, %v, want the sync's playlist and enrichment's 4 reads, 2 videos, 12 tracks, 1 unreadable and the rate limit", row, err)
	}
	failures, err := st.Queries.ListSyncFailures(ctx, report.RunID)
	if err != nil || len(failures) != 1 || failures[0].VideoID.String != "v9" || failures[0].PlaylistID.Valid || failures[0].Error != failed.Error() {
		t.Fatalf("failures %+v, %v, want v9's failed read and no playlist", failures, err)
	}
}

// Enrichment reads through yt-dlp, which the Data API's quota does not reach,
// so it follows a sync YouTube refused for the quota and no sync that failed or
// was canceled.
func TestEnrichmentFollowsASyncRefusedForTheQuotaAndNoOtherEndedSync(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(f *fakeChannel, cancel context.CancelFunc)
		outcome string
		runs    int
	}{
		{name: "a sync that finished", arrange: func(*fakeChannel, context.CancelFunc) {}, outcome: store.OutcomeOK, runs: 1},
		{name: "a sync refused for the quota", arrange: func(f *fakeChannel, _ context.CancelFunc) { f.quota = f.units + 1 }, outcome: store.OutcomeQuotaSpent, runs: 1},
		{name: "a failed sync", arrange: func(f *fakeChannel, _ context.CancelFunc) { f.playlistsError = errors.New("connection reset") }, outcome: store.OutcomeFailed, runs: 0},
		{name: "a canceled sync", arrange: func(f *fakeChannel, cancel context.CancelFunc) { f.beforeList = func(int) { cancel() } }, outcome: store.OutcomeCanceled, runs: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
			r, _, _ := newRunner(t, f)
			mustRun(t, context.Background(), r, store.OutcomeOK)
			enricher := &fakeEnricher{}
			r.enricher = enricher
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.arrange(f, cancel)

			mustRun(t, ctx, r, c.outcome)
			if enricher.runs != c.runs {
				t.Fatalf("enrichment ran %d times, want %d", enricher.runs, c.runs)
			}
		})
	}
}

// A store failure of enrichment fails the run, and the reads it made before it
// are recorded.
func TestAStoreFailureOfEnrichmentFailsTheRun(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, _, _ := newRunner(t, f)
	stored := errors.New("store the enrichment of v1: disk I/O error")
	r.enricher = &fakeEnricher{report: enrich.Report{Reads: 1}, err: stored}

	report := mustRun(t, context.Background(), r, store.OutcomeFailed)
	if report.VideoReads != 1 || !errors.Is(report.Failures[len(report.Failures)-1].Err, stored) {
		t.Fatalf("report %+v, want one read and the store's failure", report)
	}
}

// A rate limit is a failure of the video whose read YouTube refused.
func TestARateLimitIsAFailureOfTheRefusedRead(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, _, _ := newRunner(t, f)
	refused := errors.Join(ytdlp.ErrRateLimited, errors.New("rate-limited by YouTube"))
	r.enricher = &fakeEnricher{report: enrich.Report{Reads: 1, RateLimited: true, Failures: []enrich.Failure{{VideoID: "v1", Err: refused}}}}

	report := mustRun(t, context.Background(), r, store.OutcomePartial)
	if !report.RateLimited || report.Failures[0].Video != "v1" || !errors.Is(report.Failures[0].Err, ytdlp.ErrRateLimited) {
		t.Fatalf("report %+v, want the rate limit on v1's read", report)
	}
}
