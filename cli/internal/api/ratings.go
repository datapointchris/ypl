package api

import (
	"context"
	"net/url"
	"strconv"
)

// Rating is one like, dislike or note left on a mix, with its message and
// tags. A mix holds as many as were left.
type Rating struct {
	ID                string      `json:"id"`
	RatedTs           string      `json:"rated_ts"`
	Kind              string      `json:"kind"`
	Message           *string     `json:"message"`
	PositionSeconds   *int64      `json:"position_seconds"`
	PositionEstimated bool        `json:"position_estimated"`
	Tags              []string    `json:"tags"`
	Video             PlayedVideo `json:"video"`
}

// NewRating is what CreateRating sends. ID is a version 7 UUID the caller
// makes, so a rating sent again after a lost answer is stored once. A note
// needs a Message. PositionSeconds is where in the mix it was left, and
// PositionEstimated says it came from YouTube's resume point, not a person.
type NewRating struct {
	ID                string   `json:"id"`
	VideoID           string   `json:"video_id"`
	Kind              string   `json:"kind"`
	Message           *string  `json:"message"`
	PositionSeconds   *int64   `json:"position_seconds,omitempty"`
	PositionEstimated bool     `json:"position_estimated,omitempty"`
	Tags              []string `json:"tags"`
}

// TagCount is a tag and how many videos carry it.
type TagCount struct {
	Tag    string `json:"tag"`
	Videos int64  `json:"videos"`
}

// HistoryEntry is a library video in the watch history, at its place in it,
// 1 being the newest.
//
// ResumeSeconds is where YouTube's resume point sits in it, approximate, and
// nil where the server could not tell.
type HistoryEntry struct {
	Position      int64       `json:"position"`
	ResumeSeconds *int64      `json:"resume_seconds"`
	Video         PlayedVideo `json:"video"`
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

// RatingKinds is every kind of rating, as the server spells them.
var RatingKinds = []string{"like", "dislike", "note"}

// DeleteRating deletes the rating id names, by its whole id or the last eight
// or more characters of it, and its tags with it.
func (c *Client) DeleteRating(ctx context.Context, id string) error {
	return c.Delete(ctx, "/api/v1/ratings/"+url.PathEscape(id))
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
	err := c.Get(ctx, query("/api/v1/youtube/history", [2]string{"limit", strconv.Itoa(limit)}), &entries)
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
