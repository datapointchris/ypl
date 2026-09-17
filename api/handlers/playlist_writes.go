package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// PlaylistWriter is what the API reads and writes playlists on YouTube through,
// as *youtube.Channel does. Requests and Units count every request it has sent.
type PlaylistWriter interface {
	Playlist(ctx context.Context, id youtube.PlaylistID) (youtube.Playlist, error)
	Videos(ctx context.Context, ids []youtube.VideoID) ([]youtube.Video, error)
	CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.Playlist, error)
	UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) (youtube.PlaylistDetails, error)
	DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error
	Requests() int64
	Units() int64
}

// maxPlaylistBody bounds the body of a playlist write. The longest title and
// description YouTube stores come to 5,150 code points, which JSON escapes to
// at most 12 bytes each.
const maxPlaylistBody = 64 << 10

const (
	// youtubeTimeout bounds one YouTube request of a playlist write: the read a
	// rename merges onto, or the write, with the pauses and attempts of an
	// update YouTube aborts.
	youtubeTimeout = 15 * time.Second
	// recordTimeout bounds each store step of a playlist write. SQLite lets a
	// step wait up to 5 seconds for the write lock.
	recordTimeout = 5 * time.Second
)

// WriteDuration is the longest a playlist write runs once it has begun: its
// record, its YouTube write, and the settlement that stores YouTube's answer.
// No write begins once Drain has been called.
const WriteDuration = recordTimeout + youtubeTimeout + recordTimeout

// errDraining is the cause logged for a playlist write refused because the
// server is draining.
var errDraining = errors.New("the server is shutting down, so it begins no YouTube write")

// playlistDetails is the body of POST /api/v1/playlists and PATCH
// /api/v1/playlists/{id}. A field left out is left as it is, and a create
// leaves out only the description.
type playlistDetails struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// youtubeContext is the context one YouTube request of a playlist write runs
// on, and recordContext the context of each store step. Each carries the
// request's values and no cancellation but its timeout, so a caller that goes
// away mid-write cannot leave a write YouTube made unrecorded.
func youtubeContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), youtubeTimeout)
}

func recordContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), recordTimeout)
}

// validDetails refuses a blank title, and a title or description longer than
// YouTube stores, counted as YouTube counts it. ok is false once it has
// answered a 422.
func validDetails(w http.ResponseWriter, body playlistDetails) bool {
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTitleRequired, "a playlist needs a title")
			return false
		}
		if n := utf8.RuneCountInString(title); n > youtube.MaxTitleLength {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTitleTooLong, "a playlist title is at most %d characters, and this one is %d", youtube.MaxTitleLength, n)
			return false
		}
	}
	if body.Description != nil {
		if n := utf8.RuneCountInString(strings.TrimSpace(*body.Description)); n > youtube.MaxDescriptionLength {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeDescriptionTooLong, "a playlist description is at most %d characters, and this one is %d", youtube.MaxDescriptionLength, n)
			return false
		}
	}
	return true
}

// createPlaylist creates a private playlist on YouTube and stores it under the
// id YouTube gave it, as YouTube's answer reports it.
func (h *Handlers) createPlaylist(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[playlistDetails](w, r, maxPlaylistBody, "a playlist")
	if !ok {
		return
	}
	if body.Title == nil {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTitleRequired, "a playlist needs a title")
		return
	}
	if !validDetails(w, body) {
		return
	}
	details := youtube.PlaylistDetails{Title: *body.Title}
	if body.Description != nil {
		details.Description = *body.Description
	}

	release, ok := h.takeWriteTurn(r)
	if !ok {
		return
	}
	defer release()
	var created youtube.Playlist
	made := h.writeToYouTube(w, r, youtubeWrite{
		method: youtube.MethodPlaylistsInsert,
		send: func(ctx context.Context) (youtube.PlaylistID, error) {
			var err error
			created, err = h.youtube.CreatePlaylist(ctx, details)
			return created.ID, err
		},
		record: func(ctx context.Context, tx *store.Tx) error {
			return tx.UpsertPlaylist(ctx, generated.UpsertPlaylistParams{
				PlaylistID: string(created.ID), Title: created.Title, Description: created.Description, Privacy: created.Privacy,
			})
		},
		made: func() string { return "YouTube created playlist " + string(created.ID) },
	})
	if !made {
		return
	}
	w.Header().Set("Location", "/api/v1/playlists/"+string(created.ID))
	wire.JSON(w, http.StatusCreated, playlistSummary{ID: string(created.ID), Title: created.Title, Description: created.Description, Privacy: created.Privacy})
}

// updatePlaylist sets the title or description, or both, of a stored playlist
// on YouTube and in the store. YouTube replaces both together, so the field a
// request leaves out is sent as YouTube holds it, read just before the write.
func (h *Handlers) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := decodeJSON[playlistDetails](w, r, maxPlaylistBody, "a playlist")
	if !ok {
		return
	}
	if body.Title == nil && body.Description == nil {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeNoChanges, "the body sets neither title nor description")
		return
	}
	if !validDetails(w, body) {
		return
	}

	release, ok := h.takeWriteTurn(r)
	if !ok {
		return
	}
	defer release()
	current, ok := h.currentDetails(w, r, id)
	if !ok {
		return
	}
	details := current
	if body.Title != nil {
		details.Title = *body.Title
	}
	if body.Description != nil {
		details.Description = *body.Description
	}

	var updated youtube.PlaylistDetails
	made := h.writeToYouTube(w, r, youtubeWrite{
		method:   youtube.MethodPlaylistsUpdate,
		playlist: id,
		send: func(ctx context.Context) (youtube.PlaylistID, error) {
			var err error
			updated, err = h.youtube.UpdatePlaylist(ctx, youtube.PlaylistID(id), details)
			return youtube.PlaylistID(id), err
		},
		record: func(ctx context.Context, tx *store.Tx) error {
			_, err := tx.UpdatePlaylistDetails(ctx, generated.UpdatePlaylistDetailsParams{PlaylistID: id, Title: updated.Title, Description: updated.Description})
			return err
		},
		made: func() string { return "YouTube updated playlist " + id },
	})
	if !made {
		return
	}
	ctx, cancel := recordContext(r)
	defer cancel()
	summary, err := h.store.Queries.GetPlaylistSummary(ctx, id)
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+id)
		return
	}
	wire.JSON(w, http.StatusOK, playlistSummary{
		ID:               summary.PlaylistID,
		Title:            summary.Title,
		Description:      summary.Description,
		Privacy:          summary.Privacy,
		ItemCount:        summary.ItemCount,
		UnavailableCount: summary.UnavailableCount,
		EnrichedCount:    summary.EnrichedCount,
	})
}

// currentDetails is the title and description of the stored playlist id that
// a rename merges onto. That is YouTube's, read now, unless the API wrote the
// playlist later than that read can show, when it is the store's, which holds
// the write. A playlist the read finds gone is deleted from the store and
// answered 404. ok is false once it has answered.
func (h *Handlers) currentDetails(w http.ResponseWriter, r *http.Request, id string) (youtube.PlaylistDetails, bool) {
	ctx, cancel := recordContext(r)
	stored, err := h.store.Queries.GetPlaylist(ctx, id)
	cancel()
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+id)
		return youtube.PlaylistDetails{}, false
	}

	readAt := h.now()
	ctx, cancel = youtubeContext(r)
	read, readErr := h.youtube.Playlist(ctx, youtube.PlaylistID(id))
	cancel()
	gone := errors.Is(readErr, youtube.ErrPlaylistNotFound)
	switch {
	case errors.Is(readErr, youtube.ErrQuotaSpent):
		h.refuseSpentQuota(w, r, readErr)
		return youtube.PlaylistDetails{}, false
	case readErr != nil && !gone:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusBadGateway, wire.CodeYouTubeReadFailed, readErr,
			"YouTube did not answer the read of playlist %s a rename merges onto, so nothing was written", id)
		return youtube.PlaylistDetails{}, false
	}

	ctx, cancel = recordContext(r)
	defer cancel()
	_, written, err := store.WriteNewerThanRead(ctx, h.store.Queries, id, readAt)
	switch {
	case err != nil:
		h.writeInternalError(w, r, err)
		return youtube.PlaylistDetails{}, false
	case written:
		return youtube.PlaylistDetails{Title: stored.Title, Description: stored.Description}, true
	case gone:
		if err := h.store.Queries.DeletePlaylist(ctx, id); err != nil {
			h.writeInternalError(w, r, err)
			return youtube.PlaylistDetails{}, false
		}
		wire.Refuse(w, http.StatusNotFound, wire.CodeNotFound, "playlist %s not found: YouTube no longer has it", id)
		return youtube.PlaylistDetails{}, false
	}
	return youtube.PlaylistDetails{Title: read.Title, Description: read.Description}, true
}

// deletePlaylist deletes a stored playlist on YouTube and from the store, with
// its items. A playlist YouTube has already deleted is deleted from the store.
func (h *Handlers) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	release, ok := h.takeWriteTurn(r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := recordContext(r)
	_, err := h.store.Queries.GetPlaylist(ctx, id)
	cancel()
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+id)
		return
	}

	made := h.writeToYouTube(w, r, youtubeWrite{
		method:   youtube.MethodPlaylistsDelete,
		playlist: id,
		send: func(ctx context.Context) (youtube.PlaylistID, error) {
			return youtube.PlaylistID(id), h.youtube.DeletePlaylist(ctx, youtube.PlaylistID(id))
		},
		record: func(ctx context.Context, tx *store.Tx) error {
			return tx.DeletePlaylist(ctx, id)
		},
		madeWhenAbsent: true,
		made:           func() string { return "YouTube deleted playlist " + id },
	})
	if made {
		w.WriteHeader(http.StatusNoContent)
	}
}

// Drain stops the API beginning a playlist write. A write already begun runs
// to its end, which WriteDuration bounds.
func (h *Handlers) Drain() {
	h.drainOnce.Do(func() { close(h.draining) })
}

// takeWriteTurn waits for the API's turn to write playlists, one request at a
// time, and returns the function that ends the turn. Taking the turn for the
// whole of a write keeps two writes from merging onto the same read, and keeps
// what the channel counts between a write's record and its settlement that
// write's own. ok is false when the caller went away while it waited, which
// leaves nothing to answer.
func (h *Handlers) takeWriteTurn(r *http.Request) (func(), bool) {
	release := func() { <-h.writing }
	select {
	case h.writing <- struct{}{}:
		return release, true
	default:
	}
	select {
	case h.writing <- struct{}{}:
		return release, true
	case <-r.Context().Done():
		return nil, false
	}
}

// youtubeWrite is one playlist write the API makes on YouTube.
type youtubeWrite struct {
	method string
	// playlist is the playlist the write names, and empty for a create.
	playlist string
	// send makes the write, and returns the id of the playlist it wrote.
	send func(context.Context) (youtube.PlaylistID, error)
	// record stores what the write changed, in the transaction that settles it.
	record func(context.Context, *store.Tx) error
	// madeWhenAbsent is whether YouTube answering that the playlist does not
	// exist leaves the playlist as the write would have, as it does a delete.
	madeWhenAbsent bool
	// made says what YouTube did, for a write the store could not record.
	made func() string
}

// writeToYouTube records yw as pending, sends it, and settles it with
// YouTube's answer. It holds the write turn the caller took. It returns true
// once YouTube made the write and the store recorded it, and false once it has
// answered a write that was not made, or not recorded.
func (h *Handlers) writeToYouTube(w http.ResponseWriter, r *http.Request, yw youtubeWrite) bool {
	select {
	case <-h.draining:
		h.refuseAndLog(w, r, slog.LevelInfo, http.StatusServiceUnavailable, wire.CodeShuttingDown, errDraining,
			"the server is shutting down, so it made no change; send the request again")
		return false
	default:
	}

	ctx, cancel := recordContext(r)
	var writeID int64
	err := h.store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		writeID, err = tx.BeginWrite(ctx, store.Write{Method: yw.method, PlaylistID: yw.playlist, SentAt: h.now()})
		return err
	})
	cancel()
	if err != nil {
		h.writeInternalError(w, r, err)
		return false
	}
	requests, units := h.youtube.Requests(), h.youtube.Units()

	ctx, cancel = youtubeContext(r)
	playlist, sendErr := yw.send(ctx)
	cancel()
	outcome := store.WriteOutcome(sendErr)
	made := outcome == store.WriteApplied || (outcome == store.WriteAbsent && yw.madeWhenAbsent)

	settlement := store.Settlement{
		WriteID:    writeID,
		PlaylistID: string(playlist),
		Outcome:    outcome,
		SettledAt:  h.now(),
		Requests:   h.youtube.Requests() - requests,
		Units:      h.youtube.Units() - units,
		Err:        sendErr,
	}
	ctx, cancel = recordContext(r)
	defer cancel()
	settleErr := h.store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.SettleWrite(ctx, settlement); err != nil {
			return err
		}
		if !made {
			return nil
		}
		return yw.record(ctx, tx)
	})

	switch {
	case made && settleErr != nil:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusInternalServerError, wire.CodeYouTubeWriteUnrecorded, settleErr,
			"%s, and the server could not record it; the next sync stores what YouTube holds, so do not send the request again", yw.made())
		return false
	case made:
		return true
	case settleErr != nil:
		h.log.ErrorContext(r.Context(), "YouTube write not recorded", "method", r.Method, "path", r.URL.Path, "outcome", outcome, "err", settleErr)
	}
	h.refuseUnmade(w, r, outcome, sendErr)
	return false
}

// refuseUnmade answers a YouTube write that did not make what the request asked
// for, which ended as outcome with sendErr.
func (h *Handlers) refuseUnmade(w http.ResponseWriter, r *http.Request, outcome string, sendErr error) {
	switch outcome {
	case store.WriteQuotaSpent:
		h.refuseSpentQuota(w, r, sendErr)
	case store.WriteRefused, store.WriteAbsent:
		message, _ := youtube.RefusalMessage(sendErr)
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeYouTubeRefused, "YouTube refused the write, and nothing changed: %s", message)
	case store.WriteUnanswered:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusBadGateway, wire.CodeYouTubeWriteFailed, sendErr,
			"YouTube did not confirm the write, which may still have landed; the next sync shows what YouTube holds")
	case store.WriteApplied, store.WritePending:
		h.writeInternalError(w, r, fmt.Errorf("a YouTube write that ended %q made nothing", outcome))
	default:
		h.writeInternalError(w, r, fmt.Errorf("a YouTube write ended %q, which is not an outcome the API knows: %w", outcome, sendErr))
	}
}

// refuseSpentQuota answers YouTube's refusal of a request for a spent quota
// with a 503 whose Retry-After is the quota's reset.
func (h *Handlers) refuseSpentQuota(w http.ResponseWriter, r *http.Request, err error) {
	now := h.now()
	wait := youtube.QuotaReset(now).Sub(now)
	w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(wait.Seconds())), 10))
	h.refuseAndLog(w, r, slog.LevelWarn, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent, err,
		"YouTube's daily quota is spent, and it resets at midnight Pacific")
}
