package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

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

const cookieJar = "# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t0\tSID\tabc\n"

func ratingBody(n int, video, rest string) string {
	return fmt.Sprintf(`{"id": "01920000-0000-7000-8000-%012d", "video_id": %q%s}`, n, video, rest)
}

func TestAVideosVerdictIsItsLatestUpOrDownAndANoteLeavesIt(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	for i, rest := range []string{
		`, "verdict": "down"`,
		`, "verdict": "up", "tags": [" Deep  House ", "deep house", "Sunrise"]`,
		`, "note": "great closing hour"`,
	} {
		f.now = f.now.Add(time.Minute)
		decode[rating](t, f.do(http.MethodPost, "/api/v1/ratings", ratingBody(i+1, "a", rest)), http.StatusCreated)
	}
	refused(t, f.do(http.MethodPost, "/api/v1/ratings", ratingBody(9, "b", "")), http.StatusUnprocessableEntity, wire.CodeRatingEmpty)

	up := decode[page[libraryVideo]](t, f.get("/api/v1/videos?verdict=up&tag=deep%20house&note=CLOSING"), http.StatusOK)
	if len(up.Data) != 1 || up.Data[0].ID != "a" || *up.Data[0].Verdict != "up" {
		t.Fatalf("filtering by verdict, tag and note answered %+v, want video a rated up", up.Data)
	}
	if got := up.Data[0].Tags; len(got) != 2 || got[0] != "deep house" || got[1] != "sunrise" {
		t.Errorf("tags %v, want [deep house sunrise]", got)
	}
	unrated := decode[page[libraryVideo]](t, f.get("/api/v1/videos?verdict=unrated"), http.StatusOK)
	for _, v := range unrated.Data {
		if v.ID == "a" {
			t.Errorf("video a is listed as unrated")
		}
	}
	tags := decode[[]tagCount](t, f.get("/api/v1/tags"), http.StatusOK)
	if len(tags) != 2 || tags[0].Videos != 1 {
		t.Errorf("tags answered %+v, want two each on one video", tags)
	}
}

func TestTheHistoryAnswersLibraryVideosAndAJarReadSignedOutIsReported(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	history := &fakeHistory{entries: []ytdlp.HistoryEntry{{ID: "zzzzzzzzzzz"}, {ID: "b"}, {ID: "a"}}}
	f.h.ReadHistoryWith(history)

	refused(t, f.get("/api/v1/history"), http.StatusConflict, wire.CodeYouTubeSessionMissing)
	decode[youtubeSession](t, f.do(http.MethodPut, "/api/v1/youtube/session", fmt.Sprintf(`{"cookie_jar": %q}`, cookieJar)), http.StatusOK)

	entries := decode[[]historyEntry](t, f.get("/api/v1/history"), http.StatusOK)
	if len(entries) != 2 || entries[0].Video.ID != "b" || entries[0].Position != 2 {
		t.Fatalf("history answered %+v, want b at 2 then a, skipping the video outside the library", entries)
	}
	if history.reads != 1 {
		t.Errorf("%d reads, want the session's own read reused", history.reads)
	}

	history.err = fmt.Errorf("wrapped: %w", ytdlp.ErrSignedOut)
	f.now = f.now.Add(historyReuse)
	refused(t, f.get("/api/v1/history"), http.StatusBadGateway, wire.CodeYouTubeSignedOut)
	shown := decode[status](t, f.get("/api/v1/status"), http.StatusOK)
	if s := shown.YouTubeSession; s == nil || s.LastRefusedTs == nil || *s.LastRefusedTs <= *s.LastOKTs {
		t.Errorf("status session %+v, want a refusal after the last ok read", s)
	}
}
