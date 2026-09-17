package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// maxOrderBody bounds the body of an edit of a playlist's order, far above the
// ids of the largest playlist the channel holds.
const maxOrderBody = 1 << 20

// playlistOrder is the body of PUT /api/v1/playlists/{id}/items: the whole new
// order of the playlist, one video id a slot, repeats allowed.
type playlistOrder struct {
	VideoIDs []string `json:"video_ids"`
}

// videoID is the characters a YouTube video id is made of. A read of videos
// joins ids with commas, so an id holding anything else could name two.
var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// entityTag is revision as a strong entity tag.
func entityTag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// errRevisionMoved and errVideoUnavailable are the refusals of an edit found
// inside the transaction that would store it.
var (
	errRevisionMoved    = errors.New("the playlist's order changed since the revision the edit names")
	errVideoUnavailable = errors.New("a video the edit adds is unavailable")
)

// replacePlaylistItems sets the server's order of a stored playlist to the
// order the body names, if the order is still at the revision If-Match names.
// The sync pushes the new order to YouTube on its next run. Each video takes the
// earliest entry holding it that no earlier video took, keeping the YouTube item
// that entry is held in, and a video no entry is left for takes a new entry. A
// video the store has never seen is read from YouTube.
func (h *Handlers) replacePlaylistItems(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	revision, ok := revisionPrecondition(w, r)
	if !ok {
		return
	}
	body, ok := decodeJSON[playlistOrder](w, r, maxOrderBody, "a playlist's order")
	if !ok {
		return
	}
	if body.VideoIDs == nil {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoIDsRequired, "the body names no video_ids; an empty playlist is []")
		return
	}
	for _, video := range body.VideoIDs {
		if !videoID.MatchString(video) {
			wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, "%q is not a YouTube video id", video)
			return
		}
	}

	ctx := r.Context()
	current, err := h.store.Queries.GetPlaylistState(ctx, id)
	switch {
	case err != nil:
		h.writeItemError(w, r, err, "playlist "+id)
		return
	case current.Revision != revision:
		refuseRevision(w, id, revision)
		return
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(body.VideoIDs)))
	known, err := h.store.Queries.ListVideoAvailability(ctx, distinct)
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	unavailable := map[string]bool{}
	for _, row := range known {
		unavailable[row.VideoID] = row.IsUnavailable
	}
	unknown := slices.DeleteFunc(slices.Clone(distinct), func(video string) bool {
		_, ok := unavailable[video]
		return ok
	})
	found, ok := h.readVideos(w, r, unknown)
	if !ok {
		return
	}

	var shown playlist
	var added []string
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		state, err := tx.GetPlaylistState(ctx, id)
		if err != nil {
			return err
		}
		if state.Revision != revision {
			return errRevisionMoved
		}
		for _, video := range found {
			params := generated.UpsertAvailableVideoParams{VideoID: string(video.ID), Title: video.Title, ChannelTitle: video.ChannelTitle}
			if err := tx.UpsertAvailableVideo(ctx, params); err != nil {
				return err
			}
		}
		entries, err := store.Entries(ctx, tx.Queries, id)
		if err != nil {
			return err
		}
		ordered := orderedEntries(entries, body.VideoIDs)
		for _, entry := range ordered {
			if entry.ID == 0 && unavailable[entry.VideoID] {
				added = append(added, entry.VideoID)
			}
		}
		if len(added) > 0 {
			return errVideoUnavailable
		}
		if !slices.Equal(ordered, entries) {
			if err := tx.ReplaceEntries(ctx, id, ordered); err != nil {
				return err
			}
			if n, err := tx.BumpRevision(ctx, generated.BumpRevisionParams{PlaylistID: id, Revision: revision}); err != nil || n != 1 {
				return errors.Join(err, errRevisionMoved)
			}
		}
		shown, err = readPlaylist(ctx, tx.Queries, id)
		return err
	})
	switch {
	case errors.Is(err, errRevisionMoved):
		refuseRevision(w, id, revision)
	case errors.Is(err, errVideoUnavailable):
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoUnavailable, "YouTube will not add a private or deleted video: %s", strings.Join(slices.Compact(slices.Sorted(slices.Values(added))), ", "))
	case err != nil:
		h.writeItemError(w, r, err, "playlist "+id)
	default:
		writePlaylist(w, shown)
	}
}

// refuseRevision answers an edit naming a revision the playlist has moved past.
func refuseRevision(w http.ResponseWriter, id string, revision int64) {
	wire.Refuse(w, http.StatusPreconditionFailed, wire.CodeRevisionMismatch, "playlist %s is no longer at revision %d; read it again and edit that", id, revision)
}

// revisionPrecondition is the revision the request's If-Match names. ok is false
// once it has answered: a 428 when If-Match is absent, and a 400 when it is not
// one strong entity tag holding a revision.
func revisionPrecondition(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		wire.Refuse(w, http.StatusPreconditionRequired, wire.CodeRevisionRequired, "an edit of a playlist's order names the revision it edits in If-Match, as the playlist's ETag gives it")
		return 0, false
	}
	tag := strings.TrimSpace(values[0])
	unquoted, quoted := strings.CutPrefix(tag, `"`)
	unquoted, closed := strings.CutSuffix(unquoted, `"`)
	revision, err := strconv.ParseInt(unquoted, 10, 64)
	if len(values) > 1 || !quoted || !closed || err != nil || revision < 1 {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidRevision, "If-Match %q is not one playlist revision, such as \"3\"", strings.Join(values, ", "))
		return 0, false
	}
	return revision, true
}

// readVideos reads from YouTube each of ids the store has never seen, and
// returns the videos a playlist can hold. A private video is not one: the store
// holds a private video a playlist read reports as unavailable. ok is false once
// it has answered: a 422 naming each id YouTube returns no public or unlisted
// video for, or YouTube's failure to answer.
func (h *Handlers) readVideos(w http.ResponseWriter, r *http.Request, ids []string) ([]youtube.Video, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	// The read takes the write turn, so a playlist write's count of the
	// channel's requests holds only its own.
	release, ok := h.takeWriteTurn(r)
	if !ok {
		return nil, false
	}
	defer release()
	videoIDs := make([]youtube.VideoID, len(ids))
	for i, id := range ids {
		videoIDs[i] = youtube.VideoID(id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), youtubeTimeout)
	videos, err := h.youtube.Videos(ctx, videoIDs)
	cancel()
	switch {
	case errors.Is(err, youtube.ErrQuotaSpent):
		h.refuseSpentQuota(w, r, err)
		return nil, false
	case err != nil:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusBadGateway, wire.CodeYouTubeReadFailed, err,
			"YouTube did not answer the read of the videos the edit adds, so nothing changed")
		return nil, false
	}
	videos = slices.DeleteFunc(videos, func(v youtube.Video) bool { return v.Privacy == "private" })
	missing := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
		return slices.ContainsFunc(videos, func(v youtube.Video) bool { return string(v.ID) == id })
	})
	if len(missing) > 0 {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoNotFound, "YouTube has no video this channel can add for %s", strings.Join(missing, ", "))
		return nil, false
	}
	return videos, true
}

// orderedEntries maps videoIDs onto entries: each video takes the earliest entry
// holding it that no earlier video took, keeping the entry's id and item, and a
// video no entry is left for takes a new entry.
func orderedEntries(entries []store.Entry, videoIDs []string) []store.Entry {
	holding := map[string][]store.Entry{}
	for _, entry := range entries {
		holding[entry.VideoID] = append(holding[entry.VideoID], entry)
	}
	ordered := make([]store.Entry, len(videoIDs))
	for i, video := range videoIDs {
		if left := holding[video]; len(left) > 0 {
			ordered[i], holding[video] = left[0], left[1:]
			continue
		}
		ordered[i] = store.Entry{VideoID: video}
	}
	return ordered
}
