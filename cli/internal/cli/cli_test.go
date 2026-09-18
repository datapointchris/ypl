package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/datapointchris/goclilogin"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// fixture is the whole command tree over a server and a keychain of its own.
// Nothing here reaches the machine's real config, keychain or network, so the
// suite runs the same on a workstation as on a machine that has never been
// logged in.
type fixture struct {
	t *testing.T
	// asked holds every request the tree made, in order, so a test can assert
	// what a flag turned into rather than only what came back.
	asked []*url.URL
	// sent holds the same requests whole. A write is its method, its headers and
	// its body as much as it is an address, and none of those is in a URL.
	sent   []request
	answer http.HandlerFunc
	app    *app
	// store is one keychain for the whole fixture. A fresh one per call would
	// lose what `auth login` saved before `auth status` looked for it, which is
	// the opposite of how a keychain behaves.
	store *goclilogin.TokenStore
	// stdin is what a verb reading a document reads. It is always set, and
	// always to something that is not an *os.File, so the tree reads as having
	// been piped to rather than as holding a terminal — which is what the real
	// gate asks, and what a suite cannot otherwise answer without a pty.
	stdin io.Reader
}

// newFixture is the tree answered by answer. The config the commands resolve is
// this fixture's, since a test that read the machine's own would pass or fail on
// whether someone had logged in.
func newFixture(t *testing.T, answer http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{t: t, answer: answer, stdin: strings.NewReader("")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.asked = append(f.asked, r.URL)
		f.sent = append(f.sent, request{Method: r.Method, URL: r.URL, Header: r.Header.Clone(), Body: string(body)})
		f.answer(w, r)
	}))
	t.Cleanup(server.Close)

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("YPL_API_BASE", server.URL)
	t.Setenv("YPL_OIDC_ISSUER", "https://issuer.test")
	t.Setenv("YPL_CLIENT_ID", "ypl-cli-test")

	f.app = &app{
		client: func(context.Context) (*api.Client, error) {
			return api.New(server.URL, server.Client()), nil
		},
		tokens: func(login goclilogin.Config) *goclilogin.TokenStore {
			if f.store == nil {
				f.store = goclilogin.NewTestTokenStore(login)
			}
			return f.store
		},
	}
	return f
}

// answered is what a run of the tree produced.
type answered struct {
	out  string
	err  string
	code int
}

// run invokes the tree with args, taking its streams rather than the process's.
func (f *fixture) run(args ...string) answered {
	f.t.Helper()
	root := newRootCommand(f.app)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(f.stdin)
	root.SetArgs(args)
	err := root.Execute()
	report(&errOut, err)
	return answered{out: out.String(), err: errOut.String(), code: exitCodeFor(err)}
}

// asJSON decodes what a --json run wrote to stdout, which is the door a test
// asks what a command found. Asserting a table would be asserting a width and a
// wording the terminal owns.
func asJSON[T any](t *testing.T, got answered) T {
	t.Helper()
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	var v T
	decodeInto(t, got.out, &v)
	return v
}

// decodeInto reads written as JSON, for a test that has already asserted on the
// exit code and cannot use asJSON.
func decodeInto(t *testing.T, written string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(written), into); err != nil {
		t.Fatalf("decode %q: %v", written, err)
	}
}

// serves answers each path with the JSON beside it, and 404s anything else so a
// request a test did not expect fails loudly rather than decoding as nothing.
func serves(bodies map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "no route answers ` + r.URL.Path + `", "code": "route_not_found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// refuses answers every request with status and code, as the server words a
// refusal.
func refuses(status int, code, sentence string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error": ` + quoted(sentence) + `, "code": "` + code + `"}`))
	}
}

func quoted(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

// lastAsked is the URL of the last request the tree made.
func (f *fixture) lastAsked() *url.URL {
	f.t.Helper()
	if len(f.asked) == 0 {
		f.t.Fatal("the tree made no request")
	}
	return f.asked[len(f.asked)-1]
}

// pipe is what the next run reads from stdin.
func (f *fixture) pipe(text string) { f.stdin = strings.NewReader(text) }

// request is one request the tree made, kept whole.
type request struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   string
}

// writes is every request that was not a read, which is what a test asserting
// on a write wants — the reads a verb makes first are not its subject.
func (f *fixture) writes() []request {
	f.t.Helper()
	var found []request
	for _, one := range f.sent {
		if one.Method != http.MethodGet {
			found = append(found, one)
		}
	}
	return found
}

// onlyWrite is the one write the tree made, and a failure where it made another
// number of them. A verb that wrote twice and a verb that wrote nothing are both
// findings, and asserting on the last one hides each.
func (f *fixture) onlyWrite() request {
	f.t.Helper()
	found := f.writes()
	if len(found) != 1 {
		f.t.Fatalf("the tree made %d writes, want 1: %+v", len(found), found)
	}
	return found[0]
}

// answer is what the fake server sends for one route.
type answer struct {
	status int
	body   string
	header map[string]string
}

// answers routes on the method as well as the path, keyed as the server's own
// route table is keyed — "PUT /api/v1/playlists/{id}/items" is a different
// answer from the GET at that address, and a map of paths alone cannot say so.
func answers(routes map[string]answer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "no route answers ` + r.Method + " " + r.URL.Path + `", "code": "route_not_found"}`))
			return
		}
		for name, value := range route.header {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		status := route.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(route.body))
	}
}

const onePlaylist = `[{"id": "PLA", "title": "Alpha", "description": "First", "privacy": "private",
	"item_count": 3, "unavailable_count": 1, "enriched_count": 2}]`

func TestPlaylistsListReadsEveryPlaylist(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/playlists": onePlaylist}))

	got := asJSON[[]api.PlaylistSummary](t, f.run("playlists", "list", "--json"))
	if len(got) != 1 || got[0].ID != "PLA" || got[0].Title != "Alpha" || got[0].ItemCount != 3 {
		t.Fatalf("playlists = %+v", got)
	}
}

// A title holds spaces and punctuation, so it reaches the server escaped or it
// reaches a different path.
func TestPlaylistsShowSendsTheNameAsOnePathSegment(t *testing.T) {
	body := `{"id": "PLA", "title": "Sunday / Morning", "description": "", "privacy": "public",
		"item_count": 0, "unavailable_count": 0, "enriched_count": 0, "items": []}`
	f := newFixture(t, serves(map[string]string{"/api/v1/playlists/Sunday / Morning": body}))

	got := asJSON[api.Playlist](t, f.run("playlists", "show", "Sunday / Morning", "--json"))
	if got.ID != "PLA" {
		t.Fatalf("playlist = %+v", got)
	}
	if path := f.lastAsked().EscapedPath(); path != "/api/v1/playlists/Sunday%20%2F%20Morning" {
		t.Fatalf("asked for %q, want the name as one escaped segment", path)
	}
}

func TestVideosListTurnsItsFlagsIntoTheServersParameters(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/videos": `[]`}))

	got := f.run("videos", "list", "--json",
		"--playlist", "alpha", "--artist", "björk", "--min-minutes", "90", "--max-minutes", "120", "--sort", "longest")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	want := url.Values{
		"playlist":    {"alpha"},
		"artist":      {"björk"},
		"min_seconds": {"5400"},
		"max_seconds": {"7200"},
		"sort":        {"longest"},
	}
	if asked := f.lastAsked().Query(); asked.Encode() != want.Encode() {
		t.Fatalf("asked %q, want %q", asked.Encode(), want.Encode())
	}
}

// A bound of zero is one a caller can mean, so absence has to be spelled as
// something else or --min-minutes 0 would be dropped as unset.
func TestVideosListSendsAZeroBoundAndOmitsAnAbsentOne(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/videos": `[]`}))

	f.run("videos", "list", "--json", "--min-minutes", "0")
	asked := f.lastAsked().Query()
	if got := asked.Get("min_seconds"); got != "0" {
		t.Errorf("min_seconds = %q, want 0", got)
	}
	if asked.Has("max_seconds") {
		t.Errorf("max_seconds was sent as %q, want it left off", asked.Get("max_seconds"))
	}
}

func TestVideosShowReadsTheTracklist(t *testing.T) {
	body := `{"id": "a", "title": "Zebra", "channel_title": "One", "duration_seconds": 3600,
		"upload_date": "2020-01-01", "is_unavailable": false, "enriched_ts": "2026-01-01T00:00:00Z",
		"track_count": 2, "artists": ["Björk"], "playlists": [{"id": "PLA", "title": "Alpha"}],
		"description": "A long set",
		"tracks": [{"position": 1, "start_seconds": 0, "end_seconds": 600, "artist": "Björk",
			"title": "Track", "raw_text": "Björk - Track", "source": "chapter"}]}`
	f := newFixture(t, serves(map[string]string{"/api/v1/videos/a": body}))

	got := asJSON[api.Video](t, f.run("videos", "show", "a", "--json"))
	if len(got.Tracks) != 1 || got.Tracks[0].Title != "Track" || got.Tracks[0].Source != "chapter" {
		t.Fatalf("tracks = %+v", got.Tracks)
	}
	if len(got.Artists) != 1 || got.Artists[0] != "Björk" {
		t.Fatalf("artists = %+v", got.Artists)
	}
}

func TestVideosSortsListsTheOrdersWithoutReachingTheServer(t *testing.T) {
	f := newFixture(t, serves(nil))

	got := asJSON[[]string](t, f.run("videos", "sorts", "--json"))
	if len(got) != len(api.VideoSorts) || got[0] != api.VideoSorts[0] {
		t.Fatalf("sorts = %v, want %v", got, api.VideoSorts)
	}
	if len(f.asked) != 0 {
		t.Errorf("a vocabulary the binary already holds cost %d requests", len(f.asked))
	}
}

// The server sends at most 100 rows a page, so a larger ask is several requests
// and the cursor is the last row of the page before.
func TestPlaysListReadsAsManyPagesAsTheLimitTakes(t *testing.T) {
	first := make([]string, 100)
	for i := range first {
		first[i] = play(i + 1)
	}
	pages := []string{
		`{"data": [` + strings.Join(first, ",") + `], "has_more": true}`,
		`{"data": [` + play(101) + `], "has_more": false}`,
	}
	sent := 0
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pages[min(sent, len(pages)-1)]))
		sent++
	})

	got := asJSON[[]api.Play](t, f.run("plays", "list", "--limit", "150", "--json"))
	if len(got) != 101 {
		t.Fatalf("read %d plays, want 101", len(got))
	}
	if len(f.asked) != 2 {
		t.Fatalf("made %d requests, want 2", len(f.asked))
	}
	firstAsk, secondAsk := f.asked[0].Query(), f.asked[1].Query()
	if firstAsk.Get("limit") != "100" || firstAsk.Has("starting_after") {
		t.Errorf("first page asked %q, want 100 rows and no cursor", firstAsk.Encode())
	}
	if secondAsk.Get("limit") != "50" || secondAsk.Get("starting_after") != "id-100" {
		t.Errorf("second page asked %q, want 50 rows after id-100", secondAsk.Encode())
	}
}

// play is one play as the server sends it, keyed so a test can read the cursor
// back out of the page it came on.
func play(handle int) string {
	return `{"id": "id-` + strconv.Itoa(handle) + `", "handle": ` + strconv.Itoa(handle) + `, "played_ts": "2026-09-01T00:00:00Z",
		"video": {"id": "a", "title": "Zebra", "channel_title": "One"}}`
}

func TestPlaysShowNamesThePlayInThePath(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/plays/41": play(41)}))

	got := asJSON[api.Play](t, f.run("plays", "show", "41", "--json"))
	if got.Handle != 41 {
		t.Fatalf("play = %+v", got)
	}
}

func TestNextCarriesTheURLThatPlaysEachSuggestion(t *testing.T) {
	body := `[{"id": "a", "title": "Zebra", "channel_title": "One", "duration_seconds": 3600,
		"play_count": 0, "last_played_ts": null}]`
	f := newFixture(t, serves(map[string]string{"/api/v1/suggestions": body}))

	got := asJSON[[]suggestedVideo](t, f.run("next", "--playlist", "alpha", "--limit", "5", "--json"))
	if len(got) != 1 || got[0].URL != "https://www.youtube.com/watch?v=a" {
		t.Fatalf("suggestions = %+v", got)
	}
	asked := f.lastAsked().Query()
	if asked.Get("playlist") != "alpha" || asked.Get("limit") != "5" {
		t.Errorf("asked %q, want playlist alpha and limit 5", asked.Encode())
	}
}

// A status bar runs this on a timer, so a draw with nothing in it has to be
// distinguishable from one that worked without reading the output.
func TestNextExitsOneWhenThereIsNothingToPlay(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/suggestions": `[]`}))

	if got := f.run("next"); got.code != 1 {
		t.Fatalf("exited %d with %q, want 1", got.code, got.out+got.err)
	}
}

func TestStatusReadsTheLibraryAndTheLatestRuns(t *testing.T) {
	body := `{"library": {"playlists": 3, "videos": 6, "unavailable_videos": 1, "enriched_videos": 2,
		"tracks": 5, "plays": 4}, "last_run": ` + run(2, "partial") + `, "last_ok_run": ` + run(1, "ok") + `}`
	f := newFixture(t, serves(map[string]string{"/api/v1/status": body}))

	got := asJSON[api.Status](t, f.run("status", "--json"))
	switch {
	case got.Library.Videos != 6 || got.Library.Tracks != 5:
		t.Fatalf("library = %+v", got.Library)
	case got.LastRun == nil || got.LastRun.Outcome != "partial":
		t.Fatalf("last run = %+v", got.LastRun)
	case got.LastOKRun == nil || got.LastOKRun.ID != 1:
		t.Fatalf("last ok run = %+v", got.LastOKRun)
	}
}

func run(id int, outcome string) string {
	return `{"id": ` + strconv.Itoa(id) + `, "started_ts": "2026-09-01T00:00:00Z", "finished_ts": "2026-09-01T00:01:00Z",
		"quota_date": "2026-09-01", "outcome": "` + outcome + `", "playlists": 3, "playlists_deleted": 0,
		"playlists_skipped": 0, "playlists_deferred": 0, "items_added": 2, "items_removed": 1, "writes": 0,
		"requests": 5, "units": 5, "write_units": 0, "video_reads": 4, "videos_enriched": 3, "tracks_found": 12,
		"videos_unreadable": 1, "is_rate_limited": false, "enrichment_paused": false, "failures": []}`
}

func TestSyncRunsListReadsTheNewestRuns(t *testing.T) {
	body := `{"data": [` + run(2, "ok") + `], "has_more": false}`
	f := newFixture(t, serves(map[string]string{"/api/v1/sync/runs": body}))

	got := asJSON[[]api.SyncRun](t, f.run("sync", "runs", "list", "--json"))
	if len(got) != 1 || got[0].TracksFound != 12 {
		t.Fatalf("runs = %+v", got)
	}
	if asked := f.lastAsked().Query().Get("limit"); asked != "20" {
		t.Errorf("asked for %q rows, want the default of 20", asked)
	}
}

// A read that stopped at its limit and a read that reached the end put the same
// rows on screen, so the one that stopped has to say so and name the flag that
// reads further.
func TestATruncatedListSaysSoAndNamesTheFlagThatWidensIt(t *testing.T) {
	for _, c := range []struct {
		args []string
		path string
		body string
	}{
		{[]string{"plays", "list", "--limit", "1"}, "/api/v1/plays", `{"data": [` + play(1) + `], "has_more": true}`},
		{[]string{"sync", "runs", "list", "--limit", "1"}, "/api/v1/sync/runs", `{"data": [` + run(2, "ok") + `], "has_more": true}`},
	} {
		f := newFixture(t, serves(map[string]string{c.path: c.body}))
		got := f.run(c.args...)
		if got.code != 0 {
			t.Fatalf("%v exited %d: %s%s", c.args, got.code, got.out, got.err)
		}
		if !strings.Contains(got.err, "--limit") {
			t.Errorf("%v read a truncated page and said %q, want it to name --limit", c.args, got.err)
		}
	}
}

// A complete read says nothing, or the sentence stops meaning anything.
func TestACompleteListSaysNothingAboutMore(t *testing.T) {
	f := newFixture(t, serves(map[string]string{
		"/api/v1/plays": `{"data": [` + play(1) + `], "has_more": false}`,
	}))

	if got := f.run("plays", "list"); strings.Contains(got.err, "--limit") {
		t.Fatalf("a complete read named --limit anyway: %q", got.err)
	}
}

// A video YouTube will not serve is filtered out of the enrichment queue, so
// promising a later read tells a reader to wait for something that cannot
// happen.
func TestAnUnavailableVideoIsNotPromisedALaterRead(t *testing.T) {
	gone := `{"id": "u", "title": "Gone", "channel_title": "Four", "duration_seconds": null,
		"upload_date": null, "is_unavailable": true, "enriched_ts": null, "track_count": 0,
		"artists": [], "playlists": [], "description": null, "tracks": []}`
	f := newFixture(t, serves(map[string]string{"/api/v1/videos/u": gone}))

	got := f.run("videos", "show", "u")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if strings.Contains(got.out, "later run") || strings.Contains(got.out, "not read this one yet") {
		t.Fatalf("an unavailable video was promised a read that cannot happen:\n%s", got.out)
	}
	if !strings.Contains(got.out, "will not serve") {
		t.Fatalf("the reason it has no tracklist is not stated:\n%s", got.out)
	}
	// Its length is null too, and a mix of unknown length is not a mix of no
	// length. A zero here would read as a video that is really that long.
	if strings.Contains(got.out, "0:00") {
		t.Fatalf("a length the server does not hold was rendered as a zero:\n%s", got.out)
	}
}

// A consumer writes one filter against a collection, or it writes a filter and
// a null guard and finds out which it needed the day a read matches nothing.
func TestAnEmptyCollectionIsAnEmptyListAndNeverNull(t *testing.T) {
	f := newFixture(t, serves(map[string]string{
		"/api/v1/playlists":   `[]`,
		"/api/v1/videos":      `[]`,
		"/api/v1/suggestions": `[]`,
		"/api/v1/plays":       `{"data": [], "has_more": false}`,
		"/api/v1/sync/runs":   `{"data": [], "has_more": false}`,
	}))

	for _, args := range [][]string{
		{"playlists", "list", "--json"},
		{"videos", "list", "--json"},
		{"next", "--json"},
		{"plays", "list", "--json"},
		{"sync", "runs", "list", "--json"},
	} {
		got := f.run(args...)
		if trimmed := strings.TrimSpace(got.out); trimmed != "[]" {
			t.Errorf("%v wrote %q, want []", args, trimmed)
		}
	}
}
