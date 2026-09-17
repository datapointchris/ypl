package enrich

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/tracklist"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// start is 08:00 Pacific on 2026-09-17.
var start = time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)

// fakeReader answers each read of a video from answers, a video with that id
// carrying a tracklist when it holds none, and records the reads in order. The
// default carries one because a read that stores none holds its video back the
// way a failed read does, which is a case a test asks for rather than gets.
type fakeReader struct {
	answers map[string]error
	videos  map[string]ytdlp.Video
	read    []string
}

func (f *fakeReader) Video(ctx context.Context, id string) (ytdlp.Video, error) {
	f.read = append(f.read, id)
	if err := f.answers[id]; err != nil {
		return ytdlp.Video{}, err
	}
	if video, ok := f.videos[id]; ok {
		return video, nil
	}
	return ytdlp.Video{ID: id, Description: listing("Default")}, nil
}

// withoutTracklist is a video whose read finds nothing to store.
func withoutTracklist(id string) ytdlp.Video {
	return ytdlp.Video{ID: id, Description: "Follow us", Comments: []string{"Gorgeous set"}}
}

// fixture is an Enricher over a store whose playlist PLA holds the videos
// named, added in that order, on a clock the test moves, whose waits are
// recorded and do not wait. PLB holds the last of those videos as well, so
// every case crosses a video two playlists hold.
type fixture struct {
	st      *store.Store
	reader  *fakeReader
	e       *Enricher
	clock   time.Time
	waited  []time.Duration
	waitErr error
}

// testLimits is what a fixture reads within: a batch bigger than any fixture's
// videos and a budget longer than any test's clock moves inside a run, so a
// test reaches those bounds only by setting them itself.
var testLimits = Limits{Pace: 10 * time.Second, Batch: 10, Budget: time.Hour}

func newFixture(t *testing.T, videos ...string) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, reader: &fakeReader{answers: map[string]error{}, videos: map[string]ytdlp.Video{}}, clock: start}
	f.e = New(st, f.reader, testLimits)
	f.e.now = func() time.Time { return f.clock }
	f.e.wait = func(_ context.Context, d time.Duration) error {
		f.waited = append(f.waited, d)
		return f.waitErr
	}
	err = st.InTx(ctx, func(tx *store.Tx) error {
		for _, playlist := range []string{"PLA", "PLB"} {
			if err := tx.UpsertPlaylist(ctx, generated.UpsertPlaylistParams{PlaylistID: playlist, Title: playlist, Privacy: "private"}); err != nil {
				return err
			}
		}
		var entries []store.Entry
		for _, video := range videos {
			if err := tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: video, Title: "Video " + video, ChannelTitle: "Channel"}); err != nil {
				return err
			}
			entries = append(entries, store.Entry{VideoID: video})
		}
		if _, err := tx.ReplaceOrder(ctx, "PLA", 1, entries); err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		_, err := tx.ReplaceOrder(ctx, "PLB", 1, entries[len(entries)-1:])
		return err
	})
	if err != nil {
		t.Fatalf("store the playlist: %v", err)
	}
	return f
}

func (f *fixture) run(t *testing.T) Report {
	t.Helper()
	report, err := f.e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func listing(from string) string {
	return fmt.Sprintf("0:00 %[1]s - One\n5:00 %[1]s - Two\n9:00 %[1]s - Three", from)
}

// failure is the video's recorded failure, and false when it has none.
func (f *fixture) failure(t *testing.T, video string) (generated.EnrichFailure, bool) {
	t.Helper()
	failure, err := f.st.Queries.GetEnrichFailure(context.Background(), video)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return failure, false
	case err != nil:
		t.Fatalf("get the failure of %s: %v", video, err)
	}
	return failure, true
}

// Each read stores what it reports and the best tracklist it holds, from
// chapters, the description, a comment, or none.
func TestAReadStoresWhatItReportsAndItsTracklist(t *testing.T) {
	f := newFixture(t, "vnone", "vcomment", "vdescription", "vchapters")
	f.reader.videos["vchapters"] = ytdlp.Video{
		ID: "vchapters", DurationSeconds: 754, UploadDate: "2023-01-11", Description: listing("Ignored"),
		Chapters: []tracklist.Chapter{
			{StartSeconds: 0, EndSeconds: 251, Title: "Intro"},
			{StartSeconds: 251, EndSeconds: 500, Title: "B - Two"},
			{StartSeconds: 500, EndSeconds: 754, Title: "C - Three"},
		},
	}
	f.reader.videos["vnone"] = withoutTracklist("vnone")
	f.reader.videos["vdescription"] = ytdlp.Video{ID: "vdescription", Description: listing("Description")}
	f.reader.videos["vcomment"] = ytdlp.Video{ID: "vcomment", Description: "Follow us", Comments: []string{"Gorgeous set", listing("Comment")}}

	report := f.run(t)
	if report.Reads != 4 || report.Enriched != 4 || report.Tracks != 9 || report.Failures != nil {
		t.Fatalf("report %+v, want 4 reads enriching 4 videos with 9 tracks", report)
	}
	ctx := context.Background()
	video, err := f.st.Queries.GetVideo(ctx, "vchapters")
	if err != nil || video.DurationSeconds.Int64 != 754 || video.UploadDate.String != "2023-01-11" || video.EnrichedTs.String != store.Timestamp(start) || video.Title != "Video vchapters" {
		t.Fatalf("vchapters = %+v, %v, want its duration, upload date and enriched time stored and its title kept", video, err)
	}
	for id, want := range map[string]struct {
		tracks int
		source tracklist.Source
	}{
		"vchapters":    {3, tracklist.SourceChapter},
		"vdescription": {3, tracklist.SourceDescription},
		"vcomment":     {3, tracklist.SourceComment},
		"vnone":        {0, ""},
	} {
		tracks, err := f.st.Queries.ListTracks(ctx, id)
		if err != nil || len(tracks) != want.tracks || want.tracks > 0 && tracklist.Source(tracks[0].Source) != want.source {
			t.Errorf("tracks of %s = %+v, %v, want %d from %s", id, tracks, err, want.tracks, want.source)
		}
		if video, err := f.st.Queries.GetVideo(ctx, id); err != nil || !video.EnrichedTs.Valid || !video.Description.Valid {
			t.Errorf("%s = %+v, %v, want it enriched with its description", id, video, err)
		}
	}
	chapters, _ := f.st.Queries.ListTracks(ctx, "vchapters")
	if intro := chapters[0]; intro.Artist.Valid || intro.Title != "Intro" || intro.EndSeconds != (sql.NullInt64{Int64: 251, Valid: true}) {
		t.Errorf("vchapters' first track = %+v, want Intro with no artist ending at 251", intro)
	}
	described, _ := f.st.Queries.ListTracks(ctx, "vdescription")
	if last := described[len(described)-1]; last.EndSeconds.Valid || last.Artist.String != "Description" || last.StartSeconds.Int64 != 540 {
		t.Errorf("vdescription's last track = %+v, want Description's at 540 with no end", last)
	}
	if again := f.run(t); again.Reads != 0 {
		t.Fatalf("the next run read %d videos, want none", again.Reads)
	}
	failure, failed := f.failure(t, "vnone")
	if !failed || failure.Attempts != 1 || failure.RetryTs.String != store.Timestamp(start.Add(firstRetry)) {
		t.Fatalf("mark on vnone = %+v, %v, want one attempt read again in %v, since a tracklist may be posted later", failure, failed, firstRetry)
	}
}

// A read that makes no tracklist leaves the tracks a video already holds, which
// an import of a Python mirror is where most of them come from, and the queue
// passes over a video holding any.
func TestAReadThatFindsNoTracklistKeepsTheTracksAVideoHolds(t *testing.T) {
	f := newFixture(t, "vheld")
	ctx := context.Background()
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		return tx.ReplaceTracks(ctx, "vheld", []generated.InsertTrackParams{
			{Position: 1, Title: "By hand", RawText: "By hand", Source: "manual"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if report := f.run(t); report.Reads != 0 {
		t.Fatalf("report %+v, want no read of a video that already holds a track", report)
	}

	// The queue is one guard and the store is the other, since a track may be
	// written between a run listing a video and its read coming back.
	stored, err := f.e.storeVideo(ctx, ytdlp.Video{ID: "vheld", Description: "no tracklist"})
	if err != nil || stored != 0 {
		t.Fatalf("storeVideo = %d, %v, want no track stored", stored, err)
	}
	tracks, err := f.st.Queries.ListTracks(ctx, "vheld")
	if err != nil || len(tracks) != 1 || tracks[0].Title != "By hand" {
		t.Fatalf("tracks of vheld = %+v, %v, want the hand-entered one kept", tracks, err)
	}
}

// A video a playlist gained latest is read first. An enriched or unavailable
// video, one a failure holds back for good or until later, and one in no
// playlist are not read.
func TestOnlyVideosWaitingAreReadNewestFirstUpToTheBatch(t *testing.T) {
	f := newFixture(t, "vold", "vretrydue", "venriched", "vunavailable", "vforever", "vlater", "vnew")
	ctx := context.Background()
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.SetVideoEnrichment(ctx, generated.SetVideoEnrichmentParams{VideoID: "venriched", Description: sql.NullString{Valid: true}, EnrichedTs: sql.NullString{String: store.Timestamp(start), Valid: true}}); err != nil {
			return err
		}
		if err := tx.UpsertUnavailableVideo(ctx, generated.UpsertUnavailableVideoParams{VideoID: "vunavailable", Title: "Private video"}); err != nil {
			return err
		}
		if err := tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: "vnowhere", Title: "Loose", ChannelTitle: "Channel"}); err != nil {
			return err
		}
		for video, retry := range map[string]sql.NullString{
			"vforever":  {},
			"vlater":    {String: store.Timestamp(start.Add(time.Second)), Valid: true},
			"vretrydue": {String: store.Timestamp(start), Valid: true},
		} {
			failure := generated.UpsertEnrichFailureParams{VideoID: video, AttemptedTs: store.Timestamp(start), Reason: "failed", Attempts: 1, RetryTs: retry}
			if err := tx.UpsertEnrichFailure(ctx, failure); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	f.e.limits.Batch = 2
	f.run(t)
	if want := []string{"vnew", "vretrydue"}; !slices.Equal(f.reader.read, want) {
		t.Fatalf("read %v, want %v", f.reader.read, want)
	}
	f.run(t)
	if want := []string{"vnew", "vretrydue", "vold"}; !slices.Equal(f.reader.read, want) {
		t.Fatalf("read %v after the next run, want %v", f.reader.read, want)
	}
	if _, failed := f.failure(t, "vretrydue"); failed {
		t.Fatal("vretrydue kept its failure after a read that stored it")
	}
}

func TestReadsArePacedWithAJitteredGap(t *testing.T) {
	f := newFixture(t, "va", "vb", "vc", "vd")
	f.run(t)
	if len(f.waited) != 3 {
		t.Fatalf("waited %v between 4 reads, want 3 waits", f.waited)
	}
	for _, d := range f.waited {
		if d < f.e.limits.Pace || d > f.e.limits.Pace+f.e.jitter {
			t.Fatalf("waited %v, want between %v and %v", d, f.e.limits.Pace, f.e.limits.Pace+f.e.jitter)
		}
	}
	if !slices.ContainsFunc(f.waited, func(d time.Duration) bool { return d != f.e.limits.Pace }) {
		t.Fatalf("waited %v, the pace every time, want jitter added", f.waited)
	}
}

// YouTube refuses the second read, so the run stops there, and a run within a
// day of a run recording the refusal reads nothing and says why.
func TestARateLimitStopsTheRunAndPausesReadsForADay(t *testing.T) {
	f := newFixture(t, "vc", "vb", "va")
	f.reader.answers["vb"] = fmt.Errorf("read video vb: %w: ERROR: rate-limited by YouTube", ytdlp.ErrRateLimited)

	report := f.run(t)
	if report.Reads != 2 || !report.RateLimited || len(report.Failures) != 1 || report.Failures[0].VideoID != "vb" || !errors.Is(report.Failures[0].Err, ytdlp.ErrRateLimited) {
		t.Fatalf("report %+v, want two reads stopped by vb's rate limit", report)
	}
	if _, failed := f.failure(t, "vb"); failed {
		t.Fatal("a rate-limited read recorded a failure of vb, which says nothing about vb")
	}
	_, err := f.st.Queries.InsertSyncRun(context.Background(), generated.InsertSyncRunParams{
		StartedTs: store.Timestamp(start), FinishedTs: store.Timestamp(start), QuotaDate: "2026-09-17", Outcome: store.OutcomePartial, IsRateLimited: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	delete(f.reader.answers, "vb")

	// The pause is what a refusal asks for, so it is recorded on the run rather
	// than as a failure of it. A run that failed for being paused would make
	// every run of the day after one refusal partial.
	f.clock = start.Add(RateLimitPause - time.Second)
	if report := f.run(t); report.Reads != 0 || !report.Paused || report.Failures != nil {
		t.Fatalf("report %+v a second short of a day, want no read, paused, and no failure", report)
	}
	f.clock = start.Add(RateLimitPause)
	if report := f.run(t); report.Reads != 2 || report.Paused {
		t.Fatalf("report %+v a day on, want vb and va read and no pause", report)
	}
}

func TestAVideoClosedToReadingIsNeverReadAgainAndFailsNothing(t *testing.T) {
	f := newFixture(t, "va")
	f.reader.answers["va"] = fmt.Errorf("read video va: %w: ERROR: Private video", ytdlp.ErrUnreadable)

	if report := f.run(t); report.Reads != 1 || report.Unreadable != 1 || report.Failures != nil {
		t.Fatalf("report %+v, want va read and found unreadable with no failure", report)
	}
	failure, failed := f.failure(t, "va")
	if !failed || failure.RetryTs.Valid || failure.Attempts != 1 || failure.AttemptedTs != store.Timestamp(start) {
		t.Fatalf("failure of va = %+v, %v, want one attempt held back for good", failure, failed)
	}
	f.clock = start.Add(365 * 24 * time.Hour)
	if report := f.run(t); report.Reads != 0 {
		t.Fatalf("a year on the run read %d videos, want none", report.Reads)
	}
}

// Each failed read waits twice as long as the last before the video is read
// again, and a read that stores the video clears its failure.
func TestAFailedReadWaitsLongerEachTime(t *testing.T) {
	f := newFixture(t, "va")
	cause := errors.New("read video va: yt-dlp: Unable to extract initial player response")
	f.reader.answers["va"] = cause

	if report := f.run(t); len(report.Failures) != 1 || report.Failures[0].VideoID != "va" || !errors.Is(report.Failures[0].Err, cause) {
		t.Fatalf("report %+v, want va's failure", report)
	}
	failure, _ := f.failure(t, "va")
	if failure.Attempts != 1 || failure.RetryTs.String != store.Timestamp(start.Add(6*time.Hour)) || failure.Reason != cause.Error() {
		t.Fatalf("failure of va = %+v, want one attempt retried 6 hours on with its reason", failure)
	}
	f.clock = start.Add(6*time.Hour - time.Second)
	if report := f.run(t); report.Reads != 0 {
		t.Fatalf("before its retry the run read %d videos, want none", report.Reads)
	}
	f.clock = start.Add(6 * time.Hour)
	f.run(t)
	if failure, _ := f.failure(t, "va"); failure.Attempts != 2 || failure.RetryTs.String != store.Timestamp(f.clock.Add(12*time.Hour)) {
		t.Fatalf("failure of va = %+v, want two attempts retried 12 hours on", failure)
	}
	delete(f.reader.answers, "va")
	f.clock = f.clock.Add(12 * time.Hour)
	if report := f.run(t); report.Enriched != 1 {
		t.Fatalf("report %+v, want va enriched", report)
	}
	if _, failed := f.failure(t, "va"); failed {
		t.Fatal("va kept its failure once a read stored it")
	}
}

func TestTheRetryWaitDoublesUpToAWeek(t *testing.T) {
	want := []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 96 * time.Hour, 168 * time.Hour, 168 * time.Hour}
	for i, wait := range want {
		if got := retryAfter(int64(i + 1)); got != wait {
			t.Errorf("retryAfter(%d) = %v, want %v", i+1, got, wait)
		}
	}
}

// The README states how enrichment reads as the code holds it.
func TestTheREADMEStatesHowEnrichmentReads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read the README: %v", err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, want := range []string{
		fmt.Sprintf("the first of its top %d comments holding at least %d timestamped lines", ytdlp.MaxComments, tracklist.MinimumTracks),
		fmt.Sprintf("tried again %d hours later, then %d, doubling up to a week", int(firstRetry.Hours()), int(2*firstRetry.Hours())),
		"no run reads for a day after",
		"or up to half as long again",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the README does not say %q", want)
		}
	}
	jitter := New(nil, nil, Limits{Pace: 2 * time.Second, Batch: 1}).jitter
	if RateLimitPause != 24*time.Hour || lastRetry != 7*24*time.Hour || jitter != time.Second {
		t.Errorf("the README says a day's pause, a week's longest wait and half the pace again, and the code holds %v, %v and %v", RateLimitPause, lastRetry, jitter)
	}
}

// A video whose reads keep storing no tracklist is let go after MaxAttempts,
// and reset-enrichment is what puts it back.
func TestAVideoStopsBeingReadAfterEnoughAttemptsAndCanBePutBack(t *testing.T) {
	f := newFixture(t, "va")
	f.reader.videos["va"] = withoutTracklist("va")
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if report := f.run(t); report.Reads != 1 {
			t.Fatalf("attempt %d read %d videos, want 1", attempt, report.Reads)
		}
		failure, _ := f.failure(t, "va")
		if failure.Attempts != int64(attempt) {
			t.Fatalf("after attempt %d the mark counts %d", attempt, failure.Attempts)
		}
		if failure.RetryTs.Valid != (attempt < MaxAttempts) {
			t.Fatalf("after attempt %d of %d the mark reads again = %v", attempt, MaxAttempts, failure.RetryTs.Valid)
		}
		f.clock = f.clock.Add(lastRetry)
	}
	if report := f.run(t); report.Reads != 0 {
		t.Fatalf("report %+v past %d attempts, want no read", report, MaxAttempts)
	}

	ctx := context.Background()
	held, err := f.st.Queries.ListEnrichFailuresHeld(ctx)
	if err != nil || len(held) != 1 || held[0].VideoID != "va" {
		t.Fatalf("held = %+v, %v, want va", held, err)
	}
	err = f.st.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ForgetReadsOfEnrichFailuresHeld(ctx); err != nil {
			return err
		}
		cleared, err := tx.ClearEnrichFailuresHeld(ctx)
		if err == nil && cleared != 1 {
			t.Errorf("cleared %d marks, want 1", cleared)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if report := f.run(t); report.Reads != 1 {
		t.Fatalf("report %+v once va was put back, want it read again", report)
	}
}

// Only a refusal worded the way ytdlp's markers spell it stops a run on its
// own, so a run also stops when its reads simply keep failing. Reading on into
// a refusal nobody recognized is what turns a pause into a block.
func TestARunStopsWhenItsReadsKeepFailing(t *testing.T) {
	f := newFixture(t, "vf", "ve", "vd", "vc", "vb", "va")
	cause := errors.New("read failed: ERROR: Unable to extract initial player response")
	for _, video := range []string{"va", "vb", "vc", "vd", "ve", "vf"} {
		f.reader.answers[video] = cause
	}

	report := f.run(t)
	if report.Reads != MaxConsecutiveFailures {
		t.Fatalf("report %+v, want the run stopped after %d reads failed", report, MaxConsecutiveFailures)
	}
	last := report.Failures[len(report.Failures)-1]
	if last.VideoID != "" || !errors.Is(last.Err, ErrReadsFailing) {
		t.Fatalf("the run's last failure = %+v, want the run's own, naming ErrReadsFailing", last)
	}
}

// Reads that fail here and there are ordinary, and only reads failing one after
// another say the reading itself is wrong. So a read that lands clears the
// count.
func TestFailedReadsWithReadsBetweenThemDoNotStopARun(t *testing.T) {
	f := newFixture(t, "vf", "ve", "vd", "vc", "vb", "va")
	cause := errors.New("read failed: ERROR: Unable to extract initial player response")
	for _, video := range []string{"va", "vc", "ve"} {
		f.reader.answers[video] = cause
	}

	report := f.run(t)
	if report.Reads != 6 || report.Enriched != 3 || len(report.Failures) != 3 {
		t.Fatalf("report %+v, want all 6 read with 3 enriched and 3 failures", report)
	}
	for _, failure := range report.Failures {
		if failure.VideoID == "" {
			t.Fatalf("report %+v carries a failure of the run, want only the three reads'", report)
		}
	}
}

// A read that answers slowly spends more of a run than its pace predicts, so
// the budget rather than the batch is what bounds the run.
func TestARunStopsOnceItsReadsHaveSpentItsBudget(t *testing.T) {
	f := newFixture(t, "vc", "vb", "va")
	f.e.limits.Budget = 90 * time.Second
	f.e.wait = func(_ context.Context, d time.Duration) error {
		f.waited = append(f.waited, d)
		f.clock = f.clock.Add(time.Minute)
		return f.waitErr
	}

	report := f.run(t)
	if report.Reads != 2 || len(report.Failures) != 1 || !errors.Is(report.Failures[0].Err, ErrBudgetSpent) {
		t.Fatalf("report %+v, want two reads and the budget spent", report)
	}
}

// A batch of no videos reads nothing, which is what lets a deployment run the
// API before yt-dlp is installed.
func TestABatchOfNoneReadsNothingAndNeedsNoReader(t *testing.T) {
	f := newFixture(t, "va")
	f.e.reader = nil
	f.e.limits.Batch = 0

	if report := f.run(t); report.Reads != 0 || report.Failures != nil {
		t.Fatalf("report %+v, want nothing read", report)
	}
}

func TestACanceledRunStopsBetweenReads(t *testing.T) {
	f := newFixture(t, "vb", "va")
	f.waitErr = context.Canceled

	report, err := f.e.Run(context.Background())
	if !errors.Is(err, context.Canceled) || report.Reads != 1 {
		t.Fatalf("Run = %+v, %v, want one read and the cancel", report, err)
	}
}
