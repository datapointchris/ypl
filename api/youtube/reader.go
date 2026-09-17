package youtube

import (
	"context"
	"fmt"
	"sort"

	ytapi "google.golang.org/api/youtube/v3"
)

// pageSize is the most items one list request returns. A request that names no
// maxResults gets 5, and one that names more than 50 gets 50.
const pageSize = 50

// PlaylistID is YouTube's id for a playlist.
type PlaylistID string

// ItemID is YouTube's id for one slot in a playlist, which every write to the
// slot names.
type ItemID string

// VideoID is YouTube's id for a video.
type VideoID string

// Playlist is one playlist the channel owns, as YouTube reports it.
type Playlist struct {
	ID          PlaylistID
	Title       string
	Description string
	Privacy     string
}

// Item is one slot in a playlist.
type Item struct {
	ID         ItemID
	PlaylistID PlaylistID
	VideoID    VideoID
	Position   int64
	Title      string
	// ChannelTitle is the video owner's channel, and is empty for an
	// unavailable video.
	ChannelTitle string
	// Unavailable is true for a video that is private or deleted.
	Unavailable bool
}

// Playlists is every playlist the channel owns, in the order YouTube lists
// them.
func (c *Channel) Playlists(ctx context.Context) ([]Playlist, error) {
	call := c.service.Playlists.List([]string{"snippet", "status"}).Mine(true).MaxResults(pageSize)
	// The total a playlists page reports counts more playlists than the list
	// returns, so it is compared across pages and never with the length.
	playlists, _, err := readPages(func(token string) (page[Playlist], error) {
		response, err := send(ctx, c, playlistsList, call.PageToken(token).Context(ctx).Do)
		if err != nil {
			return page[Playlist]{}, err
		}
		if response.PageInfo == nil {
			return page[Playlist]{}, fmt.Errorf("%w: a playlists page has no pageInfo", ErrUnexpectedResponse)
		}
		p := page[Playlist]{total: response.PageInfo.TotalResults, next: response.NextPageToken}
		for _, resource := range response.Items {
			playlist, err := playlistFrom(resource)
			if err != nil {
				return page[Playlist]{}, err
			}
			p.items = append(p.items, playlist)
		}
		return p, nil
	}, func(p Playlist) string { return string(p.ID) })
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	return playlists, nil
}

// Playlist is the playlist id, read by its id. It returns ErrPlaylistNotFound
// when YouTube has no playlist with that id, which a read by id answers with no
// playlists rather than a refusal.
func (c *Channel) Playlist(ctx context.Context, id PlaylistID) (Playlist, error) {
	call := c.service.Playlists.List([]string{"snippet", "status"}).Id(string(id)).MaxResults(pageSize)
	response, err := send(ctx, c, playlistsList, call.Context(ctx).Do)
	if err != nil {
		return Playlist{}, fmt.Errorf("read playlist %s: %w", id, err)
	}
	switch {
	case len(response.Items) == 0:
		return Playlist{}, fmt.Errorf("read playlist %s: %w", id, ErrPlaylistNotFound)
	case len(response.Items) > 1 || response.Items[0].Id != string(id):
		return Playlist{}, fmt.Errorf("%w: a read of playlist %s returned %d playlists, the first %s", ErrUnexpectedResponse, id, len(response.Items), response.Items[0].Id)
	}
	return playlistFrom(response.Items[0])
}

// Items is every item in the playlist playlistID, ordered by position. Beyond
// the checks every paged read gets, it returns ErrInconsistentRead when the
// number of items differs from the total the pages report, or the positions are
// not exactly 0 through n-1.
func (c *Channel) Items(ctx context.Context, playlistID PlaylistID) ([]Item, error) {
	call := c.service.PlaylistItems.List([]string{"snippet", "status"}).PlaylistId(string(playlistID)).MaxResults(pageSize)
	items, total, err := readPages(func(token string) (page[Item], error) {
		response, err := send(ctx, c, playlistItemsList, call.PageToken(token).Context(ctx).Do)
		if err != nil {
			return page[Item]{}, err
		}
		if response.PageInfo == nil {
			return page[Item]{}, fmt.Errorf("%w: an items page has no pageInfo", ErrUnexpectedResponse)
		}
		p := page[Item]{total: response.PageInfo.TotalResults, next: response.NextPageToken}
		for _, resource := range response.Items {
			item, err := itemFrom(resource)
			if err != nil {
				return page[Item]{}, err
			}
			p.items = append(p.items, item)
		}
		return p, nil
	}, func(it Item) string { return string(it.ID) })
	if err != nil {
		return nil, fmt.Errorf("list items of playlist %s: %w", playlistID, err)
	}

	if int64(len(items)) != total {
		return nil, fmt.Errorf("%w: %s read %d items and reports %d", ErrInconsistentRead, playlistID, len(items), total)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Position < items[j].Position })
	for i, it := range items {
		if it.Position != int64(i) {
			return nil, fmt.Errorf("%w: %s has position %d where %d belongs", ErrInconsistentRead, playlistID, it.Position, i)
		}
	}
	return items, nil
}

func playlistFrom(resource *ytapi.Playlist) (Playlist, error) {
	if resource.Snippet == nil || resource.Status == nil {
		return Playlist{}, fmt.Errorf("%w: playlist %s lacks its snippet or status", ErrUnexpectedResponse, resource.Id)
	}
	switch resource.Status.PrivacyStatus {
	case "public", "unlisted", "private":
	default:
		return Playlist{}, fmt.Errorf("%w: playlist %s has privacy status %q", ErrUnexpectedResponse, resource.Id, resource.Status.PrivacyStatus)
	}
	return Playlist{
		ID:          PlaylistID(resource.Id),
		Title:       resource.Snippet.Title,
		Description: resource.Snippet.Description,
		Privacy:     resource.Status.PrivacyStatus,
	}, nil
}

func itemFrom(resource *ytapi.PlaylistItem) (Item, error) {
	if resource.Snippet == nil || resource.Snippet.ResourceId == nil || resource.Status == nil {
		return Item{}, fmt.Errorf("%w: item %s lacks its snippet, resource id or status", ErrUnexpectedResponse, resource.Id)
	}
	it := Item{
		ID:           ItemID(resource.Id),
		PlaylistID:   PlaylistID(resource.Snippet.PlaylistId),
		VideoID:      VideoID(resource.Snippet.ResourceId.VideoId),
		Position:     resource.Snippet.Position,
		Title:        resource.Snippet.Title,
		ChannelTitle: resource.Snippet.VideoOwnerChannelTitle,
	}
	switch resource.Status.PrivacyStatus {
	case "public", "unlisted":
	case "private", "privacyStatusUnspecified":
		// A deleted video's item reports privacyStatusUnspecified.
		it.Unavailable = true
	default:
		return Item{}, fmt.Errorf("%w: item %s has privacy status %q", ErrUnexpectedResponse, resource.Id, resource.Status.PrivacyStatus)
	}
	return it, nil
}

// page is what one list response contributes to a paged read.
type page[T any] struct {
	items []T
	total int64
	next  string
}

// readPages calls list with each page token in turn, starting from none, until
// a page has no next token. It returns the items of every page and the total
// the first page reported, and ErrInconsistentRead when a later page reports a
// different total or an id appears twice.
func readPages[T any](list func(token string) (page[T], error), id func(T) string) ([]T, int64, error) {
	var items []T
	var total int64
	seen := map[string]bool{}
	token := ""
	for first := true; ; first = false {
		p, err := list(token)
		if err != nil {
			return nil, 0, err
		}
		if first {
			total = p.total
		} else if p.total != total {
			return nil, 0, fmt.Errorf("%w: a page reports %d in total where the first reported %d", ErrInconsistentRead, p.total, total)
		}
		for _, item := range p.items {
			key := id(item)
			if seen[key] {
				return nil, 0, fmt.Errorf("%w: %s appears twice", ErrInconsistentRead, key)
			}
			seen[key] = true
			items = append(items, item)
		}
		if p.next == "" {
			return items, total, nil
		}
		token = p.next
	}
}
