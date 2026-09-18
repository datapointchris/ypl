package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

// maxPlayBody bounds the body of POST /api/v1/plays, many times the size of any
// play.
const maxPlayBody = 4 << 10

// clockSkew is how far a client's clock may run ahead of the server's and still
// record a play at the time the client read. A play later than that is refused:
// it would rank its video as just played until then, and deleting it is the
// only correction a stored play takes.
const clockSkew = 5 * time.Minute

// tailLength is how many of a play id's last characters name it where the
// handle is not known yet.
const tailLength = 8

// play is one listen: its id, the short handle the server gave it, when it was
// in UTC to the second, and the video.
type play struct {
	ID       string      `json:"id"`
	Handle   int64       `json:"handle"`
	PlayedTs string      `json:"played_ts"`
	Video    playedVideo `json:"video"`
}

type playedVideo struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ChannelTitle string `json:"channel_title"`
}

// newPlay is the body of POST /api/v1/plays. ID is a UUIDv7 the client
// generates, in its lowercase hyphenated form, so sending the play again
// records it once. PlayedTs is an RFC 3339 timestamp, and the time the request
// arrives when it is absent or null.
type newPlay struct {
	ID       string  `json:"id"`
	VideoID  string  `json:"video_id"`
	PlayedTs *string `json:"played_ts"`
}

// suggestion is a video to play next, with how many times it was played and
// when last.
type suggestion struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	PlayCount       int64   `json:"play_count"`
	LastPlayedTs    *string `json:"last_played_ts"`
}

var (
	errVideoNotStored = errors.New("video not stored")
	errPlayRetired    = errors.New("play retired")
)

// createPlay records a play. A new play answers 201. The same id sent again for
// the same video answers 200 with the play as stored, unless it names another
// played_ts, which answers 409 as another video does. The id of a play that was
// deleted answers 410, so a retry arriving after the delete cannot bring the
// play back.
func (h *Handlers) createPlay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	arrival := h.now()
	body, ok := decodeJSON[newPlay](w, r, maxPlayBody, "a play")
	if !ok {
		return
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id.String() != body.ID || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidPlayID, "id %q is not a version 7 UUID written lowercase with hyphens", body.ID)
		return
	}
	if body.VideoID == "" {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoIDRequired, "video_id is required")
		return
	}
	playedAt := arrival
	if body.PlayedTs != nil {
		raw := *body.PlayedTs
		if playedAt, err = time.Parse(time.RFC3339, raw); err != nil {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidPlayedTs, "played_ts %q is not an RFC 3339 timestamp", raw)
			return
		}
		if year := playedAt.UTC().Year(); year < 0 || year > 9999 {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodePlayedTsOutOfRange, "played_ts %q is outside the years 0000 to 9999 in UTC", raw)
			return
		}
		if playedAt.After(arrival.Add(clockSkew)) {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodePlayedTsInTheFuture, "played_ts %q is later than the request arrived", raw)
			return
		}
	}
	playedTs := store.Timestamp(playedAt)

	var stored generated.GetPlayRow
	var created bool
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetVideo(ctx, body.VideoID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errVideoNotStored
			}
			return err
		}
		retired, err := tx.IsPlayRetired(ctx, body.ID)
		if err != nil {
			return err
		}
		if retired {
			return errPlayRetired
		}
		n, err := tx.InsertPlay(ctx, generated.InsertPlayParams{PlayID: body.ID, VideoID: body.VideoID, PlayedTs: playedTs})
		if err != nil {
			return err
		}
		created = n == 1
		stored, err = tx.GetPlay(ctx, body.ID)
		return err
	})
	switch {
	case errors.Is(err, errVideoNotStored):
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoNotStored, "video %s is not in the store", body.VideoID)
		return
	case errors.Is(err, errPlayRetired):
		wire.Refuse(w, http.StatusGone, wire.CodePlayRetired, "play %s was deleted, and its id records nothing again", body.ID)
		return
	case err != nil:
		h.writeInternalError(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/api/v1/plays/"+stored.PlayID)
		wire.JSON(w, http.StatusCreated, playFrom(stored))
		return
	}
	if stored.VideoID != body.VideoID || (body.PlayedTs != nil && stored.PlayedTs != playedTs) {
		wire.Refuse(w, http.StatusConflict, wire.CodePlayConflict, "play %s is already stored for video %s at %s", stored.PlayID, stored.VideoID, stored.PlayedTs)
		return
	}
	wire.JSON(w, http.StatusOK, playFrom(stored))
}

func (h *Handlers) showPlay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("id")
	var stored generated.GetPlayRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		stored, err = resolvePlay(ctx, q, "play", ref)
		return err
	})
	if err != nil {
		h.writeItemError(w, r, err, "play "+ref)
		return
	}
	wire.JSON(w, http.StatusOK, playFrom(stored))
}

// deletePlay deletes the play ref names, which answers 204. Its id and handle
// are kept in retired_plays, so the handle is never given to another play and
// the id, sent again, is refused rather than stored a second time.
func (h *Handlers) deletePlay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("id")
	err := h.store.InTx(ctx, func(tx *store.Tx) error {
		stored, err := resolvePlay(ctx, tx.Queries, "play", ref)
		if err != nil {
			return err
		}
		if err := tx.RetirePlay(ctx, stored.PlayID); err != nil {
			return err
		}
		return tx.DeletePlay(ctx, stored.PlayID)
	})
	if err != nil {
		h.writeItemError(w, r, err, "play "+ref)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolvePlay is the play ref names: its id, its handle, or the last tailLength
// characters of its id. A ref naming more than one play, a tail two ids share or
// an all-digit tail that is also another play's handle, is a referenceError
// listing their handles. name is what the error calls ref.
func resolvePlay(ctx context.Context, q *generated.Queries, name, ref string) (generated.GetPlayRow, error) {
	if id, err := uuid.Parse(ref); err == nil && id.String() == ref {
		row, err := q.GetPlay(ctx, ref)
		return row, paramRow(err, referenceError{name: name, value: ref})
	}
	var found []generated.GetPlayRow
	if handle, err := strconv.ParseInt(ref, 10, 64); err == nil && handle > 0 && strconv.FormatInt(handle, 10) == ref {
		row, err := q.GetPlayByHandle(ctx, handle)
		switch {
		case err == nil:
			found = append(found, generated.GetPlayRow(row))
		case !errors.Is(err, sql.ErrNoRows):
			return generated.GetPlayRow{}, err
		}
	}
	if isTail(ref) {
		rows, err := q.ListPlaysByTail(ctx, ref)
		if err != nil {
			return generated.GetPlayRow{}, err
		}
		for _, row := range rows {
			if !slices.ContainsFunc(found, func(f generated.GetPlayRow) bool { return f.PlayID == row.PlayID }) {
				found = append(found, generated.GetPlayRow(row))
			}
		}
	}
	switch len(found) {
	case 0:
		return generated.GetPlayRow{}, referenceError{name: name, value: ref}
	case 1:
		return found[0], nil
	}
	handles := make([]int64, len(found))
	for i, row := range found {
		handles[i] = row.Handle
	}
	slices.Sort(handles)
	candidates := make([]string, len(handles))
	for i, handle := range handles {
		candidates[i] = strconv.FormatInt(handle, 10)
	}
	return generated.GetPlayRow{}, referenceError{name: name, value: ref, candidates: candidates}
}

// isTail reports whether ref has the shape of a play id's last tailLength
// characters: lowercase hexadecimal digits.
func isTail(ref string) bool {
	if len(ref) != tailLength {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// listPlays answers a page of plays, newest first.
func (h *Handlers) listPlays(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, playsPage)
	if !ok {
		return
	}
	after := r.URL.Query().Get("starting_after")
	var plays []play
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if after == "" {
			rows, err := q.ListNewestPlays(ctx, limit+1)
			for _, row := range rows {
				plays = append(plays, playFrom(generated.GetPlayRow(row)))
			}
			return err
		}
		cursor, err := resolvePlay(ctx, q, "starting_after", after)
		if err != nil {
			return err
		}
		rows, err := q.ListPlaysBefore(ctx, generated.ListPlaysBeforeParams{PlayedTs: cursor.PlayedTs, PlayID: cursor.PlayID, MaxRows: limit + 1})
		for _, row := range rows {
			plays = append(plays, playFrom(generated.GetPlayRow(row)))
		}
		return err
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}
	if plays == nil {
		plays = []play{}
	}
	wire.JSON(w, http.StatusOK, pageOf(plays, limit))
}

// listSuggestions answers the videos to play next, least recently played first
// and never played before any, from the playlist playlist or from every
// playlist. limit is how many to draw. Videos last played at the same time come
// in a new order on every request, so the answer is a draw rather than a page
// of a stable list, and it has no next page.
func (h *Handlers) listSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok := limitParam(w, r, suggestionsDraw)
	if !ok {
		return
	}
	params := generated.ListSuggestionsParams{PlaylistID: optionalText(r, "playlist"), MaxRows: limit}
	var rows []generated.ListSuggestionsRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if params.PlaylistID.Valid {
			id, err := resolvePlaylist(ctx, q, "playlist", params.PlaylistID.String, loosely)
			if err != nil {
				return err
			}
			params.PlaylistID.String = id
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
			PlayCount:       row.PlayCount,
			LastPlayedTs:    lastPlayed,
		}
	}
	wire.JSON(w, http.StatusOK, suggestions)
}

func playFrom(row generated.GetPlayRow) play {
	return play{
		ID:       row.PlayID,
		Handle:   row.Handle,
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
