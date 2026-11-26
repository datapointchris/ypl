package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/datapointchris/goapiauth"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeDataAPI answers the Data API routes' calls, counting each request at the
// price the Data API's quota calculator gives its method.
type fakeDataAPI struct {
	requests, units atomic.Int64
	deleted         []youtube.ItemID
}

func (f *fakeDataAPI) spend(units int64) {
	f.requests.Add(1)
	f.units.Add(units)
}

func (f *fakeDataAPI) Items(context.Context, youtube.PlaylistID) ([]youtube.Item, error) {
	f.spend(1)
	return nil, nil
}

func (f *fakeDataAPI) AppendItem(context.Context, youtube.PlaylistID, youtube.VideoID) (youtube.ItemID, error) {
	f.spend(youtube.WriteUnits)
	return "item-new", nil
}

func (f *fakeDataAPI) DeleteItem(_ context.Context, id youtube.ItemID) error {
	f.spend(youtube.WriteUnits)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeDataAPI) Uploads(context.Context, string, string, int64) ([]youtube.Item, string, error) {
	f.spend(1)
	return nil, "", nil
}

func (f *fakeDataAPI) VideoDetails(_ context.Context, ids []youtube.VideoID) ([]youtube.VideoDetails, error) {
	f.spend(int64((len(ids) + 49) / 50))
	details := make([]youtube.VideoDetails, len(ids))
	for i, id := range ids {
		details[i] = youtube.VideoDetails{Video: youtube.Video{ID: id, Title: "Video " + string(id), Privacy: "public"}, LiveBroadcastContent: "none"}
	}
	return details, nil
}

func (f *fakeDataAPI) TopComments(context.Context, youtube.VideoID) ([]youtube.Comment, error) {
	f.spend(1)
	return nil, youtube.ErrCommentsDisabled
}

func (f *fakeDataAPI) Requests() int64 { return f.requests.Load() }
func (f *fakeDataAPI) Units() int64    { return f.units.Load() }

// ingestion is a service client calling the Data API routes.
var ingestion = goapiauth.Identity{ClientID: "ypl-svc-ingestion", Service: true, Scopes: []string{"ypl.videos.read", "ypl.playlist-items.write"}}

// doAs answers the request as identity sends it.
func (f *fixture) doAs(identity goapiauth.Identity, method, target, body string) *httptest.ResponseRecorder {
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	ctx := goapiauth.WithIdentity(context.Background(), identity)
	f.mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, target, reader))
	return rec
}

// A service's call is charged to its client, and status splits the day's
// units between the server's own and each client's.
func TestADataAPICallIsChargedToItsCallerAndStatusSplitsTheDay(t *testing.T) {
	f := newFixture(t)
	f.h.CallDataAPIWith(&fakeDataAPI{})
	if rec := f.doAs(ingestion, http.MethodGet, "/api/v1/youtube/videos?ids=a,b", ""); rec.Code != http.StatusOK {
		t.Fatalf("the read of two videos answered %d: %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(ingestion, http.MethodGet, "/api/v1/youtube/videos/a/comments", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"comments_disabled":true`) {
		t.Fatalf("the read of a video with comments off answered %d: %s", rec.Code, rec.Body)
	}

	var shown struct {
		Sync struct {
			Quota struct {
				UnitsSpent int64 `json:"units_spent"`
				ByCaller   []struct {
					Caller string `json:"caller"`
					Units  int64  `json:"units"`
				} `json:"by_caller"`
			} `json:"quota"`
		} `json:"sync"`
	}
	if err := json.Unmarshal(f.get("/api/v1/status").Body.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	quota := shown.Sync.Quota
	if quota.UnitsSpent != 2 || len(quota.ByCaller) != 2 || quota.ByCaller[0].Caller != store.OwnCaller || quota.ByCaller[0].Units != 0 ||
		quota.ByCaller[1].Caller != ingestion.ClientID || quota.ByCaller[1].Units != 2 {
		t.Fatalf("quota %+v, want 2 units, none the server's and both the ingestion client's", quota)
	}
}

// A delete that the day's quota cannot afford, beside what the sync keeps
// back, is refused before it is sent, until the quota resets at midnight
// Pacific.
func TestADeleteTheDayCannotAffordWaitsForThePacificReset(t *testing.T) {
	f := newFixture(t)
	api := &fakeDataAPI{}
	f.h.CallDataAPIWith(api)
	err := f.st.Queries.InsertServiceCall(context.Background(), generated.InsertServiceCallParams{
		ClientID: "ypl-svc-other", Method: youtube.MethodPlaylistItemsDelete, SentTs: store.Timestamp(arrival),
		QuotaDate: youtube.QuotaDate(arrival), Requests: 176, Units: 8_800,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := f.doAs(ingestion, http.MethodDelete, "/api/v1/youtube/playlists/PLX/items/itemX", "")
	refused(t, rec, http.StatusServiceUnavailable, "youtube_quota_spent")
	// arrival is half a second past 05:00 Pacific, 19 hours short of the reset.
	if got := rec.Header().Get("Retry-After"); got != "68400" || len(api.deleted) != 0 {
		t.Fatalf("Retry-After %q with %d deletes sent, want 68400 and none", got, len(api.deleted))
	}

	f.now = youtube.QuotaReset(arrival)
	if rec := f.doAs(ingestion, http.MethodDelete, "/api/v1/youtube/playlists/PLX/items/itemX", ""); rec.Code != http.StatusNoContent || len(api.deleted) != 1 {
		t.Fatalf("the delete after the reset answered %d with %d deletes sent: %s", rec.Code, len(api.deleted), rec.Body)
	}
}
