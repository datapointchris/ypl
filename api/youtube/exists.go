package youtube

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ExistingPlaylists is the ids among ids that YouTube still has a playlist for,
// which is how an absence from Playlists is confirmed. A playlist deleted seconds
// before can still be returned.
func (c *Channel) ExistingPlaylists(ctx context.Context, ids []PlaylistID) ([]PlaylistID, error) {
	return existing(ids, func(joined string) ([]string, error) {
		call := c.service.Playlists.List([]string{"id"}).Id(joined).MaxResults(pageSize)
		response, err := send(ctx, c, playlistsList, call.Context(ctx).Do)
		if err != nil {
			return nil, fmt.Errorf("read playlists by id: %w", err)
		}
		if response.NextPageToken != "" {
			return nil, fmt.Errorf("%w: a read of playlists by id has a next page", ErrUnexpectedResponse)
		}
		found := make([]string, len(response.Items))
		for i, resource := range response.Items {
			found[i] = resource.Id
		}
		return found, nil
	})
}

// ExistingItems is the ids among ids that YouTube still has a playlist item for,
// which is how an item absent from Items is confirmed gone.
func (c *Channel) ExistingItems(ctx context.Context, ids []ItemID) ([]ItemID, error) {
	return existing(ids, func(joined string) ([]string, error) {
		call := c.service.PlaylistItems.List([]string{"id"}).Id(joined).MaxResults(pageSize)
		response, err := send(ctx, c, playlistItemsList, call.Context(ctx).Do)
		if err != nil {
			return nil, fmt.Errorf("read playlist items by id: %w", err)
		}
		if response.NextPageToken != "" {
			return nil, fmt.Errorf("%w: a read of playlist items by id has a next page", ErrUnexpectedResponse)
		}
		found := make([]string, len(response.Items))
		for i, resource := range response.Items {
			found[i] = resource.Id
		}
		return found, nil
	})
}

// existing reads ids through list, pageSize at a time. Each request names its
// ids as one comma-separated value, the form YouTube was measured answering,
// and YouTube leaves an id it has nothing for out of its answer.
func existing[ID ~string](ids []ID, list func(joined string) ([]string, error)) ([]ID, error) {
	var found []ID
	for chunk := range slices.Chunk(ids, pageSize) {
		names := make([]string, len(chunk))
		for i, id := range chunk {
			names[i] = string(id)
		}
		answered, err := list(strings.Join(names, ","))
		if err != nil {
			return nil, err
		}
		for _, id := range answered {
			found = append(found, ID(id))
		}
	}
	return found, nil
}
