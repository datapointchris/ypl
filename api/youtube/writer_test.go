package youtube

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	ytapi "google.golang.org/api/youtube/v3"
)

func TestCreatePlaylistMakesAPrivatePlaylist(t *testing.T) {
	api := newFakeAPI(t)
	channel := api.channel()

	id, err := channel.CreatePlaylist(context.Background(), PlaylistDetails{Title: "Late Night", Description: "Slow sets"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	listed, err := channel.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	want := Playlist{ID: id, Title: "Late Night", Description: "Slow sets", Privacy: "private"}
	if id == "" || !slices.Equal(listed, []Playlist{want}) {
		t.Fatalf("playlists %+v, want %+v with an id", listed, want)
	}
}

func TestTheFirstInsertIntoANewPlaylistIsSentAgain(t *testing.T) {
	api := newFakeAPI(t)
	api.videos["vidA"] = publicVideo("vidA")
	channel := api.channel()
	ctx := context.Background()

	playlist, err := channel.CreatePlaylist(ctx, PlaylistDetails{Title: "New"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if _, err := channel.InsertItem(ctx, playlist, "vidA", 0); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if !slices.Equal(api.pauses, []time.Duration{time.Second}) {
		t.Fatalf("paused %v, want one second before the second attempt", api.pauses)
	}
	if requests, units := channel.Requests(), channel.Units(); requests != 3 || units != 150 {
		t.Fatalf("counted %d requests and %d units, want a create and two insert attempts: 3 and 150", requests, units)
	}
	assertVideos(t, channel, playlist, "vidA")
}

// Only an insert was measured to leave nothing behind when YouTube aborts it, so
// only an insert is sent again.
func TestOnlyAnAbortedInsertIsSentAgain(t *testing.T) {
	item := Item{ID: "PLA-item-000", PlaylistID: "PLA", VideoID: "vid000"}
	writes := map[string]struct {
		write    func(context.Context, *Channel) error
		attempts int64
	}{
		"CreatePlaylist": {write: func(ctx context.Context, c *Channel) error {
			_, err := c.CreatePlaylist(ctx, PlaylistDetails{Title: "New"})
			return err
		}, attempts: 1},
		"UpdatePlaylist": {write: func(ctx context.Context, c *Channel) error {
			return c.UpdatePlaylist(ctx, "PLA", PlaylistDetails{Title: "Renamed"})
		}, attempts: 1},
		"DeletePlaylist": {write: func(ctx context.Context, c *Channel) error { return c.DeletePlaylist(ctx, "PLA") }, attempts: 1},
		"MoveItem":       {write: func(ctx context.Context, c *Channel) error { return c.MoveItem(ctx, item, 1) }, attempts: 1},
		"DeleteItem":     {write: func(ctx context.Context, c *Channel) error { return c.DeleteItem(ctx, item.ID) }, attempts: 1},
		"InsertItem": {write: func(ctx context.Context, c *Channel) error {
			_, err := c.InsertItem(ctx, "PLA", "vidA", 0)
			return err
		}, attempts: 4},
		"AppendItem": {write: func(ctx context.Context, c *Channel) error {
			_, err := c.AppendItem(ctx, "PLA", "vidA")
			return err
		}, attempts: 4},
	}
	for name, w := range writes {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.answer = &fakeAnswer{status: http.StatusConflict, body: string(api.recorded["playlistItems.insert SERVICE_UNAVAILABLE"])}
			channel := api.channel()

			if err := w.write(context.Background(), channel); !aborted(err) {
				t.Fatalf("%s = %v, want YouTube's abort", name, err)
			}
			if requests := channel.Requests(); requests != w.attempts {
				t.Fatalf("counted %d requests, want %d", requests, w.attempts)
			}
			if w.attempts == 4 && !slices.Equal(api.pauses, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
				t.Fatalf("paused %v, want 1s, 2s and 4s", api.pauses)
			}
		})
	}
}

// This body is not a recorded one: no request made against YouTube drew a 409
// with any other reason.
func TestAConflictOtherThanAnAbortIsNotSentAgain(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{
		status: http.StatusConflict,
		body:   `{"error":{"code":409,"message":"conflict","errors":[{"reason":"conflict","domain":"global"}]}}`,
	}
	channel := api.channel()

	if _, err := channel.InsertItem(context.Background(), "PLA", "vidA", 0); err == nil {
		t.Fatal("InsertItem succeeded, want the conflict")
	}
	if requests := channel.Requests(); requests != 1 || len(api.pauses) != 0 {
		t.Fatalf("counted %d requests after %d pauses, want 1 and none", requests, len(api.pauses))
	}
}

func TestAPauseTheContextEndsStopsTheRequest(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{status: http.StatusConflict, body: string(api.recorded["playlistItems.insert SERVICE_UNAVAILABLE"])}
	channel := api.channel()
	ctx, cancel := context.WithCancel(context.Background())
	channel.pause = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	if _, err := channel.InsertItem(ctx, "PLA", "vidA", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("InsertItem = %v, want context.Canceled", err)
	}
	if requests := channel.Requests(); requests != 1 {
		t.Fatalf("counted %d requests, want the one attempt before the pause", requests)
	}
}

func TestInsertItemPlacesTheVideoAtThePositionGiven(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 3)
	api.videos["first"] = publicVideo("first")
	api.videos["last"] = publicVideo("last")
	channel := api.channel()
	ctx := context.Background()

	first, err := channel.InsertItem(ctx, "PLA", "first", 0)
	if err != nil {
		t.Fatalf("InsertItem at 0: %v", err)
	}
	if _, err := channel.InsertItem(ctx, "PLA", "last", 4); err != nil {
		t.Fatalf("InsertItem at the item count: %v", err)
	}
	items := assertVideos(t, channel, "PLA", "first", "vid000", "vid001", "vid002", "last")
	if first == "" || items[0].ID != first {
		t.Fatalf("InsertItem returned %q, want the id of the item at 0, %q", first, items[0].ID)
	}
}

// The fake appends an insert that names no position, so an append that sent one
// would land at 0.
func TestAppendItemAddsTheVideoLast(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 2)
	api.videos["vidA"] = publicVideo("vidA")
	channel := api.channel()

	if _, err := channel.AppendItem(context.Background(), "PLA", "vidA"); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}
	assertVideos(t, channel, "PLA", "vid000", "vid001", "vidA")
}

func TestAVideoAlreadyInThePlaylistGetsASecondItem(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = []map[string]any{}
	api.videos["vidA"] = publicVideo("vidA")
	channel := api.channel()
	ctx := context.Background()

	one, err := channel.InsertItem(ctx, "PLA", "vidA", 0)
	if err != nil {
		t.Fatalf("first InsertItem: %v", err)
	}
	two, err := channel.AppendItem(ctx, "PLA", "vidA")
	if err != nil {
		t.Fatalf("AppendItem: %v", err)
	}
	if one == two {
		t.Fatalf("both inserts returned item %s, want two items", one)
	}
	assertVideos(t, channel, "PLA", "vidA", "vidA")
}

func TestAnInsertYouTubeRefusesIsNamed(t *testing.T) {
	cases := map[string]struct {
		playlist PlaylistID
		video    VideoID
		position int64
		want     error
	}{
		"a video YouTube has no record of":   {playlist: "PLA", video: "gone", want: ErrVideoNotFound},
		"another channel's private video":    {playlist: "PLA", video: "private", want: ErrVideoRefused},
		"a deleted playlist":                 {playlist: "PLdeleted", video: "vidA", want: ErrPlaylistNotFound},
		"a position past the playlist's end": {playlist: "PLA", video: "vidA", position: 3},
	}
	sentinels := []error{ErrVideoNotFound, ErrVideoRefused, ErrPlaylistNotFound, ErrManualSortRequired}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.items["PLA"] = fakeItems(t, "PLA", 2)
			api.deletedPlaylists["PLdeleted"] = true
			api.videos["vidA"] = publicVideo("vidA")
			api.videos["private"] = privateVideo()

			_, err := api.channel().InsertItem(context.Background(), c.playlist, c.video, c.position)
			if err == nil {
				t.Fatal("InsertItem succeeded, want a refusal")
			}
			for _, sentinel := range sentinels {
				if errors.Is(err, sentinel) != (sentinel == c.want) {
					t.Fatalf("InsertItem = %v, want %v and no other sentinel", err, c.want)
				}
			}
		})
	}
}

// The Data API reference documents manualSortRequired for an insert or a move
// that names a position in a playlist not sorted manually. No request drew it,
// so this body follows the reference rather than a recording.
func TestAPositionInAPlaylistNotSortedManuallyIsNamed(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":400,"message":"manual sort required","errors":[{"reason":"manualSortRequired","domain":"youtube.playlistItem"}]}}`,
	}
	channel := api.channel()
	ctx := context.Background()

	if _, err := channel.InsertItem(ctx, "PLA", "vidA", 0); !errors.Is(err, ErrManualSortRequired) {
		t.Errorf("InsertItem = %v, want ErrManualSortRequired", err)
	}
	if err := channel.MoveItem(ctx, Item{ID: "PLA-item-000", PlaylistID: "PLA", VideoID: "vid000"}, 1); !errors.Is(err, ErrManualSortRequired) {
		t.Errorf("MoveItem = %v, want ErrManualSortRequired", err)
	}
}

func TestMoveItemMovesTheItemWithinItsPlaylist(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 5)
	channel := api.channel()
	ctx := context.Background()
	items, err := channel.Items(ctx, "PLA")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}

	if err := channel.MoveItem(ctx, items[4], 0); err != nil {
		t.Fatalf("MoveItem to 0: %v", err)
	}
	if err := channel.MoveItem(ctx, items[0], 4); err != nil {
		t.Fatalf("MoveItem to the last position: %v", err)
	}
	assertVideos(t, channel, "PLA", "vid004", "vid001", "vid002", "vid003", "vid000")

	err = channel.MoveItem(ctx, items[1], 5)
	if err == nil || errors.Is(err, ErrItemNotFound) {
		t.Fatalf("MoveItem to the item count = %v, want YouTube's unnamed refusal", err)
	}
}

func TestAMoveOfADeletedItemIsItemNotFound(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 3)
	channel := api.channel()
	ctx := context.Background()
	items, err := channel.Items(ctx, "PLA")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}

	if err := channel.DeleteItem(ctx, items[1].ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if err := channel.MoveItem(ctx, items[1], 0); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("MoveItem of the deleted item = %v, want ErrItemNotFound", err)
	}
}

func TestDeleteItemDeletesOnceAndThenReportsItGone(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 3)
	channel := api.channel()
	ctx := context.Background()

	if err := channel.DeleteItem(ctx, "PLA-item-001"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	assertVideos(t, channel, "PLA", "vid000", "vid002")
	if err := channel.DeleteItem(ctx, "PLA-item-001"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("DeleteItem a second time = %v, want ErrItemNotFound", err)
	}
}

func TestUpdatePlaylistSetsTheDetailsAndKeepsThePrivacy(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	channel := api.channel()
	ctx := context.Background()

	if err := channel.UpdatePlaylist(ctx, "PLA", PlaylistDetails{Title: "Renamed", Description: "About PLA"}); err != nil {
		t.Fatalf("UpdatePlaylist: %v", err)
	}
	listed, err := channel.Playlists(ctx)
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	want := Playlist{ID: "PLA", Title: "Renamed", Description: "About PLA", Privacy: "private"}
	if !slices.Equal(listed, []Playlist{want}) {
		t.Fatalf("playlists %+v, want %+v", listed, want)
	}
}

// YouTube clears a description a request leaves out and one sent empty alike,
// so only the request shows the empty description was sent.
func TestAnEmptyDescriptionIsSentAsEmpty(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	channel := api.channel()

	if err := channel.UpdatePlaylist(context.Background(), "PLA", PlaylistDetails{Title: "Renamed"}); err != nil {
		t.Fatalf("UpdatePlaylist: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if description, sent := api.lastBody["snippet"].(map[string]any)["description"]; !sent || description != "" {
		t.Fatalf("request body %v, want the snippet to carry an empty description", api.lastBody)
	}
}

func TestDeletePlaylistTakesItsItemsWithIt(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	api.items["PLA"] = fakeItems(t, "PLA", 2)
	channel := api.channel()
	ctx := context.Background()

	if err := channel.DeletePlaylist(ctx, "PLA"); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}
	if _, err := channel.Items(ctx, "PLA"); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("Items of the deleted playlist = %v, want ErrPlaylistNotFound", err)
	}
	if err := channel.DeleteItem(ctx, "PLA-item-000"); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("DeleteItem of one of its items = %v, want ErrItemNotFound", err)
	}
	if err := channel.DeletePlaylist(ctx, "PLA"); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("DeletePlaylist a second time = %v, want ErrPlaylistNotFound", err)
	}
}

func TestUnitsAreEachRequestsMethodPrice(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	api.items["PLA"] = fakeItems(t, "PLA", 60)
	api.videos["vidA"] = publicVideo("vidA")
	channel := api.channel()
	ctx := context.Background()

	if _, err := channel.Playlists(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := channel.Items(ctx, "PLA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.InsertItem(ctx, "PLA", "vidA", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.AppendItem(ctx, "PLA", "vidA"); err != nil {
		t.Fatal(err)
	}
	if err := channel.MoveItem(ctx, items[59], 0); err != nil {
		t.Fatal(err)
	}
	if err := channel.DeleteItem(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := channel.UpdatePlaylist(ctx, "PLA", PlaylistDetails{Title: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := channel.DeletePlaylist(ctx, "PLA"); err != nil {
		t.Fatal(err)
	}

	// One playlists page and two items pages at 1 unit, six writes at 50.
	if requests, units := channel.Requests(), channel.Units(); requests != 9 || units != 303 {
		t.Fatalf("counted %d requests and %d units, want 9 and 303", requests, units)
	}
	if served := api.requests.Load(); served != channel.Requests() {
		t.Fatalf("the fake served %d requests and the channel counted %d", served, channel.Requests())
	}
}

// A write reads only the id from YouTube's answer, so an answer in a shape this
// package does not know still returns the id of what was written. These bodies
// are not recorded ones.
func TestAWriteAnswerReturnsTheIDWhateverElseItCarries(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{status: http.StatusOK, body: `{"kind":"youtube#playlistItem","id":"PLA-new","status":{"privacyStatus":"membersOnly"}}`}
	channel := api.channel()

	if id, err := channel.InsertItem(context.Background(), "PLA", "vidA", 0); err != nil || id != "PLA-new" {
		t.Fatalf("InsertItem = %q, %v, want PLA-new", id, err)
	}

	api.answer = &fakeAnswer{status: http.StatusOK, body: `{"kind":"youtube#playlistItem"}`}
	if id, err := channel.AppendItem(context.Background(), "PLA", "vidA"); !errors.Is(err, ErrUnexpectedResponse) || id != "" {
		t.Fatalf("AppendItem answered with no id = %q, %v, want ErrUnexpectedResponse", id, err)
	}
	api.answer = &fakeAnswer{status: http.StatusOK, body: `{"kind":"youtube#playlist"}`}
	if id, err := channel.CreatePlaylist(context.Background(), PlaylistDetails{Title: "New"}); !errors.Is(err, ErrUnexpectedResponse) || id != "" {
		t.Fatalf("CreatePlaylist answered with no id = %q, %v, want ErrUnexpectedResponse", id, err)
	}
}

// The fake answers each of these as the Data API did when the same request was
// made against it. They are sent as raw requests so each is exactly the request
// that was measured.
func TestTheFakeAnswersWritesAsTheDataAPIDoes(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	api.items["PLA"] = fakeItems(t, "PLA", 2)
	api.videos["vidA"] = publicVideo("vidA")
	api.deletedPlaylists["PLdeleted"] = true
	api.deletedItems["PLA-item-gone"] = true
	service := api.channel().service
	ctx := context.Background()
	video := &ytapi.ResourceId{Kind: "youtube#video", VideoId: "vidA"}

	appended, err := service.PlaylistItems.Insert([]string{"snippet", "status"}, &ytapi.PlaylistItem{
		Snippet: &ytapi.PlaylistItemSnippet{PlaylistId: "PLA", ResourceId: video},
	}).Context(ctx).Do()
	if err != nil || appended.Snippet.Position != 2 {
		t.Errorf("an insert with no position = %+v, %v, want the item at position 2", appended, err)
	}

	renamed, err := service.Playlists.Update([]string{"snippet"}, &ytapi.Playlist{
		Id: "PLA", Snippet: &ytapi.PlaylistSnippet{Title: "Renamed"},
	}).Context(ctx).Do()
	if err != nil || renamed.Snippet.Description != "" || renamed.Status != nil {
		t.Errorf("an update naming only a title = %+v, %v, want the description cleared and no status", renamed, err)
	}

	ghost, err := service.Playlists.Update([]string{"snippet"}, &ytapi.Playlist{
		Id: "PLdeleted", Snippet: &ytapi.PlaylistSnippet{Title: "Renamed"},
	}).Context(ctx).Do()
	if err != nil || ghost.Snippet.Title != "Renamed" {
		t.Errorf("an update to a deleted playlist = %+v, %v, want it accepted", ghost, err)
	}

	refusals := map[string]struct {
		do     func() error
		code   int
		reason string
	}{
		"an update with no resource id": {
			do: func() error {
				_, err := service.PlaylistItems.Update([]string{"snippet"}, &ytapi.PlaylistItem{
					Id: "PLA-item-000", Snippet: &ytapi.PlaylistItemSnippet{PlaylistId: "PLA", Position: 1},
				}).Context(ctx).Do()
				return err
			},
			code: http.StatusBadRequest, reason: "invalidResourceType",
		},
		"an update to a deleted item": {
			do: func() error {
				_, err := service.PlaylistItems.Update([]string{"snippet"}, &ytapi.PlaylistItem{
					Id: "PLA-item-gone", Snippet: &ytapi.PlaylistItemSnippet{PlaylistId: "PLA", ResourceId: video},
				}).Context(ctx).Do()
				return err
			},
			code: http.StatusBadRequest, reason: "invalidSnippet",
		},
		"a playlist update with no title": {
			do: func() error {
				_, err := service.Playlists.Update([]string{"snippet"}, &ytapi.Playlist{
					Id: "PLA", Snippet: &ytapi.PlaylistSnippet{Description: "no title"},
				}).Context(ctx).Do()
				return err
			},
			code: http.StatusBadRequest, reason: "playlistTitleRequired",
		},
	}
	for name, c := range refusals {
		var google *googleapi.Error
		if err := c.do(); !errors.As(err, &google) || google.Code != c.code || len(google.Errors) != 1 || google.Errors[0].Reason != c.reason {
			t.Errorf("%s = %v, want a %d %s", name, err, c.code, c.reason)
		}
	}
}

// assertVideos fails t unless the playlist holds exactly videoIDs, in order, and
// returns its items.
func assertVideos(t *testing.T, channel *Channel, playlist PlaylistID, videoIDs ...VideoID) []Item {
	t.Helper()
	items, err := channel.Items(context.Background(), playlist)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	var got []VideoID
	for _, item := range items {
		got = append(got, item.VideoID)
	}
	if !slices.Equal(got, videoIDs) {
		t.Fatalf("playlist %s holds %v, want %v", playlist, got, videoIDs)
	}
	return items
}
