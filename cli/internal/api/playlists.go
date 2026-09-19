package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// PlaylistSummary is a playlist as the collection lists it, without its items.
type PlaylistSummary struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	Privacy          string `json:"privacy"`
	ItemCount        int64  `json:"item_count"`
	UnavailableCount int64  `json:"unavailable_count"`
	EnrichedCount    int64  `json:"enriched_count"`
}

// Playlist is one playlist with its items, in the order the server holds them.
type Playlist struct {
	PlaylistSummary
	Items []PlaylistItem `json:"items"`
}

// PlaylistItem is one slot of a playlist's order. ID is the YouTube item
// holding it, and null until the sync has put it there.
type PlaylistItem struct {
	ID       *string      `json:"id"`
	Position int64        `json:"position"`
	Video    VideoSummary `json:"video"`
}

// ItemID is the YouTube item holding this slot, and "" for a slot the sync has
// not pushed yet.
func (item PlaylistItem) ItemID() string {
	if item.ID == nil {
		return ""
	}
	return *item.ID
}

// ListPlaylists is every playlist, by title, read whole or a page at a time as
// the server answers.
func (c *Client) ListPlaylists(ctx context.Context) ([]PlaylistSummary, error) {
	return all(ctx, c, "/api/v1/playlists", nil, func(playlist PlaylistSummary) string { return playlist.ID })
}

// Reference names a playlist to the server: its YouTube id, its title, or that
// title with case, spacing and punctuation removed. PlaylistTitle is what a
// playlist is called.
//
// They are separate types because a rename takes one of each, adjacent, and
// nothing at the call site says which slot is which. Transposed, it renames the
// playlist named by the new title to the old one, which is a YouTube write
// nothing here can undo.
type (
	Reference     string
	PlaylistTitle string
)

// Revision is which order an edit edits, as the server's ETag names it.
type Revision string

// GetPlaylist is the playlist name reaches. Part of a title reaches one here.
func (c *Client) GetPlaylist(ctx context.Context, name Reference) (Playlist, error) {
	var playlist Playlist
	err := c.Get(ctx, "/api/v1/playlists/"+ref(string(name)), &playlist)
	return playlist, err
}

// playlistDetails is the body of a create and of a rename. The server leaves a
// field the body leaves out as it holds it, and both verbs here set the title
// and nothing else — no part of ypl has ever written a playlist description.
type playlistDetails struct {
	Title string `json:"title"`
}

// CreatePlaylist makes a playlist on YouTube and stores it, answering it as the
// collection lists it.
func (c *Client) CreatePlaylist(ctx context.Context, title PlaylistTitle) (PlaylistSummary, error) {
	return post[PlaylistSummary](ctx, c, "/api/v1/playlists", playlistDetails{Title: string(title)})
}

// RenamePlaylist retitles the playlist name reaches, and answers the playlist
// as it stands afterwards. Part of a title does not reach one.
func (c *Client) RenamePlaylist(ctx context.Context, name Reference, title PlaylistTitle) (PlaylistSummary, error) {
	return patch[PlaylistSummary](ctx, c, "/api/v1/playlists/"+ref(string(name)), playlistDetails{Title: string(title)})
}

// DeletePlaylist deletes the playlist name reaches, on YouTube and from the
// server. Part of a title does not reach one.
func (c *Client) DeletePlaylist(ctx context.Context, name Reference) error {
	return c.Delete(ctx, "/api/v1/playlists/"+ref(string(name)))
}

// Order is a playlist's order: one video id a slot, in the order it plays, with
// the same video allowed in more than one.
type Order struct {
	VideoIDs []string `json:"video_ids"`
	// Revision is which order this is, as the server's ETag names it. It is a
	// header rather than a field, so it is neither decoded from a body nor sent
	// in one.
	Revision Revision `json:"-"`
}

// ErrNoRevision is an order answered without an ETag. An edit says which order
// it edited, so there is nothing to edit that answer from. It is a value rather
// than a sentence because the only caller that can act on it has to recognize
// it rather than read it.
var ErrNoRevision = errors.New("the server sent an order without an ETag, and an edit names the order it edits")

// PlaylistItems is the order of the playlist name reaches. Part of a title does
// not reach one.
//
// The server resolves this read as narrowly as it resolves a change, which is
// what makes it the answer to whether a reference reaches a playlist precisely
// enough to change it. A verb that asks before it destroys reads it first,
// because a question about a playlist the verb cannot reach is one whose answer
// it cannot act on.
func (c *Client) PlaylistItems(ctx context.Context, name Reference) (Order, error) {
	var order Order
	_, err := c.send(ctx, http.MethodGet, orderPath(name), nil, nil, &order)
	return order, err
}

// PlaylistOrder is PlaylistItems with the revision an edit of it names.
func (c *Client) PlaylistOrder(ctx context.Context, name Reference) (Order, error) {
	var order Order
	header, err := c.send(ctx, http.MethodGet, orderPath(name), nil, nil, &order)
	if err != nil {
		return Order{}, err
	}
	// Named here rather than left to the edit, where a request sent without a
	// revision is refused for a precondition the caller never chose to leave
	// out.
	if order.Revision = Revision(header.Get("ETag")); order.Revision == "" {
		return Order{}, fmt.Errorf("%w: %s", ErrNoRevision, name)
	}
	return order, nil
}

// ReplacePlaylistOrder sets the order of the playlist name reaches to videoIDs,
// as long as its order is still the one revision names. Part of a title does
// not reach one. The server pushes the new order to YouTube on its next sync
// run.
func (c *Client) ReplacePlaylistOrder(ctx context.Context, name Reference, revision Revision, videoIDs []string) (Order, error) {
	var replaced Order
	header := http.Header{"If-Match": []string{string(revision)}}
	answered, err := c.send(ctx, http.MethodPut, orderPath(name), header, Order{VideoIDs: videoIDs}, &replaced)
	if err != nil {
		return Order{}, err
	}
	replaced.Revision = Revision(answered.Get("ETag"))
	return replaced, nil
}

func orderPath(name Reference) string { return "/api/v1/playlists/" + ref(string(name)) + "/items" }
