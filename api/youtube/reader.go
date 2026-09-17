package youtube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"google.golang.org/api/googleapi"
	ytapi "google.golang.org/api/youtube/v3"
)

// pageSize is the most items one Data API list call returns.
const pageSize = 50

// ErrInconsistentRead is the refusal for a playlist whose items do not match
// what the playlist reported: a count that differs from itemCount, or
// positions that are not exactly 0 through n-1. It is never a read of an
// emptied or shortened playlist, so a caller must not take the missing items
// as removed.
var ErrInconsistentRead = errors.New("a playlist read does not match the playlist")

// Playlist is one playlist the channel owns, as YouTube reports it.
type Playlist struct {
	ID          string
	Title       string
	Description string
	Privacy     string
	ItemCount   int64
}

// Item is one slot in a playlist.
type Item struct {
	// ID is the playlistItem id, which every write to this slot names.
	ID       string
	VideoID  string
	Position int64
	Title    string
	// ChannelTitle is the video owner's channel, and is empty for an
	// unavailable video.
	ChannelTitle string
	// Unavailable is true for a video that is private or deleted.
	Unavailable bool
}

// Reader lists a channel's playlists and their items, charging each page to a
// ledger before requesting it.
type Reader struct {
	service *ytapi.Service
	ledger  *Ledger
}

// NewReader reads through service and charges ledger.
func NewReader(service *ytapi.Service, ledger *Ledger) *Reader {
	return &Reader{service: service, ledger: ledger}
}

// Playlists is every playlist the channel owns.
func (r *Reader) Playlists(ctx context.Context) ([]Playlist, error) {
	var playlists []Playlist
	call := r.service.Playlists.List([]string{"snippet", "status", "contentDetails"}).Mine(true).MaxResults(pageSize)
	err := r.pages(ctx, MethodPlaylistsList, func(token string) (string, error) {
		response, err := call.PageToken(token).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		for _, p := range response.Items {
			playlist := Playlist{ID: p.Id}
			if p.Snippet != nil {
				playlist.Title, playlist.Description = p.Snippet.Title, p.Snippet.Description
			}
			if p.Status != nil {
				playlist.Privacy = p.Status.PrivacyStatus
			}
			if p.ContentDetails != nil {
				playlist.ItemCount = p.ContentDetails.ItemCount
			}
			playlists = append(playlists, playlist)
		}
		return response.NextPageToken, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	return playlists, nil
}

// Items is every item in playlist, ordered by position. It returns
// ErrInconsistentRead when the items read disagree with playlist.ItemCount or
// their positions have a gap or a repeat.
func (r *Reader) Items(ctx context.Context, playlist Playlist) ([]Item, error) {
	var items []Item
	call := r.service.PlaylistItems.List([]string{"snippet", "status"}).PlaylistId(playlist.ID).MaxResults(pageSize)
	err := r.pages(ctx, MethodPlaylistItemsList, func(token string) (string, error) {
		response, err := call.PageToken(token).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		for _, p := range response.Items {
			items = append(items, item(p))
		}
		return response.NextPageToken, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list items of playlist %s: %w", playlist.ID, err)
	}

	if int64(len(items)) != playlist.ItemCount {
		return nil, fmt.Errorf("%w: %s read %d items and reports %d", ErrInconsistentRead, playlist.ID, len(items), playlist.ItemCount)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Position < items[j].Position })
	for i, it := range items {
		if it.Position != int64(i) {
			return nil, fmt.Errorf("%w: %s has position %d where %d belongs", ErrInconsistentRead, playlist.ID, it.Position, i)
		}
	}
	return items, nil
}

func item(p *ytapi.PlaylistItem) Item {
	it := Item{ID: p.Id}
	if p.Snippet != nil {
		it.Position = p.Snippet.Position
		it.Title = p.Snippet.Title
		it.ChannelTitle = p.Snippet.VideoOwnerChannelTitle
		if p.Snippet.ResourceId != nil {
			it.VideoID = p.Snippet.ResourceId.VideoId
		}
	}
	// A deleted video's item reports privacyStatusUnspecified, and a private
	// one's reports private.
	if p.Status != nil {
		switch p.Status.PrivacyStatus {
		case "private", "privacyStatusUnspecified":
			it.Unavailable = true
		}
	}
	return it
}

// pages calls fetch once per page, charging method to the ledger before each
// call, until fetch returns no next page token.
func (r *Reader) pages(ctx context.Context, method string, fetch func(token string) (string, error)) error {
	token := ""
	for {
		if err := r.ledger.Spend(ctx, method); err != nil {
			return err
		}
		next, err := fetch(token)
		if err != nil {
			return apiError(err)
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

// apiError names YouTube's own quota refusal as ErrQuotaSpent, since the
// project's quota also covers calls this ledger never saw.
func apiError(err error) error {
	var google *googleapi.Error
	if errors.As(err, &google) && google.Code == http.StatusForbidden {
		for _, reason := range google.Errors {
			if reason.Reason == "quotaExceeded" {
				return fmt.Errorf("%w: YouTube reports the project's quota exceeded", ErrQuotaSpent)
			}
		}
	}
	return err
}
