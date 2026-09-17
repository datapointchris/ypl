package handlers

import (
	"cmp"
	"database/sql"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"golang.org/x/text/search"

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

// libraryVideo is a video as GET /api/v1/videos lists it: what decides whether
// it belongs in a set without its tracklist being read. Artists come commonest
// first, and playlists by title.
type libraryVideo struct {
	videoSummary
	Artists   []string      `json:"artists"`
	Playlists []playlistRef `json:"playlists"`
}

// video is one video with its description and tracklist.
type video struct {
	libraryVideo
	Description *string `json:"description"`
	Tracks      []track `json:"tracks"`
}

type playlistRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// track is one track of a video's tracklist. Source names where its text came
// from.
type track struct {
	Position     int64   `json:"position"`
	StartSeconds *int64  `json:"start_seconds"`
	EndSeconds   *int64  `json:"end_seconds"`
	Artist       *string `json:"artist"`
	Title        string  `json:"title"`
	RawText      string  `json:"raw_text"`
	Source       string  `json:"source"`
}

// videoOrder is an order the library lists in, by the name the sort parameter
// takes. compare orders two videos by the order's own field and is nil for the
// random order.
type videoOrder struct {
	name    string
	compare func(a, b *libraryVideo) int
}

// videoOrders holds every order, the first being the one used when sort is
// absent. A video with no value for the field comes after every video with one,
// and ties go by title, then by id.
var videoOrders = []videoOrder{
	{"longest", func(a, b *libraryVideo) int { return compareKnown(a.DurationSeconds, b.DurationSeconds, descending) }},
	{"shortest", func(a, b *libraryVideo) int { return compareKnown(a.DurationSeconds, b.DurationSeconds, ascending) }},
	{"newest", func(a, b *libraryVideo) int { return compareKnown(a.UploadDate, b.UploadDate, descending) }},
	{"oldest", func(a, b *libraryVideo) int { return compareKnown(a.UploadDate, b.UploadDate, ascending) }},
	{"title", func(*libraryVideo, *libraryVideo) int { return 0 }},
	{"random", nil},
}

const (
	ascending  = 1
	descending = -1
)

// compareKnown orders a and b in direction, with a nil after every value.
func compareKnown[T cmp.Ordered](a, b *T, direction int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return direction * cmp.Compare(*a, *b)
}

func orderNamed(name string) (videoOrder, bool) {
	if name == "" {
		return videoOrders[0], true
	}
	i := slices.IndexFunc(videoOrders, func(o videoOrder) bool { return o.name == name })
	if i < 0 {
		return videoOrder{}, false
	}
	return videoOrders[i], true
}

func orderNames() string {
	names := make([]string, len(videoOrders))
	for i, o := range videoOrders {
		names[i] = o.name
	}
	return strings.Join(names, ", ")
}

// listVideos answers every available video some playlist holds. playlist keeps
// the videos that playlist holds, min_seconds and max_seconds bound the
// duration, artist keeps the videos with an artist whose name contains it
// ignoring case and accents, and sort names the order.
func (h *Handlers) listVideos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := generated.ListLibraryVideosParams{PlaylistID: optionalText(r, "playlist")}
	var ok bool
	if filter.MinSeconds, ok = optionalCount(w, r, "min_seconds"); !ok {
		return
	}
	if filter.MaxSeconds, ok = optionalCount(w, r, "max_seconds"); !ok {
		return
	}
	if filter.MinSeconds.Valid && filter.MaxSeconds.Valid && filter.MinSeconds.Int64 > filter.MaxSeconds.Int64 {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "min_seconds %d is more than max_seconds %d", filter.MinSeconds.Int64, filter.MaxSeconds.Int64)
		return
	}
	order, ok := orderNamed(r.URL.Query().Get("sort"))
	if !ok {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "sort %q is not one of %s", r.URL.Query().Get("sort"), orderNames())
		return
	}
	artist := r.URL.Query().Get("artist")

	var rows []generated.ListLibraryVideosRow
	var artists []generated.ListVideoArtistsRow
	var playlists []generated.ListVideoPlaylistsRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if filter.PlaylistID.Valid {
			if _, err := q.GetPlaylist(ctx, filter.PlaylistID.String); err != nil {
				return paramRow(err, referenceError{name: "playlist", value: filter.PlaylistID.String})
			}
		}
		var err error
		if rows, err = q.ListLibraryVideos(ctx, filter); err != nil {
			return err
		}
		if artists, err = q.ListVideoArtists(ctx, sql.NullString{}); err != nil {
			return err
		}
		playlists, err = q.ListVideoPlaylists(ctx, sql.NullString{})
		return err
	})
	if err != nil {
		h.writeListError(w, r, err)
		return
	}

	c := newCollator()
	artistsOf := groupArtists(artists, c)
	playlistsOf := groupPlaylists(playlists, c)
	matcher := search.New(language.Und, search.Loose)
	videos := make([]libraryVideo, 0, len(rows))
	for _, row := range rows {
		v := libraryVideo{
			videoSummary: videoSummary{
				ID:              row.VideoID,
				Title:           row.Title,
				ChannelTitle:    row.ChannelTitle,
				DurationSeconds: nullableInt(row.DurationSeconds),
				UploadDate:      nullableText(row.UploadDate),
				EnrichedTs:      nullableText(row.EnrichedTs),
				TrackCount:      row.TrackCount,
			},
			Artists:   orEmpty(artistsOf[row.VideoID]),
			Playlists: orEmpty(playlistsOf[row.VideoID]),
		}
		if artist != "" && !slices.ContainsFunc(v.Artists, func(a string) bool { return contains(matcher, a, artist) }) {
			continue
		}
		videos = append(videos, v)
	}

	if order.compare == nil {
		rand.Shuffle(len(videos), func(i, j int) { videos[i], videos[j] = videos[j], videos[i] })
	} else {
		slices.SortFunc(videos, func(a, b libraryVideo) int {
			return cmp.Or(order.compare(&a, &b), c.CompareString(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
		})
	}
	wire.JSON(w, http.StatusOK, videos)
}

func (h *Handlers) showVideo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	only := sql.NullString{String: id, Valid: true}
	var stored generated.Video
	var tracks []generated.Track
	var artists []generated.ListVideoArtistsRow
	var playlists []generated.ListVideoPlaylistsRow
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		if stored, err = q.GetVideo(ctx, id); err != nil {
			return err
		}
		if tracks, err = q.ListTracks(ctx, id); err != nil {
			return err
		}
		if artists, err = q.ListVideoArtists(ctx, only); err != nil {
			return err
		}
		playlists, err = q.ListVideoPlaylists(ctx, only)
		return err
	})
	if err != nil {
		h.writeItemError(w, r, err, "video "+id)
		return
	}
	c := newCollator()
	shown := video{
		libraryVideo: libraryVideo{
			videoSummary: videoSummary{
				ID:              stored.VideoID,
				Title:           stored.Title,
				ChannelTitle:    stored.ChannelTitle,
				DurationSeconds: nullableInt(stored.DurationSeconds),
				UploadDate:      nullableText(stored.UploadDate),
				IsUnavailable:   stored.IsUnavailable,
				EnrichedTs:      nullableText(stored.EnrichedTs),
				TrackCount:      int64(len(tracks)),
			},
			Artists:   orEmpty(groupArtists(artists, c)[id]),
			Playlists: orEmpty(groupPlaylists(playlists, c)[id]),
		},
		Description: nullableText(stored.Description),
		Tracks:      make([]track, len(tracks)),
	}
	for i, t := range tracks {
		shown.Tracks[i] = track{
			Position:     t.Position,
			StartSeconds: nullableInt(t.StartSeconds),
			EndSeconds:   nullableInt(t.EndSeconds),
			Artist:       nullableText(t.Artist),
			Title:        t.Title,
			RawText:      t.RawText,
			Source:       t.Source,
		}
	}
	wire.JSON(w, http.StatusOK, shown)
}

// contains reports whether pattern occurs in s as matcher compares them.
func contains(matcher *search.Matcher, s, pattern string) bool {
	start, _ := matcher.IndexString(s, pattern)
	return start >= 0
}

// groupArtists is each video's artists, the one most of its tracks name first,
// and artists named as often in c's order.
func groupArtists(rows []generated.ListVideoArtistsRow, c *collate.Collator) map[string][]string {
	byVideo := make(map[string][]generated.ListVideoArtistsRow)
	for _, row := range rows {
		byVideo[row.VideoID] = append(byVideo[row.VideoID], row)
	}
	grouped := make(map[string][]string, len(byVideo))
	for id, named := range byVideo {
		slices.SortFunc(named, func(a, b generated.ListVideoArtistsRow) int {
			return cmp.Or(cmp.Compare(b.Appearances, a.Appearances), c.CompareString(a.Artist.String, b.Artist.String), strings.Compare(a.Artist.String, b.Artist.String))
		})
		names := make([]string, len(named))
		for i, row := range named {
			names[i] = row.Artist.String
		}
		grouped[id] = names
	}
	return grouped
}

// groupPlaylists is the playlists holding each video, by title in c's order.
func groupPlaylists(rows []generated.ListVideoPlaylistsRow, c *collate.Collator) map[string][]playlistRef {
	grouped := make(map[string][]playlistRef)
	for _, row := range rows {
		grouped[row.VideoID] = append(grouped[row.VideoID], playlistRef{ID: row.PlaylistID, Title: row.Title})
	}
	for _, refs := range grouped {
		slices.SortFunc(refs, func(a, b playlistRef) int {
			return cmp.Or(c.CompareString(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
		})
	}
	return grouped
}

// orEmpty is s, or an empty slice when s is nil, so it encodes as [].
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
