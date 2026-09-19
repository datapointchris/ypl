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

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// editOrder sets the server's order of the playlist to videos, one character a
// video, through the API's own edit at the ETag it reads first.
func editOrder(t *testing.T, mux *http.ServeMux, playlist youtube.PlaylistID, videos string) {
	t.Helper()
	target := "/api/v1/playlists/" + string(playlist) + "/items"
	etag := send(t, mux, http.MethodGet, target, "", http.StatusOK).Header().Get("ETag")
	ids, _ := json.Marshal(strings.Split(videos, "")[:len(videos)])
	if videos == "" {
		ids = []byte("[]")
	}
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(`{"video_ids": `+string(ids)+`}`))
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s %s at %s answered %d %s", target, videos, etag, rec.Code, rec.Body)
	}
}

// etag is the ETag of the playlist's order.
func etag(t *testing.T, mux *http.ServeMux, playlist youtube.PlaylistID) string {
	t.Helper()
	return send(t, mux, http.MethodGet, "/api/v1/playlists/"+string(playlist)+"/items", "", http.StatusOK).Header().Get("ETag")
}

// settledOn fails t unless the server's order of the playlist is what YouTube
// holds, with every entry on YouTube, and the base is YouTube's items with no
// write unanswered against it.
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
	if err != nil || !slices.Equal(baseIDs, f.itemIDs(playlist)) || state.UnansweredWriteID.Valid {
		t.Fatalf("base %v in state %+v, %v, want YouTube's items %v with no write unanswered", baseIDs, state, err, f.itemIDs(playlist))
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
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abce"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "abe")
	f.add("PLA", "x", 4)

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "abex" || report.ItemsAdded != 1 || report.Writes != 1 {
		t.Fatalf("YouTube holds %q with report %+v, want abex after one delete and x added here", f.videos("PLA"), report)
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

// An edit's one write lands after y is added first on YouTube, so the write
// puts its item one place early. The next run keeps y and moves the item to
// where the edit put it.
func TestAnAdditionOnYouTubeDuringAPushKeepsTheServersOrder(t *testing.T) {
	cases := []struct {
		name, held, edit, pushed, synced string
	}{
		{name: "a move", held: "abcde", edit: "bcdea", pushed: "ybcdae", synced: "ybcdea"},
		{name: "an insert", held: "abc", edit: "aebc", pushed: "yeabc", synced: "yaebc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": c.held})
			r, st, clock := newRunner(t, f)
			ctx := context.Background()
			mustRun(t, ctx, r, store.OutcomeOK)
			f.videoTitles["e"] = "Video e"
			editOrder(t, api(st, f), "PLA", c.edit)
			f.beforeWrite = func(n int) {
				if n == 1 {
					f.add("PLA", "y", 0)
				}
			}

			mustRun(t, ctx, r, store.OutcomeOK)
			if f.videos("PLA") != c.pushed {
				t.Fatalf("YouTube holds %q after the push, want %s", f.videos("PLA"), c.pushed)
			}
			afterTheLag(clock)
			report := mustRun(t, ctx, r, store.OutcomeOK)
			if f.videos("PLA") != c.synced || report.ItemsAdded != 1 || report.Writes != 1 {
				t.Fatalf("YouTube holds %q with report %+v, want %s after y added there and one move", f.videos("PLA"), report, c.synced)
			}
			settledOn(t, st, f, "PLA")
		})
	}
}

// YouTube loses c just before the push moves it, so the move finds it gone and
// the push of the playlist ends. The next run reads c gone and moves b.
func TestAnItemGoneFromYouTubeDuringAPushEndsThePlaylistsPush(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	editOrder(t, api(st, f), "PLA", "cba")
	f.beforeWrite = func(n int) {
		if n == 1 {
			f.remove("PLA", 2)
		}
	}

	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || f.videos("PLA") != "ab" {
		t.Fatalf("report %+v with YouTube holding %q, want the push ended after the move of c", report, f.videos("PLA"))
	}
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || report.ItemsRemoved != 1 || f.videos("PLA") != "ba" {
		t.Fatalf("report %+v with YouTube holding %q, want c removed and b moved", report, f.videos("PLA"))
	}
	settledOn(t, st, f, "PLA")
}

// The insert lands and its answer is lost, so the store cannot know it landed
// until a read after the lag shows it.
func TestAnInsertWhoseAnswerIsLostIsNotMadeTwice(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	f.videoTitles["e"] = "Video e"
	editOrder(t, api(st, f), "PLA", "abe")
	f.faults[1] = fault{applied: true, err: errors.New("connection reset")}

	report := mustRun(t, ctx, r, store.OutcomePartial)
	state, err := st.Queries.GetPlaylistState(ctx, "PLA")
	if f.videos("PLA") != "abe" || report.Writes != 1 || err != nil || state.UnansweredWriteID != (sql.NullInt64{Int64: 1, Valid: true}) {
		t.Fatalf("YouTube holds %q after %d writes, state %+v, %v; want abe after one write, write 1 unanswered", f.videos("PLA"), report.Writes, state, err)
	}
	_, ids := stored(t, st, "PLA")
	if row, err := st.Queries.GetYouTubeWrite(ctx, 1); err != nil || row.Outcome != store.WriteUnanswered || row.VideoID.String != "e" || row.Position.Int64 != 2 || !row.EntryID.Valid {
		t.Fatalf("recorded write %+v, %v, with the server's items %v, want an unanswered insert of e at 2 for its entry", row, err, ids)
	}

	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 || report.ItemsAdded != 0 {
		t.Fatalf("the next run made %d writes and added %d items, want neither", report.Writes, report.ItemsAdded)
	}
	settledOn(t, st, f, "PLA")
}

// The insert of e lands with its answer lost, and an edit then removes e, so the
// item YouTube made is the server's own and is deleted.
func TestAnInsertWhoseAnswerIsLostAndWhoseEntryAnEditRemovedIsDeleted(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	f.videoTitles["e"] = "Video e"
	editOrder(t, mux, "PLA", "abe")
	f.faults[1] = fault{applied: true, err: errors.New("connection reset")}
	mustRun(t, ctx, r, store.OutcomePartial)
	editOrder(t, mux, "PLA", "ab")

	afterTheLag(c)
	report := mustRun(t, ctx, r, store.OutcomeOK)
	if f.videos("PLA") != "ab" || report.ItemsAdded != 0 || report.Writes != 1 {
		t.Fatalf("YouTube holds %q with report %+v, want ab after one delete and nothing added", f.videos("PLA"), report)
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
	edited := etag(t, mux, "PLA")

	report := mustRun(t, ctx, r, store.OutcomePartial)
	if !errors.Is(report.Failures[0].Err, youtube.ErrVideoRefused) || f.videos("PLA") != "ab" {
		t.Fatalf("failures %v with YouTube holding %q, want z refused and ab", report.Failures, f.videos("PLA"))
	}
	revision, _ := strconv.Atoi(strings.Trim(edited, `"`))
	if got := etag(t, mux, "PLA"); got != `"`+strconv.Itoa(revision+1)+`"` {
		t.Fatalf("PLA's order is at %s after the refusal, and was at %s after the edit, want one change more", got, edited)
	}
	if videos, _ := stored(t, st, "PLA"); videos != "ab" {
		t.Fatalf("the server holds %q, want ab with z dropped", videos)
	}
	afterTheLag(c)
	mustRun(t, ctx, r, store.OutcomeOK)
	settledOn(t, st, f, "PLA")
}

// A playlist YouTube orders itself refuses the insert naming a position, which
// the run records. The next run takes YouTube's order and appends e, which
// YouTube puts first, and the one after changes nothing. When the channel orders
// it by hand again, an edit of its order reaches YouTube in positions.
func TestAPlaylistYouTubeOrdersItselfTakesYouTubesOrderUntilAnEdit(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	f.automatic["PLA"] = true
	f.videoTitles["e"] = "Video e"
	editOrder(t, mux, "PLA", "ecab")

	report := mustRun(t, ctx, r, store.OutcomePartial)
	if report.Writes != 1 || !errors.Is(report.Failures[0].Err, youtube.ErrManualSortRequired) || f.videos("PLA") != "abc" {
		t.Fatalf("report %+v with YouTube holding %q, want one refused write recorded and abc", report, f.videos("PLA"))
	}
	if state, err := st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Sort != store.SortAutomatic {
		t.Fatalf("state %+v, %v, want the playlist sorted automatically", state, err)
	}
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || f.videos("PLA") != "eabc" {
		t.Fatalf("report %+v with YouTube holding %q, want one append that YouTube put first", report, f.videos("PLA"))
	}
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 0 {
		t.Fatalf("the next run made %d writes, want none", report.Writes)
	}
	settledOn(t, st, f, "PLA")

	f.automatic["PLA"] = false
	editOrder(t, mux, "PLA", "cabe")
	afterTheLag(c)
	mustRun(t, ctx, r, store.OutcomeOK)
	if state, err := st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Sort != store.SortManual || f.videos("PLA") != "cabe" {
		t.Fatalf("state %+v, %v with YouTube holding %q, want cabe sorted manually", state, err, f.videos("PLA"))
	}
	settledOn(t, st, f, "PLA")
}

// YouTube refuses the move for a reason that says nothing about the video or the
// playlist's sort. A later run that day holds the push and records why, the
// next day's run sends the move again, and an edit sends the push it plans.
func TestARefusedWriteWaitsForTheNextDayOrAChange(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, c := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	editOrder(t, mux, "PLA", "ba")
	f.faults[1] = fault{err: refusedAs(nil)}
	f.faults[2] = fault{err: refusedAs(nil)}

	if report := mustRun(t, ctx, r, store.OutcomePartial); report.Writes != 1 || !errors.Is(report.Failures[0].Err, youtube.ErrRefused) {
		t.Fatalf("report %+v, want one write refused", report)
	}
	afterTheLag(c)
	report := mustRun(t, ctx, r, store.OutcomePartial)
	if report.Writes != 0 || !errors.Is(report.Failures[0].Err, ErrPushHeld) || !strings.Contains(report.Failures[0].Err.Error(), "write 1") {
		t.Fatalf("report %+v, want no write and the push held on write 1", report)
	}
	c.now = c.now.Add(24 * time.Hour)
	if report := mustRun(t, ctx, r, store.OutcomePartial); report.Writes != 1 || errors.Is(report.Failures[0].Err, ErrPushHeld) {
		t.Fatalf("report %+v, want the move sent again the next day and refused", report)
	}
	editOrder(t, mux, "PLA", "b")
	afterTheLag(c)
	if report := mustRun(t, ctx, r, store.OutcomeOK); report.Writes != 1 || f.videos("PLA") != "b" {
		t.Fatalf("report %+v with YouTube holding %q, want the edit's delete made", report, f.videos("PLA"))
	}
	settledOn(t, st, f, "PLA")
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

// PLA, PLB and PLC each carry an edit. The API deletes PLB while PLA's push
// makes its write, so PLB's push ends before it begins and PLC's is made.
func TestAPlaylistTheAPIDeletesBeforeItsPushEndsOnlyItsPush(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab", "PLB": "ce", "PLC": "fg"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	editOrder(t, mux, "PLA", "ba")
	editOrder(t, mux, "PLB", "ec")
	editOrder(t, mux, "PLC", "gf")
	f.beforeWrite = func(n int) {
		if n == 1 {
			send(t, mux, http.MethodDelete, "/api/v1/playlists/PLB", "", http.StatusNoContent)
		}
	}

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if report.Writes != 2 || f.videos("PLA") != "ba" || f.videos("PLC") != "gf" {
		t.Fatalf("report %+v with PLA holding %q and PLC %q, want PLA and PLC pushed in one write each", report, f.videos("PLA"), f.videos("PLC"))
	}
}

// The API deletes PLA while its push deletes an item of it, so YouTube answers
// that the item is gone, and the write settles with the base and entries gone
// with the playlist. PLB's push is made.
func TestAPlaylistTheAPIDeletesDuringItsPushSettlesTheWrite(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "fg"})
	r, st, _ := newRunner(t, f)
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)
	editOrder(t, mux, "PLA", "ab")
	editOrder(t, mux, "PLB", "gf")
	f.beforeWrite = func(n int) {
		if n == 1 {
			send(t, mux, http.MethodDelete, "/api/v1/playlists/PLA", "", http.StatusNoContent)
		}
	}

	report := mustRun(t, ctx, r, store.OutcomeOK)
	if report.Writes != 2 || f.videos("PLB") != "gf" {
		t.Fatalf("report %+v with PLB holding %q, want PLA's write settled and PLB pushed", report, f.videos("PLB"))
	}
	if row, err := st.Queries.GetYouTubeWrite(ctx, 1); err != nil || row.Method != youtube.MethodPlaylistItemsDelete || row.Outcome != store.WriteAbsent {
		t.Fatalf("PLA's write = %+v, %v, want the item delete settled as absent", row, err)
	}
	if spent, err := st.Queries.SumWriteUnits(ctx, youtube.QuotaDate(start)); err != nil || spent.Pending != 0 {
		t.Fatalf("writes = %+v, %v, want none left pending", spent, err)
	}
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
	if row, rowErr := st.Queries.GetYouTubeWrite(ctx, 1); err != nil || rowErr != nil || state.UnansweredWriteID.Valid || row.Outcome != store.WriteQuotaSpent {
		t.Fatalf("state %+v, %v and write %+v, %v, want the refusal recorded and no write unanswered", state, err, row, rowErr)
	}
}

// The run is canceled while YouTube makes its first write, which is still made
// and recorded, and no write follows it.
func TestACanceledRunSettlesTheWriteInFlight(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "ab"})
	r, st, _ := newRunner(t, f)
	mustRun(t, context.Background(), r, store.OutcomeOK)
	f.videoTitles["e"] = "Video e"
	f.videoTitles["f"] = "Video f"
	editOrder(t, api(st, f), "PLA", "abef")
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeWrite = func(int) { cancel() }

	report := mustRun(t, ctx, r, store.OutcomeCanceled)
	videos, ids := stored(t, st, "PLA")
	if report.Writes != 1 || f.videos("PLA") != "abe" || videos != "abef" || ids[2] == "" || ids[3] != "" {
		t.Fatalf("report %+v with YouTube holding %q and the server %q as %v, want e made and recorded and f left", report, f.videos("PLA"), videos, ids)
	}
	if row, err := st.Queries.GetYouTubeWrite(context.Background(), 1); err != nil || row.Outcome != store.WriteApplied || row.ItemID != (sql.NullString{String: ids[2], Valid: true}) {
		t.Fatalf("recorded write %+v, %v, want the insert applied as %s", row, err, ids[2])
	}
}

// Every outcome the store records for a write means something to the push,
// except pending, which no answered write ends as.
func TestEveryWriteOutcomeMeansSomethingToThePush(t *testing.T) {
	for _, outcome := range append(store.WriteOutcomes(), "someFutureOutcome") {
		_, err := answerOf(outcome, merge.Insert)
		if known := outcome != store.WritePending && outcome != "someFutureOutcome"; (err == nil) != known {
			t.Errorf("answerOf(%q) = %v, want an answer %v", outcome, err, known)
		}
	}
}

func TestEverySortTheStoreHoldsHasAMergeSort(t *testing.T) {
	for _, sort := range store.PlaylistSorts() {
		if _, err := mergeSort(sort); err != nil {
			t.Errorf("mergeSort(%q) = %v, want a sort", sort, err)
		}
	}
	if sort, err := mergeSort("someFutureSort"); err == nil {
		t.Errorf("mergeSort of an unknown sort = %q, want it refused", sort)
	}
}
