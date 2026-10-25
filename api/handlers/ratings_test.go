package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/jarseal"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// fakeHistory answers a history read with entries, or with err, and writes the
// jar back with a mark so a test can see the written-back jar was kept.
type fakeHistory struct {
	entries []ytdlp.HistoryEntry
	err     error
	reads   int
}

func (f *fakeHistory) History(_ context.Context, jar string, _ int) ([]ytdlp.HistoryEntry, string, error) {
	f.reads++
	if f.err != nil {
		return nil, "", f.err
	}
	return f.entries, jar + "# rotated\n", nil
}

// sealer is a jarseal.Sealer under a fixed test key.
func sealer(t *testing.T) *jarseal.Sealer {
	t.Helper()
	s, err := jarseal.New("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const cookieJar = "# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t0\tSID\tabc\n"

func ratingBody(n int, video, rest string) string {
	return fmt.Sprintf(`{"id": "01920000-0000-7000-8000-%012d", "video_id": %q%s}`, n, video, rest)
}

func TestEveryRatingIsKeptAndTheListFiltersByKindTagAndText(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	for i, rest := range []string{
		`, "kind": "dislike"`,
		`, "kind": "like", "message": "great closing hour", "tags": [" Deep  House ", "deep house", "Sunrise"]`,
		`, "kind": "note", "message": "too much talk early on"`,
	} {
		f.now = f.now.Add(time.Minute)
		decode[rating](t, f.do(http.MethodPost, "/api/v1/ratings", ratingBody(i+1, "a", rest)), http.StatusCreated)
	}
	refused(t, f.do(http.MethodPost, "/api/v1/ratings", ratingBody(9, "b", `, "kind": "note"`)), http.StatusUnprocessableEntity, wire.CodeMessageRequired)

	liked := decode[page[libraryVideo]](t, f.get("/api/v1/videos?rated=like&tag=deep%20house&text=CLOSING"), http.StatusOK)
	if len(liked.Data) != 1 || liked.Data[0].ID != "a" || liked.Data[0].Likes != 1 || liked.Data[0].Dislikes != 1 || liked.Data[0].Notes != 1 {
		t.Fatalf("filtering by kind, tag and text answered %+v, want video a with one of each kind", liked.Data)
	}
	if got := liked.Data[0].Tags; len(got) != 2 || got[0] != "deep house" || got[1] != "sunrise" {
		t.Errorf("tags %v, want [deep house sunrise]", got)
	}
	shown := decode[video](t, f.get("/api/v1/videos/a"), http.StatusOK)
	if len(shown.Ratings) != 3 || shown.Ratings[0].Kind != "note" || shown.Ratings[2].Kind != "dislike" {
		t.Errorf("video a lists %+v, want all three ratings newest first", shown.Ratings)
	}
	if got := f.do(http.MethodDelete, "/api/v1/ratings/000000000001", ""); got.Code != http.StatusNoContent {
		t.Fatalf("deleting the dislike by its id's tail answered %d", got.Code)
	}
	if shown := decode[video](t, f.get("/api/v1/videos/a"), http.StatusOK); len(shown.Ratings) != 2 {
		t.Errorf("video a lists %d ratings after one was deleted, want 2", len(shown.Ratings))
	}
	unrated := decode[page[libraryVideo]](t, f.get("/api/v1/videos?rated=unrated"), http.StatusOK)
	for _, v := range unrated.Data {
		if v.ID == "a" {
			t.Errorf("video a is listed as unrated")
		}
	}
}

func TestTheHistoryAnswersLibraryVideosAndAJarReadSignedOutIsReported(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	history := &fakeHistory{entries: []ytdlp.HistoryEntry{{ID: "zzzzzzzzzzz"}, {ID: "b"}, {ID: "a"}}}
	f.h.ReadHistoryWith(history, sealer(t))

	refused(t, f.get("/api/v1/youtube/history"), http.StatusConflict, wire.CodeYouTubeSessionMissing)
	decode[youtubeSession](t, f.do(http.MethodPut, "/api/v1/youtube/session", fmt.Sprintf(`{"cookie_jar": %q}`, cookieJar)), http.StatusOK)

	entries := decode[[]historyEntry](t, f.get("/api/v1/youtube/history"), http.StatusOK)
	if len(entries) != 2 || entries[0].Video.ID != "b" || entries[0].Position != 2 {
		t.Fatalf("history answered %+v, want b at 2 then a, skipping the video outside the library", entries)
	}
	stored, err := f.st.Queries.GetYouTubeSession(t.Context())
	if err != nil || strings.Contains(stored.CookieJar, "SID") {
		t.Errorf("the store holds the jar as %q (%v), want it sealed", stored.CookieJar, err)
	}
	if history.reads != 1 {
		t.Errorf("%d reads, want the session's own read reused", history.reads)
	}

	history.err = fmt.Errorf("wrapped: %w", ytdlp.ErrSignedOut)
	f.now = f.now.Add(historyReuse)
	refused(t, f.get("/api/v1/youtube/history"), http.StatusBadGateway, wire.CodeYouTubeSignedOut)
	shown := decode[status](t, f.get("/api/v1/status"), http.StatusOK)
	if s := shown.YouTubeSession; s == nil || s.LastRefusedTs == nil || *s.LastRefusedTs <= *s.LastOKTs {
		t.Errorf("status session %+v, want a refusal after the last ok read", s)
	}
}
