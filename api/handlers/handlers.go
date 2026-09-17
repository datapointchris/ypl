// Package handlers serves the API's resources as JSON over the store: the
// playlists and videos the sync stores, plays, suggestions of what to play
// next, and the sync's own runs.
//
// A value the store does not hold is null, and a collection with no members is
// []. A collection that grows without bound is paged: its body is {"data": [...],
// "has_more": ...}, and the next page is the same request with starting_after
// set to the last id on this one. An error answers with {"error": "..."} and the
// status that names it.
package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/datapointchris/ypl/api/store"
)

// Handlers answers the API's requests from one store.
type Handlers struct {
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

// New is Handlers over st, logging the cause of every failure it answers with a
// 500 to log.
func New(st *store.Store, log *slog.Logger) *Handlers {
	return &Handlers{store: st, log: log, now: time.Now}
}

// Register adds every resource's routes to mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/playlists", h.listPlaylists)
	mux.HandleFunc("GET /api/v1/playlists/{id}", h.showPlaylist)
	mux.HandleFunc("GET /api/v1/videos", h.listVideos)
	mux.HandleFunc("GET /api/v1/videos/{id}", h.showVideo)
	mux.HandleFunc("POST /api/v1/plays", h.createPlay)
	mux.HandleFunc("GET /api/v1/plays", h.listPlays)
	mux.HandleFunc("GET /api/v1/plays/{id}", h.showPlay)
	mux.HandleFunc("GET /api/v1/suggestions", h.listSuggestions)
	mux.HandleFunc("GET /api/v1/sync/runs", h.listSyncRuns)
	mux.HandleFunc("GET /api/v1/status", h.showStatus)
}

type errorBody struct {
	Error string `json:"error"`
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

// newCollator orders names the way a reader expects them sorted: by the Unicode
// Collation Algorithm, where case and accents decide only between names
// otherwise equal. A Collator is not safe for concurrent use, so each request
// makes its own.
func newCollator() *collate.Collator {
	return collate.New(language.Und)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, errorBody{Error: fmt.Sprintf(format, args...)})
}

// unknownParamError is a query parameter naming a row the store does not hold.
type unknownParamError struct {
	name, value string
}

func (e unknownParamError) Error() string {
	return fmt.Sprintf("%s %q names nothing the store holds", e.name, e.value)
}

// paramRow is err from reading the row the query parameter name names, with a
// row that is not there as an unknownParamError.
func paramRow(err error, name, value string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return unknownParamError{name: name, value: value}
	}
	return err
}

// writeItemError answers a failed read of the resource the path names: its row
// not being there is a 404 naming what, and anything else a 500.
func (h *Handlers) writeItemError(w http.ResponseWriter, r *http.Request, err error, what string) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "%s not found", what)
		return
	}
	h.writeInternalError(w, r, err)
}

// writeListError answers a failed read of a collection: a query parameter
// naming nothing is a 400, and anything else a 500.
func (h *Handlers) writeListError(w http.ResponseWriter, r *http.Request, err error) {
	var unknown unknownParamError
	if errors.As(err, &unknown) {
		writeError(w, http.StatusBadRequest, "%s", unknown.Error())
		return
	}
	h.writeInternalError(w, r, err)
}

// writeInternalError answers a 500, logging err rather than sending it.
func (h *Handlers) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// limitParam is the query parameter limit as a count from 1 to most, or
// fallback when it is absent. ok is false once it has answered a 400.
func limitParam(w http.ResponseWriter, r *http.Request, fallback, most int64) (int64, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > most {
		writeError(w, http.StatusBadRequest, "limit %q is not a whole number from 1 to %d", raw, most)
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
		writeError(w, http.StatusBadRequest, "%s %q is not a whole number of at least 0", name, raw)
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
