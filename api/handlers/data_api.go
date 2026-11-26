package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/datapointchris/goapiauth"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// DataAPI is what the Data API routes under /api/v1/youtube/ call YouTube
// through, as *youtube.Channel does. Requests and Units count every request it
// has sent.
type DataAPI interface {
	Items(ctx context.Context, id youtube.PlaylistID) ([]youtube.Item, error)
	AppendItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID) (youtube.ItemID, error)
	DeleteItem(ctx context.Context, id youtube.ItemID) error
	Uploads(ctx context.Context, channel, token string, max int64) ([]youtube.Item, string, error)
	VideoDetails(ctx context.Context, ids []youtube.VideoID) ([]youtube.VideoDetails, error)
	TopComments(ctx context.Context, video youtube.VideoID) ([]youtube.Comment, error)
	Requests() int64
	Units() int64
}

// dataAPICalls holds the channel that every Data API route calls YouTube
// through. One call runs at a time, so what the channel counts during a call
// is that call's own.
type dataAPICalls struct {
	channel DataAPI
	mu      sync.Mutex
}

// CallDataAPIWith sets the channel that every Data API route calls YouTube
// through. Without it those routes answer 503.
func (h *Handlers) CallDataAPIWith(channel DataAPI) {
	h.dataAPI = &dataAPICalls{channel: channel}
}

// dataAPITimeout bounds one call: a read of every page of a playlist of 5,000
// items is 100 requests.
const dataAPITimeout = time.Minute

// errQuotaReserved is a call the day's quota cannot afford beside what the
// sync keeps back.
var errQuotaReserved = errors.New("the day's quota cannot afford the call beside what the sync keeps back")

// callDataAPI makes call, which spends about units, for the caller of r, once
// the day's quota has room for it beside what the sync keeps back. It records
// the requests and units the call spent against the caller, whether YouTube
// answered or not. The error is errQuotaReserved where the day cannot afford
// the call. A call runs to its end, or its timeout, when its caller leaves,
// so what it spent is recorded.
func callDataAPI[T any](h *Handlers, r *http.Request, method string, units int64, call func(context.Context, DataAPI) (T, error)) (T, error) {
	var none T
	calls := h.dataAPI
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if h.isDraining() {
		return none, errDraining
	}
	ctx, cancel := recordContext(r)
	room, err := h.quotaRoom(ctx, units)
	cancel()
	switch {
	case err != nil:
		return none, err
	case !room:
		return none, errQuotaReserved
	}
	requests, spent := calls.channel.Requests(), calls.channel.Units()
	sentAt := h.now()
	ctx, cancel = context.WithTimeout(context.WithoutCancel(r.Context()), dataAPITimeout)
	got, callErr := call(ctx, calls.channel)
	cancel()

	row := generated.InsertServiceCallParams{
		ClientID:  callerOf(r),
		Method:    method,
		SentTs:    store.Timestamp(sentAt),
		QuotaDate: youtube.QuotaDate(sentAt),
		Requests:  calls.channel.Requests() - requests,
		Units:     calls.channel.Units() - spent,
	}
	if callErr != nil {
		row.Error = sql.NullString{String: callErr.Error(), Valid: true}
	}
	ctx, cancel = recordContext(r)
	defer cancel()
	if err := h.store.InTx(ctx, func(tx *store.Tx) error { return tx.InsertServiceCall(ctx, row) }); err != nil {
		h.log.ErrorContext(r.Context(), "a Data API call was not recorded, so the day's quota counts it nowhere", "method", method, "units", row.Units, "err", err)
	}
	return got, callErr
}

// quotaRoom is whether the day's quota has room for units more beside what it
// has spent and what the sync keeps back.
func (h *Handlers) quotaRoom(ctx context.Context, units int64) (bool, error) {
	now := h.now()
	var room bool
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		spent, err := store.Spent(ctx, q, youtube.QuotaDate(now))
		if err != nil {
			return err
		}
		perTick, err := store.ReadsPerTick(ctx, q)
		if err != nil {
			return err
		}
		room = spent+units+store.Reserved(now, h.sync.Interval, perTick, false) <= youtube.DailyQuota
		return nil
	})
	return room, err
}

// callerOf is who the ledger records a call against: the client that received
// the token.
func callerOf(r *http.Request) string {
	identity, ok := goapiauth.IdentityFrom(r.Context())
	switch {
	case !ok:
		return "unidentified"
	case identity.ClientID != "":
		return identity.ClientID
	}
	return identity.Subject
}

// liveItem is one item of a playlist as YouTube holds it at the read. ItemID is
// what a delete of the item names.
type liveItem struct {
	ItemID       string  `json:"item_id"`
	VideoID      string  `json:"video_id"`
	Position     int64   `json:"position"`
	Title        string  `json:"title"`
	ChannelTitle *string `json:"channel_title"`
	Unavailable  bool    `json:"unavailable"`
}

func liveItemsFrom(items []youtube.Item) []liveItem {
	shown := make([]liveItem, len(items))
	for i, item := range items {
		shown[i] = liveItem{ItemID: string(item.ID), VideoID: string(item.VideoID), Position: item.Position, Title: item.Title, Unavailable: item.Unavailable}
		if item.ChannelTitle != "" {
			shown[i].ChannelTitle = &item.ChannelTitle
		}
	}
	return shown
}

// livePlaylist is the body of GET /api/v1/youtube/playlists/{id}/items.
type livePlaylist struct {
	PlaylistID string     `json:"playlist_id"`
	Items      []liveItem `json:"items"`
}

// addedItem is the body of POST /api/v1/youtube/playlists/{id}/items, and of
// its answer.
type addedItem struct {
	ItemID     string `json:"item_id,omitempty"`
	PlaylistID string `json:"playlist_id,omitempty"`
	VideoID    string `json:"video_id"`
}

// uploadsPage is one page of a channel's uploads, newest first. NextPageToken
// is what page_token takes for the page after, and null on the last.
type uploadsPage struct {
	Data          []liveItem `json:"data"`
	NextPageToken *string    `json:"next_page_token"`
}

// liveVideo is a video as YouTube reports its details at the read.
// LiveBroadcastContent is none, live or upcoming. Where the owner hides a
// count, it is 0.
type liveVideo struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	ChannelID            string `json:"channel_id"`
	ChannelTitle         string `json:"channel_title"`
	PublishedAt          string `json:"published_at"`
	Privacy              string `json:"privacy"`
	DurationSeconds      *int64 `json:"duration_seconds"`
	LiveBroadcastContent string `json:"live_broadcast_content"`
	ViewCount            uint64 `json:"view_count"`
	LikeCount            uint64 `json:"like_count"`
	CommentCount         uint64 `json:"comment_count"`
}

// videoComments is the body of GET /api/v1/youtube/videos/{id}/comments.
// Comments is empty and CommentsDisabled true for a video whose comments are
// off.
type videoComments struct {
	VideoID          string         `json:"video_id"`
	CommentsDisabled bool           `json:"comments_disabled"`
	Comments         []videoComment `json:"comments"`
}

type videoComment struct {
	ID              string  `json:"id"`
	Author          string  `json:"author"`
	AuthorChannelID *string `json:"author_channel_id"`
	Text            string  `json:"text"`
	LikeCount       int64   `json:"like_count"`
	ReplyCount      int64   `json:"reply_count"`
	PublishedAt     string  `json:"published_at"`
}

// youtubeID is the shape of an id YouTube gives a playlist, an item or a
// channel, or a channel's handle.
var youtubeID = regexp.MustCompile(`^@?[A-Za-z0-9_.-]+$`)

// uploadsPageSize is how many uploads a page answers: 50 when limit is absent,
// and at most 50, which is one request.
var uploadsPageSize = pageSize{fallback: 50, most: 50}

// maxVideoDetails is the most ids one read of video details names.
const maxVideoDetails = 500

// listLiveItems answers every item of the path's playlist, read from YouTube
// now. The path names a stored playlist by its id or its whole title or slug,
// or any other playlist by its YouTube id.
func (h *Handlers) listLiveItems(w http.ResponseWriter, r *http.Request) {
	id, ok := h.livePlaylistID(w, r)
	if !ok {
		return
	}
	items, err := callDataAPI(h, r, youtube.MethodPlaylistItemsList, 1, func(ctx context.Context, api DataAPI) ([]youtube.Item, error) {
		return api.Items(ctx, youtube.PlaylistID(id))
	})
	if h.refuseDataAPI(w, r, err, "playlist "+id) {
		return
	}
	wire.JSON(w, http.StatusOK, livePlaylist{PlaylistID: id, Items: liveItemsFrom(items)})
}

// addLiveItem appends the body's video to the path's playlist on YouTube, and
// answers the new item's id.
func (h *Handlers) addLiveItem(w http.ResponseWriter, r *http.Request) {
	id, ok := h.livePlaylistID(w, r)
	if !ok {
		return
	}
	body, ok := decodeJSON[addedItem](w, r, 1<<10, "a video to add")
	if !ok {
		return
	}
	if body.ItemID != "" || body.PlaylistID != "" {
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeInvalidBody, "the body names only video_id")
		return
	}
	if !videoID.MatchString(body.VideoID) {
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, []string{body.VideoID}, "%q is not a YouTube video id", body.VideoID)
		return
	}
	item, err := callDataAPI(h, r, youtube.MethodPlaylistItemsInsert, youtube.WriteUnits, func(ctx context.Context, api DataAPI) (youtube.ItemID, error) {
		return api.AppendItem(ctx, youtube.PlaylistID(id), youtube.VideoID(body.VideoID))
	})
	if h.refuseDataAPI(w, r, err, "playlist "+id) {
		return
	}
	wire.JSON(w, http.StatusCreated, addedItem{ItemID: string(item), PlaylistID: id, VideoID: body.VideoID})
}

// deleteLiveItem deletes the path's playlist item on YouTube. YouTube takes
// the item's id alone, so nothing checks the item against the path's
// playlist. An item already gone is answered as deleted.
func (h *Handlers) deleteLiveItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.livePlaylistID(w, r); !ok {
		return
	}
	item := r.PathValue("item")
	if !youtubeID.MatchString(item) {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "%q is not a playlist item id", item)
		return
	}
	_, err := callDataAPI(h, r, youtube.MethodPlaylistItemsDelete, youtube.WriteUnits, func(ctx context.Context, api DataAPI) (struct{}, error) {
		return struct{}{}, api.DeleteItem(ctx, youtube.ItemID(item))
	})
	if errors.Is(err, youtube.ErrItemNotFound) {
		err = nil
	}
	if h.refuseDataAPI(w, r, err, "playlist item "+item) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listUploads answers a page of a channel's uploads, newest first. The path
// names the channel by its id or its handle.
func (h *Handlers) listUploads(w http.ResponseWriter, r *http.Request) {
	if h.dataAPIUnavailable(w) {
		return
	}
	channel := r.PathValue("id")
	if !youtubeID.MatchString(channel) {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "%q is not a channel id or handle", channel)
		return
	}
	limit, ok := limitParam(w, r, uploadsPageSize)
	if !ok {
		return
	}
	type read struct {
		items []youtube.Item
		next  string
	}
	got, err := callDataAPI(h, r, youtube.MethodPlaylistItemsList, 2, func(ctx context.Context, api DataAPI) (read, error) {
		items, next, err := api.Uploads(ctx, channel, r.URL.Query().Get("page_token"), limit)
		return read{items: items, next: next}, err
	})
	if h.refuseDataAPI(w, r, err, "channel "+channel) {
		return
	}
	answer := uploadsPage{Data: liveItemsFrom(got.items)}
	if got.next != "" {
		answer.NextPageToken = &got.next
	}
	wire.JSON(w, http.StatusOK, answer)
}

// listLiveVideos answers the details of each video in ids, in the order
// YouTube returns them. YouTube leaves out a deleted video, a private one and
// an id that no video has.
func (h *Handlers) listLiveVideos(w http.ResponseWriter, r *http.Request) {
	if h.dataAPIUnavailable(w) {
		return
	}
	var ids []youtube.VideoID
	var invalid []string
	for id := range strings.SplitSeq(r.URL.Query().Get("ids"), ",") {
		switch {
		case id == "":
		case !videoID.MatchString(id):
			invalid = append(invalid, id)
		default:
			ids = append(ids, youtube.VideoID(id))
		}
	}
	switch {
	case len(invalid) > 0:
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, invalid, "these are not YouTube video ids: %q", invalid)
		return
	case len(ids) == 0:
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeVideoIDsRequired, "ids names no video, such as ids=abc,def")
		return
	case len(ids) > maxVideoDetails:
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeTooManyVideos, "ids names %d videos, and one read names at most %d", len(ids), maxVideoDetails)
		return
	}
	units := int64((len(ids) + 49) / 50)
	details, err := callDataAPI(h, r, youtube.MethodVideosList, units, func(ctx context.Context, api DataAPI) ([]youtube.VideoDetails, error) {
		return api.VideoDetails(ctx, ids)
	})
	if h.refuseDataAPI(w, r, err, "the videos") {
		return
	}
	shown := make([]liveVideo, len(details))
	for i, video := range details {
		shown[i] = liveVideo{
			ID:                   string(video.ID),
			Title:                video.Title,
			Description:          video.Description,
			ChannelID:            video.ChannelID,
			ChannelTitle:         video.ChannelTitle,
			PublishedAt:          video.PublishedAt,
			Privacy:              video.Privacy,
			DurationSeconds:      video.DurationSeconds,
			LiveBroadcastContent: video.LiveBroadcastContent,
			ViewCount:            video.ViewCount,
			LikeCount:            video.LikeCount,
			CommentCount:         video.CommentCount,
		}
	}
	wire.JSON(w, http.StatusOK, shown)
}

// listComments answers up to 100 of the video's top-level comments, those
// YouTube ranks most relevant first.
func (h *Handlers) listComments(w http.ResponseWriter, r *http.Request) {
	if h.dataAPIUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	if !videoID.MatchString(id) {
		wire.RefuseVideos(w, http.StatusUnprocessableEntity, wire.CodeInvalidVideoID, []string{id}, "%q is not a YouTube video id", id)
		return
	}
	comments, err := callDataAPI(h, r, youtube.MethodCommentThreadsList, 1, func(ctx context.Context, api DataAPI) ([]youtube.Comment, error) {
		return api.TopComments(ctx, youtube.VideoID(id))
	})
	answer := videoComments{VideoID: id, Comments: []videoComment{}}
	if errors.Is(err, youtube.ErrCommentsDisabled) {
		answer.CommentsDisabled, err = true, nil
	}
	if h.refuseDataAPI(w, r, err, "video "+id) {
		return
	}
	for _, comment := range comments {
		shown := videoComment{ID: comment.ID, Author: comment.Author, Text: comment.Text, LikeCount: comment.LikeCount, ReplyCount: comment.ReplyCount, PublishedAt: comment.PublishedAt}
		if comment.AuthorChannelID != "" {
			shown.AuthorChannelID = &comment.AuthorChannelID
		}
		answer.Comments = append(answer.Comments, shown)
	}
	wire.JSON(w, http.StatusOK, answer)
}

// livePlaylistID is the YouTube id of the path's playlist. Where the path
// names a stored playlist by its id or exactly by its title or slug, that is
// the stored one's id, and otherwise it is the path itself. ok is false once
// it has answered.
func (h *Handlers) livePlaylistID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.dataAPIUnavailable(w) {
		return "", false
	}
	ref := r.PathValue("id")
	ctx, cancel := recordContext(r)
	defer cancel()
	id, err := resolvePlaylist(ctx, h.store.Queries, "playlist", ref, exactly)
	var unknown referenceError
	switch {
	case err == nil:
		return id, true
	case errors.As(err, &unknown) && len(unknown.candidates) == 0 && youtubeID.MatchString(ref) && !strings.HasPrefix(ref, "@"):
		return ref, true
	}
	h.writeItemError(w, r, err, "playlist "+ref)
	return "", false
}

// dataAPIUnavailable answers 503 where the server has no channel to call the
// Data API through, and reports whether it answered.
func (h *Handlers) dataAPIUnavailable(w http.ResponseWriter) bool {
	if h.dataAPI != nil {
		return false
	}
	wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeYouTubeReadsUnavailable, "this server calls no Data API for its clients")
	return true
}

// refuseDataAPI answers a Data API call that failed with err, naming what the
// call was of, and reports whether it answered.
func (h *Handlers) refuseDataAPI(w http.ResponseWriter, r *http.Request, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errQuotaReserved):
		now := h.now()
		retryAfter(w, youtube.QuotaReset(now).Sub(now))
		h.refuseAndLog(w, r, slog.LevelWarn, http.StatusServiceUnavailable, wire.CodeYouTubeQuotaSpent, err,
			"the day's quota cannot afford this call beside what the sync keeps back, and it resets at midnight Pacific")
	case errors.Is(err, youtube.ErrQuotaSpent):
		h.refuseSpentQuota(w, r, err)
	case errors.Is(err, errDraining):
		h.refuseAndLog(w, r, slog.LevelInfo, http.StatusServiceUnavailable, wire.CodeShuttingDown, err,
			"the server is shutting down, so it made no call to YouTube; send the request again")
	case errors.Is(err, youtube.ErrPlaylistNotFound), errors.Is(err, youtube.ErrChannelNotFound), errors.Is(err, youtube.ErrVideoNotFound):
		wire.Refuse(w, http.StatusNotFound, wire.CodeNotFound, "%s not found: YouTube has no record of it", what)
	case errors.Is(err, youtube.ErrRefused):
		message, _ := youtube.RefusalMessage(err)
		wire.Refuse(w, http.StatusUnprocessableEntity, wire.CodeYouTubeRefused, "YouTube refused the call: %s", message)
	case r.Method != http.MethodGet:
		h.refuseAndLog(w, r, slog.LevelError, http.StatusBadGateway, wire.CodeYouTubeWriteFailed, err,
			"YouTube did not confirm the write to %s, which may still have landed", what)
	default:
		h.refuseAndLog(w, r, slog.LevelWarn, http.StatusBadGateway, wire.CodeYouTubeReadFailed, err, "YouTube did not answer the read of %s", what)
	}
	return true
}
