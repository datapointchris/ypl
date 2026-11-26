package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datapointchris/goapiauth"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

// fakeDataAPI answers the Data API routes' calls, counting each request at the
// price the Data API's quota calculator gives its method. A read of items waits
// on hold where it is set, and answers refused where that is set.
type fakeDataAPI struct {
	requests, units atomic.Int64
	deleted         []youtube.ItemID
	hold            chan struct{}
	refused         error
}

func (f *fakeDataAPI) spend(units int64) {
	f.requests.Add(1)
	f.units.Add(units)
}

func (f *fakeDataAPI) Items(context.Context, youtube.PlaylistID) ([]youtube.Item, error) {
	if f.hold != nil {
		<-f.hold
	}
	f.spend(1)
	return nil, f.refused
}

func (f *fakeDataAPI) AppendItem(context.Context, youtube.PlaylistID, youtube.VideoID) (youtube.ItemID, error) {
	f.spend(youtube.WriteUnits)
	return "item-new", f.refused
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
	f.spend(youtube.VideoDetailsEstimate(len(ids)).Units)
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

// A slow read holds the Data API's one turn, and a service's append queued
// behind it times out. Its caller has left, so the append is never sent and
// costs nothing.
func TestAnAppendWhoseCallerLeftWhileQueuedIsNeverSent(t *testing.T) {
	f := newFixture(t)
	api := &fakeDataAPI{hold: make(chan struct{})}
	f.h.CallDataAPIWith(api)
	read := make(chan int, 1)
	go func() { read <- f.doAs(ingestion, http.MethodGet, "/api/v1/youtube/playlists/PLX/items", "").Code }()
	for deadline := time.Now().Add(time.Second); len(f.h.dataAPI.turn) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the read did not take the Data API's turn within a second")
		}
	}

	ctx, leave := context.WithCancel(goapiauth.WithIdentity(context.Background(), ingestion))
	appended := make(chan struct{})
	go func() {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/youtube/playlists/PLX/items", strings.NewReader(`{"video_id": "Wr1tt3nVid0"}`))
		f.mux.ServeHTTP(httptest.NewRecorder(), req)
		close(appended)
	}()
	leave()
	<-appended
	close(api.hold)
	if code := <-read; code != http.StatusOK {
		t.Fatalf("the read holding the turn answered %d, want 200", code)
	}
	if units := api.Units(); units != 1 {
		t.Fatalf("YouTube was charged %d units, want the read's 1 and no append", units)
	}
}

// YouTube limiting the server's rate, or refusing its own credential, is not
// the caller's mistake. Only a refusal of what the call asked answers 422.
func TestARateLimitOrARefusedServerCredentialIsNotTheCallersMistake(t *testing.T) {
	f := newFixture(t)
	api := &fakeDataAPI{}
	f.h.CallDataAPIWith(api)
	items := "/api/v1/youtube/playlists/PLX/items"
	cases := []struct {
		refused        error
		method, body   string
		status         int
		code           wire.Code
		wantRetryAfter bool
	}{
		{youtube.ErrThrottled, http.MethodGet, "", http.StatusServiceUnavailable, "youtube_rate_limited", true},
		{youtube.ErrCredentialRefused, http.MethodGet, "", http.StatusBadGateway, "youtube_read_failed", false},
		{youtube.ErrCredentialRefused, http.MethodPost, `{"video_id": "Wr1tt3nVid0"}`, http.StatusBadGateway, "youtube_write_failed", false},
		{nil, http.MethodPost, `{"video_id": "Wr1tt3nVid0"}`, http.StatusUnprocessableEntity, "youtube_refused", false},
	}
	for _, c := range cases {
		api.refused = youtube.ErrRefused
		if c.refused != nil {
			api.refused = errors.Join(youtube.ErrRefused, c.refused)
		}
		rec := f.doAs(ingestion, c.method, items, c.body)
		refused(t, rec, c.status, c.code)
		if got := rec.Header().Get("Retry-After") != ""; got != c.wantRetryAfter {
			t.Errorf("%s refused with %v carries a Retry-After: %v, want %v", c.method, c.refused, got, c.wantRetryAfter)
		}
	}
}
