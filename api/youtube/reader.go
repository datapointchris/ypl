package youtube

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

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

// Playlist is one playlist the channel owns, as YouTube reports it. ItemCount
// is YouTube's own count of its items, which a read costs one unit for every
// playlist at once rather than one a page of each.
type Playlist struct {
	ID          PlaylistID
	Title       string
	Description string
	Privacy     string
	ItemCount   int64
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
	call := c.service.Playlists.List(playlistParts).Mine(true).MaxResults(pageSize)
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
			playlist, err := readPlaylistFrom(resource)
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

// Video is a video as a read of it by id reports it. DurationSeconds is nil
// where YouTube reports no length, as it does for a live stream.
type Video struct {
	ID              VideoID
	Title           string
	ChannelTitle    string
	Privacy         string
	DurationSeconds *int64
}

// isoDuration is a video's length as YouTube writes it, an ISO 8601 duration:
// P, then days, then T and hours, minutes and seconds, each part optional.
var isoDuration = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// lengthOf is the seconds iso names, and nil where it names none: a zero,
// which YouTube reports for a live stream, or text of another shape.
func lengthOf(iso string) *int64 {
	parts := isoDuration.FindStringSubmatch(iso)
	if parts == nil {
		return nil
	}
	var total int64
	for i, unit := range []int64{24 * 60 * 60, 60 * 60, 60, 1} {
		if parts[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return nil
		}
		total += n * unit
	}
	if total == 0 {
		return nil
	}
	return &total
}

// Videos is each of ids that YouTube returns a video for, read 50 ids a
// request. YouTube leaves out an id no video has, a deleted video, and a
// private video another channel owns, and refuses none of them.
func (c *Channel) Videos(ctx context.Context, ids []VideoID) ([]Video, error) {
	var videos []Video
	for chunk := range slices.Chunk(ids, pageSize) {
		names := make([]string, len(chunk))
		for i, id := range chunk {
			names[i] = string(id)
		}
		call := c.service.Videos.List([]string{"snippet", "status", "contentDetails"}).Id(strings.Join(names, ","))
		response, err := send(ctx, c, videosList, call.Context(ctx).Do)
		if err != nil {
			return nil, fmt.Errorf("read videos by id: %w", err)
		}
		for _, resource := range response.Items {
			if resource.Snippet == nil || resource.Status == nil {
				return nil, fmt.Errorf("%w: video %s lacks its snippet or status", ErrUnexpectedResponse, resource.Id)
			}
			switch resource.Status.PrivacyStatus {
			case "public", "unlisted", "private":
			default:
				return nil, fmt.Errorf("%w: video %s has privacy status %q", ErrUnexpectedResponse, resource.Id, resource.Status.PrivacyStatus)
			}
			if !slices.Contains(chunk, VideoID(resource.Id)) {
				return nil, fmt.Errorf("%w: a read of videos by id returned %s, which it did not name", ErrUnexpectedResponse, resource.Id)
			}
			video := Video{
				ID:           VideoID(resource.Id),
				Title:        resource.Snippet.Title,
				ChannelTitle: resource.Snippet.ChannelTitle,
				Privacy:      resource.Status.PrivacyStatus,
			}
			if resource.ContentDetails != nil {
				video.DurationSeconds = lengthOf(resource.ContentDetails.Duration)
			}
			videos = append(videos, video)
		}
	}
	return videos, nil
}

// ExistingItems is each of ids that YouTube still has a playlist item for, read
// 50 ids a request, which is how an item absent from Items is confirmed gone.
// YouTube leaves out an id it has no item for.
func (c *Channel) ExistingItems(ctx context.Context, ids []ItemID) ([]ItemID, error) {
	var found []ItemID
	for chunk := range slices.Chunk(ids, pageSize) {
		names := make([]string, len(chunk))
		for i, id := range chunk {
			names[i] = string(id)
		}
		call := c.service.PlaylistItems.List([]string{"id"}).Id(strings.Join(names, ",")).MaxResults(pageSize)
		response, err := send(ctx, c, playlistItemsList, call.Context(ctx).Do)
		if err != nil {
			return nil, fmt.Errorf("read playlist items by id: %w", err)
		}
		if response.NextPageToken != "" {
			return nil, fmt.Errorf("%w: a read of playlist items by id has a next page", ErrUnexpectedResponse)
		}
		for _, resource := range response.Items {
			if !slices.Contains(chunk, ItemID(resource.Id)) {
				return nil, fmt.Errorf("%w: a read of playlist items by id returned %s, which it did not name", ErrUnexpectedResponse, resource.Id)
			}
			found = append(found, ItemID(resource.Id))
		}
	}
	return found, nil
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

// playlistParts is what the listing of the channel's playlists asks for. A
// list costs one unit whatever parts it names.
var playlistParts = []string{"contentDetails", "snippet", "status"}

// readPlaylistFrom is a playlist the listing returned, which asked for its
// count.
func readPlaylistFrom(resource *ytapi.Playlist) (Playlist, error) {
	if resource.ContentDetails == nil {
		return Playlist{}, fmt.Errorf("%w: playlist %s lacks its content details", ErrUnexpectedResponse, resource.Id)
	}
	return playlistFrom(resource)
}

// playlistFrom is a playlist YouTube answered with. Only the listing asks for a
// count, so ItemCount is 0 in any other.
func playlistFrom(resource *ytapi.Playlist) (Playlist, error) {
	if resource.Snippet == nil || resource.Status == nil {
		return Playlist{}, fmt.Errorf("%w: playlist %s lacks its snippet or status", ErrUnexpectedResponse, resource.Id)
	}
	var count int64
	if resource.ContentDetails != nil {
		count = resource.ContentDetails.ItemCount
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
		ItemCount:   count,
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
