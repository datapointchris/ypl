package reconcile

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/api/handlers"
	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/youtube"
)

// api is the API's routes over st, writing playlists through f.
func api(st *store.Store, f *fakeChannel) *http.ServeMux {
	mux := http.NewServeMux()
	handlers.New(st, f, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux
}

// send makes one request of mux, and fails t unless it answered status.
func send(t *testing.T, mux *http.ServeMux, method, target, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	if rec.Code != status {
		t.Fatalf("%s %s answered %d %s, want %d", method, target, rec.Code, rec.Body, status)
	}
	return rec
}

// The API writes while a run is under way: a create just before the run lists
// the channel, a rename while it reads the renamed playlist's items, and a
// delete while it reads the deleted one's. No read the run makes shows any of
// them, as reads within seconds of a write do not. The run and the API take
// their times from the same clock.
func TestAnAPIWriteMadeDuringARunIsNotUndoneByIt(t *testing.T) {
	f := newFakeChannel(map[youtube.PlaylistID]string{"PLA": "abc", "PLB": "d"})
	r, st, _ := newRunner(t, f)
	r.now = time.Now
	ctx := context.Background()
	mustRun(t, ctx, r, store.OutcomeOK)
	mux := api(st, f)

	var location string
	f.beforeList = func(int) {
		location = send(t, mux, http.MethodPost, "/api/v1/playlists", `{"title": "New"}`, http.StatusCreated).Header().Get("Location")
		f.unseen[youtube.PlaylistID(strings.TrimPrefix(location, "/api/v1/playlists/"))] = true
	}
	reads := 0
	f.beforeItems = func(int) {
		reads++
		switch reads {
		case 1:
			send(t, mux, http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`, http.StatusOK)
		case 2:
			held := f.items["PLB"]
			send(t, mux, http.MethodDelete, "/api/v1/playlists/PLB", "", http.StatusNoContent)
			f.items["PLB"] = held
		}
	}

	mustRun(t, ctx, r, store.OutcomeOK)
	if body := send(t, mux, http.MethodGet, "/api/v1/playlists/PLA", "", http.StatusOK).Body.String(); !strings.Contains(body, `"title":"Alpha Two"`) {
		t.Errorf("PLA after the run = %s, want the API's rename", body)
	}
	send(t, mux, http.MethodGet, location, "", http.StatusOK)
	send(t, mux, http.MethodGet, "/api/v1/playlists/PLB", "", http.StatusNotFound)
}
