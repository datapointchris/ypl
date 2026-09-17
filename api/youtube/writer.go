package youtube

import (
	"context"
	"fmt"

	ytapi "google.golang.org/api/youtube/v3"
)

// PlaylistDetails is what a create or an update sets on a playlist.
//
// YouTube trims the whitespace around a title and a description before it
// stores them. It refuses a title longer than MaxTitleLength, or a description
// longer than MaxDescriptionLength, once trimmed, with invalidPlaylistSnippet.
// It refused "a < b > c" as a title and as a description the same way, and
// accepted "a < b" and "b > a" as titles.
type PlaylistDetails struct {
	Title       string
	Description string
}

// MaxTitleLength and MaxDescriptionLength are the most code points YouTube
// stores in a playlist's title and description. A title of 150 characters
// was accepted and one of 151 refused, as was a title of 76 letters each
// carrying a combining accent. A description of 5,000 characters was accepted
// and one of 5,001 refused.
const (
	MaxTitleLength       = 150
	MaxDescriptionLength = 5000
)

// MaxPlaylistItems is the most videos YouTube's documentation lets a playlist
// hold.
const MaxPlaylistItems = 5000

// CreatePlaylist creates a private playlist with details, and returns it as
// YouTube's answer reports it.
//
// An answer that lacks the new playlist's id, snippet or status is
// ErrUnexpectedResponse, although YouTube created the playlist.
func (c *Channel) CreatePlaylist(ctx context.Context, details PlaylistDetails) (Playlist, error) {
	call := c.service.Playlists.Insert([]string{"snippet", "status"}, &ytapi.Playlist{
		Snippet: &ytapi.PlaylistSnippet{Title: details.Title, Description: details.Description},
		Status:  &ytapi.PlaylistStatus{PrivacyStatus: "private"},
	})
	resource, err := send(ctx, c, playlistsInsert, call.Context(ctx).Do)
	if err != nil {
		return Playlist{}, fmt.Errorf("create playlist %q: %w", details.Title, err)
	}
	if resource.Id == "" {
		return Playlist{}, fmt.Errorf("%w: the created playlist %q came back with no id", ErrUnexpectedResponse, details.Title)
	}
	created, err := playlistFrom(resource)
	if err != nil {
		return Playlist{}, fmt.Errorf("create playlist %q: %w", details.Title, err)
	}
	return created, nil
}

// UpdatePlaylist sets the title and description of the playlist id, leaves its
// privacy as it is, and returns the details as YouTube's answer reports them.
// YouTube replaces the whole snippet and clears a description the request
// leaves out, so an empty description is sent as empty.
//
// YouTube accepts an update to a playlist it has deleted, so success does not
// show that the playlist exists. An answer that lacks the snippet is
// ErrUnexpectedResponse, although YouTube made the update.
func (c *Channel) UpdatePlaylist(ctx context.Context, id PlaylistID, details PlaylistDetails) (PlaylistDetails, error) {
	call := c.service.Playlists.Update([]string{"snippet"}, &ytapi.Playlist{
		Id: string(id),
		Snippet: &ytapi.PlaylistSnippet{
			Title:           details.Title,
			Description:     details.Description,
			ForceSendFields: []string{"Description"},
		},
	})
	resource, err := send(ctx, c, playlistsUpdate, call.Context(ctx).Do)
	if err != nil {
		return PlaylistDetails{}, fmt.Errorf("update playlist %s: %w", id, err)
	}
	if resource.Snippet == nil {
		return PlaylistDetails{}, fmt.Errorf("%w: the update to playlist %s came back with no snippet", ErrUnexpectedResponse, id)
	}
	return PlaylistDetails{Title: resource.Snippet.Title, Description: resource.Snippet.Description}, nil
}

// DeletePlaylist deletes the playlist id and every item in it. It returns
// ErrPlaylistNotFound when no playlist has that id.
func (c *Channel) DeletePlaylist(ctx context.Context, id PlaylistID) error {
	call := c.service.Playlists.Delete(string(id))
	if _, err := send(ctx, c, playlistsDelete, noContent(call.Context(ctx).Do)); err != nil {
		return fmt.Errorf("delete playlist %s: %w", id, err)
	}
	return nil
}

// InsertItem adds the video to the playlist at position, and returns the new
// item's id. A position from 0 to the playlist's item count is accepted on a
// playlist sorted manually, and the item count puts the item last. A video
// already in the playlist gets a second item.
//
// YouTube aborts an insert sent right after the playlist was created. An aborted
// insert is sent again after pauses of 1, 2 and 4 seconds.
//
// It returns ErrPlaylistNotFound when no playlist has that id, ErrVideoNotFound
// when no video does, ErrVideoRefused when YouTube will not add the video, and
// ErrManualSortRequired for a playlist not sorted manually.
func (c *Channel) InsertItem(ctx context.Context, playlist PlaylistID, video VideoID, position int64) (ItemID, error) {
	snippet := itemSnippet(playlist, video)
	snippet.Position = position
	// YouTube appends an insert whose position is left out, so position 0 has
	// to be sent.
	snippet.ForceSendFields = []string{"Position"}
	return c.insertItem(ctx, snippet)
}

// AppendItem adds the video to the end of the playlist, and returns the new
// item's id. It names no position, so a playlist sorted by something other than
// its manual order accepts it. It retries an abort and returns the refusals
// InsertItem does, except ErrManualSortRequired.
func (c *Channel) AppendItem(ctx context.Context, playlist PlaylistID, video VideoID) (ItemID, error) {
	return c.insertItem(ctx, itemSnippet(playlist, video))
}

func (c *Channel) insertItem(ctx context.Context, snippet *ytapi.PlaylistItemSnippet) (ItemID, error) {
	// Only the id is read from YouTube's answer, so an item that was added is
	// never reported as refused.
	call := c.service.PlaylistItems.Insert([]string{"snippet", "status"}, &ytapi.PlaylistItem{Snippet: snippet})
	resource, err := send(ctx, c, playlistItemsInsert, call.Context(ctx).Do)
	if err != nil {
		return "", fmt.Errorf("insert video %s into playlist %s: %w", snippet.ResourceId.VideoId, snippet.PlaylistId, err)
	}
	if resource.Id == "" {
		return "", fmt.Errorf("%w: the item added to playlist %s came back with no id", ErrUnexpectedResponse, snippet.PlaylistId)
	}
	return ItemID(resource.Id), nil
}

// MoveItem moves item to position in its playlist. A position from 0 to one
// less than the playlist's item count is accepted on a playlist sorted manually,
// and the item leaves its slot and lands at exactly that position. It returns
// ErrItemNotFound for an item already deleted, and ErrManualSortRequired for a
// playlist not sorted manually.
func (c *Channel) MoveItem(ctx context.Context, item Item, position int64) error {
	snippet := itemSnippet(item.PlaylistID, item.VideoID)
	snippet.Position = position
	snippet.ForceSendFields = []string{"Position"}
	call := c.service.PlaylistItems.Update([]string{"snippet"}, &ytapi.PlaylistItem{Id: string(item.ID), Snippet: snippet})
	if _, err := send(ctx, c, playlistItemsUpdate, call.Context(ctx).Do); err != nil {
		return fmt.Errorf("move item %s to position %d: %w", item.ID, position, err)
	}
	return nil
}

// DeleteItem deletes the playlist item id. It returns ErrItemNotFound when no
// item has that id.
func (c *Channel) DeleteItem(ctx context.Context, id ItemID) error {
	call := c.service.PlaylistItems.Delete(string(id))
	if _, err := send(ctx, c, playlistItemsDelete, noContent(call.Context(ctx).Do)); err != nil {
		return fmt.Errorf("delete item %s: %w", id, err)
	}
	return nil
}

// itemSnippet is the snippet naming the video in the playlist.
func itemSnippet(playlist PlaylistID, video VideoID) *ytapi.PlaylistItemSnippet {
	return &ytapi.PlaylistItemSnippet{
		PlaylistId: string(playlist),
		ResourceId: &ytapi.ResourceId{Kind: "youtube#video", VideoId: string(video)},
	}
}
