// Package handlers serves the API's resources as JSON over the store: the
// playlists and videos the sync stores, plays, suggestions of what to play
// next, and the sync's own runs. Creating, renaming and deleting a playlist
// write to YouTube in the request, one request at a time, and each write is
// recorded in the store before it is sent and settled with YouTube's answer.
//
// A value the store does not hold is null, and a collection with no members is
// []. A collection that grows without bound is paged: its body is {"data": [...],
// "has_more": ...}, and the next page is the same request with starting_after
// set to the last id on this one. Every refusal, a request no route answers
// included, is the envelope package wire writes.
//
// A playlist or a play is named by its key or by something shorter a person can
// retype, at every verb that names one, and a name reaching more than one row is
// refused naming each rather than answered with one of them.
package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/wire"
)

// Handlers answers the API's requests from one store, making the playlist
// writes a request asks for on YouTube.
type Handlers struct {
	store   *store.Store
	youtube PlaylistWriter
	log     *slog.Logger
	now     func() time.Time
	// writing holds a token while a request has the turn to write playlists.
	writing chan struct{}
	// draining is closed once Drain is called.
	draining  chan struct{}
	drainOnce sync.Once
}

// New is Handlers over st, reading and writing playlists on YouTube through
// youtube. Every answer with a 5xx is made by refuseAndLog, which logs its
// cause to log.
func New(st *store.Store, youtube PlaylistWriter, log *slog.Logger) *Handlers {
	return &Handlers{
		store:    st,
		youtube:  youtube,
		log:      log,
		now:      time.Now,
		writing:  make(chan struct{}, 1),
		draining: make(chan struct{}),
	}
}

// route is one method on one path and the handler answering it.
type route struct {
	method, path string
	handle       http.HandlerFunc
}

// routes is every request the API answers.
func (h *Handlers) routes() []route {
	return []route{
		{http.MethodGet, "/api/v1/playlists", h.listPlaylists},
		{http.MethodPost, "/api/v1/playlists", h.createPlaylist},
		{http.MethodGet, "/api/v1/playlists/{id}", h.showPlaylist},
		{http.MethodPatch, "/api/v1/playlists/{id}", h.updatePlaylist},
		{http.MethodDelete, "/api/v1/playlists/{id}", h.deletePlaylist},
		{http.MethodGet, "/api/v1/playlists/{id}/items", h.showPlaylistItems},
		{http.MethodPut, "/api/v1/playlists/{id}/items", h.replacePlaylistItems},
		{http.MethodGet, "/api/v1/videos", h.listVideos},
		{http.MethodGet, "/api/v1/videos/{id}", h.showVideo},
		{http.MethodPost, "/api/v1/plays", h.createPlay},
		{http.MethodGet, "/api/v1/plays", h.listPlays},
		{http.MethodGet, "/api/v1/plays/{id}", h.showPlay},
		{http.MethodDelete, "/api/v1/plays/{id}", h.deletePlay},
		{http.MethodGet, "/api/v1/suggestions", h.listSuggestions},
		{http.MethodGet, "/api/v1/sync/runs", h.listSyncRuns},
		{http.MethodGet, "/api/v1/status", h.showStatus},
	}
}

// Register adds every route to mux. A path under /api/v1/ that no route
// matches is a 404, and a method no route on its path takes is a 405 naming
// the methods it does take.
func (h *Handlers) Register(mux *http.ServeMux) {
	allowed := make(map[string][]string)
	for _, rt := range h.routes() {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handle)
		allowed[rt.path] = append(allowed[rt.path], rt.method)
	}
	for path, methods := range allowed {
		mux.HandleFunc(path, methodNotAllowed(methods))
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		wire.Refuse(w, http.StatusNotFound, wire.CodeRouteNotFound, "no route answers %s", r.URL.Path)
	})
}

func methodNotAllowed(methods []string) http.HandlerFunc {
	allow := slices.Clone(methods)
	if slices.Contains(allow, http.MethodGet) {
		allow = append(allow, http.MethodHead)
	}
	slices.Sort(allow)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		wire.Refuse(w, http.StatusMethodNotAllowed, wire.CodeMethodNotAllowed, "%s takes %s, not %s", r.URL.Path, strings.Join(allow, ", "), r.Method)
	}
}

// page is one page of a paged collection. HasMore is true when rows follow the
// last one in Data.
type page[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// pageOf is the page holding the first limit of rows, which the store was asked
// for one more of than limit so the page can say whether more follow.
func pageOf[T any](rows []T, limit int64) page[T] {
	if int64(len(rows)) > limit {
		return page[T]{Data: rows[:limit], HasMore: true}
	}
	return page[T]{Data: rows, HasMore: false}
}

// decodeJSON reads the request body, of at most maxBytes, as exactly one JSON
// value of T with no field T does not name. what names T in a refusal. ok is
// false once it has answered a 400.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, maxBytes int64, what string) (T, bool) {
	var body T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidBody, "the body is not %s: %v", what, err)
		return body, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidBody, "the body holds more than one JSON value")
		return body, false
	}
	return body, true
}

// newCollator orders names the way a reader expects them sorted: by the Unicode
// Collation Algorithm, where case and accents decide only between names
// otherwise equal. A Collator is not safe for concurrent use, so each request
// makes its own.
func newCollator() *collate.Collator {
	return collate.New(language.Und)
}

// referenceError is a query parameter or path segment naming no row the store
// holds, or naming more than one. candidates names each of those rows by what a
// request can name it with, so the answer says what to send instead.
type referenceError struct {
	name, value string
	candidates  []string
	// nearby names the rows a reference that reached nothing exactly is a
	// fragment of, so the answer carries what to send instead of only that
	// nothing answered to what was sent.
	nearby []string
	// deleted is a reference naming a play that was deleted, which the store
	// can tell from one it never held because it keeps what each deleted play
	// was named by.
	deleted bool
}

// shownCandidates is the most rows a refusal of an ambiguous reference names.
// Part of a title can match hundreds of videos in a library of long mixes.
const shownCandidates = 10

// The sentence for nearby says what the store can see and no more. A reference
// matching no row exactly and sitting inside one title is a fragment of that
// title; it is also a reference to a row deleted moments ago whose id happens
// to read as one. Nothing here can tell those apart, so the sentence names the
// title it found and leaves the caller to recognize their own reference.
func (e referenceError) Error() string {
	switch {
	case len(e.candidates) > shownCandidates:
		return fmt.Sprintf("%s %q names more than one: %s, and %d more, which more of the title narrows",
			e.name, e.value, strings.Join(e.candidates[:shownCandidates], ", "), len(e.candidates)-shownCandidates)
	case len(e.candidates) > 0:
		return fmt.Sprintf("%s %q names more than one: %s", e.name, e.value, strings.Join(e.candidates, ", "))
	case len(e.nearby) > 0:
		return fmt.Sprintf("%[1]s %[2]q names none exactly, and is inside %[3]s; a %[1]s is named by its whole title or its id, never by part of one",
			e.name, e.value, strings.Join(e.nearby, ", "))
	case e.deleted:
		return fmt.Sprintf("%s %q names a play that was deleted", e.name, e.value)
	}
	return fmt.Sprintf("%s %q names nothing the store holds", e.name, e.value)
}

// paramRow is err from reading the row a query parameter names, with a row
// that is not there as a referenceError naming the parameter.
func paramRow(err error, param referenceError) error {
	if errors.Is(err, sql.ErrNoRows) {
		return param
	}
	return err
}

// writeItemError answers a failed read of the resource the path names: its row
// not being there is a 404 naming what, a reference naming more than one row a
// 400, and anything else a 500. A reference sitting inside a title is a 404
// too, and keeps its own sentence for the title it names. The status is the
// same because the condition is: nothing answers to what was sent, whether the
// caller shortened a title or named a row that has gone.
//
// A deleted play is the one row whose going the store records, and it answers
// 410, so a caller can tell a play taken back from a reference mistyped.
func (h *Handlers) writeItemError(w http.ResponseWriter, r *http.Request, err error, what string) {
	var ref referenceError
	switch {
	case errors.As(err, &ref) && len(ref.candidates) > 0:
		wire.Refuse(w, http.StatusBadRequest, wire.CodeAmbiguousReference, "%s", ref.Error())
	case errors.As(err, &ref) && ref.deleted:
		wire.Refuse(w, http.StatusGone, wire.CodePlayDeleted, "%s was deleted", what)
	case errors.As(err, &ref) && len(ref.nearby) > 0:
		wire.Refuse(w, http.StatusNotFound, wire.CodeNotFound, "%s", ref.Error())
	case errors.Is(err, sql.ErrNoRows), errors.As(err, &ref):
		wire.Refuse(w, http.StatusNotFound, wire.CodeNotFound, "%s not found", what)
	default:
		h.writeInternalError(w, r, err)
	}
}

// writeListError answers a failed read of a collection: a query parameter
// naming nothing is a 400, as is one naming more than one row, and anything
// else a 500.
func (h *Handlers) writeListError(w http.ResponseWriter, r *http.Request, err error) {
	var ref referenceError
	switch {
	case errors.As(err, &ref) && len(ref.candidates) > 0:
		wire.Refuse(w, http.StatusBadRequest, wire.CodeAmbiguousReference, "%s", ref.Error())
	case errors.As(err, &ref):
		wire.Refuse(w, http.StatusBadRequest, wire.CodeUnknownReference, "%s", ref.Error())
	default:
		h.writeInternalError(w, r, err)
	}
}

// writeInternalError answers a 500, logging err rather than sending it.
func (h *Handlers) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	h.refuseAndLog(w, r, slog.LevelError, http.StatusInternalServerError, wire.CodeInternal, err, "internal error")
}

// refuseAndLog answers a failure the caller did not cause with status, code and
// the sentence format and args make, and logs cause at level with them.
func (h *Handlers) refuseAndLog(w http.ResponseWriter, r *http.Request, level slog.Level, status int, code wire.Code, cause error, format string, args ...any) {
	h.log.Log(r.Context(), level, "request failed", "method", r.Method, "path", r.URL.Path, "status", status, "code", code, "err", cause)
	wire.Refuse(w, status, code, format, args...)
}

// pageSize is how many rows a list answers: fallback when limit is absent, and
// at most most.
type pageSize struct {
	fallback, most int64
}

// everyRow is a fallback that answers the whole list as one page. The library
// lists are read and ordered whole in memory for every request, so a caller
// reading all of one is served by one request rather than one per page.
const everyRow = math.MaxInt64

var (
	playsPage       = pageSize{fallback: 20, most: 100}
	runsPage        = pageSize{fallback: 20, most: 100}
	suggestionsDraw = pageSize{fallback: 1, most: 100}
	videosPage      = pageSize{fallback: everyRow, most: 100}
	playlistsPage   = pageSize{fallback: everyRow, most: 100}
)

// pageAfter is the page of rows, in their order, that starts after the row
// whose id is after, and the first page where after is empty. ok is false once
// it has answered a 400 for an after naming no row of rows, which is how a list
// that changed between two of its pages arrives.
//
// rows is the whole list, read and ordered in memory, since both lists it
// pages are filtered and collated in Go rather than in the store.
func pageAfter[T any](w http.ResponseWriter, rows []T, id func(T) string, after string, limit int64) (page[T], bool) {
	start := 0
	if after != "" {
		i := slices.IndexFunc(rows, func(row T) bool { return id(row) == after })
		if i < 0 {
			wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter,
				"starting_after %q is not in this list, which has changed since the page before; read it again from the start", after)
			return page[T]{}, false
		}
		start = i + 1
	}
	return pageOf(rows[start:], limit), true
}

// limitParam is the query parameter limit as a count within size. ok is false
// once it has answered a 400.
func limitParam(w http.ResponseWriter, r *http.Request, size pageSize) (int64, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return size.fallback, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > size.most {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidLimit, "limit %q is not a whole number from 1 to %d", raw, size.most)
		return 0, false
	}
	return n, true
}

// optionalCount is the query parameter name as a whole number of at least 0, or
// an invalid NullInt64 when it is absent. ok is false once it has answered a
// 400.
func optionalCount(w http.ResponseWriter, r *http.Request, name string) (sql.NullInt64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return sql.NullInt64{}, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "%s %q is not a whole number of at least 0", name, raw)
		return sql.NullInt64{}, false
	}
	return sql.NullInt64{Int64: n, Valid: true}, true
}

func optionalText(r *http.Request, name string) sql.NullString {
	raw := r.URL.Query().Get(name)
	return sql.NullString{String: raw, Valid: raw != ""}
}

func nullableInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullableText(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}
