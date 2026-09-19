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

// VideoFilter narrows the library. A field left at its zero asks nothing of
// that field, which is why the bounds are pointers: zero seconds is a bound a
// caller can mean, and an int could not tell it from one nobody set.
type VideoFilter struct {
	// Playlist keeps the videos one playlist holds, named by id or title.
	Playlist string
	// Artist keeps the videos with an artist whose name holds it, ignoring
	// case and accents.
	Artist string
	// MinSeconds and MaxSeconds bound the duration. Nil is no bound.
	MinSeconds *int64
	MaxSeconds *int64
	// Sort is the order, and "" is the server's own first one. The server owns
	// the vocabulary and refuses an order it does not have, naming every one it
	// does, so nothing here narrows what may be sent.
	Sort string
}

// VideoSorts is every order this build knows the library can be read in, the
// first being the one the server uses when none is asked for.
//
// It is a copy of a vocabulary the server owns, kept because nothing publishes
// it on the wire. Nothing refuses an order against it, so a copy fallen behind
// costs a reader one under-reported listing rather than a working order.
var VideoSorts = []string{"longest", "shortest", "newest", "oldest", "title", "random"}

// ListVideos is every available video some playlist holds, narrowed by filter,
// read whole or a page at a time as the server answers.
func (c *Client) ListVideos(ctx context.Context, filter VideoFilter) ([]LibraryVideo, error) {
	return all(ctx, c, "/api/v1/videos", [][2]string{
		{"playlist", filter.Playlist},
		{"artist", filter.Artist},
		{"min_seconds", seconds(filter.MinSeconds)},
		{"max_seconds", seconds(filter.MaxSeconds)},
		{"sort", filter.Sort},
	}, func(video LibraryVideo) string { return video.ID })
}

// seconds is a duration bound as the server takes it, and "" for no bound.
func seconds(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

// GetVideo is one video with its tracklist, by its YouTube id.
func (c *Client) GetVideo(ctx context.Context, id string) (Video, error) {
	var video Video
	err := c.Get(ctx, "/api/v1/videos/"+ref(id), &video)
	return video, err
}
