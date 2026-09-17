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
	"time"
	"unicode/utf8"

	"google.golang.org/api/googleapi"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeYouTube is the channel's playlists on YouTube, answering as
// youtube.Channel was measured to. A write stores its title and description
// trimmed and answers with them trimmed. A title or description YouTube refuses
// is ErrRefused carrying YouTube's message. An update to a deleted playlist
// succeeds, and a delete of one is ErrPlaylistNotFound. A read by id of a
// playlist YouTube does not hold is ErrPlaylistNotFound. A read costs 1 unit and
// a write 50.
type fakeYouTube struct {
	mu        sync.Mutex
	playlists map[youtube.PlaylistID]youtube.Playlist
	// stale is what a read of a playlist returns in place of what YouTube
	// holds, and unseen each playlist a read passes over, as reads within
	// seconds of a write do.
	stale  map[youtube.PlaylistID]youtube.Playlist
	unseen map[youtube.PlaylistID]bool
	// created, updated and deleted are the writes made, in order, and reads
	// the reads by id.
	created []youtube.PlaylistDetails
	updated []playlistUpdate
	deleted []youtube.PlaylistID
	reads   int
	// err, when set, fails every write, and readErr every read.
	err, readErr error
	// beforeWrite, when set, runs as each write begins, before it is made.
	beforeWrite func()
	// canceled is whether any request's context had ended when it was made.
	canceled        bool
	requests, units int64
}

type playlistUpdate struct {
	id      youtube.PlaylistID
	details youtube.PlaylistDetails
}

func newFakeYouTube() *fakeYouTube {
	return &fakeYouTube{
		playlists: map[youtube.PlaylistID]youtube.Playlist{},
		stale:     map[youtube.PlaylistID]youtube.Playlist{},
		unseen:    map[youtube.PlaylistID]bool{},
	}
}

// refusal is the error youtube.Channel returns for a refusal YouTube answered
// with status, reason and message: ErrRefused, the named sentinels, and
// YouTube's own error.
func refusal(status int, reason, message string, named ...error) error {
	answer := &googleapi.Error{Code: status, Message: message, Errors: []googleapi.ErrorItem{{Reason: reason, Message: message}}}
	wrapped := []any{youtube.ErrRefused}
	for _, sentinel := range named {
		wrapped = append(wrapped, sentinel)
	}
	wrapped = append(wrapped, answer)
	return fmt.Errorf(strings.Repeat("%w: ", len(wrapped)-1)+"%w", wrapped...)
}

var errInvalidSnippet = refusal(http.StatusBadRequest, "invalidPlaylistSnippet", "Invalid playlist snippet.")

// stores is the title and description YouTube stores for details, or the
// refusal YouTube answers.
func stores(details youtube.PlaylistDetails) (youtube.PlaylistDetails, error) {
	title, description := strings.Trim(details.Title, " \n"), strings.Trim(details.Description, " \n")
	tagged := func(text string) bool {
		opening := strings.Index(text, "<")
		return opening >= 0 && strings.Contains(text[opening:], ">")
	}
	if utf8.RuneCountInString(title) > youtube.MaxTitleLength || utf8.RuneCountInString(description) > youtube.MaxDescriptionLength || tagged(title) || tagged(description) {
		return youtube.PlaylistDetails{}, errInvalidSnippet
	}
	return youtube.PlaylistDetails{Title: title, Description: description}, nil
}

// begin counts a request costing units and returns the error fail sets. It is
// called with f.mu held.
func (f *fakeYouTube) begin(ctx context.Context, units int64, fail error) error {
	f.canceled = f.canceled || ctx.Err() != nil
	f.requests++
	f.units += units
	return fail
}

func (f *fakeYouTube) write(ctx context.Context) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	f.mu.Lock()
	return f.begin(ctx, 50, f.err)
}

func (f *fakeYouTube) Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.begin(ctx, 1, f.readErr); err != nil {
		return youtube.Playlist{}, err
	}
	if read, ok := f.stale[id]; ok {
		return read, nil
	}
	held, ok := f.playlists[id]
	if !ok || f.unseen[id] {
		return youtube.Playlist{}, fmt.Errorf("read playlist %s: %w", id, youtube.ErrPlaylistNotFound)
	}
	return held, nil
}

func (f *fakeYouTube) CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.Playlist, error) {
	err := f.write(ctx)
	defer f.mu.Unlock()
	if err != nil {
		return youtube.Playlist{}, err
	}
	f.created = append(f.created, details)
	stored, err := stores(details)
	if err != nil {
		return youtube.Playlist{}, err
	}
	created := youtube.Playlist{ID: youtube.PlaylistID(fmt.Sprintf("PLcreated%d", len(f.created))), Title: stored.Title, Description: stored.Description, Privacy: "private"}
	f.playlists[created.ID] = created
	return created, nil
}

func (f *fakeYouTube) UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) (youtube.PlaylistDetails, error) {
	err := f.write(ctx)
	defer f.mu.Unlock()
	if err != nil {
		return youtube.PlaylistDetails{}, err
	}
	f.updated = append(f.updated, playlistUpdate{id: id, details: details})
	stored, err := stores(details)
	if err != nil {
		return youtube.PlaylistDetails{}, err
	}
	if held, ok := f.playlists[id]; ok {
		held.Title, held.Description = stored.Title, stored.Description
		f.playlists[id] = held
	}
	return stored, nil
}

func (f *fakeYouTube) DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error {
	err := f.write(ctx)
	defer f.mu.Unlock()
	if err != nil {
		return err
	}
	if _, ok := f.playlists[id]; !ok {
		return fmt.Errorf("delete playlist %s: %w", id, refusal(http.StatusNotFound, "playlistNotFound", "Playlist not found.", youtube.ErrPlaylistNotFound))
	}
	f.deleted = append(f.deleted, id)
	delete(f.playlists, id)
	return nil
}

func (f *fakeYouTube) Requests() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeYouTube) Units() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.units
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

// written is the store's record of the write numbered id, counting from 1.
func (f *fixture) written(t *testing.T, id int64) generated.YoutubeWrite {
	t.Helper()
	row, err := f.st.Queries.GetYouTubeWrite(context.Background(), id)
	if err != nil {
		t.Fatalf("write %d: %v", id, err)
	}
	return row
}

// assertNoWriteRecorded fails t if the store records any write.
func (f *fixture) assertNoWriteRecorded(t *testing.T) {
	t.Helper()
	if row, err := f.st.Queries.GetYouTubeWrite(context.Background(), 1); err == nil {
		t.Fatalf("the store records write %+v, want none", row)
	}
}

func TestAPlaylistIsCreatedOnYouTubeAndStoredAsYouTubeAnswers(t *testing.T) {
	f := newFixture(t)

	rec := f.do(http.MethodPost, "/api/v1/playlists", `{"title": "  Late Night  ", "description": "Slow sets\n"}`)
	got := decode[wirePlaylistSummary](t, rec, http.StatusCreated)
	want := wirePlaylistSummary{ID: "PLcreated1", Title: "Late Night", Description: "Slow sets", Privacy: "private"}
	if got != want {
		t.Errorf("created %+v, want %+v as YouTube stored it", got, want)
	}
	if location := rec.Header().Get("Location"); location != "/api/v1/playlists/PLcreated1" {
		t.Errorf("Location %q, want the playlist's own path", location)
	}
	if len(f.youtube.created) != 1 || f.youtube.created[0] != (youtube.PlaylistDetails{Title: "  Late Night  ", Description: "Slow sets\n"}) {
		t.Errorf("YouTube saw %+v, want the body as sent", f.youtube.created)
	}
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLcreated1"), http.StatusOK); shown.wirePlaylistSummary != want || shown.Items == nil {
		t.Errorf("stored %+v, want %+v with []", shown, want)
	}
	row := f.written(t, 1)
	if row.Method != youtube.MethodPlaylistsInsert || row.PlaylistID.String != "PLcreated1" || row.Outcome != store.WriteApplied || row.Requests.Int64 != 1 || row.Units.Int64 != 50 || row.QuotaDate != "2026-09-17" {
		t.Errorf("recorded write %+v, want an applied insert of PLcreated1 costing 1 request and 50 units", row)
	}
}

func TestAPlaylistCreatedWithoutADescriptionHasAnEmptyOne(t *testing.T) {
	f := newFixture(t)

	got := decode[wirePlaylistSummary](t, f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`), http.StatusCreated)
	if got.Description != "" || f.youtube.created[0].Description != "" {
		t.Errorf("created %+v, and YouTube saw %+v", got, f.youtube.created)
	}
}

// A title of 150 characters with space around it is 150 once YouTube trims it.
func TestAPlaylistWriteTheAPICannotMakeIsRefusedBeforeYouTube(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	long := strings.Repeat("é", youtube.MaxTitleLength+1)
	longest := strings.Repeat("é", youtube.MaxTitleLength)
	tooLongDescription := strings.Repeat("x", youtube.MaxDescriptionLength+1)

	cases := []struct {
		method, target, body string
		status               int
		code                 wire.Code
	}{
		{http.MethodPost, "/api/v1/playlists", `{"description": "no title"}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPost, "/api/v1/playlists", `{"title": "  "}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPost, "/api/v1/playlists", `{"title": "` + long + `"}`, http.StatusUnprocessableEntity, wire.CodeTitleTooLong},
		{http.MethodPost, "/api/v1/playlists", `{"title": "x", "description": "` + tooLongDescription + `"}`, http.StatusUnprocessableEntity, wire.CodeDescriptionTooLong},
		{http.MethodPost, "/api/v1/playlists", `{"title": "x", "privacy": "public"}`, http.StatusBadRequest, wire.CodeInvalidBody},
		{http.MethodPost, "/api/v1/playlists", `{"title":`, http.StatusBadRequest, wire.CodeInvalidBody},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{}`, http.StatusUnprocessableEntity, wire.CodeNoChanges},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"title": ""}`, http.StatusUnprocessableEntity, wire.CodeTitleRequired},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "` + long + `"}`, http.StatusUnprocessableEntity, wire.CodeTitleTooLong},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"description": "` + tooLongDescription + `"}`, http.StatusUnprocessableEntity, wire.CodeDescriptionTooLong},
		{http.MethodPatch, "/api/v1/playlists/PLZ", `{"title": "Renamed"}`, http.StatusNotFound, wire.CodeNotFound},
		{http.MethodDelete, "/api/v1/playlists/PLZ", "", http.StatusNotFound, wire.CodeNotFound},
	}
	for _, c := range cases {
		refused(t, f.do(c.method, c.target, c.body), c.status, c.code)
	}
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes, want none", n)
	}
	f.assertNoWriteRecorded(t)

	decode[wirePlaylistSummary](t, f.do(http.MethodPost, "/api/v1/playlists", `{"title": " `+longest+` "}`), http.StatusCreated)
}

// The store holds what the last sync read, and YouTube's copy has changed since:
// its description was edited in YouTube Studio.
func TestARenameMergesOntoWhatYouTubeHoldsNow(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.playlists["PLA"] = youtube.Playlist{ID: "PLA", Title: "Alpha", Description: "Edited in Studio", Privacy: "private"}

	got := decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": " Alpha Two "}`), http.StatusOK)
	want := wirePlaylistSummary{ID: "PLA", Title: "Alpha Two", Description: "Edited in Studio", Privacy: "private", ItemCount: 3, UnavailableCount: 1, EnrichedCount: 1}
	if got != want {
		t.Errorf("updated %+v, want %+v as YouTube stored it", got, want)
	}
	sent := playlistUpdate{id: "PLA", details: youtube.PlaylistDetails{Title: " Alpha Two ", Description: "Edited in Studio"}}
	if len(f.youtube.updated) != 1 || f.youtube.updated[0] != sent {
		t.Errorf("YouTube saw %+v, want %+v", f.youtube.updated, sent)
	}
	if row := f.written(t, 1); row.Method != youtube.MethodPlaylistsUpdate || row.PlaylistID.String != "PLA" || row.Outcome != store.WriteApplied || row.Units.Int64 != 50 {
		t.Errorf("recorded write %+v, want an applied update of PLA costing 50 units, the read not counted", row)
	}

	f.now = f.now.Add(youtube.ReadLag + time.Second)
	f.youtube.playlists["PLA"] = youtube.Playlist{ID: "PLA", Title: "Retitled in Studio", Description: "Edited in Studio", Privacy: "private"}
	decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"description": ""}`), http.StatusOK)
	if last := f.youtube.updated[1].details; last != (youtube.PlaylistDetails{Title: "Retitled in Studio", Description: ""}) {
		t.Errorf("clearing the description sent %+v, want YouTube's title and an empty description", last)
	}
	if shown := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); shown.Title != "Retitled in Studio" || shown.Description != "" {
		t.Errorf("stored %+v", shown.wirePlaylistSummary)
	}
}

// A read sent seconds after a write can show the playlist as it was before, so
// a rename made then merges onto the store, which holds the write.
func TestARenameRightAfterAWriteMergesOntoTheWrite(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`), http.StatusOK)
	f.youtube.stale["PLA"] = youtube.Playlist{ID: "PLA", Title: "Alpha", Description: "First", Privacy: "private"}
	f.now = f.now.Add(youtube.ReadLag - time.Second)
	decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"description": "Second"}`), http.StatusOK)
	if last := f.youtube.updated[1].details; last != (youtube.PlaylistDetails{Title: "Alpha Two", Description: "Second"}) {
		t.Errorf("the second rename sent %+v, want the first rename's title kept", last)
	}

	created := decode[wirePlaylistSummary](t, f.do(http.MethodPost, "/api/v1/playlists", `{"title": "New", "description": "Kept"}`), http.StatusCreated)
	f.youtube.unseen[youtube.PlaylistID(created.ID)] = true
	decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/"+created.ID, `{"title": "Newer"}`), http.StatusOK)
	if last := f.youtube.updated[2].details; last != (youtube.PlaylistDetails{Title: "Newer", Description: "Kept"}) {
		t.Errorf("renaming the new playlist sent %+v, want its stored description", last)
	}
}

func TestARenameOfAPlaylistYouTubeNoLongerHasForgetsIt(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	delete(f.youtube.playlists, "PLA")

	refused(t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`), http.StatusNotFound, wire.CodeNotFound)
	refused(t, f.get("/api/v1/playlists/PLA"), http.StatusNotFound, wire.CodeNotFound)
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes, want none", n)
	}
	f.assertNoWriteRecorded(t)
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
	if row := f.written(t, 1); row.Method != youtube.MethodPlaylistsDelete || row.Outcome != store.WriteApplied {
		t.Errorf("recorded write %+v, want an applied delete", row)
	}
}

func TestAPlaylistYouTubeAlreadyDeletedIsDeletedFromTheStore(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	delete(f.youtube.playlists, "PLA")

	if rec := f.do(http.MethodDelete, "/api/v1/playlists/PLA", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("answered %d %s, want 204", rec.Code, rec.Body)
	}
	refused(t, f.get("/api/v1/playlists/PLA"), http.StatusNotFound, wire.CodeNotFound)
	if row := f.written(t, 1); row.Outcome != store.WriteAbsent || !strings.Contains(row.Error.String, "Playlist not found.") {
		t.Errorf("recorded write %+v, want it settled absent with YouTube's refusal", row)
	}
}

// YouTube refused the write, so nothing changed and waiting for a sync shows
// nothing new.
func TestAWriteYouTubeRefusesIsA422ThatChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/playlists", `{"title": "a < b > c"}`},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"description": "a < b > c"}`},
	} {
		rec := f.do(c.method, c.target, c.body)
		refused(t, rec, http.StatusUnprocessableEntity, wire.CodeYouTubeRefused)
		if !strings.Contains(rec.Body.String(), "Invalid playlist snippet.") {
			t.Errorf("%s %s answered %s, want YouTube's message", c.method, c.target, rec.Body)
		}
	}
	playlists := decode[[]wirePlaylistSummary](t, f.get("/api/v1/playlists"), http.StatusOK)
	if len(playlists) != 3 || playlists[0].Title != "Alpha" || playlists[0].Description != "First" {
		t.Errorf("playlists after refused writes = %+v, want the library as it was", playlists)
	}
	for id := int64(1); id <= 2; id++ {
		if row := f.written(t, id); row.Outcome != store.WriteRefused {
			t.Errorf("recorded write %+v, want it refused", row)
		}
	}
}

func TestAWriteWithNoAnswerIsA502ThatChangesNothingStored(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.err = errors.New("connection reset")

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
	for id := int64(1); id <= 3; id++ {
		if row := f.written(t, id); row.Outcome != store.WriteUnanswered || row.Error.String != "connection reset" {
			t.Errorf("recorded write %+v, want it unanswered with its cause", row)
		}
	}
}

// The request arrives at 05:00:00.5 Pacific on 2026-09-17, 68,399.5 seconds
// before the quota resets at midnight.
func TestASpentQuotaSaysWhenToTryAgain(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	spent := refusal(http.StatusForbidden, "quotaExceeded", "quota", youtube.ErrQuotaSpent)

	f.youtube.err = spent
	rec := f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`)
	refused(t, rec, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent)
	if got := rec.Header().Get("Retry-After"); got != "68400" {
		t.Errorf("Retry-After %q, want 68400", got)
	}
	if row := f.written(t, 1); row.Outcome != store.WriteQuotaSpent {
		t.Errorf("recorded write %+v, want it settled on the spent quota", row)
	}

	f.youtube.err, f.youtube.readErr = nil, spent
	rec = f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`)
	refused(t, rec, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent)
	if got := rec.Header().Get("Retry-After"); got != "68400" || f.youtube.writes() != 0 {
		t.Errorf("a rename whose read drew the refusal answered Retry-After %q after %d writes, want 68400 and no write", got, f.youtube.writes())
	}
}

func TestARenameWhoseReadFailsIsA502ThatWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.youtube.readErr = errors.New("connection reset")

	refused(t, f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`), http.StatusBadGateway, wire.CodeYouTubeReadFailed)
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes, want none", n)
	}
	f.assertNoWriteRecorded(t)
}

// YouTube created the playlist and the store closed before recording it, so a
// caller told only that the write failed would make a second playlist.
func TestAWriteTheStoreCannotRecordSaysWhatYouTubeDid(t *testing.T) {
	f := newFixture(t)
	f.youtube.beforeWrite = func() { _ = f.st.Close() }

	rec := f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`)
	refused(t, rec, http.StatusInternalServerError, wire.CodeYouTubeWriteUnrecorded)
	if body := rec.Body.String(); !strings.Contains(body, "YouTube created playlist PLcreated1") || !strings.Contains(body, "do not send the request again") {
		t.Errorf("answered %s, want the new playlist's id and not to send the request again", body)
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
		t.Error("a request reached YouTube on a canceled context")
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

func TestNoWriteBeginsOnceTheAPIDrains(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.h.Drain()
	f.h.Drain()

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`},
		{http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`},
		{http.MethodDelete, "/api/v1/playlists/PLA", ""},
	} {
		refused(t, f.do(c.method, c.target, c.body), http.StatusServiceUnavailable, wire.CodeShuttingDown)
	}
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes, want none", n)
	}
	f.assertNoWriteRecorded(t)
}

// The first rename holds its turn while YouTube makes its write, so the second
// reads YouTube only after the first has been recorded, and merges onto it.
func TestPlaylistWritesTakeTurns(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	writing, finish := make(chan struct{}), make(chan struct{})
	first := true
	f.youtube.beforeWrite = func() {
		if first {
			first = false
			close(writing)
			<-finish
		}
	}

	var wg sync.WaitGroup
	wg.Go(func() { f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`) })
	<-writing
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	wg.Go(func() {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/playlists/PLA", strings.NewReader(`{"description": "Second"}`)).WithContext(secondCtx)
		f.mux.ServeHTTP(httptest.NewRecorder(), req)
	})
	thirdCtx, cancelThird := context.WithCancel(context.Background())
	thirdDone := make(chan struct{})
	go func() {
		defer close(thirdDone)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/PLB", nil).WithContext(thirdCtx)
		f.mux.ServeHTTP(httptest.NewRecorder(), req)
	}()

	time.Sleep(50 * time.Millisecond)
	f.youtube.mu.Lock()
	reads := f.youtube.reads
	f.youtube.mu.Unlock()
	if reads != 1 {
		t.Fatalf("YouTube saw %d reads while the first rename held its turn, want its 1", reads)
	}
	cancelThird()
	<-thirdDone
	close(finish)
	wg.Wait()
	cancelSecond()

	if len(f.youtube.updated) != 2 || f.youtube.updated[1].details != (youtube.PlaylistDetails{Title: "Alpha Two", Description: "Second"}) {
		t.Fatalf("YouTube saw %+v, want the second rename merged onto the first", f.youtube.updated)
	}
	if len(f.youtube.deleted) != 0 {
		t.Fatalf("YouTube saw deletes %v, want none from a caller that went away while it waited", f.youtube.deleted)
	}
}

// Every failure the API answers with a 5xx is in the log with its code.
func TestEveryServerSideFailureIsLoggedWithItsCode(t *testing.T) {
	spent := refusal(http.StatusForbidden, "quotaExceeded", "quota", youtube.ErrQuotaSpent)
	cases := []struct {
		code  wire.Code
		setUp func(*fixture)
		send  func(*fixture) *httptest.ResponseRecorder
	}{
		{wire.CodeInternal, func(f *fixture) { _ = f.st.Close() }, func(f *fixture) *httptest.ResponseRecorder { return f.get("/api/v1/playlists") }},
		{wire.CodeYouTubeWriteUnrecorded, func(f *fixture) { f.youtube.beforeWrite = func() { _ = f.st.Close() } }, createLateNight},
		{wire.CodeYouTubeWriteFailed, func(f *fixture) { f.youtube.err = errors.New("connection reset") }, createLateNight},
		{wire.CodeYouTubeReadFailed, func(f *fixture) { f.youtube.readErr = errors.New("connection reset") }, renameAlpha},
		{wire.CodeYouTubeQuotaSpent, func(f *fixture) { f.youtube.err = spent }, createLateNight},
		{wire.CodeShuttingDown, func(f *fixture) { f.h.Drain() }, createLateNight},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			f := newFixture(t)
			f.withLibrary(t)
			c.setUp(f)
			rec := c.send(f)
			if rec.Code < 500 || !strings.Contains(rec.Body.String(), `"code":"`+string(c.code)+`"`) {
				t.Fatalf("answered %d %s, want a 5xx with code %s", rec.Code, rec.Body, c.code)
			}
			if !strings.Contains(f.logs.String(), "code="+string(c.code)) {
				t.Fatalf("the log does not carry code %s: %s", c.code, f.logs)
			}
		})
	}
}

// The README states YouTube's limits and the read lag as the code holds them.
func TestTheREADMEStatesTheWriteLimitsAndTheReadLag(t *testing.T) {
	text := readme(t)
	for _, want := range []string{
		fmt.Sprintf("a title of at most %d characters", youtube.MaxTitleLength),
		fmt.Sprintf("a description of at most %d,%03d", youtube.MaxDescriptionLength/1000, youtube.MaxDescriptionLength%1000),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the README does not say %q", want)
		}
	}
	if youtube.ReadLag != time.Minute || !strings.Contains(text, "within a minute of a write") {
		t.Errorf("the README says a read lags a write by a minute, and youtube.ReadLag is %v", youtube.ReadLag)
	}
}

func createLateNight(f *fixture) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, "/api/v1/playlists", `{"title": "Late Night"}`)
}

func renameAlpha(f *fixture) *httptest.ResponseRecorder {
	return f.do(http.MethodPatch, "/api/v1/playlists/PLA", `{"title": "Alpha Two"}`)
}
