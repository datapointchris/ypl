package handlers

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/wire"
)

// playlistSummary is one playlist as GET /api/v1/playlists lists it, by title.
type playlistSummary struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	Privacy          string `json:"privacy"`
	ItemCount        int64  `json:"item_count"`
	UnavailableCount int64  `json:"unavailable_count"`
	EnrichedCount    int64  `json:"enriched_count"`
}

// playlist is one playlist with its items in the server's order.
type playlist struct {
	playlistSummary
	Items []playlistItem `json:"items"`
}

// playlistItem is one slot of the server's order of a playlist: the id of the
// YouTube item holding it, which is null until the sync adds it to YouTube, its
// position counting from 0, and the video in it.
type playlistItem struct {
	ID       *string      `json:"id"`
	Position int64        `json:"position"`
	Video    videoSummary `json:"video"`
}

// videoSummary is a video as a playlist or the library lists it.
type videoSummary struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ChannelTitle    string  `json:"channel_title"`
	DurationSeconds *int64  `json:"duration_seconds"`
	UploadDate      *string `json:"upload_date"`
	IsUnavailable   bool    `json:"is_unavailable"`
	EnrichedTs      *string `json:"enriched_ts"`
	TrackCount      int64   `json:"track_count"`
}

func (h *Handlers) listPlaylists(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.Queries.ListPlaylistSummaries(r.Context())
	if err != nil {
		h.writeInternalError(w, r, err)
		return
	}
	playlists := make([]playlistSummary, len(rows))
	for i, row := range rows {
		playlists[i] = playlistSummary{
			ID:               row.PlaylistID,
			Title:            row.Title,
			Description:      row.Description,
			Privacy:          row.Privacy,
			ItemCount:        row.ItemCount,
			UnavailableCount: row.UnavailableCount,
			EnrichedCount:    row.EnrichedCount,
		}
	}
	c := newCollator()
	slices.SortFunc(playlists, func(a, b playlistSummary) int {
		return cmp.Or(c.CompareString(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
	})
	wire.JSON(w, http.StatusOK, playlists)
}

func (h *Handlers) showPlaylist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.PathValue("id")
	var shown playlist
	err := h.store.InReadTx(ctx, func(q *generated.Queries) error {
		id, err := resolvePlaylist(ctx, q, "playlist", ref, loosely)
		if err != nil {
			return err
		}
		shown, err = readPlaylist(ctx, q, id)
		return err
	})
	if err != nil {
		h.writeItemError(w, r, err, "playlist "+ref)
		return
	}
	wire.JSON(w, http.StatusOK, shown)
}

// resolvePlaylist is the id of the playlist ref names, by its id or as
// resolveTitled says, looking as far as how says. name is what the error calls
// ref.
func resolvePlaylist(ctx context.Context, q *generated.Queries, name, ref string, how reach) (string, error) {
	// The id is the form every stored client already sends, so it stays a keyed
	// read. Listing every playlist to find it would grow the cost of the common
	// case with the channel, invisibly, since the id still resolves either way.
	if _, err := q.GetPlaylist(ctx, ref); err == nil {
		return ref, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	rows, err := q.ListPlaylistReferences(ctx)
	if err != nil {
		return "", err
	}
	named := make([]titled, len(rows))
	for i, row := range rows {
		named[i] = titled{id: row.PlaylistID, title: row.Title}
	}
	return resolveTitled(named, name, ref, how)
}

// slug is title lowercased, with every run of anything that is not a letter or
// a digit standing as one hyphen, and none at either end. Letters outside ASCII
// are kept rather than dropped, since dropping them leaves a playlist named in
// one unreachable by its own name.
func slug(title string) string {
	var slugged strings.Builder
	var pending bool
	for _, r := range title {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			pending = true
			continue
		}
		if pending && slugged.Len() > 0 {
			slugged.WriteByte('-')
		}
		pending = false
		slugged.WriteRune(unicode.ToLower(r))
	}
	return slugged.String()
}

// readPlaylist is the stored playlist id with its items in the server's order.
func readPlaylist(ctx context.Context, q *generated.Queries, id string) (playlist, error) {
	stored, err := q.GetPlaylist(ctx, id)
	if err != nil {
		return playlist{}, err
	}
	rows, err := q.ListPlaylistEntries(ctx, id)
	if err != nil {
		return playlist{}, err
	}
	shown := playlist{
		playlistSummary: playlistSummary{
			ID:          stored.PlaylistID,
			Title:       stored.Title,
			Description: stored.Description,
			Privacy:     stored.Privacy,
			ItemCount:   int64(len(rows)),
		},
		Items: make([]playlistItem, len(rows)),
	}
	for i, row := range rows {
		if row.IsUnavailable {
			shown.UnavailableCount++
		}
		if row.EnrichedTs.Valid {
			shown.EnrichedCount++
		}
		shown.Items[i] = playlistItem{
			ID:       nullableText(row.ItemID),
			Position: row.Position,
			Video: videoSummary{
				ID:              row.VideoID,
				Title:           row.Title,
				ChannelTitle:    row.ChannelTitle,
				DurationSeconds: nullableInt(row.DurationSeconds),
				UploadDate:      nullableText(row.UploadDate),
				IsUnavailable:   row.IsUnavailable,
				EnrichedTs:      nullableText(row.EnrichedTs),
				TrackCount:      row.TrackCount,
			},
		}
	}
	return shown, nil
}
