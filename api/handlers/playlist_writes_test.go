package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeYouTube stands in for the channel the API writes playlists through. It
// answers as YouTube was measured to: an update to a deleted playlist succeeds,
// and a delete of one is ErrPlaylistNotFound.
type fakeYouTube struct {
	mu sync.Mutex
	// created, updated and deleted are the writes made, in order.
	created []youtube.PlaylistDetails
	updated []playlistUpdate
	deleted []youtube.PlaylistID
	// gone holds the playlists YouTube has deleted.
	gone map[youtube.PlaylistID]bool
	// err, when set, fails every write.
	err error
	// canceled is whether any write's context had ended when it was made.
	canceled bool
}

type playlistUpdate struct {
	id      youtube.PlaylistID
	details youtube.PlaylistDetails
}

func (f *fakeYouTube) begin(ctx context.Context) error {
	f.canceled = f.canceled || ctx.Err() != nil
	return f.err
}

func (f *fakeYouTube) CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.PlaylistID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	f.created = append(f.created, details)
	return youtube.PlaylistID(fmt.Sprintf("PLcreated%d", len(f.created))), nil
}

func (f *fakeYouTube) UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return err
	}
	f.updated = append(f.updated, playlistUpdate{id: id, details: details})
	return nil
}

func (f *fakeYouTube) DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return err
	}
	if f.gone[id] {
		return fmt.Errorf("delete playlist %s: %w", id, youtube.ErrPlaylistNotFound)
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeYouTube) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created) + len(f.updated) + len(f.deleted)
}

// doCanceled sends a request whose caller has already gone away.
func (f *fixture) doCanceled(method, target, body string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func TestAPlaylistIsCreatedOnYouTubeAndStoredUnderItsID(t *testing.T) {
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night", "description": "Slow sets"}`)
	got := decode[wirePlaylistSummary](t, rec, http.StatusCreated)
	want := wirePlaylistSummary{ID: "PLcreated1", Title: "Late Night", Description: "Slow sets", Privacy: "private"}
	if got != want {
		t.Errorf("created %+v, want %+v", got, want)
	}
	if location := rec.Header().Get("Location"); location != "/api/v1/playlists/PLcreated1" {
		t.Errorf("Location %q, want the playlist's own path", location)
	}
	if len(f.youtube.created) != 1 || f.youtube.created[0] != (youtube.PlaylistDetails{Title: "Late Night", Description: "Slow sets"}) {
		t.Errorf("YouTube saw %+v", f.youtube.created)
	}
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLcreated1"), http.StatusOK); shown.wirePlaylistSummary != want || shown.Items == nil {
		t.Errorf("stored %+v, want %+v with []", shown, want)
	}
}

func TestAPlaylistCreatedWithoutADescriptionHasAnEmptyOne(t *testing.T) {
	f := newFixture(t)

	got := decode[wirePlaylistSummary](t, f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`), http.StatusCreated)
	if got.Description != "" || f.youtube.created[0].Description != "" {
		t.Errorf("created %+v, and YouTube saw %+v", got, f.youtube.created)
	}
}

func TestAPlaylistWriteTheAPICannotMakeIsRefusedBeforeYouTube(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	cases := []struct {
		method, target, body string
		status               int
		code                 wire.Code
	}{
		{http.MethodPost, "/api/v1/playlists", `{"description": "no title"}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPost, "/api/v1/playlists", `{"title": "  "}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPost, "/api/v1/playlists", `{"title": "x", "privacy": "public"}`, http.StatusBadRequest, wire.CodeInvalidBody},
		{http.MethodPost, "/api/v1/playlists", `{"title":`, http.StatusBadRequest, wire.CodeInvalidBody},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{}`, http.StatusUnprocessableEntity, wire.CodeNoChanges},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"title": ""}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPatch, "/api/v1/playlists/PLZ", `{"title": "Renamed"}`, http.StatusNotFound, wire.CodeNotFound},
		{http.MethodDelete, "/api/v1/playlists/PLZ", "", http.StatusNotFound, wire.CodeNotFound},
	}
	for _, c := range cases {
		refused(t, f.do(c.method, c.target, c.body), c.status, c.code)
	}
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes, want none", n)
	}
}

func TestARenameSendsTheStoredDescriptionAndStoresTheResult(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	got := decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`), http.StatusOK)
	want := wirePlaylistSummary{ID: "PLA", Title: "Alpha Two", Description: "First", Privacy: "private", ItemCount: 3, UnavailableCount: 1, EnrichedCount: 1}
	if got != want {
		t.Errorf("updated %+v, want %+v", got, want)
	}
	sent := []playlistUpdate{{id: "PLA", details: youtube.PlaylistDetails{Title: "Alpha Two", Description: "First"}}}
	if len(f.youtube.updated) != 1 || f.youtube.updated[0] != sent[0] {
		t.Errorf("YouTube saw %+v, want %+v", f.youtube.updated, sent)
	}

	decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"description": ""}`), http.StatusOK)
	if last := f.youtube.updated[1].details; last != (youtube.PlaylistDetails{Title: "Alpha Two", Description: ""}) {
		t.Errorf("clearing the description sent %+v, want the stored title and an empty description", last)
	}
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); shown.Title != "Alpha Two" || shown.Description != "" {
		t.Errorf("stored %+v", shown.wirePlaylistSummary)
	}
}

func TestADeleteRemovesThePlaylistAndItsItems(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	rec := f.do(http.MethodDelete, "/api/v1/playlists/PLA", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("answered %d with %q, want 204 and no body", rec.Code, rec.Body)
	}
	if len(f.youtube.deleted) != 1 || f.youtube.deleted[0] != "PLA" {
		t.Errorf("YouTube saw deletes %v, want PLA", f.youtube.deleted)
	}
	refused(t, f.get("/api/v1/playlists/PLA"), http.StatusNotFound, wire.CodeNotFound)
	counts, err := f.st.Queries.CountLibrary(context.Background())
	if err != nil || counts.Playlists != 2 {
		t.Fatalf("playlists = %d, %v, want the other 2", counts.Playlists, err)
	}
}

func TestAPlaylistYouTubeAlreadyDeletedIsDeletedFromTheStore(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.gone = map[youtube.PlaylistID]bool{"PLA": true}

	if rec := f.do(http.MethodDelete, "/api/v1/playlists/PLA", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("answered %d %s, want 204", rec.Code, rec.Body)
	}
	refused(t, f.get("/api/v1/playlists/PLA"), http.StatusNotFound, wire.CodeNotFound)
}

func TestAWriteYouTubeFailsChangesNothingStored(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.err = errors.New("update playlist PLA: connection reset")

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`},
		{http.MethodDelete, "/api/v1/playlists/PLA", ""},
	} {
		refused(t, f.do(c.method, c.target, c.body), http.StatusBadGateway, wire.CodeYouTubeWriteFailed)
	}
	playlists := decode[[]wirePlaylistSummary](t, f.get("/api/v1/playlists"), http.StatusOK)
	if len(playlists) != 3 || playlists[0].Title != "Alpha" {
		t.Errorf("playlists after failed writes = %+v, want the library as it was", playlists)
	}
	if !strings.Contains(f.logs.String(), "connection reset") {
		t.Errorf("the log does not carry the cause: %s", f.logs)
	}
}

// The request arrives at 05:00:00.5 Pacific on 2026-09-17, 68,399.5 seconds
// before the quota resets at midnight.
func TestASpentQuotaSaysWhenToTryAgain(t *testing.T) {
	f := newFixture(t)
	f.youtube.err = fmt.Errorf("create playlist: %w", youtube.ErrQuotaSpent)

	rec := f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`)
	refused(t, rec, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent)
	if got := rec.Header().Get("Retry-After"); got != "68400" {
		t.Errorf("Retry-After %q, want 68400", got)
	}
}

// A caller that goes away mid-write leaves the write and its record whole.
func TestAWriteWhoseCallerWentAwayIsStillMadeAndStored(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	f.doCanceled(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`)
	f.doCanceled(http.MethodPatch, "/api/v1/playlists/PLB", `{"title": "Zulu Two"}`)
	f.doCanceled(http.MethodDelete, "/api/v1/playlists/PLA", "")
	if f.youtube.canceled {
		t.Error("a write reached YouTube on a canceled context")
	}
	playlists := decode[[]wirePlaylistSummary](t, f.get("/api/v1/playlists"), http.StatusOK)
	var titles []string
	for _, p := range playlists {
		titles = append(titles, p.Title)
	}
	if strings.Join(titles, ",") != "Écoute,Late Night,Zulu Two" {
		t.Errorf("stored playlists %v, want the create, the rename and the delete recorded", titles)
	}
}
