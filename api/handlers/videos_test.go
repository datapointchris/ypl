package handlers

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
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

	got := decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos"), http.StatusOK).Data
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
		got := videoIDs(decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?sort="+sort), http.StatusOK).Data)
		if !slices.Equal(got, want) {
			t.Errorf("sort %q = %v, want %v", sort, got, want)
		}
	}

	got := videoIDs(decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?sort=random"), http.StatusOK).Data)
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "b", "c", "e"}) {
		t.Errorf("sort random = %v, want every video once", got)
	}

	// An order comes a page at a time, each after the last id of the one
	// before. A random order is a draw with no next page.
	first := decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?sort=title&limit=2"), http.StatusOK)
	rest := decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?sort=title&limit=2&starting_after=b"), http.StatusOK)
	if got := append(videoIDs(first.Data), videoIDs(rest.Data)...); !slices.Equal(got, cases["title"]) || !first.HasMore || rest.HasMore {
		t.Errorf("pages by title = %v then %v, more %v then %v, want %v over two pages", videoIDs(first.Data), videoIDs(rest.Data), first.HasMore, rest.HasMore, cases["title"])
	}
	if draw := decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?sort=random&limit=2"), http.StatusOK); len(draw.Data) != 2 || draw.HasMore {
		t.Errorf("a random draw of 2 = %+v, want 2 videos and no next page", draw)
	}
	refused(t, f.get("/api/v1/videos?sort=random&starting_after=b"), http.StatusBadRequest, wire.CodeInvalidParameter)
	refused(t, f.get("/api/v1/videos?sort=title&starting_after=gone"), http.StatusBadRequest, wire.CodeInvalidParameter)
	refused(t, f.get("/api/v1/videos?limit=101"), http.StatusBadRequest, wire.CodeInvalidLimit)
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
		got := decode[wirePage[wireLibraryVideo]](t, f.get("/api/v1/videos?"+query+"&sort=title"), http.StatusOK).Data
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

// A video is named by its id, its title, or part of its title, as a read of a
// playlist names one. A whole title or its slug beats a title merely holding
// it. Part of a title holding more than one is refused naming the first ten
// and saying what narrows the rest, and a name matching nothing is not found.
func TestAVideoIsNamedByItsIdTitleOrPartOfItsTitle(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	mixes := []generated.SeedVideoParams{{VideoID: "m", Title: "Mix", ChannelTitle: "Six"}}
	for i := 1; i <= 11; i++ {
		mixes = append(mixes, generated.SeedVideoParams{VideoID: fmt.Sprintf("m%02d", i), Title: fmt.Sprintf("Mix %02d", i), ChannelTitle: "Six"})
	}
	if err := f.st.InTx(context.Background(), func(tx *store.Tx) error {
		for _, mix := range mixes {
			if err := tx.SeedVideo(context.Background(), mix); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"a": "a", "Zebra": "a", "zeb": "a", "ZEBRA": "a", "Mix": "m", "MIX": "m"} {
		if got := decode[wireVideo](t, f.get("/api/v1/videos/"+name), http.StatusOK); got.ID != want {
			t.Errorf("video %q = %s, want %s", name, got.ID, want)
		}
	}
	refusal := decode[wireRefusal](t, f.get("/api/v1/videos/mi"), http.StatusBadRequest)
	if refusal.Code != string(wire.CodeAmbiguousReference) || strings.Count(refusal.Error, `"Mix`) != 10 || !strings.Contains(refusal.Error, "and 2 more") {
		t.Errorf("refusal = %+v, want ten of the twelve named and the other two counted", refusal)
	}
	refused(t, f.get("/api/v1/videos/zzz"), http.StatusNotFound, wire.CodeNotFound)
}
