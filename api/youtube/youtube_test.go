package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/option"
	ytapi "google.golang.org/api/youtube/v3"

	"github.com/datapointchris/ypl/api/store"
)

// fakeAPI serves playlists.list and playlistItems.list in the Data API's shape,
// paged by an offset carried in pageToken, and counts the requests it answers.
type fakeAPI struct {
	playlists []map[string]any
	items     map[string][]map[string]any
	status    int
	body      string
	requests  atomic.Int64
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}

	var all []map[string]any
	switch r.URL.Path {
	case "/youtube/v3/playlists":
		all = f.playlists
	case "/youtube/v3/playlistItems":
		all = f.items[r.URL.Query().Get("playlistId")]
	default:
		http.NotFound(w, r)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	end := min(offset+pageSize, len(all))
	page := map[string]any{"kind": "youtube#listResponse", "items": all[offset:end]}
	if end < len(all) {
		page["nextPageToken"] = strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(page)
}

func playlistResource(id string, itemCount int) map[string]any {
	return map[string]any{
		"kind":           "youtube#playlist",
		"id":             id,
		"snippet":        map[string]any{"title": "Title " + id, "description": "About " + id, "channelTitle": "A Channel"},
		"status":         map[string]any{"privacyStatus": "private"},
		"contentDetails": map[string]any{"itemCount": itemCount},
	}
}

func itemResource(playlistID string, position int, privacy string) map[string]any {
	snippet := map[string]any{
		"playlistId": playlistID,
		"position":   position,
		"title":      fmt.Sprintf("Video %d", position),
		"resourceId": map[string]any{"kind": "youtube#video", "videoId": fmt.Sprintf("vid%03d", position)},
	}
	if privacy == "public" || privacy == "unlisted" {
		snippet["videoOwnerChannelTitle"] = "Owner"
	}
	return map[string]any{
		"kind":    "youtube#playlistItem",
		"id":      fmt.Sprintf("%s-item-%03d", playlistID, position),
		"snippet": snippet,
		"status":  map[string]any{"privacyStatus": privacy},
	}
}

func items(playlistID string, n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range n {
		out[i] = itemResource(playlistID, i, "public")
	}
	return out
}

type fixture struct {
	api    *fakeAPI
	ledger *Ledger
	reader *Reader
}

func newFixture(t *testing.T, api *fakeAPI, limit int64) fixture {
	t.Helper()
	ctx := context.Background()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	service, err := ytapi.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ledger := NewLedger(st.Queries, limit)
	return fixture{api: api, ledger: ledger, reader: NewReader(service, ledger)}
}

func spent(t *testing.T, ledger *Ledger) int64 {
	t.Helper()
	units, err := ledger.Spent(context.Background())
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	return units
}

func TestQuotaDateFollowsPacificMidnight(t *testing.T) {
	cases := map[string]string{
		"2026-09-17T06:59:59Z": "2026-09-16", // 23:59:59 PDT
		"2026-09-17T07:00:00Z": "2026-09-17", // 00:00 PDT
		"2026-01-15T07:59:59Z": "2026-01-14", // 23:59:59 PST
		"2026-01-15T08:00:00Z": "2026-01-15", // 00:00 PST
	}
	for at, want := range cases {
		instant, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := QuotaDate(instant); got != want {
			t.Errorf("QuotaDate(%s) = %s, want %s", at, got, want)
		}
	}
}

func TestPlaylistsReadsEveryPage(t *testing.T) {
	api := &fakeAPI{}
	for i := range 55 {
		api.playlists = append(api.playlists, playlistResource(fmt.Sprintf("PL%02d", i), i))
	}
	f := newFixture(t, api, DailyQuota)

	playlists, err := f.reader.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	if len(playlists) != 55 {
		t.Fatalf("playlists = %d, want 55", len(playlists))
	}
	want := Playlist{ID: "PL07", Title: "Title PL07", Description: "About PL07", Privacy: "private", ItemCount: 7}
	if playlists[7] != want {
		t.Fatalf("playlist 7 = %+v, want %+v", playlists[7], want)
	}
	if requests, units := f.api.requests.Load(), spent(t, f.ledger); requests != 2 || units != 2 {
		t.Fatalf("requests %d and units %d, want 2 pages charged 2 units", requests, units)
	}
}

func TestItemsReadsEveryPageInPositionOrder(t *testing.T) {
	api := &fakeAPI{items: map[string][]map[string]any{"PLA": items("PLA", 120)}}
	// Serve the slots out of order, so the position order is the reader's.
	all := api.items["PLA"]
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	f := newFixture(t, api, DailyQuota)

	got, err := f.reader.Items(context.Background(), Playlist{ID: "PLA", ItemCount: 120})
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
	want := Item{ID: "PLA-item-042", VideoID: "vid042", Position: 42, Title: "Video 42", ChannelTitle: "Owner"}
	if got[42] != want {
		t.Fatalf("item 42 = %+v, want %+v", got[42], want)
	}
	if requests, units := f.api.requests.Load(), spent(t, f.ledger); requests != 3 || units != 3 {
		t.Fatalf("requests %d and units %d, want 3 pages charged 3 units", requests, units)
	}
}

func TestPrivateAndDeletedVideosAreUnavailable(t *testing.T) {
	api := &fakeAPI{items: map[string][]map[string]any{"PLA": {
		itemResource("PLA", 0, "public"),
		itemResource("PLA", 1, "unlisted"),
		itemResource("PLA", 2, "private"),
		itemResource("PLA", 3, "privacyStatusUnspecified"),
	}}}
	f := newFixture(t, api, DailyQuota)

	got, err := f.reader.Items(context.Background(), Playlist{ID: "PLA", ItemCount: 4})
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	for i, wantUnavailable := range []bool{false, false, true, true} {
		if got[i].Unavailable != wantUnavailable {
			t.Errorf("item %d Unavailable = %v, want %v", i, got[i].Unavailable, wantUnavailable)
		}
	}
	if got[2].ChannelTitle != "" || got[0].ChannelTitle != "Owner" {
		t.Errorf("channel titles = %q and %q, want Owner for the public video and none for the private one", got[0].ChannelTitle, got[2].ChannelTitle)
	}
}

func TestAReadThatDisagreesWithTheCountIsRefused(t *testing.T) {
	api := &fakeAPI{items: map[string][]map[string]any{"PLA": items("PLA", 2)}}
	f := newFixture(t, api, DailyQuota)

	for _, count := range []int64{3, 1} {
		_, err := f.reader.Items(context.Background(), Playlist{ID: "PLA", ItemCount: count})
		if !errors.Is(err, ErrInconsistentRead) {
			t.Errorf("Items with 2 served and %d reported = %v, want ErrInconsistentRead", count, err)
		}
	}
}

func TestAGapInPositionsIsRefused(t *testing.T) {
	api := &fakeAPI{items: map[string][]map[string]any{"PLA": {
		itemResource("PLA", 0, "public"),
		itemResource("PLA", 2, "public"),
	}}}
	f := newFixture(t, api, DailyQuota)

	_, err := f.reader.Items(context.Background(), Playlist{ID: "PLA", ItemCount: 2})
	if !errors.Is(err, ErrInconsistentRead) {
		t.Fatalf("Items with positions 0 and 2 = %v, want ErrInconsistentRead", err)
	}
}

func TestASpentDayMakesNoFurtherRequest(t *testing.T) {
	api := &fakeAPI{items: map[string][]map[string]any{"PLA": items("PLA", 120)}}
	f := newFixture(t, api, 2)

	_, err := f.reader.Items(context.Background(), Playlist{ID: "PLA", ItemCount: 120})
	if !errors.Is(err, ErrQuotaSpent) {
		t.Fatalf("Items with a 2-unit day and 3 pages = %v, want ErrQuotaSpent", err)
	}
	if requests, units := f.api.requests.Load(), spent(t, f.ledger); requests != 2 || units != 2 {
		t.Fatalf("requests %d and units %d, want the 2 pages the day covered", requests, units)
	}
}

func TestTheQuotaReturnsAtPacificMidnight(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, &fakeAPI{}, 1)
	f.ledger.now = func() time.Time { return time.Date(2026, 9, 17, 6, 59, 0, 0, time.UTC) } // 23:59 PDT

	if err := f.ledger.Spend(ctx, MethodPlaylistsList); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := f.ledger.Spend(ctx, MethodPlaylistsList); !errors.Is(err, ErrQuotaSpent) {
		t.Fatalf("second spend on a 1-unit day = %v, want ErrQuotaSpent", err)
	}

	f.ledger.now = func() time.Time { return time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC) } // 00:00 PDT
	if err := f.ledger.Spend(ctx, MethodPlaylistsList); err != nil {
		t.Fatalf("spend after Pacific midnight: %v", err)
	}
}

// Charges racing for the last units of a day never take it past the limit.
func TestConcurrentChargesStopAtTheLimit(t *testing.T) {
	const limit, callers = 20, 60
	ctx := context.Background()
	f := newFixture(t, &fakeAPI{}, limit)

	var wg sync.WaitGroup
	var charged, refused atomic.Int64
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			err := f.ledger.Spend(ctx, MethodPlaylistsList)
			switch {
			case err == nil:
				charged.Add(1)
			case errors.Is(err, ErrQuotaSpent):
				refused.Add(1)
			default:
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Spend: %v", err)
	}

	if charged.Load() != limit || refused.Load() != callers-limit || spent(t, f.ledger) != limit {
		t.Fatalf("charged %d, refused %d, spent %d; want %d charged, %d refused, %d spent",
			charged.Load(), refused.Load(), spent(t, f.ledger), limit, callers-limit, limit)
	}
}

func TestYouTubesOwnQuotaRefusalIsErrQuotaSpent(t *testing.T) {
	api := &fakeAPI{
		status: http.StatusForbidden,
		body:   `{"error":{"code":403,"message":"quota","errors":[{"reason":"quotaExceeded","domain":"youtube.quota"}]}}`,
	}
	f := newFixture(t, api, DailyQuota)

	_, err := f.reader.Playlists(context.Background())
	if !errors.Is(err, ErrQuotaSpent) {
		t.Fatalf("Playlists on YouTube's quotaExceeded = %v, want ErrQuotaSpent", err)
	}
}

func TestAnUnseededMethodIsNotReportedAsSpent(t *testing.T) {
	f := newFixture(t, &fakeAPI{}, DailyQuota)
	err := f.ledger.Spend(context.Background(), "videos.list")
	if err == nil || errors.Is(err, ErrQuotaSpent) {
		t.Fatalf("Spend of an unseeded method = %v, want an error that is not ErrQuotaSpent", err)
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
	if !strings.Contains(err.Error(), "YOUTUBE_CLIENT_SECRET, YOUTUBE_REFRESH_TOKEN") {
		t.Fatalf("error %q does not name both missing variables", err)
	}
}
