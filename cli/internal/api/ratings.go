package api

import (
	"context"
	"strconv"
)

// Rating is one thing said about a mix: a verdict of up or down, a note, tags,
// or any of them. A null verdict leaves the mix's standing verdict as it was.
type Rating struct {
	ID      string      `json:"id"`
	RatedTs string      `json:"rated_ts"`
	Verdict *string     `json:"verdict"`
	Note    *string     `json:"note"`
	Tags    []string    `json:"tags"`
	Video   PlayedVideo `json:"video"`
}

// NewRating is what CreateRating sends. ID is a version 7 UUID the caller
// makes, so a rating sent again after a lost answer is stored once.
type NewRating struct {
	ID      string   `json:"id"`
	VideoID string   `json:"video_id"`
	Verdict *string  `json:"verdict"`
	Note    *string  `json:"note"`
	Tags    []string `json:"tags"`
}

// TagCount is a tag and how many videos carry it.
type TagCount struct {
	Tag    string `json:"tag"`
	Videos int64  `json:"videos"`
}

// HistoryEntry is a library video in the watch history, at its place in it,
// 1 being the newest.
type HistoryEntry struct {
	Position int64       `json:"position"`
	Video    PlayedVideo `json:"video"`
}

// YouTubeSession is where the session the watch history is read with stands.
// A LastRefusedTs later than LastOKTs means its cookie jar needs exporting
// again.
type YouTubeSession struct {
	StoredTs      string  `json:"stored_ts"`
	LastOKTs      *string `json:"last_ok_ts"`
	LastRefusedTs *string `json:"last_refused_ts"`
	LastError     *string `json:"last_error"`
}

// Refused reports whether YouTube answered signed out more recently than it
// last answered signed in. Both are RFC 3339 in UTC, whose text sorts in time
// order.
func (s *YouTubeSession) Refused() bool {
	return s != nil && s.LastRefusedTs != nil && (s.LastOKTs == nil || *s.LastRefusedTs > *s.LastOKTs)
}

// CreateRating records a rating.
func (c *Client) CreateRating(ctx context.Context, rating NewRating) (Rating, error) {
	return post[Rating](ctx, c, "/api/v1/ratings", rating)
}

// ListRatings is the newest limit ratings, of the video named by video when it
// is not "".
func (c *Client) ListRatings(ctx context.Context, video string, limit int) (Page[Rating], error) {
	return collect(ctx, c, "/api/v1/ratings", [][2]string{{"video", video}}, limit, func(r Rating) string { return r.ID })
}

// ListTags is every tag with how many videos carry it, most carried first.
func (c *Client) ListTags(ctx context.Context) ([]TagCount, error) {
	tags := []TagCount{}
	err := c.Get(ctx, "/api/v1/tags", &tags)
	return tags, err
}

// History is the newest limit library videos in the watch history.
func (c *Client) History(ctx context.Context, limit int) ([]HistoryEntry, error) {
	entries := []HistoryEntry{}
	err := c.Get(ctx, query("/api/v1/history", [2]string{"limit", strconv.Itoa(limit)}), &entries)
	return entries, err
}

// SetSession sends a Netscape cookie jar for the server to read the watch
// history with. The server keeps it only once a read with it answers signed
// in.
func (c *Client) SetSession(ctx context.Context, jar string) (YouTubeSession, error) {
	var session YouTubeSession
	_, err := c.send(ctx, "PUT", "/api/v1/youtube/session", nil, map[string]string{"cookie_jar": jar}, &session)
	return session, err
}
