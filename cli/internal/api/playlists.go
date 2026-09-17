package api

import (
	"context"
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

// ListPlaylists is every playlist, by title. The collection is not paged, so
// this is all of them.
func (c *Client) ListPlaylists(ctx context.Context) ([]PlaylistSummary, error) {
	playlists := []PlaylistSummary{}
	err := c.Get(ctx, "/api/v1/playlists", &playlists)
	return playlists, err
}

// GetPlaylist is the playlist named by name: its YouTube id, or its title.
func (c *Client) GetPlaylist(ctx context.Context, name string) (Playlist, error) {
	var playlist Playlist
	err := c.Get(ctx, "/api/v1/playlists/"+ref(name), &playlist)
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
func (c *Client) CreatePlaylist(ctx context.Context, title string) (PlaylistSummary, error) {
	var created PlaylistSummary
	err := c.Post(ctx, "/api/v1/playlists", playlistDetails{Title: title}, &created)
	return created, err
}

// RenamePlaylist retitles the playlist named by name, and answers the playlist
// as it stands afterwards.
func (c *Client) RenamePlaylist(ctx context.Context, name, title string) (PlaylistSummary, error) {
	var renamed PlaylistSummary
	err := c.Patch(ctx, "/api/v1/playlists/"+ref(name), playlistDetails{Title: title}, &renamed)
	return renamed, err
}

// DeletePlaylist deletes the playlist named by name, on YouTube and from the
// server.
func (c *Client) DeletePlaylist(ctx context.Context, name string) error {
	return c.Delete(ctx, "/api/v1/playlists/"+ref(name))
}

// Order is a playlist's order: one video id a slot, in the order it plays, with
// the same video allowed in more than one.
type Order struct {
	VideoIDs []string `json:"video_ids"`
	// Revision is which order this is, as the server's ETag names it. It is a
	// header rather than a field, so it is neither decoded from a body nor sent
	// in one.
	Revision string `json:"-"`
}

// PlaylistOrder is the order of the playlist named by name, with the revision an
// edit of it names.
func (c *Client) PlaylistOrder(ctx context.Context, name string) (Order, error) {
	var order Order
	header, err := c.send(ctx, http.MethodGet, orderPath(name), nil, nil, &order)
	if err != nil {
		return Order{}, err
	}
	// An edit says which order it edited, so an answer carrying no revision is
	// one nothing can be edited from. Saying so here names the cause, where the
	// edit sent without one is refused for a precondition the caller never
	// chose to leave out.
	if order.Revision = header.Get("ETag"); order.Revision == "" {
		return Order{}, fmt.Errorf("the server sent the order of %s without an ETag, and an edit names the order it edits", name)
	}
	return order, nil
}

// ReplacePlaylistOrder sets the order of the playlist named by name to videoIDs,
// as long as its order is still the one revision names. The server pushes the
// new order to YouTube on its next sync run.
func (c *Client) ReplacePlaylistOrder(ctx context.Context, name, revision string, videoIDs []string) (Order, error) {
	var replaced Order
	header := http.Header{"If-Match": []string{revision}}
	answered, err := c.send(ctx, http.MethodPut, orderPath(name), header, Order{VideoIDs: videoIDs}, &replaced)
	if err != nil {
		return Order{}, err
	}
	replaced.Revision = answered.Get("ETag")
	return replaced, nil
}

func orderPath(name string) string { return "/api/v1/playlists/" + ref(name) + "/items" }
