package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
	"github.com/datapointchris/ypl/api/youtube"
)

type wirePlaylistSummary struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	Privacy          string `json:"privacy"`
	ItemCount        int64  `json:"item_count"`
	UnavailableCount int64  `json:"unavailable_count"`
	EnrichedCount    int64  `json:"enriched_count"`
}

type wirePlaylist struct {
	wirePlaylistSummary
	Items []wirePlaylistItem `json:"items"`
}

type wirePlaylistItem struct {
	ID       *string          `json:"id"`
	Position int64            `json:"position"`
	Video    wireVideoSummary `json:"video"`
}

// itemID is item's id, or "" for an item not yet on YouTube.
func (item wirePlaylistItem) itemID() string {
	if item.ID == nil {
		return ""
	}
	return *item.ID
}

type wireVideoSummary struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	UploadDate      *string `json:"upload_date"`
	IsUnavailable   bool    `json:"is_unavailable"`
	EnrichedTs      *string `json:"enriched_ts"`
	TrackCount      int64   `json:"track_count"`
}

// Their bytes order the titles Alpha, zulu, Écoute, and SQLite's NOCASE, which
// folds only ASCII, orders them the same way.
func TestPlaylistsListByTitleWithTheirCounts(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	got := decode[[]wirePlaylistSummary](t, f.get("/api/v1/playlists"), http.StatusOK)
	want := []wirePlaylistSummary{
		{ID: "PLA", Title: "Alpha", Description: "First", Privacy: "private", ItemCount: 3, UnavailableCount: 1, EnrichedCount: 1},
		{ID: "PLC", Title: "Écoute", Description: "", Privacy: "unlisted", ItemCount: 0, UnavailableCount: 0, EnrichedCount: 0},
		{ID: "PLB", Title: "zulu", Description: "", Privacy: "public", ItemCount: 3, UnavailableCount: 0, EnrichedCount: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("playlists = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("playlist %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNoPlaylistsListAsAnEmptyList(t *testing.T) {
	f := newFixture(t)
	if got := decode[[]wirePlaylistSummary](t, f.get("/api/v1/playlists"), http.StatusOK); got == nil {
		t.Fatal("no playlists answered null, want []")
	}
}

func TestAPlaylistShowsItsItemsInOrder(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	rec := f.get("/api/v1/playlists/PLA")
	got := decode[wirePlaylist](t, rec, http.StatusOK)
	summary := wirePlaylistSummary{ID: "PLA", Title: "Alpha", Description: "First", Privacy: "private", ItemCount: 3, UnavailableCount: 1, EnrichedCount: 1}
	if got.wirePlaylistSummary != summary {
		t.Errorf("summary = %+v, want %+v", got.wirePlaylistSummary, summary)
	}
	if len(got.Items) != 3 {
		t.Fatalf("items = %+v, want 3", got.Items)
	}
	for i, want := range []struct{ item, video string }{{"ia", "a"}, {"ib", "b"}, {"iu", "u"}} {
		item := got.Items[i]
		if item.itemID() != want.item || item.Position != int64(i) || item.Video.ID != want.video {
			t.Errorf("item %d = %s at %d holding %s, want %s at %d holding %s", i, item.itemID(), item.Position, item.Video.ID, want.item, i, want.video)
		}
	}
	first := got.Items[0].Video
	if first.Title != "Zebra" || first.ChannelTitle != "One" || first.DurationSeconds == nil || *first.DurationSeconds != 3600 ||
		first.UploadDate == nil || *first.UploadDate != "2020-01-01" || first.EnrichedTs == nil || first.TrackCount != 4 || first.IsUnavailable {
		t.Errorf("a = %+v", first)
	}
	if last := got.Items[2].Video; !last.IsUnavailable || last.DurationSeconds != nil || last.UploadDate != nil || last.EnrichedTs != nil {
		t.Errorf("u = %+v, want unavailable with null duration, upload date and enrichment", last)
	}
}

func TestAPlaylistWithNoItemsShowsAnEmptyList(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLC"), http.StatusOK); got.Items == nil {
		t.Fatal("a playlist with no items answered null items, want []")
	}
}

func TestAPlaylistTheStoreDoesNotHoldIsNotFound(t *testing.T) {
	f := newFixture(t)
	refused(t, f.get("/api/v1/playlists/PLZ"), http.StatusNotFound, wire.CodeNotFound)
}

// withPlaylist stores one more playlist, for a title the library's three do not
// have between them.
func (f *fixture) withPlaylist(t *testing.T, id, title string) {
	t.Helper()
	ctx := context.Background()
	err := f.st.InTx(ctx, func(tx *store.Tx) error {
		return tx.UpsertPlaylist(ctx, generated.UpsertPlaylistParams{PlaylistID: id, Title: title, Privacy: "private"})
	})
	if err != nil {
		t.Fatalf("store playlist %s: %v", id, err)
	}
	f.youtube.playlists[youtube.PlaylistID(id)] = youtube.Playlist{ID: youtube.PlaylistID(id), Title: title, Privacy: "private"}
}

func TestAPlaylistIsFoundByItsTitleHoweverItWasTyped(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	for _, ref := range []string{"PLA", "Alpha", "alpha", "ALPHA", "  alpha  ", "(alpha)"} {
		got := decode[wirePlaylist](t, f.get("/api/v1/playlists/"+url.PathEscape(ref)), http.StatusOK)
		if got.ID != "PLA" {
			t.Errorf("%q found %s, want PLA", ref, got.ID)
		}
	}
}

// Accented letters are kept rather than folded to ASCII, so the name reaches
// the playlist and a spelling without the accent does not.
func TestAnAccentedTitleIsFoundBySpellingTheAccent(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/"+url.PathEscape("écoute")), http.StatusOK); got.ID != "PLC" {
		t.Errorf("écoute found %s, want PLC", got.ID)
	}
	refused(t, f.get("/api/v1/playlists/ecoute"), http.StatusNotFound, wire.CodeNotFound)
}

// A title holds whatever the channel typed into it, a slash included, and a
// client sends one escaped. The route has to read that back as one segment or
// the title of every playlist with a slash in it is unusable.
func TestATitleHoldingASlashReachesItsPlaylist(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLD", "Deep / House")

	for _, ref := range []string{"Deep / House", "deep-house"} {
		got := decode[wirePlaylist](t, f.get("/api/v1/playlists/"+url.PathEscape(ref)), http.StatusOK)
		if got.ID != "PLD" {
			t.Errorf("%q found %s, want PLD", ref, got.ID)
		}
	}
}

func TestPartOfATitleFindsThePlaylistItNamesAlone(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/ul"), http.StatusOK); got.ID != "PLB" {
		t.Errorf("ul found %s, want PLB", got.ID)
	}
}

// Without this a playlist stops being reachable by its own name the day a
// longer name holding it is made.
func TestAWholeTitleBeatsALongerOneHoldingIt(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLD", "Deep")
	f.withPlaylist(t, "PLE", "Deep Night")

	for ref, want := range map[string]string{"deep": "PLD", "deep night": "PLE", "night": "PLE"} {
		got := decode[wirePlaylist](t, f.get("/api/v1/playlists/"+url.PathEscape(ref)), http.StatusOK)
		if got.ID != want {
			t.Errorf("%q found %s, want %s", ref, got.ID, want)
		}
	}
}

func TestATitleMatchingTwoPlaylistsIsRefusedNamingBoth(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLD", "Deep House")
	f.withPlaylist(t, "PLE", "Deep Night")

	rec := f.get("/api/v1/playlists/deep")
	refused(t, rec, http.StatusBadRequest, wire.CodeAmbiguousReference)
	for _, want := range []string{"Deep House", "PLD", "Deep Night", "PLE"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal does not name %s: %s", want, rec.Body)
		}
	}
}

// A reference resolves the same way at every verb, so which one was reached for
// never decides whether a title works.
func TestEveryReadOfAPlaylistTakesItsTitle(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	for _, target := range []string{
		"/api/v1/playlists/alpha",
		"/api/v1/playlists/alpha/items",
		"/api/v1/videos?playlist=alpha",
		"/api/v1/suggestions?playlist=alpha",
	} {
		if rec := f.get(target); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200: %s", target, rec.Code, rec.Body)
		}
	}
}

func TestAPlaylistIsRenamedAndDeletedByItsTitle(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	renamed := decode[wirePlaylistSummary](t, f.do(http.MethodPatch, "/api/v1/playlists/alpha", `{"title": "Alpha Two"}`), http.StatusOK)
	if renamed.ID != "PLA" || renamed.Title != "Alpha Two" {
		t.Errorf("rename answered %+v, want PLA titled Alpha Two", renamed)
	}
	if rec := f.do(http.MethodDelete, "/api/v1/playlists/alpha-two", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete answered %d, want 204: %s", rec.Code, rec.Body)
	}
	refused(t, f.get("/api/v1/playlists/PLA"), http.StatusNotFound, wire.CodeNotFound)
}

// A fragment matching one title is unambiguous, so the refusal that guards the
// loose form cannot see it. On a read that costs another read; on a delete it
// costs the playlist, and the guard weakens as the channel shrinks.
func TestAFragmentOfATitleReachesNoVerbThatDestroys(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/lph"), http.StatusOK); got.ID != "PLA" {
		t.Fatalf("a read of the fragment found %s, want PLA", got.ID)
	}
	refused(t, f.do(http.MethodDelete, "/api/v1/playlists/lph", ""), http.StatusNotFound, wire.CodeNotFound)
	refused(t, f.do(http.MethodPatch, "/api/v1/playlists/lph", `{"title": "Renamed"}`), http.StatusNotFound, wire.CodeNotFound)
	refused(t, f.do(http.MethodPut, "/api/v1/playlists/lph/items", `{"video_ids": []}`), http.StatusPreconditionRequired, wire.CodePreconditionRequired)

	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK); got.Title != "Alpha" {
		t.Fatalf("Alpha is %+v after three refused writes", got)
	}
	if n := f.youtube.writes(); n != 0 {
		t.Fatalf("YouTube saw %d writes for a fragment, want none", n)
	}
}

// Retrying a delete whose answer was lost is ordinary, and the second one used
// to be a clean 404. A dead id that happens to sit inside another title would
// make it delete that one instead.
func TestARetriedDeleteOfAGonePlaylistTakesNothingElse(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLX", "My PLA Favorites")

	if rec := f.do(http.MethodDelete, "/api/v1/playlists/PLA", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("the first delete answered %d, want 204", rec.Code)
	}
	refused(t, f.do(http.MethodDelete, "/api/v1/playlists/PLA", ""), http.StatusNotFound, wire.CodeNotFound)
	if got := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLX"), http.StatusOK); got.Title != "My PLA Favorites" {
		t.Fatalf("the retry took %+v as well", got)
	}
}

// Two titles a person can tell apart slug alike, so the slug step alone leaves
// each of them ambiguous and neither reachable by its own text.
func TestATitleTypedInFullBeatsOneThatSlugsTheSame(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLD", "Deep House")
	f.withPlaylist(t, "PLE", "Deep-House")

	for ref, want := range map[string]string{"Deep House": "PLD", "Deep-House": "PLE"} {
		got := decode[wirePlaylist](t, f.get("/api/v1/playlists/"+url.PathEscape(ref)), http.StatusOK)
		if got.ID != want {
			t.Errorf("%q found %s, want %s", ref, got.ID, want)
		}
	}
	// Neither title is what was typed, so the slug pass reaches both and refuses.
	refused(t, f.get("/api/v1/playlists/"+url.PathEscape("deep house")), http.StatusBadRequest, wire.CodeAmbiguousReference)
}

// The sentence carries a URL to send, and a title holds spaces and slashes.
func TestThePreconditionRefusalNamesASendableURL(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	f.withPlaylist(t, "PLD", "Deep / House")

	rec := f.do(http.MethodPut, "/api/v1/playlists/"+url.PathEscape("Deep / House")+"/items", `{"video_ids": []}`)
	refused(t, rec, http.StatusPreconditionRequired, wire.CodePreconditionRequired)
	if strings.Contains(rec.Body.String(), "Deep / House") {
		t.Fatalf("the refusal names an address the router will not route: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), url.PathEscape("Deep / House")) {
		t.Fatalf("the refusal does not name the escaped path: %s", rec.Body)
	}
}

// A query parameter naming nothing is the caller's mistake rather than a
// resource that is not there, so it is a 400 where the path segment is a 404.
func TestAPlaylistFilterNamingNothingIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	refused(t, f.get("/api/v1/videos?playlist=nothing"), http.StatusBadRequest, wire.CodeUnknownReference)
	refused(t, f.get("/api/v1/suggestions?playlist=nothing"), http.StatusBadRequest, wire.CodeUnknownReference)
}
