package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

type wireOrder struct {
	VideoIDs []string `json:"video_ids"`
}

type wireVideoRefusal struct {
	wireRefusal
	VideoIDs []string `json:"video_ids"`
}

// editItems sends PUT /api/v1/playlists/{id}/items for videos with the If-Match
// field lines given.
func (f *fixture) editItems(playlist string, videos []string, ifMatch ...string) *httptest.ResponseRecorder {
	ids, _ := json.Marshal(videos)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/playlists/"+playlist+"/items", strings.NewReader(`{"video_ids": `+string(ids)+`}`))
	for _, value := range ifMatch {
		req.Header.Add("If-Match", value)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

// etag is the ETag of the playlist's order.
func (f *fixture) etag(t *testing.T, playlist string) string {
	t.Helper()
	rec := f.get("/api/v1/playlists/" + playlist + "/items")
	decode[wireOrder](t, rec, http.StatusOK)
	return rec.Header().Get("ETag")
}

// later is the ETag an order takes after one change to it from etag.
func later(t *testing.T, etag string) string {
	t.Helper()
	revision, err := strconv.ParseInt(strings.Trim(etag, `"`), 10, 64)
	if err != nil {
		t.Fatalf("ETag %s holds no revision: %v", etag, err)
	}
	return revisionTag(revision + 1)
}

// order is each item of the playlist as video and item id, "b:ib a:ia a:".
func (f *fixture) order(t *testing.T, playlist string) string {
	t.Helper()
	var fields []string
	for _, item := range decode[wirePlaylist](t, f.get("/api/v1/playlists/"+playlist), http.StatusOK).Items {
		fields = append(fields, item.Video.ID+":"+item.itemID())
	}
	return strings.Join(fields, " ")
}

// Alpha holds a, b and u as ia, ib and iu. Each video takes the earliest entry
// holding it, so the second a is a new entry with no item yet.
func TestAnEditSetsTheOrderAndKeepsEachEntrysItem(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")

	rec := f.editItems("PLA", []string{"b", "a", "a"}, before)
	edited := decode[wireOrder](t, rec, http.StatusOK)
	if !slices.Equal(edited.VideoIDs, []string{"b", "a", "a"}) || rec.Header().Get("ETag") != later(t, before) {
		t.Fatalf("edited to %v with ETag %q, want b a a with %s", edited.VideoIDs, rec.Header().Get("ETag"), later(t, before))
	}
	if got := f.order(t, "PLA"); got != "b:ib a:ia a:" {
		t.Fatalf("stored %s, want b:ib a:ia a:", got)
	}
	shown := f.get("/api/v1/playlists/PLA/items")
	if got := decode[wireOrder](t, shown, http.StatusOK); !slices.Equal(got.VideoIDs, edited.VideoIDs) || shown.Header().Get("ETag") != rec.Header().Get("ETag") {
		t.Fatalf("GET answered %v with ETag %q, want the edit's %v with %q", got.VideoIDs, shown.Header().Get("ETag"), edited.VideoIDs, rec.Header().Get("ETag"))
	}
	if n := f.youtube.writes(); n != 0 || f.youtube.videoReads != 0 {
		t.Fatalf("YouTube saw %d writes and %d reads of videos, want none", n, f.youtube.videoReads)
	}
}

func TestAnOrderIsReadWithItsETag(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	rec := f.get("/api/v1/playlists/PLC/items")
	if got := decode[wireOrder](t, rec, http.StatusOK); got.VideoIDs == nil || len(got.VideoIDs) != 0 || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("an empty playlist's order = %v with ETag %q, want [] with \"1\"", got.VideoIDs, rec.Header().Get("ETag"))
	}
	refused(t, f.get("/api/v1/playlists/PLZ/items"), http.StatusNotFound, wire.CodeNotFound)
}

// With a stored a and a second a not yet on YouTube, an order naming one a
// keeps the earlier, which YouTube holds.
func TestARepeatedVideoKeepsItsEarliestEntry(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	decode[wireOrder](t, f.editItems("PLA", []string{"a", "b", "u", "a"}, f.etag(t, "PLA")), http.StatusOK)
	decode[wireOrder](t, f.editItems("PLA", []string{"u", "a"}, f.etag(t, "PLA")), http.StatusOK)
	if got := f.order(t, "PLA"); got != "u:iu a:ia" {
		t.Fatalf("edited to %s, want u:iu a:ia keeping the a YouTube holds", got)
	}
}

func TestAnEditThatChangesNothingKeepsTheETag(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")

	rec := f.editItems("PLA", []string{"a", "b", "u"}, before)
	decode[wireOrder](t, rec, http.StatusOK)
	if got := rec.Header().Get("ETag"); got != before || f.order(t, "PLA") != "a:ia b:ib u:iu" {
		t.Fatalf("edited to %s with ETag %q, want the order as it was at %s", f.order(t, "PLA"), got, before)
	}
}

// If-Match is read as RFC 9110 reads it. Each case names the field lines it
// sends, where current stands for the order's ETag.
func TestAnEditIsMadeOnlyWhenIfMatchMatchesTheOrder(t *testing.T) {
	const current = "current"
	cases := []struct {
		name    string
		ifMatch []string
		status  int
		code    wire.Code
	}{
		{name: "no If-Match", status: http.StatusPreconditionRequired, code: wire.CodePreconditionRequired},
		{name: "the current tag", ifMatch: []string{current}, status: http.StatusOK},
		{name: "any current order", ifMatch: []string{"*"}, status: http.StatusOK},
		{name: "a list holding the current tag", ifMatch: []string{`"99", ` + current}, status: http.StatusOK},
		{name: "field lines holding the current tag", ifMatch: []string{`"99"`, current}, status: http.StatusOK},
		{name: "another tag", ifMatch: []string{`"99"`}, status: http.StatusPreconditionFailed, code: wire.CodePreconditionFailed},
		{name: "a tag no revision has", ifMatch: []string{`"x"`}, status: http.StatusPreconditionFailed, code: wire.CodePreconditionFailed},
		{name: "the current tag weak", ifMatch: []string{"W/" + current}, status: http.StatusPreconditionFailed, code: wire.CodePreconditionFailed},
		{name: "an unquoted tag", ifMatch: []string{`2`}, status: http.StatusBadRequest, code: wire.CodeInvalidPrecondition},
		{name: "an unclosed tag", ifMatch: []string{`"2`}, status: http.StatusBadRequest, code: wire.CodeInvalidPrecondition},
		{name: "two tags with no comma", ifMatch: []string{`"1" "2"`}, status: http.StatusBadRequest, code: wire.CodeInvalidPrecondition},
		{name: "any beside a tag", ifMatch: []string{`*, "2"`}, status: http.StatusBadRequest, code: wire.CodeInvalidPrecondition},
		{name: "an empty field", ifMatch: []string{""}, status: http.StatusBadRequest, code: wire.CodeInvalidPrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.withLibrary(t)
			before := f.etag(t, "PLA")
			var fields []string
			for _, field := range c.ifMatch {
				fields = append(fields, strings.ReplaceAll(field, current, before))
			}

			rec := f.editItems("PLA", []string{"b"}, fields...)
			want := "a:ia b:ib u:iu"
			if c.status == http.StatusOK {
				decode[wireOrder](t, rec, http.StatusOK)
				want = "b:ib"
			} else {
				refused(t, rec, c.status, c.code)
			}
			if got := f.order(t, "PLA"); got != want {
				t.Fatalf("PLA = %s after If-Match %q, want %s", got, fields, want)
			}
		})
	}
}

// An edit naming an order that has moved on is refused before it reads a video
// from YouTube.
func TestAStaleEditReadsNothingFromYouTube(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}

	refused(t, f.editItems("PLA", []string{"newvid"}, `"99"`), http.StatusPreconditionFailed, wire.CodePreconditionFailed)
	if f.youtube.videoReads != 0 {
		t.Fatalf("YouTube saw %d reads of videos, want none", f.youtube.videoReads)
	}
}

func TestAnEditOfAPlaylistNotStoredIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	refused(t, f.editItems("PLZ", []string{"b"}, `"1"`), http.StatusNotFound, wire.CodeNotFound)
}

// Another edit lands while this one reads a video from YouTube, so the order it
// named is gone by the time it would store.
func TestAnEditRacedByAnotherIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}
	f.youtube.beforeVideos = func() {
		f.youtube.beforeVideos = nil
		decode[wireOrder](t, f.editItems("PLA", []string{"b"}, before), http.StatusOK)
	}

	refused(t, f.editItems("PLA", []string{"newvid", "a"}, before), http.StatusPreconditionFailed, wire.CodePreconditionFailed)
	if got := f.order(t, "PLA"); got != "b:ib" || f.etag(t, "PLA") != later(t, before) {
		t.Fatalf("PLA = %s with ETag %s, want the other edit's b at %s", got, f.etag(t, "PLA"), later(t, before))
	}
}

// The other edit sets the very order this one names, so this one would change
// nothing, and is refused all the same for naming an order that is gone. The
// other edit finds the video already stored, since this one holds the write turn
// while it reads the video from YouTube.
func TestAnEditRacedByAnIdenticalEditIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}
	f.youtube.beforeVideos = func() {
		f.youtube.beforeVideos = nil
		stored := generated.UpsertAvailableVideoParams{VideoID: "newvid", Title: "New", ChannelTitle: "Six"}
		if err := f.st.Queries.UpsertAvailableVideo(context.Background(), stored); err != nil {
			t.Errorf("store newvid: %v", err)
		}
		decode[wireOrder](t, f.editItems("PLA", []string{"newvid", "a"}, before), http.StatusOK)
	}

	refused(t, f.editItems("PLA", []string{"newvid", "a"}, before), http.StatusPreconditionFailed, wire.CodePreconditionFailed)
}

// The store closes while the edit reads a video from YouTube, so the
// transaction that would store the edit fails.
func TestAStoreFailureStoringAnEditIsAnInternalError(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}
	f.youtube.beforeVideos = func() { _ = f.st.Close() }

	refused(t, f.editItems("PLA", []string{"newvid", "a"}, before), http.StatusInternalServerError, wire.CodeInternal)
	if !strings.Contains(f.logs.String(), "database is closed") {
		t.Fatalf("logged %q, want the store's failure", f.logs)
	}
}

func TestAVideoTheStoreHasNeverSeenIsReadFromYouTube(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}

	decode[wireOrder](t, f.editItems("PLA", []string{"a", "newvid", "newvid"}, f.etag(t, "PLA")), http.StatusOK)
	if got := f.order(t, "PLA"); got != "a:ia newvid: newvid:" || f.youtube.videoReads != 1 || f.youtube.units != 1 {
		t.Fatalf("edited to %s after %d reads costing %d units, want a and two new entries of newvid after one read", got, f.youtube.videoReads, f.youtube.units)
	}
	shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK)
	if video := shown.Items[1].Video; video.Title != "New" || video.ChannelTitle != "Six" || video.IsUnavailable {
		t.Fatalf("newvid = %+v, want it stored available as YouTube reports it", video)
	}
}

// gone1 is a video YouTube returns nothing for, private1 one it returns as
// private, and u one the store holds as unavailable, which the order adds a
// second entry for. Each is refused under one code, named once.
func TestAnEditAddingAVideoYouTubeWillNotAddIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")
	f.youtube.videos["private1"] = youtube.Video{ID: "private1", Title: "Mine", ChannelTitle: "Me", Privacy: "private"}

	rec := f.editItems("PLA", []string{"a", "gone1", "u", "private1", "gone1", "u"}, before)
	got := decode[wireVideoRefusal](t, rec, http.StatusUnprocessableEntity)
	if got.Code != string(wire.CodeVideoUnavailable) || got.Error == "" || !slices.Equal(got.VideoIDs, []string{"gone1", "private1", "u"}) {
		t.Fatalf("refusal %+v, want video_unavailable naming gone1, private1 and u", got)
	}
	if f.order(t, "PLA") != "a:ia b:ib u:iu" || f.etag(t, "PLA") != before {
		t.Fatalf("PLA = %s at %s, want it unchanged at %s", f.order(t, "PLA"), f.etag(t, "PLA"), before)
	}
}

// u is unavailable. It keeps the entry it has, and a second copy would be a new
// entry YouTube will not add.
func TestAnUnavailableVideoStaysButIsNotAdded(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	decode[wireOrder](t, f.editItems("PLA", []string{"u", "a"}, f.etag(t, "PLA")), http.StatusOK)
	if got := f.order(t, "PLA"); got != "u:iu a:ia" {
		t.Fatalf("edited to %s, want u:iu a:ia", got)
	}
	got := decode[wireVideoRefusal](t, f.editItems("PLA", []string{"u", "a", "u"}, f.etag(t, "PLA")), http.StatusUnprocessableEntity)
	if got.Code != string(wire.CodeVideoUnavailable) || !slices.Equal(got.VideoIDs, []string{"u"}) {
		t.Fatalf("refusal %+v, want video_unavailable naming u", got)
	}
}

func TestAnOrderTheEditCannotReadIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")

	refused(t, f.editItems("PLA", nil, before), http.StatusUnprocessableEntity, wire.CodeVideoIDsRequired)
	invalid := decode[wireVideoRefusal](t, f.editItems("PLA", []string{"a,b", "a", "", "a,b"}, before), http.StatusUnprocessableEntity)
	if invalid.Code != string(wire.CodeInvalidVideoID) || !slices.Equal(invalid.VideoIDs, []string{"", "a,b"}) {
		t.Fatalf("refusal %+v, want invalid_video_id naming \"\" and a,b once each", invalid)
	}
	tooMany := slices.Repeat([]string{"a"}, youtube.MaxPlaylistItems+1)
	refused(t, f.editItems("PLA", tooMany, before), http.StatusUnprocessableEntity, wire.CodeTooManyVideos)
	decode[wireOrder](t, f.editItems("PLA", slices.Repeat([]string{"a"}, youtube.MaxPlaylistItems), before), http.StatusOK)

	rec := f.editItems("PLA", []string{}, later(t, before))
	if got := decode[wireOrder](t, rec, http.StatusOK); got.VideoIDs == nil || len(got.VideoIDs) != 0 || rec.Header().Get("ETag") != later(t, later(t, before)) {
		t.Fatalf("an empty order answered %v with ETag %q, want [] one change later", got.VideoIDs, rec.Header().Get("ETag"))
	}
}

func TestAnEditWhoseReadOfVideosFailsChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	before := f.etag(t, "PLA")
	f.youtube.readErr = errors.New("connection reset")

	refused(t, f.editItems("PLA", []string{"a", "newvid"}, before), http.StatusBadGateway, wire.CodeYouTubeReadFailed)
	f.youtube.readErr = refusal(http.StatusForbidden, "quotaExceeded", "quota", youtube.ErrQuotaSpent)
	refused(t, f.editItems("PLA", []string{"a", "newvid"}, before), http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent)
	if f.order(t, "PLA") != "a:ia b:ib u:iu" || f.etag(t, "PLA") != before {
		t.Fatalf("PLA = %s at %s, want it unchanged at %s", f.order(t, "PLA"), f.etag(t, "PLA"), before)
	}
}

// A playlist YouTube refused a position in is sorted automatically. An edit that
// changes the order sorts it manually, so the push tries positions again, and
// one that changes nothing leaves it.
func TestAnEditOfTheOrderTriesPositionsAgain(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	ctx := context.Background()
	automatic := generated.SetPlaylistSortParams{PlaylistID: "PLA", Sort: store.SortAutomatic}
	if err := f.st.Queries.SetPlaylistSort(ctx, automatic); err != nil {
		t.Fatal(err)
	}

	decode[wireOrder](t, f.editItems("PLA", []string{"a", "b", "u"}, f.etag(t, "PLA")), http.StatusOK)
	if state, err := f.st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Sort != store.SortAutomatic {
		t.Fatalf("after an edit changing nothing the state is %+v, %v, want it still automatic", state, err)
	}
	decode[wireOrder](t, f.editItems("PLA", []string{"b", "a", "u"}, f.etag(t, "PLA")), http.StatusOK)
	if state, err := f.st.Queries.GetPlaylistState(ctx, "PLA"); err != nil || state.Sort != store.SortManual {
		t.Fatalf("after an edit changing the order the state is %+v, %v, want it manual", state, err)
	}
}
