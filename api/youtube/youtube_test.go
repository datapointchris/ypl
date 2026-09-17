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
	reader := api.reader()

	playlists, err := reader.Playlists(context.Background())
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
	if served, made := api.requests.Load(), reader.Requests(); served != 2 || made != 2 {
		t.Fatalf("served %d requests and the reader counted %d, want 2 pages of 50", served, made)
	}
}

func TestItemsReadsEveryPageInPositionOrder(t *testing.T) {
	api := newFakeAPI(t)
	all := fakeItems(t, "PLA", 120)
	slices.Reverse(all)
	api.items["PLA"] = all
	reader := api.reader()

	got, err := reader.Items(context.Background(), "PLA")
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
	want := Item{ID: "PLA-item-042", VideoID: "vid042", Position: 42, Title: "Two Hours of House", ChannelTitle: "A Mix Channel"}
	if got[42] != want {
		t.Fatalf("item 42 = %+v, want %+v", got[42], want)
	}
	if served, made := api.requests.Load(), reader.Requests(); served != 3 || made != 3 {
		t.Fatalf("served %d requests and the reader counted %d, want 3 pages of 50", served, made)
	}
}

func TestRecordedItemsReadAsYouTubeReportsThem(t *testing.T) {
	const playlist = "PLx0recordedpagex0000000000000000a"
	api := newFakeAPI(t)
	api.items[playlist] = recordedItems(t, "playlistItems.json")

	got, err := api.reader().Items(context.Background(), playlist)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	want := []Item{
		{
			ID:           "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4wQTFCMkMzRDRFNUY2QTdC",
			VideoID:      "x0public001",
			Position:     0,
			Title:        "Two Hours of House",
			ChannelTitle: "A Mix Channel",
		},
		{
			ID:          "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4xQjJDM0Q0RTVGNkE3QjhD",
			VideoID:     "x0deleted02",
			Position:    1,
			Title:       "Deleted video",
			Unavailable: true,
		},
		{
			ID:          "UEx4MHJlY29yZGVkcGFnZXgwMDAwMDAwMDAwMDAwMDAwYS4yQzNENEU1RjZBN0I4QzlE",
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
	playlists := func(r *Reader) error { _, err := r.Playlists(context.Background()); return err }
	items := func(r *Reader) error { _, err := r.Items(context.Background(), "PLA"); return err }
	cases := map[string]struct {
		read  func(*Reader) error
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

			if err := c.read(api.reader()); !errors.Is(err, ErrUnexpectedResponse) {
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

			items, err := api.reader().Items(context.Background(), "PLA")
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

func TestAReadThatDisagreesWithItsTotalIsRefused(t *testing.T) {
	for _, total := range []int{3, 1} {
		api := newFakeAPI(t)
		api.items["PLA"] = fakeItems(t, "PLA", 2)
		api.itemsTotal["PLA"] = total

		if _, err := api.reader().Items(context.Background(), "PLA"); !errors.Is(err, ErrInconsistentRead) {
			t.Errorf("Items with 2 served and a total of %d = %v, want ErrInconsistentRead", total, err)
		}
	}
}

func TestAGapInPositionsIsRefused(t *testing.T) {
	api := newFakeAPI(t)
	api.items["PLA"] = []map[string]any{fakeItem(t, recordedPublic, "PLA", 0), fakeItem(t, recordedPublic, "PLA", 2)}

	if _, err := api.reader().Items(context.Background(), "PLA"); !errors.Is(err, ErrInconsistentRead) {
		t.Fatalf("Items with positions 0 and 2 = %v, want ErrInconsistentRead", err)
	}
}

// Each edit lands after the first page is served and before the second.
func TestAnEditBetweenPagesIsRefused(t *testing.T) {
	cases := map[string]struct {
		read func(*Reader) error
		edit func(api *fakeAPI)
	}{
		"an item moved to the end": {
			read: func(r *Reader) error { _, err := r.Items(context.Background(), "PLA"); return err },
			edit: func(api *fakeAPI) {
				items := api.items["PLA"]
				api.items["PLA"] = append(items[1:], items[0])
				renumber(api.items["PLA"])
			},
		},
		"an item deleted": {
			read: func(r *Reader) error { _, err := r.Items(context.Background(), "PLA"); return err },
			edit: func(api *fakeAPI) {
				api.items["PLA"] = api.items["PLA"][1:]
				renumber(api.items["PLA"])
			},
		},
		"a playlist moved to the end": {
			read: func(r *Reader) error { _, err := r.Playlists(context.Background()); return err },
			edit: func(api *fakeAPI) { api.playlists = append(api.playlists[1:], api.playlists[0]) },
		},
		"a playlist deleted": {
			read: func(r *Reader) error { _, err := r.Playlists(context.Background()); return err },
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

			if err := c.read(api.reader()); !errors.Is(err, ErrInconsistentRead) {
				t.Fatalf("read = %v, want ErrInconsistentRead", err)
			}
		})
	}
}

// Reader's documentation says a read cannot see this edit, so an absence is not
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

	items, err := api.reader().Items(context.Background(), "PLA")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	missing := !slices.ContainsFunc(items, func(it Item) bool { return it.ID == "PLA-item-050" })
	if len(items) != 120 || !missing {
		t.Fatalf("read %d items with PLA-item-050 missing = %v, want 120 items with it missing", len(items), missing)
	}
}

func TestYouTubesQuotaRefusalIsErrQuotaSpent(t *testing.T) {
	api := newFakeAPI(t)
	api.refusal = &fakeRefusal{
		status: http.StatusForbidden,
		body:   `{"error":{"code":403,"message":"quota","errors":[{"reason":"quotaExceeded","domain":"youtube.quota"}]}}`,
	}

	if _, err := api.reader().Playlists(context.Background()); !errors.Is(err, ErrQuotaSpent) {
		t.Fatalf("Playlists on YouTube's quotaExceeded = %v, want ErrQuotaSpent", err)
	}
}

// The fake answers each of these as the Data API did when the same request was
// made against it.
func TestTheFakeAnswersAsTheDataAPIDoes(t *testing.T) {
	api := newFakeAPI(t)
	api.playlists = []map[string]any{fakePlaylist(t, "PLA"), fakePlaylist(t, "PLB")}
	api.items["PLA"] = fakeItems(t, "PLA", 17)
	server := api.reader().service
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
