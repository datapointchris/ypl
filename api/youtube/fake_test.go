package youtube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/option"
)

// fakeAPI serves the Data API methods this package calls as the Data API
// answers them, from resources shaped like the pages recorded in testdata.
//
// Lists:
//
//   - A resource carries kind, etag and id, and only the parts the request names,
//     whether as one comma-separated part or several. A part a playlist item
//     does not have is a 400 unknownPart.
//   - A request with no filter is a 400 missingRequiredParameter.
//   - A request with no maxResults gets 5 items a page, one above 50 gets 50,
//     and maxResults=0 gets no items and a next page token.
//   - A page token the service did not issue is a 400 invalidPageToken.
//   - A playlists page reports a total larger than the playlists it lists, by
//     as many as the recorded page does.
//   - Listing the items of a deleted playlist is a 404 playlistNotFound.
//
// Writes:
//
//   - A playlist insert makes a playlist with the title, description and privacy
//     it names. The first item insert into that playlist is a 409
//     SERVICE_UNAVAILABLE and changes nothing.
//   - An item insert with no position appends, and a position from 0 to the item
//     count inserts there. A larger position is a 400 badRequest. A video already
//     in the playlist gets a second item.
//   - An item insert naming a video the fake has no record of is a 404
//     videoNotFound, a private video a 400 failedPrecondition, and a deleted
//     playlist a 404 playlistNotFound.
//   - An item update moves the item to a position from 0 to one less than the
//     item count. A larger position is a 400 invalidPlaylistItemPosition, an
//     update with no resource id a 400 invalidResourceType, and an update to a
//     deleted item a 400 invalidSnippet.
//   - A playlist update replaces the title and description, and clears a
//     description it leaves out. One with no title is a 400
//     playlistTitleRequired. An update to a deleted playlist succeeds.
//   - A delete of an item or playlist already deleted is a 404
//     playlistItemNotFound or playlistNotFound. A playlist delete takes its
//     items with it, and a delete of one of those is a 404 playlistItemNotFound.
//
// Every refusal's body is the one recorded in testdata/errors.json. A read the
// fake answers just after a write sees the write, which the Data API does not
// promise. A request outside what those measurements cover fails the test
// rather than getting an invented answer.
type fakeAPI struct {
	t *testing.T
	// unlisted is how many more playlists a playlists page reports in its total
	// than the fake serves.
	unlisted int
	// recorded is each refusal body in testdata/errors.json, by its name there.
	recorded map[string]json.RawMessage
	// playlistTemplate and itemTemplate are the recorded resources a write
	// builds its resource from.
	playlistTemplate map[string]any
	itemTemplate     map[string]any

	mu        sync.Mutex
	playlists []map[string]any
	items     map[string][]map[string]any
	// itemsTotal replaces the total an items page reports, by playlist id.
	itemsTotal map[string]int
	// videos is every video an insert can name, by id. An id absent from it is
	// a video YouTube has no record of.
	videos map[string]fakeVideo
	// deletedPlaylists and deletedItems hold the id of each playlist and item a
	// delete removed. orphanedItems holds the items a playlist delete took.
	deletedPlaylists map[string]bool
	deletedItems     map[string]bool
	orphanedItems    map[string]bool
	// fresh holds each playlist the fake created that no item insert has named.
	fresh   map[string]bool
	created int
	// refusal, when set, is the status and body of every response.
	refusal *fakeRefusal
	// beforeRequest runs before each request is answered, with the request's
	// number counting from 1, and may edit the collections.
	beforeRequest func(request int64)

	requests atomic.Int64
	// pauses is each wait the channel made between attempts.
	pauses []time.Duration
}

type fakeRefusal struct {
	status int
	body   string
}

// fakeVideo is a video as an item insert finds it.
type fakeVideo struct {
	title   string
	channel string
	privacy string
}

func publicVideo(id string) fakeVideo {
	return fakeVideo{title: "Video " + id, channel: "Channel of " + id, privacy: "public"}
}

// fakeResource is what the fake knows about one resource type, by request path.
type fakeResource struct {
	listKind string
	parts    []string
}

var fakeResources = map[string]fakeResource{
	"/youtube/v3/playlists": {
		listKind: "youtube#playlistListResponse",
		parts:    []string{"contentDetails", "id", "localizations", "player", "snippet", "status"},
	},
	"/youtube/v3/playlistItems": {
		listKind: "youtube#playlistItemListResponse",
		parts:    []string{"contentDetails", "id", "snippet", "status"},
	},
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	page := recordedPage(t, "playlists.json")
	total := page["pageInfo"].(map[string]any)["totalResults"].(float64)
	raw, err := os.ReadFile("testdata/errors.json")
	if err != nil {
		t.Fatalf("read errors.json: %v", err)
	}
	var recorded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("decode errors.json: %v", err)
	}
	return &fakeAPI{
		t:                t,
		unlisted:         int(total) - len(page["items"].([]any)),
		recorded:         recorded,
		playlistTemplate: recordedItems(t, "playlists.json")[0],
		itemTemplate:     recordedItems(t, "playlistItems.json")[recordedPublic],
		items:            map[string][]map[string]any{},
		itemsTotal:       map[string]int{},
		videos:           map[string]fakeVideo{},
		deletedPlaylists: map[string]bool{},
		deletedItems:     map[string]bool{},
		orphanedItems:    map[string]bool{},
		fresh:            map[string]bool{},
	}
}

// channel is a Channel whose requests go to f, and whose pauses between
// attempts are recorded in f.pauses without waiting.
func (f *fakeAPI) channel() *Channel {
	f.t.Helper()
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)
	c, err := NewChannel(context.Background(), Credentials{}, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		f.t.Fatalf("NewChannel: %v", err)
	}
	c.pause = func(ctx context.Context, d time.Duration) error {
		f.pauses = append(f.pauses, d)
		return ctx.Err()
	}
	return c
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

	resource, known := fakeResources[r.URL.Path]
	if !known {
		f.unmodeled(w, "path %s", r.URL.Path)
		return
	}
	var parts []string
	for _, value := range r.URL.Query()["part"] {
		parts = append(parts, strings.Split(value, ",")...)
	}

	switch r.Method + " " + strings.TrimPrefix(r.URL.Path, "/youtube/v3/") {
	case "GET playlists", "GET playlistItems":
		f.list(w, r, resource, parts)
	case "POST playlists":
		f.insertPlaylist(w, r, parts)
	case "PUT playlists":
		f.updatePlaylist(w, r, parts)
	case "DELETE playlists":
		f.deletePlaylist(w, r)
	case "POST playlistItems":
		f.insertItem(w, r, parts)
	case "PUT playlistItems":
		f.updateItem(w, r, parts)
	case "DELETE playlistItems":
		f.deleteItem(w, r)
	default:
		f.unmodeled(w, "%s %s", r.Method, r.URL.Path)
	}
}

func (f *fakeAPI) list(w http.ResponseWriter, r *http.Request, resource fakeResource, parts []string) {
	query := r.URL.Query()
	for _, part := range parts {
		switch {
		case slices.Contains(resource.parts, part):
		case r.URL.Path == "/youtube/v3/playlistItems":
			f.refuse(w, "playlistItems.list unknownPart")
			return
		default:
			f.unmodeled(w, "part %q on %s", part, r.URL.Path)
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
		if f.deletedPlaylists[id] {
			f.refuse(w, "playlistItems.list playlistNotFound")
			return
		}
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
		if r.URL.Path == "/youtube/v3/playlists" {
			f.refuse(w, "playlists.list missingRequiredParameter")
		} else {
			f.refuse(w, "playlistItems.list missingRequiredParameter")
		}
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
			f.refuse(w, "playlistItems.list invalidPageToken")
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
		"kind":     resource.listKind,
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

func (f *fakeAPI) insertPlaylist(w http.ResponseWriter, r *http.Request, parts []string) {
	body, ok := f.writeBody(w, r, parts, "snippet", "status")
	if !ok {
		return
	}
	title, _ := field(body, "snippet", "title").(string)
	description, _ := field(body, "snippet", "description").(string)
	privacy, _ := field(body, "status", "privacyStatus").(string)
	if title == "" || privacy == "" {
		f.unmodeled(w, "a playlist insert without a title or privacy")
		return
	}
	f.created++
	playlist := clone(f.playlistTemplate)
	playlist["id"] = fmt.Sprintf("PLfake%07d", f.created)
	setSnippet(playlist, title, description)
	playlist["status"] = map[string]any{"privacyStatus": privacy}
	playlist["contentDetails"] = map[string]any{"itemCount": 0}

	id := playlist["id"].(string)
	f.playlists = append(f.playlists, playlist)
	f.items[id] = []map[string]any{}
	f.fresh[id] = true
	_ = json.NewEncoder(w).Encode(withParts(playlist, parts))
}

func (f *fakeAPI) updatePlaylist(w http.ResponseWriter, r *http.Request, parts []string) {
	body, ok := f.writeBody(w, r, parts, "snippet")
	if !ok {
		return
	}
	id, _ := body["id"].(string)
	title, _ := field(body, "snippet", "title").(string)
	description, _ := field(body, "snippet", "description").(string)
	index := slices.IndexFunc(f.playlists, func(p map[string]any) bool { return p["id"] == id })
	switch {
	case index >= 0 && title == "":
		f.refuse(w, "playlists.update playlistTitleRequired")
	case index >= 0:
		setSnippet(f.playlists[index], title, description)
		_ = json.NewEncoder(w).Encode(withParts(f.playlists[index], parts))
	case f.deletedPlaylists[id] && title != "":
		ghost := clone(f.playlistTemplate)
		ghost["id"] = id
		setSnippet(ghost, title, description)
		_ = json.NewEncoder(w).Encode(withParts(ghost, parts))
	default:
		f.unmodeled(w, "an update to playlist %q with title %q", id, title)
	}
}

func (f *fakeAPI) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	index := slices.IndexFunc(f.playlists, func(p map[string]any) bool { return p["id"] == id })
	switch {
	case index >= 0:
		f.playlists = slices.Delete(f.playlists, index, index+1)
		for _, item := range f.items[id] {
			f.orphanedItems[item["id"].(string)] = true
		}
		delete(f.items, id)
		delete(f.fresh, id)
		f.deletedPlaylists[id] = true
		w.WriteHeader(http.StatusNoContent)
	case f.deletedPlaylists[id]:
		f.refuse(w, "playlists.delete playlistNotFound")
	default:
		f.unmodeled(w, "a delete of playlist %q", id)
	}
}

func (f *fakeAPI) insertItem(w http.ResponseWriter, r *http.Request, parts []string) {
	body, ok := f.writeBody(w, r, parts, "snippet", "status")
	if !ok {
		return
	}
	playlistID, _ := field(body, "snippet", "playlistId").(string)
	videoID, _ := field(body, "snippet", "resourceId", "videoId").(string)
	items, served := f.items[playlistID]
	switch {
	case f.deletedPlaylists[playlistID]:
		f.refuse(w, "playlistItems.insert playlistNotFound")
		return
	case !served || videoID == "":
		f.unmodeled(w, "an insert of video %q into playlist %q", videoID, playlistID)
		return
	case f.fresh[playlistID]:
		delete(f.fresh, playlistID)
		f.refuse(w, "playlistItems.insert SERVICE_UNAVAILABLE")
		return
	}
	video, known := f.videos[videoID]
	switch {
	case !known:
		f.refuse(w, "playlistItems.insert videoNotFound")
		return
	case video.privacy == "private":
		f.refuse(w, "playlistItems.insert failedPrecondition")
		return
	case video.privacy != "public":
		f.unmodeled(w, "an insert of a video whose privacy is %q", video.privacy)
		return
	}
	position := len(items)
	if raw, named := field(body, "snippet", "position").(float64); named {
		switch {
		case raw != math.Trunc(raw) || raw < 0:
			f.unmodeled(w, "an insert at position %v", raw)
			return
		case int(raw) > len(items):
			f.refuse(w, "playlistItems.insert badRequest")
			return
		}
		position = int(raw)
	}

	f.created++
	item := clone(f.itemTemplate)
	item["id"] = fmt.Sprintf("%s-added-%03d", playlistID, f.created)
	snippet := item["snippet"].(map[string]any)
	snippet["playlistId"], snippet["title"], snippet["videoOwnerChannelTitle"] = playlistID, video.title, video.channel
	snippet["resourceId"].(map[string]any)["videoId"] = videoID
	item["contentDetails"].(map[string]any)["videoId"] = videoID
	item["status"] = map[string]any{"privacyStatus": video.privacy}

	f.items[playlistID] = slices.Insert(items, position, item)
	renumber(f.items[playlistID])
	_ = json.NewEncoder(w).Encode(withParts(item, parts))
}

func (f *fakeAPI) updateItem(w http.ResponseWriter, r *http.Request, parts []string) {
	body, ok := f.writeBody(w, r, parts, "snippet")
	if !ok {
		return
	}
	id, _ := body["id"].(string)
	playlistID, index := f.findItem(id)
	if index < 0 {
		if f.deletedItems[id] && field(body, "snippet", "resourceId") != nil {
			f.refuse(w, "playlistItems.update invalidSnippet")
		} else {
			f.unmodeled(w, "an update to item %q", id)
		}
		return
	}
	items := f.items[playlistID]
	item := items[index]
	if field(body, "snippet", "resourceId") == nil {
		f.refuse(w, "playlistItems.update invalidResourceType")
		return
	}
	named, _ := field(body, "snippet", "playlistId").(string)
	videoID, _ := field(body, "snippet", "resourceId", "videoId").(string)
	position, hasPosition := field(body, "snippet", "position").(float64)
	switch {
	case named != playlistID || videoID != field(item, "snippet", "resourceId", "videoId"):
		f.unmodeled(w, "an update to item %s naming playlist %q and video %q", id, named, videoID)
		return
	case !hasPosition || position != math.Trunc(position) || position < 0:
		f.unmodeled(w, "an update to item %s without a whole position", id)
		return
	case int(position) >= len(items):
		f.refuse(w, "playlistItems.update invalidPlaylistItemPosition")
		return
	}
	items = slices.Delete(items, index, index+1)
	f.items[playlistID] = slices.Insert(items, int(position), item)
	renumber(f.items[playlistID])
	_ = json.NewEncoder(w).Encode(withParts(item, parts))
}

func (f *fakeAPI) deleteItem(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	playlistID, index := f.findItem(id)
	switch {
	case index >= 0:
		f.items[playlistID] = slices.Delete(f.items[playlistID], index, index+1)
		renumber(f.items[playlistID])
		f.deletedItems[id] = true
		w.WriteHeader(http.StatusNoContent)
	case f.deletedItems[id] || f.orphanedItems[id]:
		f.refuse(w, "playlistItems.delete playlistItemNotFound")
	default:
		f.unmodeled(w, "a delete of item %q", id)
	}
}

// findItem is the playlist holding the item id and its index there, or an index
// of -1.
func (f *fakeAPI) findItem(id string) (string, int) {
	for playlistID, items := range f.items {
		if index := slices.IndexFunc(items, func(it map[string]any) bool { return it["id"] == id }); index >= 0 {
			return playlistID, index
		}
	}
	return "", -1
}

// writeBody is the decoded body of a write that names exactly the parts want,
// the parts every measured write of its method named.
func (f *fakeAPI) writeBody(w http.ResponseWriter, r *http.Request, parts []string, want ...string) (map[string]any, bool) {
	if !slices.Equal(slices.Sorted(slices.Values(parts)), want) {
		f.unmodeled(w, "%s %s with parts %v", r.Method, r.URL.Path, parts)
		return nil, false
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.unmodeled(w, "%s %s with a body that is not a JSON object: %v", r.Method, r.URL.Path, err)
		return nil, false
	}
	return body, true
}

// refuse answers with the recorded refusal name.
func (f *fakeAPI) refuse(w http.ResponseWriter, name string) {
	body, ok := f.recorded[name]
	if !ok {
		f.t.Errorf("fake Data API: testdata/errors.json records no refusal %q", name)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	var envelope struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		f.t.Errorf("fake Data API: refusal %q: %v", name, err)
	}
	w.WriteHeader(envelope.Error.Code)
	_, _ = w.Write(body)
}

func (f *fakeAPI) unmodeled(w http.ResponseWriter, format string, args ...any) {
	f.t.Errorf("fake Data API: no measurement covers "+format, args...)
	w.WriteHeader(http.StatusNotImplemented)
}

// field is the value at path in a decoded JSON object, or nil.
func field(object map[string]any, path ...string) any {
	var value any = object
	for _, key := range path {
		next, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = next[key]
	}
	return value
}

// setSnippet sets a playlist's title and description, and the localized copies
// YouTube reports beside them.
func setSnippet(playlist map[string]any, title, description string) {
	snippet := playlist["snippet"].(map[string]any)
	snippet["title"], snippet["description"] = title, description
	snippet["localized"] = map[string]any{"title": title, "description": description}
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

// clone is a deep copy of a decoded JSON object.
func clone(object map[string]any) map[string]any {
	out := make(map[string]any, len(object))
	for key, value := range object {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return clone(v)
	case []any:
		out := make([]any, len(v))
		for i, element := range v {
			out[i] = cloneValue(element)
		}
		return out
	default:
		return v
	}
}

// fakePlaylist is the recorded playlist with the id id, titled "Playlist <id>".
func fakePlaylist(t *testing.T, id string) map[string]any {
	t.Helper()
	playlist := clone(recordedItems(t, "playlists.json")[0])
	playlist["id"] = id
	setSnippet(playlist, "Playlist "+id, "About "+id)
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
	item := clone(recordedItems(t, "playlistItems.json")[which])
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
