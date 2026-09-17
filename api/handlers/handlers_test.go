package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// arrival is the time every request in these tests arrives: not UTC and not on
// a whole second, so a stamp taken from it shows both being normalized.
var arrival = time.Date(2026, 9, 17, 14, 0, 0, 500_000_000, time.FixedZone("CEST", 2*60*60))

// fixture is the handlers over a store of their own, writing playlists to a
// fake YouTube. A request arrives at now, which starts at arrival.
type fixture struct {
	st      *store.Store
	h       *Handlers
	mux     *http.ServeMux
	logs    *bytes.Buffer
	now     time.Time
	youtube *fakeYouTube
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, mux: http.NewServeMux(), logs: &bytes.Buffer{}, now: arrival, youtube: newFakeYouTube()}
	f.h = New(st, f.youtube, slog.New(slog.NewTextHandler(f.logs, nil)))
	f.h.now = func() time.Time { return f.now }
	f.h.Register(f.mux)
	return f
}

func (f *fixture) do(method, target, body string) *httptest.ResponseRecorder {
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(method, target, reader))
	return rec
}

func (f *fixture) get(target string) *httptest.ResponseRecorder {
	return f.do(http.MethodGet, target, "")
}

// decode is the JSON body of rec as T, failing the test unless rec answered
// status with JSON holding no field T does not name.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("answered %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", got)
	}
	var v T
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// refused fails the test unless rec answered status with a refusal carrying
// code and a sentence.
func refused(t *testing.T, rec *httptest.ResponseRecorder, status int, code wire.Code) {
	t.Helper()
	body := decode[wireRefusal](t, rec, status)
	if body.Code != string(code) || body.Error == "" {
		t.Fatalf("refusal %+v, want code %s with a sentence", body, code)
	}
}

type wireRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// withLibrary stores three playlists and six videos. Alpha (PLA) holds a, b and
// the unavailable u in that order, zulu (PLB) holds b, c and e, and Écoute (PLC)
// holds nothing. No playlist holds d. a carries four tracks by three artists and
// b one. The names put an accented or lowercase name where its bytes would sort
// it elsewhere.
func (f *fixture) withLibrary(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	videos := []generated.ImportVideoParams{
		{VideoID: "a", Title: "Zebra", ChannelTitle: "One", DurationSeconds: known(3600), Description: text("A long set"), UploadDate: text("2020-01-01"), EnrichedTs: text("2026-01-01T00:00:00Z")},
		{VideoID: "b", Title: "apple", ChannelTitle: "Two", DurationSeconds: known(1800), UploadDate: text("2022-05-01")},
		{VideoID: "c", Title: "Évora", ChannelTitle: "Two"},
		{VideoID: "e", Title: "Aardvark", ChannelTitle: "Three", DurationSeconds: known(1800)},
		{VideoID: "u", Title: "Gone", ChannelTitle: "Four", IsUnavailable: true},
		{VideoID: "d", Title: "Loose", ChannelTitle: "Five", DurationSeconds: known(60)},
	}
	playlists := []generated.UpsertPlaylistParams{
		{PlaylistID: "PLA", Title: "Alpha", Description: "First", Privacy: "private"},
		{PlaylistID: "PLB", Title: "zulu", Description: "", Privacy: "public"},
		{PlaylistID: "PLC", Title: "Écoute", Description: "", Privacy: "unlisted"},
	}
	items := map[string][]store.BaseItem{
		"PLA": {{ItemID: "ia", VideoID: "a"}, {ItemID: "ib", VideoID: "b"}, {ItemID: "iu", VideoID: "u"}},
		"PLB": {{ItemID: "jb", VideoID: "b"}, {ItemID: "jc", VideoID: "c"}, {ItemID: "je", VideoID: "e"}},
	}
	tracks := map[string][]generated.InsertTrackParams{
		"a": {trackBy(1, "Björk", known(0)), trackBy(2, "Moby", known(600)), trackBy(3, "Björk", known(1200)), trackBy(4, "Âme", known(1800))},
		"b": {trackBy(1, "moby", sql.NullInt64{})},
	}
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		for _, v := range videos {
			if err := tx.ImportVideo(ctx, v); err != nil {
				return err
			}
		}
		for _, p := range playlists {
			if err := tx.UpsertPlaylist(ctx, p); err != nil {
				return err
			}
			f.youtube.playlists[youtube.PlaylistID(p.PlaylistID)] = youtube.Playlist{
				ID: youtube.PlaylistID(p.PlaylistID), Title: p.Title, Description: p.Description, Privacy: p.Privacy,
			}
			if err := tx.ReplaceBase(ctx, p.PlaylistID, items[p.PlaylistID]); err != nil {
				return err
			}
			var entries []store.Entry
			for _, item := range items[p.PlaylistID] {
				entries = append(entries, store.Entry{VideoID: item.VideoID, ItemID: item.ItemID})
			}
			if _, err := tx.ReplaceOrder(ctx, p.PlaylistID, 1, entries); err != nil {
				return err
			}
		}
		for id, list := range tracks {
			if err := tx.ReplaceTracks(ctx, id, list); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store the library: %v", err)
	}
}

func trackBy(position int64, artist string, start sql.NullInt64) generated.InsertTrackParams {
	return generated.InsertTrackParams{
		Position:     position,
		StartSeconds: start,
		Artist:       text(artist),
		Title:        "Track",
		RawText:      artist + " - Track",
		Source:       "chapter",
	}
}

func known(n int64) sql.NullInt64 {
	return sql.NullInt64{Int64: n, Valid: true}
}

func text(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

func TestAFailedStoreReadIsA500ThatLogsItsCause(t *testing.T) {
	f := newFixture(t)
	_ = f.st.Close()

	rec := f.get("/api/v1/playlists")
	refused(t, rec, http.StatusInternalServerError, wire.CodeInternal)
	if strings.Contains(rec.Body.String(), "closed") {
		t.Errorf("the 500 carries its cause: %s", rec.Body)
	}
	if !strings.Contains(f.logs.String(), "database is closed") {
		t.Errorf("the log does not carry the cause: %s", f.logs)
	}
}

func TestALimitOutsideItsRangeIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{
		"/api/v1/plays?limit=0",
		"/api/v1/plays?limit=101",
		"/api/v1/plays?limit=ten",
		"/api/v1/sync/runs?limit=0",
		"/api/v1/sync/runs?limit=101",
		"/api/v1/suggestions?limit=0",
		"/api/v1/suggestions?limit=101",
	} {
		refused(t, f.get(target), http.StatusBadRequest, wire.CodeInvalidLimit)
	}
	for _, target := range []string{"/api/v1/plays?limit=100", "/api/v1/sync/runs?limit=100", "/api/v1/suggestions?limit=100"} {
		if rec := f.get(target); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200 at the most a limit takes", target, rec.Code)
		}
	}
}

func TestARequestNoRouteAnswersIsRefusedInTheEnvelope(t *testing.T) {
	f := newFixture(t)

	refused(t, f.get("/api/v1/playlist"), http.StatusNotFound, wire.CodeRouteNotFound)
	refused(t, f.get("/api/v1/plays/a/b"), http.StatusNotFound, wire.CodeRouteNotFound)
	for target, allow := range map[string]string{
		"/api/v1/plays":       "GET, HEAD, POST",
		"/api/v1/videos/a":    "GET, HEAD",
		"/api/v1/sync/runs":   "GET, HEAD",
		"/api/v1/playlists/x": "DELETE, GET, HEAD, PATCH",
	} {
		method := http.MethodPut
		rec := f.do(method, target, "")
		refused(t, rec, http.StatusMethodNotAllowed, wire.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("%s %s Allow = %q, want %q", method, target, got, allow)
		}
	}
}

// readme is the repository's README.
func readme(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read the README: %v", err)
	}
	return string(data)
}

// The README's endpoint table has a row for every route and no other.
func TestTheREADMEListsEveryRoute(t *testing.T) {
	var want []string
	for _, rt := range New(nil, nil, nil).routes() {
		want = append(want, rt.method+" "+rt.path)
	}
	row := regexp.MustCompile("(?m)^\\| `([A-Z]+ /api/v1/[^`]*)` \\|")
	var got []string
	for _, match := range row.FindAllStringSubmatch(readme(t), -1) {
		got = append(got, match[1])
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("README endpoint rows = %v, want one per route: %v", got, want)
	}
}
