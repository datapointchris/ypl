package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

type wirePlay struct {
	ID       string          `json:"id"`
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
	want := wirePlay{ID: play1, PlayedTs: "2026-09-01T10:00:00Z", Video: wirePlayedVideo{ID: "a", Title: "Zebra", ChannelTitle: "One"}}
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

	for name, body := range map[string]string{
		"another video": fmt.Sprintf(`{"id": %q, "video_id": "b", "played_ts": "2026-09-01T10:00:00Z"}`, play1),
		"another time":  fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01T11:00:00Z"}`, play1),
	} {
		if rec := f.do(http.MethodPost, "/api/v1/plays", body); rec.Code != http.StatusConflict {
			t.Errorf("%s: answered %d, want 409: %s", name, rec.Code, rec.Body)
		}
	}
	shown := decode[wirePlay](t, f.get("/api/v1/plays/"+play1), http.StatusOK)
	if shown.Video.ID != "a" || shown.PlayedTs != "2026-09-01T10:00:00Z" {
		t.Errorf("the stored play became %+v", shown)
	}
}

func TestAPlayTheServerCannotRecordIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	cases := map[string]struct {
		body   string
		status int
	}{
		"not JSON":                          {`{"id":`, http.StatusBadRequest},
		"an unknown field":                  {fmt.Sprintf(`{"id": %q, "video_id": "a", "rating": 5}`, play1), http.StatusBadRequest},
		"two values":                        {fmt.Sprintf(`{"id": %q, "video_id": "a"} {}`, play1), http.StatusBadRequest},
		"no id":                             {`{"video_id": "a"}`, http.StatusUnprocessableEntity},
		"an id that is no UUID":             {`{"id": "play-1", "video_id": "a"}`, http.StatusUnprocessableEntity},
		"a version 4 UUID":                  {`{"id": "4f7c2b1e-9d3a-4c8e-b5f6-0a1b2c3d4e5f", "video_id": "a"}`, http.StatusUnprocessableEntity},
		"a version 7 id of another variant": {`{"id": "01890000-0000-7000-0000-000000000001", "video_id": "a"}`, http.StatusUnprocessableEntity},
		"no video":                          {fmt.Sprintf(`{"id": %q}`, play1), http.StatusUnprocessableEntity},
		"a video not stored":                {fmt.Sprintf(`{"id": %q, "video_id": "zzz"}`, play1), http.StatusUnprocessableEntity},
		"a time without a zone":             {fmt.Sprintf(`{"id": %q, "video_id": "a", "played_ts": "2026-09-01 10:00:00"}`, play1), http.StatusUnprocessableEntity},
	}
	for name, c := range cases {
		if rec := f.do(http.MethodPost, "/api/v1/plays", c.body); rec.Code != c.status {
			t.Errorf("%s: answered %d, want %d: %s", name, rec.Code, c.status, rec.Body)
		}
	}
	if n := f.countPlays(t); n != 0 {
		t.Fatalf("plays stored = %d, want 0", n)
	}
	missing := refused(t, f.do(http.MethodPost, "/api/v1/plays", fmt.Sprintf(`{"id": %q}`, play1)), http.StatusUnprocessableEntity)
	if !strings.Contains(missing, "video_id is required") {
		t.Errorf("a play with no video was refused with %q, want it to name video_id as required", missing)
	}
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

	got := decode[wirePage[wirePlay]](t, f.get("/api/v1/plays"), http.StatusOK)
	if len(got.Data) != 5 || got.HasMore {
		t.Errorf("plays = %d with has_more %v, want all 5 and no more", len(got.Data), got.HasMore)
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
	refused(t, f.get("/api/v1/plays?starting_after="+play1), http.StatusBadRequest)
}

func TestAPlayTheStoreDoesNotHoldIsNotFound(t *testing.T) {
	f := newFixture(t)
	refused(t, f.get("/api/v1/plays/"+play1), http.StatusNotFound)
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
	refused(t, f.get("/api/v1/suggestions?playlist=PLZ"), http.StatusBadRequest)
}

func TestAnAggregateOfAnotherTypeIsRefused(t *testing.T) {
	if _, err := textOrNull(int64(5)); err == nil {
		t.Fatal("textOrNull accepted an integer")
	}
	if got, err := textOrNull(nil); got != nil || err != nil {
		t.Fatalf("textOrNull(nil) = %v, %v", got, err)
	}
}
