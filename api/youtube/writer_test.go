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

	created, err := channel.CreatePlaylist(context.Background(), "Late Night", "Slow sets")
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	want := Playlist{ID: created.ID, Title: "Late Night", Description: "Slow sets", Privacy: "private"}
	if created.ID == "" || created != want {
		t.Fatalf("created %+v, want %+v with an id", created, want)
	}
	listed, err := channel.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	if !slices.Contains(listed, want) {
		t.Fatalf("playlists %+v, want %+v among them", listed, want)
	}
}

func TestTheFirstInsertIntoANewPlaylistIsSentAgain(t *testing.T) {
	api := newFakeAPI(t)
	api.videos["vidA"] = publicVideo("vidA")
	channel := api.channel()
	ctx := context.Background()

	created, err := channel.CreatePlaylist(ctx, "New", "")
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	item, err := channel.InsertItem(ctx, created.ID, "vidA", 0)
	if err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if item.VideoID != "vidA" || item.Position != 0 || item.PlaylistID != created.ID {
		t.Fatalf("inserted %+v, want vidA at 0 in %s", item, created.ID)
	}
	if !slices.Equal(api.pauses, []time.Duration{time.Second}) {
		t.Fatalf("paused %v, want one second before the second attempt", api.pauses)
	}
	api.mu.Lock()
	held := len(api.items[created.ID])
	api.mu.Unlock()
	if held != 1 {
		t.Fatalf("playlist holds %d items, want the one insert that landed", held)
	}
	if requests, units := channel.Requests(), channel.Units(); requests != 3 || units != 150 {
		t.Fatalf("counted %d requests and %d units, want a create and two insert attempts: 3 and 150", requests, units)
	}
}

func TestARequestYouTubeKeepsAbortingStopsAfterFourAttempts(t *testing.T) {
	api := newFakeAPI(t)
	api.refusal = &fakeRefusal{status: http.StatusConflict, body: string(api.recorded["playlistItems.insert SERVICE_UNAVAILABLE"])}
	channel := api.channel()

	_, err := channel.InsertItem(context.Background(), "PLA", "vidA", 0)
	if !aborted(err) {
		t.Fatalf("InsertItem = %v, want YouTube's abort", err)
	}
	if !slices.Equal(api.pauses, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("paused %v, want 1s, 2s and 4s", api.pauses)
	}
	if requests := channel.Requests(); requests != 4 {
		t.Fatalf("counted %d requests, want 4 attempts", requests)
	}
}

// Only the abort YouTube was measured answering is known to leave nothing
// behind, so a 409 with any other reason is not sent again. This body is not a
// recorded one: no request made against YouTube drew another 409.
func TestAConflictOtherThanAnAbortIsNotSentAgain(t *testing.T) {
	api := newFakeAPI(t)
	api.refusal = &fakeRefusal{
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
	api.refusal = &fakeRefusal{status: http.StatusConflict, body: string(api.recorded["playlistItems.insert SERVICE_UNAVAILABLE"])}
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
	last, err := channel.InsertItem(ctx, "PLA", "last", 4)
	if err != nil {
		t.Fatalf("InsertItem at the item count: %v", err)
	}
	if first.Position != 0 || last.Position != 4 {
		t.Fatalf("inserted at %d and %d, want 0 and 4", first.Position, last.Position)
	}
	want := Item{ID: first.ID, PlaylistID: "PLA", VideoID: "first", Position: 0, Title: "Video first", ChannelTitle: "Channel of first"}
	if first != want {
		t.Fatalf("inserted %+v, want %+v", first, want)
	}
	assertVideos(t, channel, "PLA", "first", "vid000", "vid001", "vid002", "last")
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
	two, err := channel.InsertItem(ctx, "PLA", "vidA", 1)
	if err != nil {
		t.Fatalf("second InsertItem: %v", err)
	}
	if one.ID == two.ID {
		t.Fatalf("both inserts returned item %s, want two items", one.ID)
	}
	assertVideos(t, channel, "PLA", "vidA", "vidA")
}

func TestAnInsertYouTubeRefusesIsNamed(t *testing.T) {
	cases := map[string]struct {
		playlist string
		video    string
		position int64
		want     error
	}{
		"a video YouTube has no record of":   {playlist: "PLA", video: "gone", want: ErrVideoNotFound},
		"a private video":                    {playlist: "PLA", video: "private", want: ErrVideoRefused},
		"a deleted playlist":                 {playlist: "PLdeleted", video: "vidA", want: ErrPlaylistNotFound},
		"a position past the playlist's end": {playlist: "PLA", video: "vidA", position: 3},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.items["PLA"] = fakeItems(t, "PLA", 2)
			api.deletedPlaylists["PLdeleted"] = true
			api.videos["vidA"] = publicVideo("vidA")
			api.videos["private"] = fakeVideo{title: "Private video", privacy: "private"}

			_, err := api.channel().InsertItem(context.Background(), c.playlist, c.video, c.position)
			if err == nil {
				t.Fatal("InsertItem succeeded, want a refusal")
			}
			for _, sentinel := range []error{ErrVideoNotFound, ErrVideoRefused, ErrPlaylistNotFound} {
				if errors.Is(err, sentinel) != (sentinel == c.want) {
					t.Fatalf("InsertItem = %v, want %v and no other sentinel", err, c.want)
				}
			}
		})
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

	if err := channel.MoveItem(ctx, items[1], 5); err == nil {
		t.Fatal("MoveItem to the item count succeeded, want YouTube's refusal")
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

func TestUpdatePlaylistSetsTheTitleAndKeepsTheDescription(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
	channel := api.channel()
	ctx := context.Background()

	renamed := Playlist{ID: "PLA", Title: "Renamed", Description: "About PLA", Privacy: "private"}
	if err := channel.UpdatePlaylist(ctx, renamed); err != nil {
		t.Fatalf("UpdatePlaylist: %v", err)
	}
	listed, err := channel.Playlists(ctx)
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	if !slices.Equal(listed, []Playlist{renamed}) {
		t.Fatalf("playlists %+v, want %+v", listed, renamed)
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
	if err := channel.MoveItem(ctx, items[59], 0); err != nil {
		t.Fatal(err)
	}
	if err := channel.DeleteItem(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := channel.UpdatePlaylist(ctx, Playlist{ID: "PLA", Title: "Renamed"}); err != nil {
		t.Fatal(err)
	}

	// One playlists page and two items pages at 1 unit, four writes at 50.
	if requests, units := channel.Requests(), channel.Units(); requests != 7 || units != 203 {
		t.Fatalf("counted %d requests and %d units, want 7 and 203", requests, units)
	}
	if served := api.requests.Load(); served != channel.Requests() {
		t.Fatalf("the fake served %d requests and the channel counted %d", served, channel.Requests())
	}
}

// The fake answers each of these writes as the Data API did when the same
// request was made against it. Channel never makes them, so no other test does.
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

// assertVideos fails t unless the playlist playlistID holds exactly videoIDs, in
// order.
func assertVideos(t *testing.T, channel *Channel, playlistID string, videoIDs ...string) {
	t.Helper()
	items, err := channel.Items(context.Background(), playlistID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	var got []string
	for _, item := range items {
		got = append(got, item.VideoID)
	}
	if !slices.Equal(got, videoIDs) {
		t.Fatalf("playlist %s holds %v, want %v", playlistID, got, videoIDs)
	}
}
