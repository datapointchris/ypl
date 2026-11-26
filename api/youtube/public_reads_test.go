package youtube

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
)

// A channel's uploads cost channels.list once, for its uploads playlist, and
// one request a page of that playlist after.
func TestUploadsReadsChannelsListOnceThenTheUploadsPlaylist(t *testing.T) {
	api := newFakeAPI(t)
	const uploads = "UUx0otherChannel00000000"
	api.channels["@writtenChannel"] = uploads
	api.items[uploads] = fakeItems(t, uploads, 70)
	channel := api.channel()
	ctx := context.Background()

	first, next, err := channel.Uploads(ctx, "@writtenChannel", "", 50)
	if err != nil || len(first) != 50 || next == "" || first[0].PlaylistID != uploads {
		t.Fatalf("the first page = %d items of %v, next %q, %v, want 50 of %s and a next page", len(first), first[0].PlaylistID, next, err, uploads)
	}
	second, last, err := channel.Uploads(ctx, "@writtenChannel", next, 50)
	if err != nil || len(second) != 20 || last != "" {
		t.Fatalf("the second page = %d items, next %q, %v, want the last 20", len(second), last, err)
	}
	if want := []string{"channels", "playlistItems", "playlistItems"}; !slices.Equal(api.served, want) || channel.Units() != 3 {
		t.Fatalf("served %v for %d units, want %v for 3", api.served, channel.Units(), want)
	}
	if _, _, err := channel.Uploads(ctx, "UCx0noSuchChannel0000000", "", 50); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("the uploads of a channel YouTube has no record of = %v, want ErrChannelNotFound", err)
	}
}

func TestVideoDetailsCarryTheCountsAndWhetherAVideoIsLive(t *testing.T) {
	api := newFakeAPI(t)
	api.videos["vidA"] = publicVideo("vidA")
	got, err := api.channel().VideoDetails(context.Background(), []VideoID{"vidA", "unknown"})
	if err != nil || len(got) != 1 {
		t.Fatalf("VideoDetails = %+v, %v, want vidA alone", got, err)
	}
	details := got[0]
	if details.ID != "vidA" || details.ChannelID != "UCx0otherChannel00000000" || details.ViewCount != 1200 || details.LikeCount != 34 || details.CommentCount != 5 ||
		details.LiveBroadcastContent != "none" || details.PublishedAt == "" || details.DurationSeconds == nil {
		t.Fatalf("details %+v, want vidA's channel, counts, length, publication and live state", details)
	}
}

// A page of comment threads in the shape the Data API reference shows reads as
// each thread's top-level comment, and a video whose comments are off is
// ErrCommentsDisabled.
func TestTopCommentsReadEachThreadsTopLevelComment(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{status: http.StatusOK, body: `{
		"kind": "youtube#commentThreadListResponse", "etag": "fakePageEtag",
		"pageInfo": {"totalResults": 1, "resultsPerPage": 100},
		"items": [{
			"kind": "youtube#commentThread", "etag": "fakeThreadEtag", "id": "UgxWrittenThread0",
			"snippet": {
				"channelId": "UCx0otherChannel00000000", "videoId": "vidA",
				"topLevelComment": {
					"kind": "youtube#comment", "etag": "fakeCommentEtag", "id": "UgxWrittenThread0",
					"snippet": {
						"channelId": "UCx0otherChannel00000000", "videoId": "vidA",
						"textDisplay": "0:00 Written - One", "textOriginal": "0:00 Written - One",
						"authorDisplayName": "@writtenListener", "authorChannelId": {"value": "UCx0listener000000000000"},
						"canRate": true, "viewerRating": "none", "likeCount": 7,
						"publishedAt": "2026-01-02T03:04:05Z", "updatedAt": "2026-01-02T03:04:05Z"
					}
				},
				"canReply": true, "totalReplyCount": 2, "isPublic": true
			}
		}]
	}`}
	channel := api.channel()
	got, err := channel.TopComments(context.Background(), "vidA")
	want := Comment{ID: "UgxWrittenThread0", Author: "@writtenListener", AuthorChannelID: "UCx0listener000000000000", Text: "0:00 Written - One", LikeCount: 7, ReplyCount: 2, PublishedAt: "2026-01-02T03:04:05Z"}
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("TopComments = %+v, %v, want %+v", got, err, want)
	}

	api.answer = &fakeAnswer{status: http.StatusForbidden, body: `{"error": {"code": 403,
		"message": "The video identified by the <code><a href=\"/youtube/v3/docs/commentThreads/list#videoId\">videoId</a></code> parameter has disabled comments.",
		"errors": [{"message": "The video identified by the <code><a href=\"/youtube/v3/docs/commentThreads/list#videoId\">videoId</a></code> parameter has disabled comments.",
			"domain": "youtube.commentThread", "reason": "commentsDisabled", "location": "videoId", "locationType": "parameter"}]}}`}
	if _, err := channel.TopComments(context.Background(), "vidA"); !errors.Is(err, ErrCommentsDisabled) {
		t.Fatalf("the comments of a video with comments off = %v, want ErrCommentsDisabled", err)
	}
}
