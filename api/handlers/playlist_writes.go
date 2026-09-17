package handlers

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// PlaylistWriter is what the API creates, renames and deletes playlists on
// YouTube through, as *youtube.Channel does.
type PlaylistWriter interface {
	CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.PlaylistID, error)
	UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) error
	DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error
}

// maxPlaylistBody bounds the body of a playlist write, which YouTube's own
// limits on a title and a description keep far smaller.
const maxPlaylistBody = 64 << 10

// youtubeWriteTimeout bounds a YouTube write and the store write recording it.
const youtubeWriteTimeout = 30 * time.Second

// createdPrivacy is the privacy YouTube gives every playlist the API creates.
const createdPrivacy = "private"

// playlistDetails is the body of POST /api/v1/playlists and PATCH
// /api/v1/playlists/{id}. A field left out is left as it is, and a create
// leaves out only the description.
type playlistDetails struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// writeContext is the context a YouTube write and its record in the store run
// on: the request's values, with no cancellation but the timeout. A caller that
// goes away mid-write would otherwise leave a write YouTube applied unrecorded.
func writeContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), youtubeWriteTimeout)
}

// createPlaylist creates a private playlist on YouTube and stores it under the
// id YouTube gave it.
func (h *Handlers) createPlaylist(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeJSON[playlistDetails](w, r, maxPlaylistBody, "a playlist")
	if !ok {
		return
	}
	if body.Title == nil || strings.TrimSpace(*body.Title) == "" {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTitleRequired, "a playlist needs a title")
		return
	}
	details := youtube.PlaylistDetails{Title: *body.Title}
	if body.Description != nil {
		details.Description = *body.Description
	}

	ctx, cancel := writeContext(r)
	defer cancel()
	id, err := h.youtube.CreatePlaylist(ctx, details)
	if err != nil {
		h.writeYouTubeError(w, r, err)
		return
	}
	stored := generated.UpsertPlaylistParams{PlaylistID: string(id), Title: details.Title, Description: details.Description, Privacy: createdPrivacy}
	if err := h.store.Queries.UpsertPlaylist(ctx, stored); err != nil {
		h.writeInternalError(w, r, errors.Join(errors.New("YouTube created playlist "+string(id)+", which the next sync stores"), err))
		return
	}
	w.Header().Set("Location", "/api/v1/playlists/"+string(id))
	wire.JSON(w, http.StatusCreated, playlistSummary{ID: string(id), Title: details.Title, Description: details.Description, Privacy: createdPrivacy})
}

// updatePlaylist sets the title or description, or both, of a stored playlist
// on YouTube and in the store. YouTube replaces both together, so the field a
// request leaves out is sent as stored.
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
	if body.Title != nil && strings.TrimSpace(*body.Title) == "" {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTitleRequired, "a playlist needs a title")
		return
	}

	ctx, cancel := writeContext(r)
	defer cancel()
	stored, err := h.store.Queries.GetPlaylist(ctx, id)
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+id)
		return
	}
	details := youtube.PlaylistDetails{Title: stored.Title, Description: stored.Description}
	if body.Title != nil {
		details.Title = *body.Title
	}
	if body.Description != nil {
		details.Description = *body.Description
	}
	if err := h.youtube.UpdatePlaylist(ctx, youtube.PlaylistID(id), details); err != nil {
		h.writeYouTubeError(w, r, err)
		return
	}
	params := generated.UpdatePlaylistDetailsParams{PlaylistID: id, Title: details.Title, Description: details.Description}
	if _, err := h.store.Queries.UpdatePlaylistDetails(ctx, params); err != nil {
		h.writeInternalError(w, r, errors.Join(errors.New("YouTube updated playlist "+id+", which the next sync stores"), err))
		return
	}
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

// deletePlaylist deletes a stored playlist on YouTube and from the store, with
// its items. A playlist YouTube has already deleted is deleted from the store.
func (h *Handlers) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := writeContext(r)
	defer cancel()
	if _, err := h.store.Queries.GetPlaylist(ctx, id); err != nil {
		h.writeItemError(w, r, err, "playlist "+id)
		return
	}
	if err := h.youtube.DeletePlaylist(ctx, youtube.PlaylistID(id)); err != nil && !errors.Is(err, youtube.ErrPlaylistNotFound) {
		h.writeYouTubeError(w, r, err)
		return
	}
	if err := h.store.Queries.DeletePlaylist(ctx, id); err != nil {
		h.writeInternalError(w, r, errors.Join(errors.New("YouTube deleted playlist "+id+", which the next sync removes"), err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeYouTubeError answers a YouTube write that failed. A spent quota is a 503
// whose Retry-After is the quota's reset. Anything else is a 502 whose cause is
// logged: the write may still have landed, and the next sync stores what
// YouTube holds.
func (h *Handlers) writeYouTubeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, youtube.ErrQuotaSpent) {
		now := h.now()
		wait := youtube.QuotaReset(now).Sub(now)
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(wait.Seconds())), 10))
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent, "YouTube's daily quota is spent, and it resets at midnight Pacific")
		return
	}
	h.log.ErrorContext(r.Context(), "YouTube write failed", "method", r.Method, "path", r.URL.Path, "err", err)
	wire.Refuse(w, http.StatusBadGateway, wire.CodeYouTubeWriteFailed, "YouTube did not confirm the write, which may still have landed; the next sync shows what YouTube holds")
}
