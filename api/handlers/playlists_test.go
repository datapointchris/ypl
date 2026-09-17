package handlers

import (
	"net/http"
	"testing"

	"github.com/datapointchris/ypl/api/wire"
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
	ID       string           `json:"id"`
	Position int64            `json:"position"`
	Video    wireVideoSummary `json:"video"`
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

	got := decode[wirePlaylist](t, f.get("/api/v1/playlists/PLA"), http.StatusOK)
	summary := wirePlaylistSummary{ID: "PLA", Title: "Alpha", Description: "First", Privacy: "private", ItemCount: 3, UnavailableCount: 1, EnrichedCount: 1}
	if got.wirePlaylistSummary != summary {
		t.Errorf("summary = %+v, want %+v", got.wirePlaylistSummary, summary)
	}
	if len(got.Items) != 3 {
		t.Fatalf("items = %+v, want 3", got.Items)
	}
	for i, want := range []struct{ item, video string }{{"ia", "a"}, {"ib", "b"}, {"iu", "u"}} {
		item := got.Items[i]
		if item.ID != want.item || item.Position != int64(i) || item.Video.ID != want.video {
			t.Errorf("item %d = %s at %d holding %s, want %s at %d holding %s", i, item.ID, item.Position, item.Video.ID, want.item, i, want.video)
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
