package youtube

import (
	"context"
	"fmt"
	"strings"

	ytapi "google.golang.org/api/youtube/v3"
)

// ItemsPage is one page of at most max items of the playlist id, any channel's,
// from the page token names, "" being the first. next is the next page's
// token, and "" on the last page.
func (c *Channel) ItemsPage(ctx context.Context, id PlaylistID, token string, max int64) (items []Item, next string, err error) {
	call := c.service.PlaylistItems.List([]string{"snippet", "status"}).PlaylistId(string(id)).MaxResults(max)
	if token != "" {
		call = call.PageToken(token)
	}
	response, err := send(ctx, c, playlistItemsList, call.Context(ctx).Do)
	if err != nil {
		return nil, "", fmt.Errorf("list a page of the items of playlist %s: %w", id, err)
	}
	items = []Item{}
	for _, resource := range response.Items {
		item, err := itemFrom(resource)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	return items, response.NextPageToken, nil
}

// Uploads is one page of the videos a channel uploaded, newest first, read as
// ItemsPage reads the channel's uploads playlist. channel is a channel id, or a
// handle starting with @. The first read naming a channel finds its uploads
// playlist with channels.list, and every read after reuses it.
func (c *Channel) Uploads(ctx context.Context, channel, token string, max int64) (items []Item, next string, err error) {
	uploads, err := c.uploadsPlaylist(ctx, channel)
	if err != nil {
		return nil, "", err
	}
	return c.ItemsPage(ctx, uploads, token, max)
}

func (c *Channel) uploadsPlaylist(ctx context.Context, channel string) (PlaylistID, error) {
	if found, ok := c.uploads.Load(channel); ok {
		return found.(PlaylistID), nil
	}
	call := c.service.Channels.List([]string{"contentDetails"})
	if strings.HasPrefix(channel, "@") {
		call = call.ForHandle(channel)
	} else {
		call = call.Id(channel)
	}
	response, err := send(ctx, c, channelsList, call.Context(ctx).Do)
	if err != nil {
		return "", fmt.Errorf("read channel %s: %w", channel, err)
	}
	if len(response.Items) == 0 {
		return "", fmt.Errorf("%w: %s", ErrChannelNotFound, channel)
	}
	resource := response.Items[0]
	if resource.ContentDetails == nil || resource.ContentDetails.RelatedPlaylists == nil || resource.ContentDetails.RelatedPlaylists.Uploads == "" {
		return "", fmt.Errorf("%w: channel %s names no uploads playlist", ErrUnexpectedResponse, resource.Id)
	}
	uploads := PlaylistID(resource.ContentDetails.RelatedPlaylists.Uploads)
	c.uploads.Store(channel, uploads)
	return uploads, nil
}

// VideoDetails is a video as a read of its details reports it: what Video
// holds, its description, its channel and when it was published. It also
// carries whether it is live or upcoming, as "none", "live" or "upcoming", and
// its counts. A count YouTube hides, as an owner can hide likes, is 0.
type VideoDetails struct {
	Video
	Description          string
	ChannelID            string
	PublishedAt          string
	LiveBroadcastContent string
	ViewCount            uint64
	LikeCount            uint64
	CommentCount         uint64
}

// VideoDetails is the details of each of ids that YouTube returns a video for,
// read 50 ids a request, as Videos reads them.
func (c *Channel) VideoDetails(ctx context.Context, ids []VideoID) ([]VideoDetails, error) {
	resources, err := c.readVideos(ctx, ids, []string{"snippet", "status", "contentDetails", "statistics"})
	if err != nil {
		return nil, err
	}
	details := make([]VideoDetails, len(resources))
	for i, resource := range resources {
		details[i] = VideoDetails{
			Video:                videoFrom(resource),
			Description:          resource.Snippet.Description,
			ChannelID:            resource.Snippet.ChannelId,
			PublishedAt:          resource.Snippet.PublishedAt,
			LiveBroadcastContent: resource.Snippet.LiveBroadcastContent,
		}
		if stats := resource.Statistics; stats != nil {
			details[i].ViewCount, details[i].LikeCount, details[i].CommentCount = stats.ViewCount, stats.LikeCount, stats.CommentCount
		}
	}
	return details, nil
}

// Comment is one top-level comment on a video, in plain text.
type Comment struct {
	ID              string
	Author          string
	AuthorChannelID string
	Text            string
	LikeCount       int64
	ReplyCount      int64
	PublishedAt     string
}

// MaxComments is the most top-level comments one read of a video's comments
// returns, which is the most one request to commentThreads.list returns.
const MaxComments = 100

// TopComments is up to MaxComments of the video's top-level comments, those
// YouTube ranks most relevant first, read in one request. YouTube answers
// ErrCommentsDisabled for a video whose comments are off, and ErrVideoNotFound
// for one it has no record of.
func (c *Channel) TopComments(ctx context.Context, video VideoID) ([]Comment, error) {
	call := c.service.CommentThreads.List([]string{"snippet"}).VideoId(string(video)).Order("relevance").TextFormat("plainText").MaxResults(MaxComments)
	response, err := send(ctx, c, commentThreadsList, call.Context(ctx).Do)
	if err != nil {
		return nil, fmt.Errorf("read the comments of video %s: %w", video, err)
	}
	comments := []Comment{}
	for _, thread := range response.Items {
		comment, err := commentFrom(thread)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, nil
}

func commentFrom(thread *ytapi.CommentThread) (Comment, error) {
	if thread.Snippet == nil || thread.Snippet.TopLevelComment == nil || thread.Snippet.TopLevelComment.Snippet == nil {
		return Comment{}, fmt.Errorf("%w: comment thread %s lacks its top-level comment", ErrUnexpectedResponse, thread.Id)
	}
	top := thread.Snippet.TopLevelComment
	comment := Comment{
		ID:          top.Id,
		Author:      top.Snippet.AuthorDisplayName,
		Text:        top.Snippet.TextDisplay,
		LikeCount:   top.Snippet.LikeCount,
		ReplyCount:  thread.Snippet.TotalReplyCount,
		PublishedAt: top.Snippet.PublishedAt,
	}
	if top.Snippet.AuthorChannelId != nil {
		comment.AuthorChannelID = top.Snippet.AuthorChannelId.Value
	}
	return comment, nil
}
