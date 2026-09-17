package api

import "context"

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
