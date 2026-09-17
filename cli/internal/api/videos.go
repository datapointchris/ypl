package api

import (
	"context"
	"strconv"
)

// VideoSummary is a video as a playlist or the library lists it.
type VideoSummary struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	UploadDate      *string `json:"upload_date"`
	IsUnavailable   bool    `json:"is_unavailable"`
	EnrichedTs      *string `json:"enriched_ts"`
	TrackCount      int64   `json:"track_count"`
}

// LibraryVideo is a video as the library lists it: what decides whether it
// belongs in a set, without its tracklist being read.
type LibraryVideo struct {
	VideoSummary
	Artists   []string      `json:"artists"`
	Playlists []PlaylistRef `json:"playlists"`
}

// Video is one video with its description and its tracklist.
type Video struct {
	LibraryVideo
	Description *string `json:"description"`
	Tracks      []Track `json:"tracks"`
}

// PlaylistRef is a playlist a video is in.
type PlaylistRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Track is one track of a video's tracklist. Source names where its text came
// from: a chapter, the description, or a comment.
type Track struct {
	Position     int64   `json:"position"`
	StartSeconds *int64  `json:"start_seconds"`
	EndSeconds   *int64  `json:"end_seconds"`
	Artist       *string `json:"artist"`
	Title        string  `json:"title"`
	RawText      string  `json:"raw_text"`
	Source       string  `json:"source"`
}

// VideoFilter narrows the library. A zero field asks nothing of that field.
type VideoFilter struct {
	// Playlist keeps the videos one playlist holds, named by id or title.
	Playlist string
	// Artist keeps the videos with an artist whose name holds it, ignoring
	// case and accents.
	Artist string
	// MinSeconds and MaxSeconds bound the duration, and -1 is no bound.
	MinSeconds int64
	MaxSeconds int64
	// Sort is the order, and "" is the server's own first one.
	Sort string
}

// VideoSorts is every order the library can be read in, the first being the one
// the server uses when none is asked for.
var VideoSorts = []string{"longest", "shortest", "newest", "oldest", "title", "random"}

// ListVideos is every available video some playlist holds, narrowed by filter.
// The collection is not paged, so this is all of them.
func (c *Client) ListVideos(ctx context.Context, filter VideoFilter) ([]LibraryVideo, error) {
	videos := []LibraryVideo{}
	target := query("/api/v1/videos",
		[2]string{"playlist", filter.Playlist},
		[2]string{"artist", filter.Artist},
		[2]string{"min_seconds", seconds(filter.MinSeconds)},
		[2]string{"max_seconds", seconds(filter.MaxSeconds)},
		[2]string{"sort", filter.Sort},
	)
	err := c.Get(ctx, target, &videos)
	return videos, err
}

// GetVideo is one video with its tracklist, by its YouTube id.
func (c *Client) GetVideo(ctx context.Context, id string) (Video, error) {
	var video Video
	err := c.Get(ctx, "/api/v1/videos/"+ref(id), &video)
	return video, err
}

// seconds is a duration bound as the server takes it, and "" for no bound. Zero
// is a bound a caller can mean, so absence is spelled as a negative rather than
// as zero.
func seconds(n int64) string {
	if n < 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}
