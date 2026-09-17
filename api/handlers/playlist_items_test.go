package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// editItems sends PUT /api/v1/playlists/{id}/items for videos with the If-Match
// values given.
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

// order is each item of the playlist as video and item id, "b:ib a:ia a:".
func order(shown wirePlaylist) string {
	var fields []string
	for _, item := range shown.Items {
		fields = append(fields, item.Video.ID+":"+item.itemID())
	}
	return strings.Join(fields, " ")
}

// Alpha holds a, b and u as ia, ib and iu. Each video takes the earliest entry
// holding it, so the second a is a new entry with no item yet.
func TestAnEditSetsTheOrderAndKeepsEachEntrysItem(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	rec := f.editItems("PLA", []string{"b", "a", "a"}, `"1"`)
	shown := decode[wirePlaylist](t, rec, http.StatusOK)
	if order(shown) != "b:ib a:ia a:" || shown.Revision != 2 || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("edited to %s at revision %d with ETag %q, want b:ib a:ia a: at revision 2", order(shown), shown.Revision, rec.Header().Get("ETag"))
	}
	if again := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); order(again) != order(shown) {
		t.Fatalf("stored %s, want %s", order(again), order(shown))
	}
	if n := f.youtube.writes(); n != 0 || f.youtube.videoReads != 0 {
		t.Fatalf("YouTube saw %d writes and %d reads of videos, want none", n, f.youtube.videoReads)
	}
}

// With a stored a and a second a not yet on YouTube, an order naming one a
// keeps the earlier, which YouTube holds.
func TestARepeatedVideoKeepsItsEarliestEntry(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	decode[wirePlaylist](t, f.editItems("PLA", []string{"a", "b", "u", "a"}, `"1"`), http.StatusOK)
	shown := decode[wirePlaylist](t, f.editItems("PLA", []string{"u", "a"}, `"2"`), http.StatusOK)
	if order(shown) != "u:iu a:ia" {
		t.Fatalf("edited to %s, want u:iu a:ia keeping the a YouTube holds", order(shown))
	}
}

func TestAnEditThatChangesNothingKeepsTheRevision(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	shown := decode[wirePlaylist](t, f.editItems("PLA", []string{"a", "b", "u"}, `"1"`), http.StatusOK)
	if order(shown) != "a:ia b:ib u:iu" || shown.Revision != 1 {
		t.Fatalf("edited to %s at revision %d, want the order as it was at revision 1", order(shown), shown.Revision)
	}
}

func TestAnEditNamesTheRevisionItEdits(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	refused(t, f.editItems("PLA", []string{"b"}), http.StatusPreconditionRequired, wire.CodeRevisionRequired)
	for _, ifMatch := range [][]string{{`W/"1"`}, {`1`}, {`"x"`}, {`*`}, {`"0"`}, {`"1"`, `"2"`}} {
		refused(t, f.editItems("PLA", []string{"b"}, ifMatch...), http.StatusBadRequest, wire.CodeInvalidRevision)
	}
	refused(t, f.editItems("PLA", []string{"b"}, `"5"`), http.StatusPreconditionFailed, wire.CodeRevisionMismatch)
	refused(t, f.editItems("PLZ", []string{"b"}, `"1"`), http.StatusNotFound, wire.CodeNotFound)
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); order(shown) != "a:ia b:ib u:iu" || shown.Revision != 1 {
		t.Fatalf("PLA = %s at revision %d, want it unchanged", order(shown), shown.Revision)
	}
}

// Another edit lands while this one reads a video from YouTube, so the revision
// it named is gone by the time it would store.
func TestAnEditRacedByAnotherIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}
	f.youtube.beforeVideos = func() {
		f.youtube.beforeVideos = nil
		decode[wirePlaylist](t, f.editItems("PLA", []string{"b"}, `"1"`), http.StatusOK)
	}

	refused(t, f.editItems("PLA", []string{"newvid", "a"}, `"1"`), http.StatusPreconditionFailed, wire.CodeRevisionMismatch)
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); order(shown) != "b:ib" || shown.Revision != 2 {
		t.Fatalf("PLA = %s at revision %d, want the other edit's b at revision 2", order(shown), shown.Revision)
	}
}

// The other edit sets the very order this one names, so this one would change
// nothing, and is refused all the same for naming a revision that is gone. The
// other edit finds the video already stored, since this one holds the write turn
// while it reads the video from YouTube.
func TestAnEditRacedByAnIdenticalEditIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}
	f.youtube.beforeVideos = func() {
		f.youtube.beforeVideos = nil
		stored := generated.UpsertAvailableVideoParams{VideoID: "newvid", Title: "New", ChannelTitle: "Six"}
		if err := f.st.Queries.UpsertAvailableVideo(context.Background(), stored); err != nil {
			t.Errorf("store newvid: %v", err)
		}
		decode[wirePlaylist](t, f.editItems("PLA", []string{"newvid", "a"}, `"1"`), http.StatusOK)
	}

	refused(t, f.editItems("PLA", []string{"newvid", "a"}, `"1"`), http.StatusPreconditionFailed, wire.CodeRevisionMismatch)
}

func TestAVideoTheStoreHasNeverSeenIsReadFromYouTube(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["newvid"] = youtube.Video{ID: "newvid", Title: "New", ChannelTitle: "Six", Privacy: "public"}

	shown := decode[wirePlaylist](t, f.editItems("PLA", []string{"a", "newvid", "newvid"}, `"1"`), http.StatusOK)
	if order(shown) != "a:ia newvid: newvid:" || f.youtube.videoReads != 1 || f.youtube.units != 1 {
		t.Fatalf("edited to %s after %d reads costing %d units, want a and two new entries of newvid after one read", order(shown), f.youtube.videoReads, f.youtube.units)
	}
	if video := shown.Items[1].Video; video.Title != "New" || video.ChannelTitle != "Six" || video.IsUnavailable {
		t.Fatalf("newvid = %+v, want it stored available as YouTube reports it", video)
	}
}

func TestAVideoYouTubeWillNotReturnIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.videos["private1"] = youtube.Video{ID: "private1", Title: "Mine", ChannelTitle: "Me", Privacy: "private"}

	rec := f.editItems("PLA", []string{"a", "gone1", "private1", "gone1"}, `"1"`)
	refused(t, rec, http.StatusUnprocessableEntity, wire.CodeVideoNotFound)
	if body := rec.Body.String(); !strings.Contains(body, "gone1, private1") {
		t.Fatalf("answered %s, want gone1 and private1 named once each", body)
	}
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); order(shown) != "a:ia b:ib u:iu" || shown.Revision != 1 {
		t.Fatalf("PLA = %s at revision %d, want it unchanged", order(shown), shown.Revision)
	}
}

// u is unavailable. It keeps the entry it has, and a second copy would be a new
// entry YouTube will not add.
func TestAnUnavailableVideoStaysButIsNotAdded(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	shown := decode[wirePlaylist](t, f.editItems("PLA", []string{"u", "a"}, `"1"`), http.StatusOK)
	if order(shown) != "u:iu a:ia" {
		t.Fatalf("edited to %s, want u:iu a:ia", order(shown))
	}
	rec := f.editItems("PLA", []string{"u", "a", "u"}, `"2"`)
	refused(t, rec, http.StatusUnprocessableEntity, wire.CodeVideoUnavailable)
	if !strings.Contains(rec.Body.String(), ": u") {
		t.Fatalf("answered %s, want u named", rec.Body)
	}
}

func TestAnOrderTheEditCannotReadIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	refused(t, f.editItems("PLA", nil, `"1"`), http.StatusUnprocessableEntity, wire.CodeVideoIDsRequired)
	refused(t, f.editItems("PLA", []string{"a,b"}, `"1"`), http.StatusUnprocessableEntity, wire.CodeInvalidVideoID)
	refused(t, f.editItems("PLA", []string{""}, `"1"`), http.StatusUnprocessableEntity, wire.CodeInvalidVideoID)
	shown := decode[wirePlaylist](t, f.editItems("PLA", []string{}, `"1"`), http.StatusOK)
	if len(shown.Items) != 0 || shown.Items == nil || shown.Revision != 2 {
		t.Fatalf("an empty order answered %+v, want [] at revision 2", shown)
	}
}

func TestAnEditWhoseReadOfVideosFailsChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.readErr = errors.New("connection reset")

	refused(t, f.editItems("PLA", []string{"a", "newvid"}, `"1"`), http.StatusBadGateway, wire.CodeYouTubeReadFailed)
	f.youtube.readErr = refusal(http.StatusForbidden, "quotaExceeded", "quota", youtube.ErrQuotaSpent)
	refused(t, f.editItems("PLA", []string{"a", "newvid"}, `"1"`), http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent)
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); order(shown) != "a:ia b:ib u:iu" || shown.Revision != 1 {
		t.Fatalf("PLA = %s at revision %d, want it unchanged", order(shown), shown.Revision)
	}
}
