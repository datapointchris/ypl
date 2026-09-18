package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// maxOrderBody bounds the body of an edit of a playlist's order, above the ids
// of the most videos a playlist holds.
const maxOrderBody = 1 << 20

// playlistOrder is the server's order of a playlist as GET and PUT
// /api/v1/playlists/{id}/items carry it: one video id a slot, repeats allowed.
type playlistOrder struct {
	VideoIDs []string `json:"video_ids"`
}

// videoID is the characters a YouTube video id is made of. A read of videos
// joins ids with commas, so an id holding anything else could name two.
var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// unavailableVideos is the refusal of an edit whose order adds videos YouTube
// will not add, found inside the transaction that would store it.
type unavailableVideos []string

func (videos unavailableVideos) Error() string {
	return "the order adds videos YouTube will not add: " + strings.Join(videos, ", ")
}

// showPlaylistItems answers the server's order of a stored playlist, with the
// order's revision as the ETag an edit names in If-Match.
//
// It resolves as narrowly as the edit it seeds. The ETag it answers with is
// only ever spent on the PUT below, so a reference this read takes and that PUT
// refuses buys the caller an editing session it then throws away. Both methods
// of one resource answering the same reference is what stops that.
func (h *Handlers) showPlaylistItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("id")
	var order playlistOrder
	var revision int64
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		id, err := resolvePlaylist(ctx, q, "playlist", ref, exactly)
		if err != nil {
			return err
		}
		state, err := q.GetPlaylistState(ctx, id)
		if err != nil {
			return err
		}
		revision = state.Revision
		order, err = readOrder(ctx, q, id)
		return err
	})
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+ref)
		return
	}
	writeOrder(w, revision, order)
}

// replacePlaylistItems sets the server's order of a stored playlist to the
// order the body names, if If-Match matches the order's current ETag. The sync
// pushes the new order to YouTube on its next run. Each video takes the earliest
// entry holding it that no earlier video took, keeping the YouTube item that
// entry is held in, and a video no entry is left for takes a new entry. A video
// the store has never seen is read from YouTube.
func (h *Handlers) replacePlaylistItems(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		// Escaped, because the sentence carries a URL to send and a title holds
		// spaces and slashes. Unescaped it names an address the router will not
		// route, which is worse than naming none.
		wire.Refuse(w, http.StatusPreconditionRequired, wire.CodePreconditionRequired,
			"an edit of a playlist's order names the order it edits in If-Match, as the ETag of GET /api/v1/playlists/%s/items gives it", url.PathEscape(ref))
		return
	}
	condition, ok := parseIfMatch(values)
	if !ok {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidPrecondition, "If-Match %q is not a list of entity tags, such as \"3\"", strings.Join(values, ", "))
		return
	}
	// On the request's own context, unlike the playlist writes: this edit reaches
	// only the store and only inside one transaction, so a caller that goes away
	// leaves nothing half-done and canceling costs nothing. It resolves as
	// narrowly as they do, because it replaces an order rather than reading one.
	ctx := r.Context()
	id, err := resolvePlaylist(ctx, h.store.Queries, "playlist", ref, exactly)
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+ref)
		return
	}
	current, err := h.store.Queries.GetPlaylistState(ctx, id)
	switch {
	case err != nil:
		h.writeItemError(w, r, err, "playlist "+ref)
		return
	case !condition.matches(revisionTag(current.Revision)):
		refusePrecondition(w, ref)
		return
	}

	body, ok := decodeJSON[playlistOrder](w, r, maxOrderBody, "a playlist's order")
	if !ok {
		return
	}
	switch {
	case body.VideoIDs == nil:
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoIDsRequired, "the body names no video_ids; an empty playlist is []")
		return
	case len(body.VideoIDs) > youtube.MaxPlaylistItems:
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTooManyVideos, "the order names %d videos, and a YouTube playlist holds at most %d", len(body.VideoIDs), youtube.MaxPlaylistItems)
		return
	}
	invalid := slices.DeleteFunc(slices.Clone(body.VideoIDs), videoID.MatchString)
	if len(invalid) > 0 {
		invalid = slices.Compact(slices.Sorted(slices.Values(invalid)))
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, invalid, "these are not YouTube video ids: %q", invalid)
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
	found, missing, ok := h.readVideos(w, r, unknown)
	if !ok {
		return
	}

	var revision int64
	err = h.store.InTx(ctx, func(tx *store.Tx) error {
		state, err := tx.GetPlaylistState(ctx, id)
		switch {
		case err != nil:
			return err
		case !condition.matches(revisionTag(state.Revision)):
			return store.ErrRevisionMoved
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
		refused := slices.Clone(missing)
		for _, entry := range ordered {
			if entry.ID == 0 && unavailable[entry.VideoID] {
				refused = append(refused, entry.VideoID)
			}
		}
		if len(refused) > 0 {
			return unavailableVideos(slices.Compact(slices.Sorted(slices.Values(refused))))
		}
		if revision, err = tx.ReplaceOrder(ctx, id, state.Revision, ordered); err != nil || revision == state.Revision {
			return err
		}
		// The edit tries positions again on a playlist YouTube refused one in,
		// which YouTube may since have let the channel order by hand.
		return tx.SetPlaylistSort(ctx, generated.SetPlaylistSortParams{PlaylistID: id, Sort: store.SortManual})
	})
	var refused unavailableVideos
	switch {
	case errors.Is(err, store.ErrRevisionMoved):
		refusePrecondition(w, ref)
	case errors.As(err, &refused):
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeVideoUnavailable, refused,
			"YouTube has no public or unlisted video this channel can add for %s", strings.Join(refused, ", "))
	case err != nil:
		h.writeItemError(w, r, err, "playlist "+ref)
	default:
		writeOrder(w, revision, playlistOrder{VideoIDs: body.VideoIDs})
	}
}

// readOrder is the server's order of the stored playlist id.
func readOrder(ctx context.Context, q *generated.Queries, id string) (playlistOrder, error) {
	entries, err := store.Entries(ctx, q, id)
	if err != nil {
		return playlistOrder{}, err
	}
	order := playlistOrder{VideoIDs: make([]string, len(entries))}
	for i, entry := range entries {
		order.VideoIDs[i] = entry.VideoID
	}
	return order, nil
}

// writeOrder answers 200 with order, and revision as its ETag.
func writeOrder(w http.ResponseWriter, revision int64, order playlistOrder) {
	w.Header().Set("ETag", revisionTag(revision))
	wire.JSON(w, http.StatusOK, order)
}

// refusePrecondition answers an edit whose If-Match the playlist's order no
// longer matches.
func refusePrecondition(w http.ResponseWriter, id string) {
	wire.Refuse(w, http.StatusPreconditionFailed, wire.CodePreconditionFailed,
		"the order of playlist %s no longer has the ETag If-Match names; read it again and edit that", id)
}

// readVideos reads from YouTube each of ids the store has never seen. It returns
// the videos a playlist can hold, and each id YouTube returns none for. A
// private video is not one: the store holds a private video a playlist read
// reports as unavailable. ok is false once it has answered YouTube's failure to
// answer.
func (h *Handlers) readVideos(w http.ResponseWriter, r *http.Request, ids []string) ([]youtube.Video, []string, bool) {
	if len(ids) == 0 {
		return nil, nil, true
	}
	// The read takes the write turn, so a playlist write's count of the
	// channel's requests holds only its own.
	release, ok := h.takeWriteTurn(r)
	if !ok {
		return nil, nil, false
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
		return nil, nil, false
	case err != nil:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusBadGateway, wire.CodeYouTubeReadFailed, err,
			"YouTube did not answer the read of the videos the edit adds, so nothing changed")
		return nil, nil, false
	}
	videos = slices.DeleteFunc(videos, func(v youtube.Video) bool { return v.Privacy == "private" })
	missing := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
		return slices.ContainsFunc(videos, func(v youtube.Video) bool { return string(v.ID) == id })
	})
	return videos, missing, true
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
