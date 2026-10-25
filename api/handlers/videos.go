package handlers

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
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
// first, and playlists by title. Likes, Dislikes and Notes count its ratings
// of each kind, and Tags is every tag they carried, alphabetically.
type libraryVideo struct {
	videoSummary
	Artists   []string      `json:"artists"`
	Playlists []playlistRef `json:"playlists"`
	Likes     int64         `json:"likes"`
	Dislikes  int64         `json:"dislikes"`
	Notes     int64         `json:"notes"`
	Tags      []string      `json:"tags"`
}

// video is one video with its description, tracklist, and every rating left
// on it, newest first.
type video struct {
	libraryVideo
	Description *string  `json:"description"`
	Tracks      []track  `json:"tracks"`
	Ratings     []rating `json:"ratings"`
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

// listVideos answers a page of the available videos some playlist holds.
// playlist keeps the videos that playlist holds, min_seconds and max_seconds
// bound the duration, artist keeps the videos with an artist whose name
// contains it ignoring case and accents, rated keeps the videos with a rating
// of that kind, or with none under unrated, tag keeps the videos a rating
// tagged with it, text keeps the videos a rating's message of contains it
// ignoring case and accents, and sort names the order.
//
// A random order comes out new on every request, so it is a draw rather than a
// page of a standing list: it answers limit videos, every video when limit is
// absent, and has no next page.
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
	rated := r.URL.Query().Get("rated")
	if rated != "" && rated != kindLike && rated != kindDislike && rated != kindNote && rated != ratedNone {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "rated %q is not like, dislike, note or unrated", rated)
		return
	}
	tag := strings.ToLower(strings.Join(strings.Fields(r.URL.Query().Get("tag")), " "))
	text := r.URL.Query().Get("text")
	limit, ok := limitParam(w, r, videosPage)
	if !ok {
		return
	}
	after := r.URL.Query().Get("starting_after")
	if after != "" && order.compare == nil {
		wire.Refuse(w, http.StatusBadRequest, wire.CodeInvalidParameter, "a random order has no next page, so it takes no starting_after")
		return
	}

	var rows []generated.ListLibraryVideosRow
	var artists []generated.ListVideoArtistsRow
	var playlists []generated.ListVideoPlaylistsRow
	var opinions videoOpinions
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		if filter.PlaylistID.Valid {
			id, err := resolvePlaylist(ctx, q, "playlist", filter.PlaylistID.String, loosely)
			if err != nil {
				return err
			}
			filter.PlaylistID.String = id
		}
		var err error
		if rows, err = q.ListLibraryVideos(ctx, filter); err != nil {
			return err
		}
		if artists, err = q.ListVideoArtists(ctx, sql.NullString{}); err != nil {
			return err
		}
		if opinions, err = readOpinions(ctx, q, sql.NullString{}); err != nil {
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
		opinions.fill(&v)
		if artist != "" && !slices.ContainsFunc(v.Artists, func(a string) bool { return contains(matcher, a, artist) }) {
			continue
		}
		if rated != "" && !opinions.rated(v, rated) {
			continue
		}
		if tag != "" && !slices.Contains(v.Tags, tag) {
			continue
		}
		if text != "" && !contains(matcher, opinions.messages[row.VideoID], text) {
			continue
		}
		videos = append(videos, v)
	}

	if order.compare == nil {
		rand.Shuffle(len(videos), func(i, j int) { videos[i], videos[j] = videos[j], videos[i] })
		wire.JSON(w, http.StatusOK, page[libraryVideo]{Data: videos[:min(int64(len(videos)), limit)], HasMore: false})
		return
	}
	slices.SortFunc(videos, func(a, b libraryVideo) int {
		return cmp.Or(order.compare(&a, &b), c.CompareString(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
	})
	answer, ok := pageAfter(w, videos, func(v libraryVideo) string { return v.ID }, after, limit)
	if ok {
		wire.JSON(w, http.StatusOK, answer)
	}
}

// resolveVideo is the id of the video ref names, by its id or loosely as
// resolveTitled says, as a read of a playlist names one.
func resolveVideo(ctx context.Context, q *generated.Queries, ref string) (string, error) {
	if _, err := q.GetVideo(ctx, ref); err == nil {
		return ref, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := q.ListVideoReferences(ctx)
	if err != nil {
		return "", err
	}
	named := make([]titled, len(rows))
	for i, row := range rows {
		named[i] = titled{id: row.VideoID, title: row.Title}
	}
	return resolveTitled(named, "video", ref, loosely)
}

func (h *Handlers) showVideo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("id")
	var id string
	var stored generated.Video
	var tracks []generated.Track
	var artists []generated.ListVideoArtistsRow
	var playlists []generated.ListVideoPlaylistsRow
	var opinions videoOpinions
	var ratings []rating
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		if id, err = resolveVideo(ctx, q, ref); err != nil {
			return err
		}
		only := sql.NullString{String: id, Valid: true}
		if stored, err = q.GetVideo(ctx, id); err != nil {
			return err
		}
		if tracks, err = q.ListTracks(ctx, id); err != nil {
			return err
		}
		if artists, err = q.ListVideoArtists(ctx, only); err != nil {
			return err
		}
		if opinions, err = readOpinions(ctx, q, only); err != nil {
			return err
		}
		rows, err := q.ListRatings(ctx, generated.ListRatingsParams{VideoID: only, MaxRows: everyRow})
		if err != nil {
			return err
		}
		tagRows, err := q.ListRatingTags(ctx, only)
		if err != nil {
			return err
		}
		tags := tagsByRating(tagRows)
		for _, row := range rows {
			ratings = append(ratings, ratingFrom(row, tags))
		}
		playlists, err = q.ListVideoPlaylists(ctx, only)
		return err
	})
	if err != nil {
		h.writeItemError(w, r, err, "video "+ref)
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
		Ratings:     orEmpty(ratings),
	}
	opinions.fill(&shown.libraryVideo)
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

// ratedNone is what the rated filter takes for a video with no rating.
const ratedNone = "unrated"

// videoOpinions is what ratings said of each video: its counts by kind, its
// messages joined, and its tags without repeats, alphabetically.
type videoOpinions struct {
	counts   map[string]generated.ListVideoOpinionsRow
	messages map[string]string
	tags     map[string][]string
}

// readOpinions is what ratings said of every video, or of the video only names
// when it is valid.
func readOpinions(ctx context.Context, q *generated.Queries, only sql.NullString) (videoOpinions, error) {
	opinions := videoOpinions{counts: map[string]generated.ListVideoOpinionsRow{}, messages: map[string]string{}, tags: map[string][]string{}}
	rows, err := q.ListVideoOpinions(ctx)
	if err != nil {
		return opinions, err
	}
	for _, row := range rows {
		opinions.counts[row.VideoID] = row
		opinions.messages[row.VideoID] = row.Messages
	}
	tagRows, err := q.ListRatingTags(ctx, only)
	if err != nil {
		return opinions, err
	}
	for _, row := range tagRows {
		if !slices.Contains(opinions.tags[row.VideoID], row.Tag) {
			opinions.tags[row.VideoID] = append(opinions.tags[row.VideoID], row.Tag)
		}
	}
	for _, tags := range opinions.tags {
		slices.Sort(tags)
	}
	return opinions, nil
}

// fill sets v's counts and tags.
func (o videoOpinions) fill(v *libraryVideo) {
	counts := o.counts[v.ID]
	v.Likes, v.Dislikes, v.Notes = counts.Likes, counts.Dislikes, counts.Notes
	v.Tags = orEmpty(o.tags[v.ID])
}

// rated reports whether v holds a rating of kind, or none at all for
// ratedNone.
func (o videoOpinions) rated(v libraryVideo, kind string) bool {
	switch kind {
	case kindLike:
		return v.Likes > 0
	case kindDislike:
		return v.Dislikes > 0
	case kindNote:
		return v.Notes > 0
	}
	return v.Likes+v.Dislikes+v.Notes == 0
}
