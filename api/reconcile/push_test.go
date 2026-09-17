package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// editOrder sets the server's order of the playlist to videos, one character a
// video, through the API's own edit at the revision it reads first.
func editOrder(t *testing.T, mux *http.ServeMux, playlist youtube.PlaylistID, videos string) {
	t.Helper()
	target := "/api/v1/playlists/" + string(playlist)
	etag := send(t, mux, http.MethodGet, target, "", http.StatusOK).Header().Get("ETag")
	ids, _ := json.Marshal(strings.Split(videos, "")[:len(videos)])
	if videos == "" {
		ids = []byte("[]")
	}
	req := httptest.NewRequest(http.MethodPut, target+"/items", strings.NewReader(`{"video_ids": `+string(ids)+`}`))
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s/items %s at %s answered %d %s", target, videos, etag, rec.Code, rec.Body)
	}
}

// settled is the server's order of the playlist and whether every entry is on
// YouTube and the base is what the channel holds, confirmed.
func settledOn(t *testing.T, st *store.Store, f *fakeChannel, playlist youtube.PlaylistID) {
	t.Helper()
	ctx := context.Background()
	videos, ids := stored(t, st, playlist)
	if videos != f.videos(playlist) || !slices.Equal(ids, f.itemIDs(playlist)) {
		t.Fatalf("the server holds %q as %v, and YouTube %q as %v", videos, ids, f.videos(playlist), f.itemIDs(playlist))
	}
	base, err := store.Base(ctx, st.Queries, string(playlist))
	if err != nil {
		t.Fatal(err)
	}
	var baseIDs []string
	for _, item := range base {
		baseIDs = append(baseIDs, item.ItemID)
	}
	state, err := st.Queries.GetPlaylistState(ctx, string(playlist))
	if err != nil || !slices.Equal(baseIDs, f.itemIDs(playlist)) || state.BaseState != store.BaseCurrent {
		t.Fatalf("base %v in state %+v, %v, want YouTube's items %v, current", baseIDs, state, err, f.itemIDs(playlist))
	}
}

// afterTheLag moves the clock past the read lag of every write so far.
func afterTheLag(c *clock) {
	c.now = c.now.Add(youtube.ReadLag + time.Second)
}

func TestAnEditHereReachesYouTubeInTheFewestWrites(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.videoTitles["d"] = "Video d"
	editOrder(t, api(st, f), "PLA", "cad")

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "cad" || report.Writes != 3 || report.WriteUnits != 3*youtube.WriteUnits {
		t.Fatalf("YouTube holds %q after %d writes costing %d units, want cad after a delete, a move and an insert", f.videos("PLA"), report.Writes, report.WriteUnits)
	}
	settledOn(t, st, f, "PLA")

	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 || report.PlaylistsDeferred != 0 {
		t.Fatalf("the next run made %d writes and deferred %d playlists, want neither", report.Writes, report.PlaylistsDeferred)
	}
	settledOn(t, st, f, "PLA")
}

func TestAnEditHereAndAnEditOnYouTubeBothLand(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abcd"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "abd")
	f.add("PLA", "x", 4)

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "abdx" || report.ItemsAdded != 1 || report.Writes != 1 {
		t.Fatalf("YouTube holds %q with report %+v, want abdx after one delete and x added here", f.videos("PLA"), report)
	}
	settledOn(t, st, f, "PLA")
}

func TestAReorderOnYouTubeWinsOverOneHere(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "bac")
	moved := f.items["PLA"][2]
	f.remove("PLA", 2)
	f.items["PLA"] = slices.Insert(f.items["PLA"], 0, moved)

	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 {
		t.Fatalf("the run made %d writes, want none", report.Writes)
	}
	settledOn(t, st, f, "PLA")
	if f.videos("PLA") != "cab" {
		t.Fatalf("YouTube holds %q, want its own order cab", f.videos("PLA"))
	}
}

// The insert lands and its answer is lost, so the store cannot know it landed
// until a read after the lag shows it.
func TestAnInsertWhoseAnswerIsLostIsNotMadeTwice(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.videoTitles["d"] = "Video d"
	editOrder(t, api(st, f), "PLA", "abd")
	f.faults[1] = fault{applied: true, err: errors.New("connection reset")}

	report := mustRun(t, ctx, r, store.OutcomePartial)
	state, err := st.Queries.GetPlaylistState(ctx, "PLA")
	if f.videos("PLA") != "abd" || report.Writes != 1 || err != nil || state.BaseState != store.BaseUnconfirmed {
		t.Fatalf("YouTube holds %q after %d writes, base %+v, %v; want abd after one write and the base unconfirmed", f.videos("PLA"), report.Writes, state, err)
	}
	if row, err := st.Queries.GetYouTubeWrite(ctx, 1); err != nil || row.Outcome != store.WriteUnanswered || row.VideoID.String != "d" || row.Position.Int64 != 2 {
		t.Fatalf("recorded write %+v, %v, want an unanswered insert of d at 2", row, err)
	}

	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 {
		t.Fatalf("the next run made %d writes, want none", report.Writes)
	}
	settledOn(t, st, f, "PLA")
}

// Reversing four items takes three moves. The second lands and its answer is
// lost, which leaves YouTube's order between the two and no base saying so.
func TestMovesWhoseAnswerIsLostFinishInTheServersOrder(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abcd"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "dcba")
	f.faults[2] = fault{applied: true, err: errors.New("connection reset")}

	if report := mustRun(t, ctx, r, store.OutcomePartial); report.Writes != 2 {
		t.Fatalf("the run made %d writes, want it to stop after the second", report.Writes)
	}
	afterTheLag(c)
	mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "dcba" {
		t.Fatalf("YouTube holds %q, want the server's dcba", f.videos("PLA"))
	}
	settledOn(t, st, f, "PLA")
}

func TestARunLeavesItemsWrittenWithinTheLagForTheNextRun(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab", "PLB": "c"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "ba")
	mustRun(t, ctx, r, store.OutcomeOK)
	f.add("PLA", "x", 0)

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if videos, _ := stored(t, st, "PLA"); report.PlaylistsDeferred != 1 || report.Playlists != 1 || videos != "ba" {
		t.Fatalf("report %+v with PLA holding %q, want PLA deferred as ba and PLB stored", report, videos)
	}

	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.PlaylistsDeferred != 0 || report.ItemsAdded != 1 {
		t.Fatalf("report %+v, want x added and nothing deferred", report)
	}
	settledOn(t, st, f, "PLA")
}

func TestAVideoYouTubeRefusesToAddLeavesTheServersOrder(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.videoTitles["z"] = "Video z"
	f.refusedVideos["z"] = true
	mux := api(st, f)
	editOrder(t, mux, "PLA", "abz")
	edited := send(t, mux, http.MethodGet, "/api/v1/playlists/PLA", "", http.StatusOK).Header().Get("ETag")

	report := mustRun(t, ctx, r, store.OutcomePartial)
	if !errors.Is(report.Failures[0].Err, youtube.ErrVideoRefused) || f.videos("PLA") != "ab" {
		t.Fatalf("failures %v with YouTube holding %q, want z refused and ab", report.Failures, f.videos("PLA"))
	}
	revision, _ := strconv.Atoi(strings.Trim(edited, `"`))
	if etag := send(t, mux, http.MethodGet, "/api/v1/playlists/PLA", "", http.StatusOK).Header().Get("ETag"); etag != `"`+strconv.Itoa(revision+1)+`"` {
		t.Fatalf("PLA is at %s after the refusal, and was at %s after the edit, want one revision more", etag, edited)
	}
	if videos, _ := stored(t, st, "PLA"); videos != "ab" {
		t.Fatalf("the server holds %q, want ab with z dropped", videos)
	}
	afterTheLag(c)
	mustRun(t, ctx, r, store.OutcomeOK)
	settledOn(t, st, f, "PLA")
}

// A playlist YouTube orders itself refuses the insert naming a position, and
// the run after appends it and moves nothing.
func TestAPlaylistYouTubeOrdersItselfTakesAppends(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.automatic["PLA"] = true
	f.videoTitles["d"] = "Video d"
	editOrder(t, api(st, f), "PLA", "dcab")

	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || f.videos("PLA") != "abc" {
		t.Fatalf("report %+v with YouTube holding %q, want one refused write and abc", report, f.videos("PLA"))
	}
	if state, err := st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Sort != store.SortAutomatic {
		t.Fatalf("state %+v, %v, want the playlist sorted automatically", state, err)
	}
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || f.videos("PLA") != "abcd" {
		t.Fatalf("report %+v with YouTube holding %q, want one append leaving abcd", report, f.videos("PLA"))
	}
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 {
		t.Fatalf("the next run made %d writes, want none", report.Writes)
	}
}

// spend records a write sent at at that cost units.
func spend(t *testing.T, st *store.Store, units int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	err := st.InTx(ctx, func(tx *store.Tx) error {
		id, err := tx.BeginWrite(ctx, store.Write{Method: youtube.MethodPlaylistsUpdate, PlaylistID: "PLother", SentAt: at})
		if err != nil {
			return err
		}
		return tx.SettleWrite(ctx, store.Settlement{WriteID: id, PlaylistID: "PLother", Outcome: store.WriteApplied, SettledAt: at, Requests: 1, Units: units})
	})
	if err != nil {
		t.Fatalf("record %d units: %v", units, err)
	}
}

// A run reads 2 units, and 16 hourly runs are left before midnight Pacific, so
// a write needs 50 units beside what the day spent and a reserve of 32. The first
// run's 2 units are recorded. So 9,920 units of writes leave 9,924 spent, and
// 9,924 + 50 + 32 is past the quota, while without the reserve it is not.
func TestTheAllowanceKeepsTheReadsOfTheDaysRemainingRuns(t *testing.T) {
	for _, c := range []struct {
		spent   int64
		outcome string
		writes  int
	}{
		{9_900, store.OutcomeOK, 1},
		{9_920, store.OutcomePartial, 0},
	} {
		t.Run(strconv.FormatInt(c.spent, 10), func(t *testing.T) {
			f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
			r, st, clock := newRunner(t, f)
			ctx := context.Background()
			mustRun(t, ctx, r, store.OutcomeOK)
			editOrder(t, api(st, f), "PLA", "ba")
			spend(t, st, c.spent, clock.now)

			report := mustRun(t, ctx, r, c.outcome)
			if report.Writes != c.writes {
				t.Fatalf("the run made %d writes, want %d", report.Writes, c.writes)
			}
			if c.writes == 0 && !errors.Is(report.Failures[0].Err, ErrAllowanceSpent) {
				t.Fatalf("failures %v, want ErrAllowanceSpent", report.Failures)
			}
		})
	}
}

// The order is edited again while the push makes its first move, so the push
// stops and the next run pushes the newer order.
func TestAnEditDuringAPushStopsIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	editOrder(t, mux, "PLA", "cba")
	f.beforeWrite = func(n int) {
		if n == 1 {
			editOrder(t, mux, "PLA", "bca")
		}
	}

	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 {
		t.Fatalf("the run made %d writes, want it to stop after the first", report.Writes)
	}
	f.beforeWrite = nil
	afterTheLag(c)
	mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "bca" {
		t.Fatalf("YouTube holds %q, want the newer order bca", f.videos("PLA"))
	}
	settledOn(t, st, f, "PLA")
}

func TestAnItemTheReadMissesButYouTubeStillHasSkipsThePlaylist(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.hidden[f.items["PLA"][1].ID] = true

	report := mustRun(t, ctx, r, store.OutcomePartial)
	if videos, _ := stored(t, st, "PLA"); report.PlaylistsSkipped != 1 || !errors.Is(report.Failures[0].Err, ErrAbsenceNotConfirmed) || videos != "abc" {
		t.Fatalf("report %+v with PLA holding %q, want PLA skipped as abc with ErrAbsenceNotConfirmed", report, videos)
	}
}

func TestYouTubesQuotaRefusalOfAWriteEndsTheRun(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "ba")
	f.quota = f.units + 2

	mustRun(t, ctx, r, store.OutcomeQuotaSpent)
	state, err := st.Queries.GetPlaylistState(ctx, "PLA")
	if row, rowErr := st.Queries.GetYouTubeWrite(ctx, 1); err != nil || rowErr != nil || state.BaseState != store.BaseCurrent || row.Outcome != store.WriteQuotaSpent {
		t.Fatalf("state %+v, %v and write %+v, %v, want the refusal recorded and the base current", state, err, row, rowErr)
	}
}

// The run is canceled while YouTube makes its first write, which is still made
// and recorded, and no write follows it.
func TestACanceledRunSettlesTheWriteInFlight(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.videoTitles["d"] = "Video d"
	f.videoTitles["e"] = "Video e"
	editOrder(t, api(st, f), "PLA", "abde")
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeWrite = func(int) { cancel() }

	report := mustRun(t, ctx, r, store.OutcomeCanceled)
	videos, ids := stored(t, st, "PLA")
	if report.Writes != 1 || f.videos("PLA") != "abd" || videos != "abde" || ids[2] == "" || ids[3] != "" {
		t.Fatalf("report %+v with YouTube holding %q and the server %q as %v, want d made and recorded and e left", report, f.videos("PLA"), videos, ids)
	}
	if row, err := st.Queries.GetYouTubeWrite(context.Background(), 1); err != nil || row.Outcome != store.WriteApplied || row.ItemID != (sql.NullString{String: ids[2], Valid: true}) {
		t.Fatalf("recorded write %+v, %v, want the insert applied as %s", row, err, ids[2])
	}
}
