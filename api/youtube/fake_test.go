package youtube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/api/option"
)

// fakeAPI serves playlists.list and playlistItems.list as the Data API answers
// them, from resources shaped like the pages recorded in testdata:
//
//   - A resource carries kind, etag and id, and only the parts the request names,
//     whether as one comma-separated part or several. A part the resource type
//     does not have is a 400 unknownPart.
//   - A request with no filter is a 400 missingRequiredParameter.
//   - A request with no maxResults gets 5 items a page, one above 50 gets 50,
//     and maxResults=0 gets no items and a next page token.
//   - A page token the service did not issue is a 400 invalidPageToken.
//   - A playlists page reports a total larger than the playlists it lists, by
//     as many as the recorded page does.
//
// A request outside what those measurements cover fails the test rather than
// getting an invented answer.
type fakeAPI struct {
	t *testing.T
	// unlisted is how many more playlists a playlists page reports in its total
	// than the fake serves.
	unlisted int

	mu        sync.Mutex
	playlists []map[string]any
	items     map[string][]map[string]any
	// itemsTotal replaces the total an items page reports, by playlist id.
	itemsTotal map[string]int
	// refusal, when set, is the status and body of every response.
	refusal *fakeRefusal
	// beforeRequest runs before each request is answered, with the request's
	// number counting from 1, and may edit the collections.
	beforeRequest func(request int64)

	requests atomic.Int64
}

type fakeRefusal struct {
	status int
	body   string
}

// fakeResource is what the fake knows about one list method, by request path.
type fakeResource struct {
	kind  string
	parts []string
}

var fakeResources = map[string]fakeResource{
	"/youtube/v3/playlists": {
		kind:  "youtube#playlistListResponse",
		parts: []string{"contentDetails", "id", "localizations", "player", "snippet", "status"},
	},
	"/youtube/v3/playlistItems": {
		kind:  "youtube#playlistItemListResponse",
		parts: []string{"contentDetails", "id", "snippet", "status"},
	},
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	page := recordedPage(t, "playlists.json")
	total := page["pageInfo"].(map[string]any)["totalResults"].(float64)
	return &fakeAPI{
		t:          t,
		unlisted:   int(total) - len(page["items"].([]any)),
		items:      map[string][]map[string]any{},
		itemsTotal: map[string]int{},
	}
}

// reader is a Reader whose requests go to f.
func (f *fakeAPI) reader() *Reader {
	f.t.Helper()
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)
	reader, err := NewReader(context.Background(), Credentials{}, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		f.t.Fatalf("NewReader: %v", err)
	}
	return reader
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request := f.requests.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeRequest != nil {
		f.beforeRequest(request)
	}
	w.Header().Set("Content-Type", "application/json")
	if f.refusal != nil {
		w.WriteHeader(f.refusal.status)
		_, _ = w.Write([]byte(f.refusal.body))
		return
	}

	query := r.URL.Query()
	resource, known := fakeResources[r.URL.Path]
	if !known {
		f.unmodeled(w, "path %s", r.URL.Path)
		return
	}
	var parts []string
	for _, value := range query["part"] {
		parts = append(parts, strings.Split(value, ",")...)
	}
	for _, part := range parts {
		if !slices.Contains(resource.parts, part) {
			refuse(w, http.StatusBadRequest, "youtube.part", "unknownPart", "part", "parameter")
			return
		}
	}

	var all []map[string]any
	var total int
	switch {
	case r.URL.Path == "/youtube/v3/playlists" && query.Get("mine") == "true":
		all = f.playlists
		total = len(all) + f.unlisted
	case r.URL.Path == "/youtube/v3/playlistItems" && query.Get("playlistId") != "":
		id := query.Get("playlistId")
		served, ok := f.items[id]
		if !ok {
			f.unmodeled(w, "playlist %s, which the fake does not serve", id)
			return
		}
		all, total = served, len(served)
		if replaced, ok := f.itemsTotal[id]; ok {
			total = replaced
		}
	case query.Get("mine") == "" && query.Get("id") == "" && query.Get("channelId") == "" && query.Get("playlistId") == "":
		refuse(w, http.StatusBadRequest, "youtube.parameter", "missingRequiredParameter", "parameters.", "other")
		return
	default:
		f.unmodeled(w, "filter %s", r.URL.RawQuery)
		return
	}

	size := 5
	if raw := query.Get("maxResults"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			f.unmodeled(w, "maxResults %q", raw)
			return
		}
		size = min(n, 50)
	}
	offset := 0
	if token := query.Get("pageToken"); token != "" {
		n, ok := fakeOffset(token)
		if !ok {
			refuse(w, http.StatusBadRequest, "youtube.parameter", "invalidPageToken", "pageToken", "parameter")
			return
		}
		offset = n
	}

	start := min(offset, len(all))
	end := min(start+size, len(all))
	served := make([]map[string]any, 0, end-start)
	for _, stored := range all[start:end] {
		served = append(served, withParts(stored, parts))
	}
	response := map[string]any{
		"kind":     resource.kind,
		"etag":     "fakePageEtag",
		"items":    served,
		"pageInfo": map[string]any{"totalResults": total, "resultsPerPage": size},
	}
	if end < len(all) {
		response["nextPageToken"] = fakeToken(end)
	}
	if start > 0 {
		response["prevPageToken"] = fakeToken(max(start-size, 0))
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (f *fakeAPI) unmodeled(w http.ResponseWriter, format string, args ...any) {
	f.t.Errorf("fake Data API: no measurement covers "+format, args...)
	w.WriteHeader(http.StatusNotImplemented)
}

func refuse(w http.ResponseWriter, status int, domain, reason, location, locationType string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code":    status,
		"message": reason,
		"errors": []map[string]any{{
			"message": reason, "domain": domain, "reason": reason, "location": location, "locationType": locationType,
		}},
	}})
}

func fakeToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("offset:" + strconv.Itoa(offset)))
}

func fakeOffset(token string) (int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "offset:"))
	if err != nil || !strings.HasPrefix(string(raw), "offset:") {
		return 0, false
	}
	return n, true
}

// withParts is resource holding only kind, etag, id and parts.
func withParts(resource map[string]any, parts []string) map[string]any {
	out := map[string]any{"kind": resource["kind"], "etag": resource["etag"], "id": resource["id"]}
	for _, part := range parts {
		if value, ok := resource[part]; ok {
			out[part] = value
		}
	}
	return out
}

// recordedPage is the response recorded in testdata/name.
func recordedPage(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return page
}

func recordedItems(t *testing.T, name string) []map[string]any {
	t.Helper()
	var items []map[string]any
	for _, item := range recordedPage(t, name)["items"].([]any) {
		items = append(items, item.(map[string]any))
	}
	return items
}

// copyResource is a deep copy of resource.
func copyResource(t *testing.T, resource map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fakePlaylist is the recorded playlist with the id id, titled "Playlist <id>".
func fakePlaylist(t *testing.T, id string) map[string]any {
	t.Helper()
	playlist := copyResource(t, recordedItems(t, "playlists.json")[0])
	playlist["id"] = id
	snippet := playlist["snippet"].(map[string]any)
	snippet["title"], snippet["description"] = "Playlist "+id, "About "+id
	return playlist
}

// Indexes of the recorded items in testdata/playlistItems.json.
const (
	recordedPublic = iota
	recordedDeleted
	recordedPrivate
)

// fakeItem is the recorded item at index which, placed in playlistID at
// position, with an id and a video id derived from the two.
func fakeItem(t *testing.T, which int, playlistID string, position int) map[string]any {
	t.Helper()
	item := copyResource(t, recordedItems(t, "playlistItems.json")[which])
	item["id"] = fmt.Sprintf("%s-item-%03d", playlistID, position)
	snippet := item["snippet"].(map[string]any)
	snippet["playlistId"], snippet["position"] = playlistID, position
	videoID := fmt.Sprintf("vid%03d", position)
	snippet["resourceId"].(map[string]any)["videoId"] = videoID
	item["contentDetails"].(map[string]any)["videoId"] = videoID
	return item
}

func fakeItems(t *testing.T, playlistID string, n int) []map[string]any {
	t.Helper()
	items := make([]map[string]any, n)
	for i := range n {
		items[i] = fakeItem(t, recordedPublic, playlistID, i)
	}
	return items
}

// renumber sets each item's position to its index, as YouTube does after an
// edit.
func renumber(items []map[string]any) {
	for i, item := range items {
		item["snippet"].(map[string]any)["position"] = i
	}
}
