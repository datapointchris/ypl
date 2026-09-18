package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/wire"
)

type wirePlay struct {
	ID       string          `json:"id"`
	Handle   int64           `json:"handle"`
	PlayedTs string          `json:"played_ts"`
	Video    wirePlayedVideo `json:"video"`
}

type wirePlayedVideo struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ChannelTitle string `json:"channel_title"`
}

type wirePage[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

type wireSuggestion struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	LastPlayedTs    *string `json:"last_played_ts"`
	PlayCount       int64   `json:"play_count"`
}

// Version 7 UUIDs in ascending order.
const (
	play1 = "01890000-0000-7000-8000-000000000001"
	play2 = "01890000-0000-7000-8000-000000000002"
	play3 = "01890000-0000-7000-8000-000000000003"
	play4 = "01890000-0000-7000-8000-000000000004"
	play5 = "01890000-0000-7000-8000-000000000005"
)

// withPlays records five plays through the API: a on the 1st and 3rd, b on the
// 2nd and 4th, and c on the 3rd in the same second as a, with the later id.
func (f *fixture) withPlays(t *testing.T) {
	t.Helper()
	for _, p := range []struct{ id, video, ts string }{
		{play1, "a", "2026-09-01T10:00:00Z"},
		{play2, "b", "2026-09-02T10:00:00Z"},
		{play3, "a", "2026-09-03T10:00:00Z"},
		{play4, "c", "2026-09-03T10:00:00Z"},
		{play5, "b", "2026-09-04T10:00:00Z"},
	} {
		body := fmt.Sprintf(`{"id": %q, "video_id": %q, "played_ts": %q}`, p.id, p.video, p.ts)
		decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusCreated)
	}
}

func (f *fixture) countPlays(t *testing.T) int64 {
	t.Helper()
	counts, err := f.st.Queries.CountLibrary(context.Background())
	if err != nil {
		t.Fatalf("count plays: %v", err)
	}
	return counts.Plays
}

func TestAPlayIsRecordedOnceHoweverManyTimesItIsSent(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	body := fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01T10:00:00Z"}`, play1)

	rec := f.do(http.MethodPost, "/api/v1/plays", body)
	created := decode[wirePlay](t, rec, http.StatusCreated)
	want := wirePlay{ID: play1, Handle: 1, PlayedTs: "2026-09-01T10:00:00Z", Video: wirePlayedVideo{ID: "a", Title: "Zebra", ChannelTitle: "One"}}
	if created != want {
		t.Errorf("created %+v, want %+v", created, want)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/plays/"+play1 {
		t.Errorf("Location %q, want the play's own path", got)
	}

	if again := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusOK); again != want {
		t.Errorf("sent again: %+v, want %+v", again, want)
	}
	if shown := decode[wirePlay](t, f.get("/api/v1/plays/"+play1), http.StatusOK); shown != want {
		t.Errorf("shown %+v, want %+v", shown, want)
	}
	if n := f.countPlays(t); n != 1 {
		t.Fatalf("plays stored = %d, want 1", n)
	}
}

func TestAPlaySentAgainWithoutATimeMatchesTheStoredPlay(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	body := fmt.Sprintf(`{"id": %q, "video_id": "a"}`, play1)

	created := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusCreated)
	if created.PlayedTs != "2026-09-17T12:00:00Z" {
		t.Errorf("played_ts = %q, want the arrival in UTC to the second", created.PlayedTs)
	}
	f.now = arrival.Add(time.Hour)
	if again := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusOK); again != created {
		t.Errorf("sent again: %+v, want %+v", again, created)
	}
}

func TestAPlayTimeIsStoredInUTCToTheSecond(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	body := fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01T10:15:30.75+02:00"}`, play1)

	if got := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusCreated); got.PlayedTs != "2026-09-01T08:15:30Z" {
		t.Errorf("played_ts = %q, want 2026-09-01T08:15:30Z", got.PlayedTs)
	}
}

func TestAPlayIDStoredForAnotherPlayConflicts(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01T10:00:00Z"}`, play1)), http.StatusCreated)

	for _, body := range []string{
		fmt.Sprintf(`{"id": %q, "video_id": "b", "played_ts": "2026-09-01T10:00:00Z"}`, play1),
		fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01T11:00:00Z"}`, play1),
	} {
		refused(t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusConflict, wire.CodePlayConflict)
	}
	shown := decode[wirePlay](t, f.get("/api/v1/plays/"+play1), http.StatusOK)
	if shown.Video.ID != "a" || shown.PlayedTs != "2026-09-01T10:00:00Z" {
		t.Errorf("the stored play became %+v", shown)
	}
}

func TestAPlayTheServerCannotRecordIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	withTime := func(ts string) string {
		return fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": %q}`, play1, ts)
	}
	cases := map[string]struct {
		body   string
		status int
		code   wire.Code
	}{
		"not JSON":                           {`{"id":`, http.StatusBadRequest, wire.CodeInvalidBody},
		"an unknown field":                   {fmt.Sprintf(`{"id": %q, "video_id": "a", "rating": 5}`, play1), http.StatusBadRequest, wire.CodeInvalidBody},
		"two values":                         {fmt.Sprintf(`{"id": %q, "video_id": "a"} {}`, play1), http.StatusBadRequest, wire.CodeInvalidBody},
		"no id":                              {`{"video_id": "a"}`, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"an id that is no UUID":              {`{"id": "play-1", "video_id": "a"}`, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"a version 4 UUID":                   {`{"id": "4f7c2b1e-9d3a-4c8e-b5f6-0a1b2c3d4e5f", "video_id": "a"}`, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"a version 7 id of another variant":  {`{"id": "01890000-0000-7000-0000-000000000001", "video_id": "a"}`, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"an id in uppercase":                 {`{"id": "01890000-0000-7000-8000-00000000000A", "video_id": "a"}`, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"an id in braces":                    {fmt.Sprintf(`{"id": "{%s}", "video_id": "a"}`, play1), http.StatusUnprocessableEntity, wire.CodeInvalidPlayID},
		"no video":                           {fmt.Sprintf(`{"id": %q}`, play1), http.StatusUnprocessableEntity, wire.CodeVideoIDRequired},
		"a video not stored":                 {fmt.Sprintf(`{"id": %q, "video_id": "zzz"}`, play1), http.StatusUnprocessableEntity, wire.CodeVideoNotStored},
		"a time without a zone":              {withTime("2026-09-01 10:00:00"), http.StatusUnprocessableEntity, wire.CodeInvalidPlayedTs},
		"a time past the year 9999 in UTC":   {withTime("9999-12-31T23:30:00-01:00"), http.StatusUnprocessableEntity, wire.CodePlayedTsOutOfRange},
		"a time before the year 0000 in UTC": {withTime("0000-01-01T00:30:00+01:00"), http.StatusUnprocessableEntity, wire.CodePlayedTsOutOfRange},
		"a time past the clock skew":         {withTime("2026-09-17T12:05:01Z"), http.StatusUnprocessableEntity, wire.CodePlayedTsInTheFuture},
	}
	for name, c := range cases {
		rec := f.do(http.MethodPost, "/api/v1/plays", c.body)
		var body wireRefusal
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != c.status || body.Code != string(c.code) {
			t.Errorf("%s: answered %d %s, want %d %s", name, rec.Code, rec.Body, c.status, c.code)
		}
	}
	if n := f.countPlays(t); n != 0 {
		t.Fatalf("plays stored = %d, want 0", n)
	}
}

// The request arrives at 2026-09-17T12:00:00Z.
func TestAPlayWithinTheClockSkewIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	body := fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-17T12:05:00Z"}`, play1)

	if got := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusCreated); got.PlayedTs != "2026-09-17T12:05:00Z" {
		t.Errorf("played_ts = %q, want the time sent", got.PlayedTs)
	}
}

func TestAPlayIsNamedByItsIDItsHandleOrItsTail(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	for ref, want := range map[string]string{
		play4:      play4,
		"4":        play4,
		"00000004": play4,
		"5":        play5,
	} {
		if got := decode[wirePlay](t, f.get("/api/v1/plays/"+ref), http.StatusOK); got.ID != want {
			t.Errorf("play %s = %s, want %s", ref, got.ID, want)
		}
	}
	lettered := "01890000-0000-7000-8000-00000000abcd"
	decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", fmt.Sprintf(`{"id": %q, "video_id": "a"}`, lettered)), http.StatusCreated)
	for _, ref := range []string{"7", "0", "04", "00000009", strings.ToUpper(lettered)} {
		refused(t, f.get("/api/v1/plays/"+ref), http.StatusNotFound, wire.CodeNotFound)
	}

	page := decode[wirePage[wirePlay]](t, f.get("/api/v1/plays?limit=2&starting_after=4"), http.StatusOK)
	if len(page.Data) != 2 || page.Data[0].ID != play3 || page.Data[1].ID != play2 {
		t.Errorf("the page after handle 4 = %+v, want plays 3 and 2", page.Data)
	}
}

func TestATailTwoPlaysShareNamesNeither(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	for _, id := range []string{"01890000-0000-7000-8000-0000aaaaaaaa", "01890000-0000-7000-8001-0000aaaaaaaa"} {
		decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", fmt.Sprintf(`{"id": %q, "video_id": "a"}`, id)), http.StatusCreated)
	}

	refused(t, f.get("/api/v1/plays/aaaaaaaa"), http.StatusBadRequest, wire.CodeAmbiguousReference)
	refused(t, f.get("/api/v1/plays?starting_after=aaaaaaaa"), http.StatusBadRequest, wire.CodeAmbiguousReference)
}

func TestPlaysPageNewestFirst(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	var seen []string
	var pages []bool
	target := "/api/v1/plays?limit=2"
	for {
		got := decode[wirePage[wirePlay]](t, f.get(target), http.StatusOK)
		for _, p := range got.Data {
			seen = append(seen, p.ID)
		}
		pages = append(pages, got.HasMore)
		if !got.HasMore || len(pages) > 5 {
			break
		}
		target = "/api/v1/plays?limit=2&starting_after=" + url.QueryEscape(got.Data[len(got.Data)-1].ID)
	}
	if want := []string{play5, play4, play3, play2, play1}; !slices.Equal(seen, want) {
		t.Errorf("plays = %v, want %v", seen, want)
	}
	if want := []bool{true, true, false}; !slices.Equal(pages, want) {
		t.Errorf("has_more by page = %v, want %v", pages, want)
	}
}

func TestPlaysDefaultToAPageOfTwenty(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)
	for i := range 16 {
		body := fmt.Sprintf(`{"id": "01890000-0000-7000-8000-0001%08x", "video_id": "e", "played_ts": "2026-08-%02dT10:00:00Z"}`, i, i+1)
		decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays", body), http.StatusCreated)
	}

	got := decode[wirePage[wirePlay]](t, f.get("/api/v1/plays"), http.StatusOK)
	if len(got.Data) != 20 || !got.HasMore {
		t.Errorf("plays = %d with has_more %v, want 20 of the 21 and more", len(got.Data), got.HasMore)
	}
	if first := got.Data[0]; first.Video != (wirePlayedVideo{ID: "b", Title: "apple", ChannelTitle: "Two"}) {
		t.Errorf("newest play's video = %+v", first.Video)
	}
}

func TestNoPlaysPageAsAnEmptyList(t *testing.T) {
	f := newFixture(t)
	got := decode[wirePage[wirePlay]](t, f.get("/api/v1/plays"), http.StatusOK)
	if got.Data == nil || got.HasMore {
		t.Errorf("no plays = %+v, want [] and no more", got)
	}
}

func TestAPlaysPageStartingAfterAPlayTheStoreDoesNotHoldIsRefused(t *testing.T) {
	f := newFixture(t)
	refused(t, f.get("/api/v1/plays?starting_after="+play1), http.StatusBadRequest, wire.CodeUnknownReference)
}

// A deleted play answers as deleted at every verb, by each name it had: a read
// is refused as gone, a delete sent again succeeds, and its id sent again is
// refused rather than stored. A play the store never held is not found. The
// deleted play's handle is never given to another play, which the newest
// handle is where reuse would show.
func TestADeletedPlayAnswersDeletedAtEveryVerbAndKeepsItsHandle(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	if deleted := f.do(http.MethodDelete, "/api/v1/plays/5", ""); deleted.Code != http.StatusNoContent {
		t.Fatalf("deleting play 5 answered %d: %s", deleted.Code, deleted.Body)
	}
	if got := f.countPlays(t); got != 4 {
		t.Errorf("the store holds %d plays after one of five was deleted, want 4", got)
	}
	for _, name := range []string{play5, "5", play5[len(play5)-8:]} {
		refused(t, f.get("/api/v1/plays/"+name), http.StatusGone, wire.CodePlayDeleted)
		if again := f.do(http.MethodDelete, "/api/v1/plays/"+name, ""); again.Code != http.StatusNoContent {
			t.Errorf("deleting deleted play %s again answered %d: %s", name, again.Code, again.Body)
		}
	}
	never := "01890000-0000-7000-8000-000000000009"
	refused(t, f.get("/api/v1/plays/"+never), http.StatusNotFound, wire.CodeNotFound)
	refused(t, f.do(http.MethodDelete, "/api/v1/plays/"+never, ""), http.StatusNotFound, wire.CodeNotFound)

	next := decode[wirePlay](t, f.do(http.MethodPost, "/api/v1/plays",
		`{"id": "01890000-0000-7000-8000-000000000006", "video_id": "a"}`), http.StatusCreated)
	if next.Handle != 6 {
		t.Errorf("the next play took handle %d, want 6 rather than the deleted play's", next.Handle)
	}
	refused(t, f.do(http.MethodPost, "/api/v1/plays", fmt.Sprintf(`{"id": %q, "video_id": "b"}`, play5)),
		http.StatusGone, wire.CodePlayDeleted)
}

// With the plays withPlays records, e has never been played, a and c were last
// played in the same second, and b most recently.
func TestSuggestionsComeNeverPlayedFirstThenLeastRecentlyPlayed(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	got := decode[[]wireSuggestion](t, f.get("/api/v1/suggestions?limit=4"), http.StatusOK)
	if len(got) != 4 {
		t.Fatalf("suggestions = %+v, want 4", got)
	}
	if got[0].ID != "e" || got[0].LastPlayedTs != nil || got[0].PlayCount != 0 {
		t.Errorf("first = %+v, want e, never played", got[0])
	}
	tied := []string{got[1].ID, got[2].ID}
	slices.Sort(tied)
	if !slices.Equal(tied, []string{"a", "c"}) {
		t.Errorf("second and third = %v, want a and c in either order", tied)
	}
	last := got[3]
	if last.ID != "b" || last.LastPlayedTs == nil || *last.LastPlayedTs != "2026-09-04T10:00:00Z" || last.PlayCount != 2 {
		t.Errorf("last = %+v, want b, played twice and last on the 4th", last)
	}
	if last.Title != "apple" || last.ChannelTitle != "Two" || last.DurationSeconds == nil || *last.DurationSeconds != 1800 {
		t.Errorf("b = %+v", last)
	}
}

func TestOneSuggestionComesByDefault(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	if got := decode[[]wireSuggestion](t, f.get("/api/v1/suggestions"), http.StatusOK); len(got) != 1 || got[0].ID != "e" {
		t.Errorf("suggestions = %+v, want only e", got)
	}
}

func TestSuggestionsFromAPlaylistComeOnlyFromItsAvailableVideos(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlays(t)

	got := decode[[]wireSuggestion](t, f.get("/api/v1/suggestions?playlist=PLA&limit=10"), http.StatusOK)
	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.ID
	}
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Errorf("suggestions from Alpha = %v, want a then b", ids)
	}
}

func TestSuggestionsFromAPlaylistTheStoreDoesNotHoldAreRefused(t *testing.T) {
	f := newFixture(t)
	refused(t, f.get("/api/v1/suggestions?playlist=PLZ"), http.StatusBadRequest, wire.CodeUnknownReference)
}

func TestAnAggregateOfAnotherTypeIsRefused(t *testing.T) {
	if _, err := textOrNull(int64(5)); err == nil {
		t.Fatal("textOrNull accepted an integer")
	}
	if got, err := textOrNull(nil); got != nil || err != nil {
		t.Fatalf("textOrNull(nil) = %v, %v", got, err)
	}
}
