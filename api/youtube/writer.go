package youtube

import (
	"context"
	"fmt"

	ytapi "google.golang.org/api/youtube/v3"
)

// CreatePlaylist creates a private playlist titled title and described by
// description, and returns it as YouTube reports it.
//
// YouTube aborts an insert into a playlist created under a second before, and
// send retries it, so the first item added to a new playlist can take a second.
func (c *Channel) CreatePlaylist(ctx context.Context, title, description string) (Playlist, error) {
	call := c.service.Playlists.Insert([]string{"snippet", "status"}, &ytapi.Playlist{
		Snippet: &ytapi.PlaylistSnippet{Title: title, Description: description},
		Status:  &ytapi.PlaylistStatus{PrivacyStatus: "private"},
	})
	resource, err := send(ctx, c, playlistsInsert, call.Context(ctx).Do)
	if err != nil {
		return Playlist{}, fmt.Errorf("create playlist %q: %w", title, err)
	}
	return playlistFrom(resource)
}

// UpdatePlaylist sets the title and description of the playlist playlist.ID to
// playlist's, and leaves its privacy as it is. YouTube replaces the whole
// snippet, so it clears a description the request leaves out, and this sends
// both.
//
// YouTube accepts an update to a playlist it has deleted, so success does not
// show that the playlist exists.
func (c *Channel) UpdatePlaylist(ctx context.Context, playlist Playlist) error {
	call := c.service.Playlists.Update([]string{"snippet"}, &ytapi.Playlist{
		Id:      playlist.ID,
		Snippet: &ytapi.PlaylistSnippet{Title: playlist.Title, Description: playlist.Description},
	})
	if _, err := send(ctx, c, playlistsUpdate, call.Context(ctx).Do); err != nil {
		return fmt.Errorf("update playlist %s: %w", playlist.ID, err)
	}
	return nil
}

// DeletePlaylist deletes the playlist playlistID and every item in it. It
// returns ErrPlaylistNotFound when no playlist has that id.
func (c *Channel) DeletePlaylist(ctx context.Context, playlistID string) error {
	call := c.service.Playlists.Delete(playlistID)
	if _, err := send(ctx, c, playlistsDelete, noContent(call.Context(ctx).Do)); err != nil {
		return fmt.Errorf("delete playlist %s: %w", playlistID, err)
	}
	return nil
}

// InsertItem adds the video videoID to the playlist playlistID at position, and
// returns the new item. A position from 0 to the playlist's item count is
// accepted, and the item count appends. A video already in the playlist gets a
// second item.
//
// It returns ErrPlaylistNotFound when no playlist has that id, ErrVideoNotFound
// when no video does, and ErrVideoRefused when YouTube will not add the video.
func (c *Channel) InsertItem(ctx context.Context, playlistID, videoID string, position int64) (Item, error) {
	call := c.service.PlaylistItems.Insert([]string{"snippet", "status"}, &ytapi.PlaylistItem{
		Snippet: &ytapi.PlaylistItemSnippet{
			PlaylistId: playlistID,
			ResourceId: &ytapi.ResourceId{Kind: "youtube#video", VideoId: videoID},
			Position:   position,
			// YouTube appends an item whose position is left out, so position 0
			// has to be sent.
			ForceSendFields: []string{"Position"},
		},
	})
	resource, err := send(ctx, c, playlistItemsInsert, call.Context(ctx).Do)
	if err != nil {
		return Item{}, fmt.Errorf("insert video %s into playlist %s: %w", videoID, playlistID, err)
	}
	return itemFrom(resource)
}

// MoveItem moves item to position in its playlist. A position from 0 to one
// less than the playlist's item count is accepted.
func (c *Channel) MoveItem(ctx context.Context, item Item, position int64) error {
	call := c.service.PlaylistItems.Update([]string{"snippet"}, &ytapi.PlaylistItem{
		Id: item.ID,
		Snippet: &ytapi.PlaylistItemSnippet{
			PlaylistId:      item.PlaylistID,
			ResourceId:      &ytapi.ResourceId{Kind: "youtube#video", VideoId: item.VideoID},
			Position:        position,
			ForceSendFields: []string{"Position"},
		},
	})
	if _, err := send(ctx, c, playlistItemsUpdate, call.Context(ctx).Do); err != nil {
		return fmt.Errorf("move item %s to position %d: %w", item.ID, position, err)
	}
	return nil
}

// DeleteItem deletes the playlist item itemID. It returns ErrItemNotFound when
// no item has that id.
func (c *Channel) DeleteItem(ctx context.Context, itemID string) error {
	call := c.service.PlaylistItems.Delete(itemID)
	if _, err := send(ctx, c, playlistItemsDelete, noContent(call.Context(ctx).Do)); err != nil {
		return fmt.Errorf("delete item %s: %w", itemID, err)
	}
	return nil
}
