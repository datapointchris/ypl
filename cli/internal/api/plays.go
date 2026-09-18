package api

import (
	"context"
	"strconv"
)

// Play is one listen: its key, the short handle the server gave it, when it was
// in UTC to the second, and the video.
type Play struct {
	ID       string      `json:"id"`
	Handle   int64       `json:"handle"`
	PlayedTs string      `json:"played_ts"`
	Video    PlayedVideo `json:"video"`
}

// PlayedVideo is the video a play is of.
type PlayedVideo struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ChannelTitle string `json:"channel_title"`
}

// Suggestion is a video to play next, with how often it has been played and
// when it was last.
type Suggestion struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	PlayCount       int64   `json:"play_count"`
	LastPlayedTs    *string `json:"last_played_ts"`
}

// PlayID is the id a play is stored under, made by the client.
type PlayID string

// VideoID is the video a play is about.
type VideoID string

// newPlay is the body of POST /api/v1/plays. The id is the client's to make, so
// a play sent twice — a retry, a command run again — is stored once.
type newPlay struct {
	ID      PlayID  `json:"id"`
	VideoID VideoID `json:"video_id"`
}

// CreatePlay records that videoID was listened to, under id. The server takes
// the moment it arrived as when it was played.
//
// The id is a version 7 UUID, which the server refuses anything else for. It is
// made by the caller rather than here so that a caller retrying a play it is
// not sure landed sends the same one, and gets the stored play back instead of
// a second row.
func (c *Client) CreatePlay(ctx context.Context, id PlayID, videoID VideoID) (Play, error) {
	var play Play
	err := c.Post(ctx, "/api/v1/plays", newPlay{ID: id, VideoID: videoID}, &play)
	return play, err
}

// ListPlays is the newest limit plays, newest first, reading as many pages as
// that takes.
func (c *Client) ListPlays(ctx context.Context, limit int) (Page[Play], error) {
	return collect(ctx, c, "/api/v1/plays", limit, func(p Play) string { return p.ID })
}

// GetPlay is the play named by name: its id, its handle, or the last eight
// characters of its id.
func (c *Client) GetPlay(ctx context.Context, name string) (Play, error) {
	var play Play
	err := c.Get(ctx, "/api/v1/plays/"+ref(name), &play)
	return play, err
}

// MaxSuggestions is the most the server draws at once. Suggestions are a draw
// reshuffled among videos last played at the same moment, so there is no next
// page to read and a larger ask is refused rather than paged.
const MaxSuggestions = PageSize

// ListSuggestions is up to limit videos to play next, never-played first and
// then least recently played, from playlist or from every playlist.
func (c *Client) ListSuggestions(ctx context.Context, playlist string, limit int) ([]Suggestion, error) {
	suggestions := []Suggestion{}
	target := query("/api/v1/suggestions",
		[2]string{"playlist", playlist},
		[2]string{"limit", strconv.Itoa(limit)},
	)
	err := c.Get(ctx, target, &suggestions)
	return suggestions, err
}
