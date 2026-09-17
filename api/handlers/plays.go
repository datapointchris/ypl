package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

// maxPlayBody bounds the body of POST /api/v1/plays, many times the size of any
// play.
const maxPlayBody = 4 << 10

// play is one listen: its id, when it was in UTC to the second, and the video.
type play struct {
	ID       string      `json:"id"`
	PlayedTs string      `json:"played_ts"`
	Video    playedVideo `json:"video"`
}

type playedVideo struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ChannelTitle string `json:"channel_title"`
}

// newPlay is the body of POST /api/v1/plays. ID is a UUIDv7 the client
// generates, so sending the play again records it once. PlayedTs is an RFC 3339
// timestamp, and the time the request arrives when it is absent or null.
type newPlay struct {
	ID       string  `json:"id"`
	VideoID  string  `json:"video_id"`
	PlayedTs *string `json:"played_ts"`
}

// suggestion is a video to play next, with when it was last played and how
// many times.
type suggestion struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	LastPlayedTs    *string `json:"last_played_ts"`
	PlayCount       int64   `json:"play_count"`
}

var errVideoNotStored = errors.New("video not stored")

// createPlay records a play. A new play answers 201. The same id sent again for
// the same video answers 200 with the play as stored, unless it names another
// played_ts, which answers 409 as another video does.
func (h *Handlers) createPlay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, ok := decodePlay(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		writeError(w, http.StatusUnprocessableEntity, "id %q is not a version 7 UUID", body.ID)
		return
	}
	if body.VideoID == "" {
		writeError(w, http.StatusUnprocessableEntity, "video_id is required")
		return
	}
	playedAt := h.now()
	if body.PlayedTs != nil {
		if playedAt, err = time.Parse(time.RFC3339, *body.PlayedTs); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "played_ts %q is not an RFC 3339 timestamp", *body.PlayedTs)
			return
		}
	}
	playedTs := playedAt.UTC().Format(time.RFC3339)

	var stored generated.GetPlayRow
	var created bool
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetVideo(ctx, body.VideoID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errVideoNotStored
			}
			return err
		}
		n, err := tx.InsertPlay(ctx, generated.InsertPlayParams{PlayID: id.String(), VideoID: body.VideoID, PlayedTs: playedTs})
		if err != nil {
			return err
		}
		created = n == 1
		stored, err = tx.GetPlay(ctx, id.String())
		return err
	})
	switch {
	case errors.Is(err, errVideoNotStored):
		writeError(w, http.StatusUnprocessableEntity, "video %s is not in the store", body.VideoID)
		return
	case err != nil:
		h.writeInternalError(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/api/v1/plays/"+stored.PlayID)
		writeJSON(w, http.StatusCreated, playFrom(stored))
		return
	}
	if stored.VideoID != body.VideoID || (body.PlayedTs != nil && stored.PlayedTs != playedTs) {
		writeError(w, http.StatusConflict, "play %s is already stored for video %s at %s", stored.PlayID, stored.VideoID, stored.PlayedTs)
		return
	}
	writeJSON(w, http.StatusOK, playFrom(stored))
}

// decodePlay reads the request body as exactly one play. ok is false once it has
// answered a 400.
func decodePlay(w http.ResponseWriter, r *http.Request) (newPlay, bool) {
	var body newPlay
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlayBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "the body is not a play: %v", err)
		return newPlay{}, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "the body holds more than one JSON value")
		return newPlay{}, false
	}
	return body, true
}

func (h *Handlers) showPlay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stored, err := h.store.Queries.GetPlay(r.Context(), id)
	if err != nil {
		h.writeItemError(w, r, err, "play "+id)
		return
	}
	writeJSON(w, http.StatusOK, playFrom(stored))
}

// listPlays answers a page of plays, newest first.
func (h *Handlers) listPlays(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, 20, 100)
	if !ok {
		return
	}
	after := r.URL.Query().Get("starting_after")
	var rows []generated.ListPlaysRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		params := generated.ListPlaysParams{MaxRows: limit + 1}
		if after != "" {
			cursor, err := q.GetPlay(ctx, after)
			if err != nil {
				return paramRow(err, "starting_after", after)
			}
			params.AfterTs = sql.NullString{String: cursor.PlayedTs, Valid: true}
			params.AfterID = sql.NullString{String: cursor.PlayID, Valid: true}
		}
		var err error
		rows, err = q.ListPlays(ctx, params)
		return err
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}
	plays := make([]play, len(rows))
	for i, row := range rows {
		plays[i] = play{
			ID:       row.PlayID,
			PlayedTs: row.PlayedTs,
			Video:    playedVideo{ID: row.VideoID, Title: row.Title, ChannelTitle: row.ChannelTitle},
		}
	}
	writeJSON(w, http.StatusOK, pageOf(plays, limit))
}

// listSuggestions answers the videos to play next, least recently played first
// and never played before any, from the playlist playlist or from every
// playlist.
func (h *Handlers) listSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, 1, 100)
	if !ok {
		return
	}
	params := generated.ListSuggestionsParams{PlaylistID: optionalText(r, "playlist"), MaxRows: limit}
	var rows []generated.ListSuggestionsRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if params.PlaylistID.Valid {
			if _, err := q.GetPlaylist(ctx, params.PlaylistID.String); err != nil {
				return paramRow(err, "playlist", params.PlaylistID.String)
			}
		}
		var err error
		rows, err = q.ListSuggestions(ctx, params)
		return err
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}
	suggestions := make([]suggestion, len(rows))
	for i, row := range rows {
		lastPlayed, err := textOrNull(row.LastPlayedTs)
		if err != nil {
			h.writeInternalError(w, r, fmt.Errorf("last play of %s: %w", row.VideoID, err))
			return
		}
		suggestions[i] = suggestion{
			ID:              row.VideoID,
			Title:           row.Title,
			ChannelTitle:    row.ChannelTitle,
			DurationSeconds: nullableInt(row.DurationSeconds),
			LastPlayedTs:    lastPlayed,
			PlayCount:       row.PlayCount,
		}
	}
	writeJSON(w, http.StatusOK, suggestions)
}

func playFrom(row generated.GetPlayRow) play {
	return play{
		ID:       row.PlayID,
		PlayedTs: row.PlayedTs,
		Video:    playedVideo{ID: row.VideoID, Title: row.Title, ChannelTitle: row.ChannelTitle},
	}
}

// textOrNull is an aggregate the driver scanned without a declared type: nil for
// NULL, or its text.
func textOrNull(v any) (*string, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	}
	return nil, fmt.Errorf("%v is a %T, want text or NULL", v, v)
}
