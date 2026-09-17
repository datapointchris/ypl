package youtube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"google.golang.org/api/googleapi"
	ytapi "google.golang.org/api/youtube/v3"
)

func TestPlaylistsReadsEveryPage(t *testing.T) {
	api := newFakeAPI(t)
	for i := range 55 {
		api.playlists = append(api.playlists, fakePlaylist(t, fmt.Sprintf("PL%02d", i)))
	}
	channel := api.channel()

	playlists, err := channel.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	if len(playlists) != 55 {
		t.Fatalf("playlists = %d, want 55", len(playlists))
	}
	want := Playlist{ID: "PL07", Title: "Playlist PL07", Description: "About PL07", Privacy: "private"}
	if playlists[7] != want {
		t.Fatalf("playlist 7 = %+v, want %+v", playlists[7], want)
	}
	if served, made := api.requests.Load(), channel.Requests(); served != 2 || made != 2 {
		t.Fatalf("served %d requests and the channel counted %d, want 2 pages of 50", served, made)
	}
}

func TestItemsReadsEveryPageInPositionOrder(t *testing.T) {
	api := newFakeAPI(t)
	all := fakeItems(t, "PLA", 120)
	slices.Reverse(all)
	api.items["PLA"] = all
	channel := api.channel()

	got, err := channel.Items(context.Background(), "PLA")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("items = %d, want 120", len(got))
	}
	for i, it := range got {
		if it.Position != int64(i) {
			t.Fatalf("item %d has position %d", i, it.Position)
		}
	}
	want := Item{ID: "PLA-item-042", PlaylistID: "PLA", VideoID: "vid042", Position: 42, Title: "Two Hours of House", ChannelTitle: "A Mix Channel"}
	if got[42] != want {
		t.Fatalf("item 42 = %+v, want %+v", got[42], want)
	}
	if served, made := api.requests.Load(), channel.Requests(); served != 3 || made != 3 {
		t.Fatalf("served %d requests and the channel counted %d, want 3 pages of 50", served, made)
	}
}

func TestRecordedItemsReadAsYouTubeReportsThem(t *testing.T) {
	const playlist = "PLx0recordedpagex0000000000000000a"
	api := newFakeAPI(t)
	api.items[playlist] = recordedItems(t, "playlistItems.json")

	got, err := api.channel().Items(context.Background(), playlist)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	want := []Item{
		{
			ID:           "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4wQTFCMkMzRDRFNUY2QTdC",
			PlaylistID:   playlist,
			VideoID:      "x0public001",
			Position:     0,
			Title:        "Two Hours of House",
			ChannelTitle: "A Mix Channel",
		},
		{
			ID:          "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4xQjJDM0Q0RTVGNkE3QjhD",
			PlaylistID:  playlist,
			VideoID:     "x0deleted02",
			Position:    1,
			Title:       "Deleted video",
			Unavailable: true,
		},
		{
			ID:          "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4yQzNENEU1RjZBN0I4QzlE",
			PlaylistID:  playlist,
			VideoID:     "x0private03",
			Position:    2,
			Title:       "Private video",
			Unavailable: true,
		},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %+v\nwant %+v", got, want)
	}
}

func TestAResourceMissingAPartIsRefused(t *testing.T) {
	playlists := func(c *Channel) error { _, err := c.Playlists(context.Background()); return err }
	items := func(c *Channel) error { _, err := c.Items(context.Background(), "PLA"); return err }
	cases := map[string]struct {
		read  func(*Channel) error
		strip func(api *fakeAPI)
	}{
		"a playlist with no snippet": {playlists, func(api *fakeAPI) { delete(api.playlists[0], "snippet") }},
		"a playlist with no status":  {playlists, func(api *fakeAPI) { delete(api.playlists[0], "status") }},
		"an item with no snippet":    {items, func(api *fakeAPI) { delete(api.items["PLA"][0], "snippet") }},
		"an item with no status":     {items, func(api *fakeAPI) { delete(api.items["PLA"][0], "status") }},
		"an item with no resource id": {items, func(api *fakeAPI) {
			delete(api.items["PLA"][0]["snippet"].(map[string]any), "resourceId")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
			api.items["PLA"] = fakeItems(t, "PLA", 1)
			c.strip(api)

			if err := c.read(api.channel()); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("read = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

func TestEveryKnownPrivacyStatusIsReadAndAnyOtherRefused(t *testing.T) {
	cases := map[string]struct {
		unavailable bool
		refused     bool
	}{
		"public":                   {},
		"unlisted":                 {},
		"private":                  {unavailable: true},
		"privacyStatusUnspecified": {unavailable: true},
		"someFutureStatus":         {refused: true},
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			api := newFakeAPI(t)
			api.items["PLA"] = fakeItems(t, "PLA", 1)
			api.items["PLA"][0]["status"] = map[string]any{"privacyStatus": status}

			items, err := api.channel().Items(context.Background(), "PLA")
			if want.refused {
				if !errors.Is(err, ErrUnexpectedResponse) {
					t.Fatalf("Items = %v, want ErrUnexpectedResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Items: %v", err)
			}
			if items[0].Unavailable != want.unavailable {
				t.Fatalf("Unavailable = %v, want %v", items[0].Unavailable, want.unavailable)
			}
		})
	}
}

func TestEveryKnownPlaylistPrivacyIsReadAndAnyOtherRefused(t *testing.T) {
	for _, privacy := range []string{"public", "unlisted", "private", "someFuturePrivacy"} {
		t.Run(privacy, func(t *testing.T) {
			api := newFakeAPI(t)
			api.playlists = []map[string]any{fakePlaylist(t, "PLA")}
			api.playlists[0]["status"] = map[string]any{"privacyStatus": privacy}

			playlists, err := api.channel().Playlists(context.Background())
			if privacy == "someFuturePrivacy" {
				if !errors.Is(err, ErrUnexpectedResponse) {
					t.Fatalf("Playlists = %v, want ErrUnexpectedResponse", err)
				}
				return
			}
			if err != nil || playlists[0].Privacy != privacy {
				t.Fatalf("Playlists = %+v, %v, want privacy %s", playlists, err, privacy)
			}
		})
	}
}

func TestAReadThatDisagreesWithItsTotalIsRefused(t *testing.T) {
	for _, total := range []int{3, 1} {
		api := newFakeAPI(t)
		api.items["PLA"] = fakeItems(t, "PLA", 2)
		api.itemsTotal["PLA"] = total

		if _, err := api.channel().Items(context.Background(), "PLA"); !errors.Is(err, ErrInconsistentRead) {
			t.Errorf("Items with 2 served and a total of %d = %v, want ErrInconsistentRead", total, err)
		}
	}
}

func TestAGapInPositionsIsRefused(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = []map[string]any{fakeItem(t, recordedPublic, "PLA", 0), fakeItem(t, recordedPublic, "PLA", 2)}

	if _, err := api.channel().Items(context.Background(), "PLA"); !errors.Is(err, ErrInconsistentRead) {
		t.Fatalf("Items with positions 0 and 2 = %v, want ErrInconsistentRead", err)
	}
}

// Each edit lands after the first page is served and before the second.
func TestAnEditBetweenPagesIsRefused(t *testing.T) {
	cases := map[string]struct {
		read func(*Channel) error
		edit func(api *fakeAPI)
	}{
		"an item moved to the end": {
			read: func(ch *Channel) error { _, err := ch.Items(context.Background(), "PLA"); return err },
			edit: func(api *fakeAPI) {
				items := api.items["PLA"]
				api.items["PLA"] = append(items[1:], items[0])
				renumber(api.items["PLA"])
			},
		},
		"an item deleted": {
			read: func(ch *Channel) error { _, err := ch.Items(context.Background(), "PLA"); return err },
			edit: func(api *fakeAPI) {
				api.items["PLA"] = api.items["PLA"][1:]
				renumber(api.items["PLA"])
			},
		},
		"a playlist moved to the end": {
			read: func(ch *Channel) error { _, err := ch.Playlists(context.Background()); return err },
			edit: func(api *fakeAPI) { api.playlists = append(api.playlists[1:], api.playlists[0]) },
		},
		"a playlist deleted": {
			read: func(ch *Channel) error { _, err := ch.Playlists(context.Background()); return err },
			edit: func(api *fakeAPI) { api.playlists = api.playlists[1:] },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.items["PLA"] = fakeItems(t, "PLA", 120)
			for i := range 55 {
				api.playlists = append(api.playlists, fakePlaylist(t, fmt.Sprintf("PL%02d", i)))
			}
			api.beforeRequest = func(request int64) {
				if request == 2 {
					c.edit(api)
				}
			}

			if err := c.read(api.channel()); !errors.Is(err, ErrInconsistentRead) {
				t.Fatalf("read = %v, want ErrInconsistentRead", err)
			}
		})
	}
}

// Channel's documentation says a read cannot see this edit, so an absence is not
// a deletion. This pins that the documentation is still true.
func TestADeleteAndAnAddBetweenPagesGoUnseen(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 120)
	added := fakeItem(t, recordedPublic, "PLA", 999)
	api.beforeRequest = func(request int64) {
		if request == 2 {
			api.items["PLA"] = append(api.items["PLA"][1:], added)
			renumber(api.items["PLA"])
		}
	}

	items, err := api.channel().Items(context.Background(), "PLA")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	missing := !slices.ContainsFunc(items, func(it Item) bool { return it.ID == "PLA-item-050" })
	if len(items) != 120 || !missing {
		t.Fatalf("read %d items with PLA-item-050 missing = %v, want 120 items with it missing", len(items), missing)
	}
}

// 55 ids take two requests of 50 and 5.
func TestVideosReadsEachVideoYouTubeReturns(t *testing.T) {
	api := newFakeAPI(t)
	api.videos["vidA"] = publicVideo("vidA")
	api.videos["private"] = privateVideo()
	channel := api.channel()
	ids := []VideoID{"vidA", "private", "unknown"}
	for i := range 52 {
		ids = append(ids, VideoID(fmt.Sprintf("none%02d", i)))
	}
	api.videos["none51"] = publicVideo("none51")

	videos, err := channel.Videos(context.Background(), ids)
	want := []Video{
		{ID: "vidA", Title: "Video vidA", ChannelTitle: "Channel of vidA", Privacy: "public"},
		{ID: "none51", Title: "Video none51", ChannelTitle: "Channel of none51", Privacy: "public"},
	}
	if err != nil || !slices.Equal(videos, want) {
		t.Fatalf("Videos = %+v, %v, want %+v", videos, err, want)
	}
	if requests, units := channel.Requests(), channel.Units(); requests != 2 || units != 2 {
		t.Fatalf("counted %d requests and %d units, want 2 and 2", requests, units)
	}
}

func TestExistingItemsFindsOnlyTheItemsYouTubeStillHas(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = fakeItems(t, "PLA", 3)
	channel := api.channel()
	ctx := context.Background()
	if err := channel.DeleteItem(ctx, "PLA-item-001"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	ids := []ItemID{"PLA-item-000", "PLA-item-001"}
	for i := range 49 {
		ids = append(ids, ItemID(fmt.Sprintf("PLmadeup-%02d", i)))
	}
	ids = append(ids, "PLA-item-002")

	found, err := channel.ExistingItems(ctx, ids)
	if err != nil || !slices.Equal(found, []ItemID{"PLA-item-000", "PLA-item-002"}) {
		t.Fatalf("ExistingItems = %v, %v, want PLA-item-000 and PLA-item-002", found, err)
	}
	if requests := channel.Requests(); requests != 3 {
		t.Fatalf("counted %d requests, want the delete and two reads", requests)
	}
}

func TestYouTubesQuotaRefusalIsErrQuotaSpent(t *testing.T) {
	api := newFakeAPI(t)
	api.answer = &fakeAnswer{
		status: http.StatusForbidden,
		body:   `{"error":{"code":403,"message":"quota","errors":[{"reason":"quotaExceeded","domain":"youtube.quota"}]}}`,
	}

	channel := api.channel()

	if _, err := channel.Playlists(context.Background()); !errors.Is(err, ErrQuotaSpent) || !errors.Is(err, ErrRefused) {
		t.Errorf("Playlists on YouTube's quotaExceeded = %v, want ErrQuotaSpent and ErrRefused", err)
	}
	if _, err := channel.InsertItem(context.Background(), "PLA", "vidA", 0); !errors.Is(err, ErrQuotaSpent) {
		t.Errorf("InsertItem on YouTube's quotaExceeded = %v, want ErrQuotaSpent", err)
	}
}

// The fake answers each of these as the Data API did when the same request was
// made against it.
func TestTheFakeAnswersAsTheDataAPIDoes(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA"), fakePlaylist(t, "PLB")}
	api.items["PLA"] = fakeItems(t, "PLA", 17)
	server := api.channel().service
	ctx := context.Background()
	items := func(parts ...string) *ytapi.PlaylistItemsListCall {
		return server.PlaylistItems.List(parts).PlaylistId("PLA")
	}

	pageSizes := map[string]struct {
		call *ytapi.PlaylistItemsListCall
		want int
		next bool
	}{
		"no maxResults": {call: items("snippet"), want: 5, next: true},
		"maxResults=51": {call: items("snippet").MaxResults(51), want: 17},
		"maxResults=0":  {call: items("snippet").MaxResults(0), want: 0, next: true},
		"maxResults=10": {call: items("snippet").MaxResults(10), want: 10, next: true},
		"a second page": {call: items("snippet").MaxResults(10).PageToken(fakeToken(10)), want: 7},
		"no part":       {call: items(), want: 5, next: true},
		"every part":    {call: items("id", "snippet", "status", "contentDetails").MaxResults(50), want: 17},
	}
	for name, c := range pageSizes {
		response, err := c.call.Context(ctx).Do()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(response.Items) != c.want || (response.NextPageToken != "") != c.next {
			t.Errorf("%s: %d items and next page %v, want %d and %v", name, len(response.Items), response.NextPageToken != "", c.want, c.next)
		}
	}

	onlyID, err := items().Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if first := onlyID.Items[0]; first.Id == "" || first.Snippet != nil || first.Status != nil {
		t.Errorf("a request naming no part = %+v, want the id and no parts", first)
	}

	playlists, err := server.Playlists.List([]string{"snippet"}).Mine(true).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if playlists.PageInfo.TotalResults <= int64(len(playlists.Items)) {
		t.Errorf("playlists total %d with %d listed, want a total above the count", playlists.PageInfo.TotalResults, len(playlists.Items))
	}

	byID := func(id string) *ytapi.PlaylistListResponse {
		response, err := server.Playlists.List([]string{"snippet", "status"}).Id(id).MaxResults(50).Context(ctx).Do()
		if err != nil {
			t.Fatalf("a read of %s by id: %v", id, err)
		}
		return response
	}
	if held := byID("PLB"); len(held.Items) != 1 || held.Items[0].Id != "PLB" || held.PageInfo.TotalResults != 1 || held.NextPageToken != "" {
		t.Errorf("a read by id of a playlist held = %+v, want it alone with a total of 1 and no next page", held)
	}
	if unknown := byID("PLnoSuchPlaylist"); len(unknown.Items) != 0 || unknown.PageInfo.TotalResults != 0 {
		t.Errorf("a read by id of an id nothing has = %+v, want no playlists and a total of 0", unknown)
	}

	api.videos["x0public001"] = publicVideo("x0public001")
	api.videos["x0private01"] = privateVideo()
	videos, err := server.Videos.List([]string{"snippet", "status"}).Id("x0public001,x0private01,x0deleted01,zzzzzzzzzzz").Context(ctx).Do()
	if err != nil || len(videos.Items) != 1 || videos.Items[0].Id != "x0public001" || videos.PageInfo.TotalResults != 1 || videos.Items[0].Status.PrivacyStatus != "public" {
		t.Errorf("a read of an available, a private, a deleted and a made-up video = %+v, %v, want the available one alone", videos, err)
	}
	onlyUnknown, err := server.Videos.List([]string{"snippet", "status"}).Id("zzzzzzzzzzz").Context(ctx).Do()
	if err != nil || len(onlyUnknown.Items) != 0 || onlyUnknown.PageInfo.TotalResults != 0 {
		t.Errorf("a read of a made-up video = %+v, %v, want no videos and a total of 0", onlyUnknown, err)
	}
	itemsByID, err := server.PlaylistItems.List([]string{"id"}).Id("PLA-item-000,UExmYWtlSXRlbUlk").MaxResults(50).Context(ctx).Do()
	if err != nil || len(itemsByID.Items) != 1 || itemsByID.Items[0].Id != "PLA-item-000" || itemsByID.PageInfo.TotalResults != 1 {
		t.Errorf("a read by id of an item held and a made-up one = %+v, %v, want the item held alone", itemsByID, err)
	}

	refusals := map[string]struct {
		call interface {
			Do(...googleapi.CallOption) (*ytapi.PlaylistItemListResponse, error)
		}
		reason string
	}{
		"an unknown part":              {call: items("snippet", "bogus"), reason: "unknownPart"},
		"no filter":                    {call: server.PlaylistItems.List([]string{"snippet"}), reason: "missingRequiredParameter"},
		"a page token it never issued": {call: items("snippet").PageToken("garbage"), reason: "invalidPageToken"},
	}
	for name, c := range refusals {
		_, err := c.call.Do()
		var google *googleapi.Error
		if !errors.As(err, &google) || google.Code != http.StatusBadRequest || len(google.Errors) != 1 || google.Errors[0].Reason != c.reason {
			t.Errorf("%s = %v, want a 400 %s", name, err, c.reason)
		}
	}
}

func TestCredentialsFromEnvNamesEveryMissingVariable(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "id")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	t.Setenv("YOUTUBE_REFRESH_TOKEN", "")

	_, err := CredentialsFromEnv()
	if !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("CredentialsFromEnv = %v, want ErrMissingCredentials", err)
	}
	if want := "missing YouTube credentials: YOUTUBE_CLIENT_SECRET, YOUTUBE_REFRESH_TOKEN"; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}

	t.Setenv("YOUTUBE_CLIENT_SECRET", "secret")
	client, err := ClientFromEnv()
	if err != nil || client != (Client{ID: "id", Secret: "secret"}) {
		t.Fatalf("ClientFromEnv = %+v, %v, want the id and secret with no refresh token", client, err)
	}
}
