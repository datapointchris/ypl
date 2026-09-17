package handlers

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/wire"
)

type wireLibraryVideo struct {
	wireVideoSummary
	Artists   []string          `json:"artists"`
	Playlists []wirePlaylistRef `json:"playlists"`
}

type wireVideo struct {
	wireLibraryVideo
	Description *string     `json:"description"`
	Tracks      []wireTrack `json:"tracks"`
}

type wirePlaylistRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type wireTrack struct {
	Position     int64   `json:"position"`
	StartSeconds *int64  `json:"start_seconds"`
	EndSeconds   *int64  `json:"end_seconds"`
	Artist       *string `json:"artist"`
	Title        string  `json:"title"`
	RawText      string  `json:"raw_text"`
	Source       string  `json:"source"`
}

func videoIDs(videos []wireLibraryVideo) []string {
	ids := make([]string, len(videos))
	for i, v := range videos {
		ids[i] = v.ID
	}
	return ids
}

func TestTheLibraryListsTheAvailableVideosSomePlaylistHolds(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	got := decode[[]wireLibraryVideo](t, f.get("/api/v1/videos"), http.StatusOK)
	byID := make(map[string]wireLibraryVideo, len(got))
	for _, v := range got {
		byID[v.ID] = v
	}
	if ids := slices.Sorted(maps.Keys(byID)); !slices.Equal(ids, []string{"a", "b", "c", "e"}) {
		t.Fatalf("videos = %v, want a, b, c and e", ids)
	}
	if a := byID["a"]; !slices.Equal(a.Artists, []string{"Björk", "Âme", "Moby"}) {
		t.Errorf("a's artists = %v, want Björk, commonest, then Âme and Moby by name", a.Artists)
	}
	if a := byID["a"]; !slices.Equal(a.Playlists, []wirePlaylistRef{{ID: "PLA", Title: "Alpha"}}) {
		t.Errorf("a's playlists = %v, want Alpha", a.Playlists)
	}
	if b := byID["b"]; !slices.Equal(b.Playlists, []wirePlaylistRef{{ID: "PLA", Title: "Alpha"}, {ID: "PLB", Title: "zulu"}}) {
		t.Errorf("b's playlists = %v, want Alpha then zulu", b.Playlists)
	}
	if c := byID["c"]; c.Artists == nil || len(c.Artists) != 0 {
		t.Errorf("c's artists = %#v, want []", c.Artists)
	}
}

// The library's durations are a 3600, b and e 1800, and c unknown; its upload
// dates are a 2020, b 2022, and c and e unknown; its titles are Zebra, apple,
// Évora and Aardvark, which their bytes order Aardvark, Zebra, apple, Évora.
func TestTheLibraryListsInTheOrderSortNames(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	cases := map[string][]string{
		"":         {"a", "e", "b", "c"},
		"longest":  {"a", "e", "b", "c"},
		"shortest": {"e", "b", "a", "c"},
		"newest":   {"b", "a", "e", "c"},
		"oldest":   {"a", "b", "e", "c"},
		"title":    {"e", "b", "c", "a"},
	}
	for sort, want := range cases {
		got := videoIDs(decode[[]wireLibraryVideo](t, f.get("/api/v1/videos?sort="+sort), http.StatusOK))
		if !slices.Equal(got, want) {
			t.Errorf("sort %q = %v, want %v", sort, got, want)
		}
	}

	got := videoIDs(decode[[]wireLibraryVideo](t, f.get("/api/v1/videos?sort=random"), http.StatusOK))
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "b", "c", "e"}) {
		t.Errorf("sort random = %v, want every video once", got)
	}
}

func TestTheLibraryKeepsOnlyTheVideosEachFilterNames(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	cases := map[string][]string{
		"playlist=PLB":                      {"b", "c", "e"},
		"min_seconds=1800":                  {"a", "b", "e"},
		"max_seconds=1800":                  {"b", "e"},
		"min_seconds=1800&max_seconds=1800": {"b", "e"},
		"artist=BJÖRK":                      {"a"},
		"artist=bjork":                      {"a"},
		"artist=AME":                        {"a"},
		"artist=MOB":                        {"a", "b"},
		"artist=moby&playlist=PLB":          {"b"},
		"artist=nobody":                     {},
	}
	for query, want := range cases {
		got := decode[[]wireLibraryVideo](t, f.get("/api/v1/videos?"+query+"&sort=title"), http.StatusOK)
		ids := videoIDs(got)
		slices.Sort(ids)
		if got == nil || !slices.Equal(ids, want) {
			t.Errorf("%s = %v, want %v", query, ids, want)
		}
	}
}

func TestALibraryRequestNamingNothingItCanAnswerIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	for query, code := range map[string]wire.Code{
		"sort=loudest":                      wire.CodeInvalidParameter,
		"min_seconds=-1":                    wire.CodeInvalidParameter,
		"max_seconds=an+hour":               wire.CodeInvalidParameter,
		"min_seconds=1801&max_seconds=1800": wire.CodeInvalidParameter,
		"playlist=PLZ":                      wire.CodeUnknownReference,
	} {
		refused(t, f.get("/api/v1/videos?"+query), http.StatusBadRequest, code)
	}
}

// The README names every order sort takes, and the one used when it is absent.
func TestTheREADMENamesEveryVideoOrder(t *testing.T) {
	line := regexp.MustCompile("`sort` is one of ([^.]*)\\.").FindStringSubmatch(readme(t))
	if line == nil {
		t.Fatal("the README has no sentence naming the orders sort takes")
	}
	var got []string
	for _, name := range regexp.MustCompile("`([a-z]+)`").FindAllStringSubmatch(line[1], -1) {
		got = append(got, name[1])
	}
	if !slices.Equal(got, strings.Split(orderNames(), ", ")) {
		t.Fatalf("README orders = %v, want %s in that order, the first being the default", got, orderNames())
	}
}

func TestAVideoShowsItsTracklistArtistsAndPlaylists(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	got := decode[wireVideo](t, f.get("/api/v1/videos/a"), http.StatusOK)
	if got.ID != "a" || got.Description == nil || *got.Description != "A long set" || got.TrackCount != 4 || got.IsUnavailable {
		t.Errorf("a = %+v", got)
	}
	if !slices.Equal(got.Artists, []string{"Björk", "Âme", "Moby"}) || !slices.Equal(got.Playlists, []wirePlaylistRef{{ID: "PLA", Title: "Alpha"}}) {
		t.Errorf("a's artists %v and playlists %v", got.Artists, got.Playlists)
	}
	if len(got.Tracks) != 4 {
		t.Fatalf("tracks = %+v, want 4", got.Tracks)
	}
	second := got.Tracks[1]
	if second.Position != 2 || second.StartSeconds == nil || *second.StartSeconds != 600 || second.EndSeconds != nil ||
		second.Artist == nil || *second.Artist != "Moby" || second.Title != "Track" || second.RawText != "Moby - Track" || second.Source != "chapter" {
		t.Errorf("second track = %+v", second)
	}
}

func TestAnUnavailableVideoShowsWithNoTracks(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)

	got := decode[wireVideo](t, f.get("/api/v1/videos/u"), http.StatusOK)
	if !got.IsUnavailable || got.Tracks == nil || len(got.Tracks) != 0 || got.Artists == nil || got.Description != nil {
		t.Errorf("u = %+v, want unavailable with empty tracks and artists and a null description", got)
	}
}

func TestAVideoTheStoreDoesNotHoldIsNotFound(t *testing.T) {
	f := newFixture(t)
	refused(t, f.get("/api/v1/videos/zzz"), http.StatusNotFound, wire.CodeNotFound)
}
