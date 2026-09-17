// Package pyimport copies videos and their tracklists out of a Python ypl
// mirror into the store, for the playlists it is given.
package pyimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

// Counts is what one import wrote.
type Counts struct {
	Videos         int
	Tracks         int
	EnrichFailures int
}

// Import copies every video in playlistIDs, with its tracks and any recorded
// enrich failure, from source into st inside one transaction. Running it again
// overwrites the same rows. A playlist id the source does not hold is an error,
// so a mistyped id cannot shrink the import unnoticed.
func Import(ctx context.Context, source *sql.DB, st *store.Store, playlistIDs []string) (Counts, error) {
	videoIDs, err := videosIn(ctx, source, playlistIDs)
	if err != nil {
		return Counts{}, err
	}

	var counts Counts
	err = st.InTx(ctx, func(q *generated.Queries) error {
		for _, videoID := range videoIDs {
			tracks, failed, err := copyVideo(ctx, source, q, videoID)
			if err != nil {
				return err
			}
			counts.Videos++
			counts.Tracks += tracks
			if failed {
				counts.EnrichFailures++
			}
		}
		return nil
	})
	if err != nil {
		return Counts{}, err
	}
	return counts, nil
}

// videosIn is every distinct video in the named playlists, sorted.
func videosIn(ctx context.Context, source *sql.DB, playlistIDs []string) ([]string, error) {
	seen := map[string]bool{}
	for _, playlistID := range playlistIDs {
		var exists int
		err := source.QueryRowContext(ctx, "SELECT count(*) FROM playlists WHERE playlist_id = ?", playlistID).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("look up playlist %s: %w", playlistID, err)
		}
		if exists == 0 {
			return nil, fmt.Errorf("the mirror holds no playlist %s", playlistID)
		}

		rows, err := source.QueryContext(ctx, "SELECT DISTINCT video_id FROM playlist_videos WHERE playlist_id = ?", playlistID)
		if err != nil {
			return nil, fmt.Errorf("read playlist %s: %w", playlistID, err)
		}
		for rows.Next() {
			var videoID string
			if err := rows.Scan(&videoID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read playlist %s: %w", playlistID, err)
			}
			seen[videoID] = true
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("read playlist %s: %w", playlistID, err)
		}
	}

	videoIDs := make([]string, 0, len(seen))
	for videoID := range seen {
		videoIDs = append(videoIDs, videoID)
	}
	sort.Strings(videoIDs)
	return videoIDs, nil
}

// copyVideo writes one video, replaces its tracklist, and carries over its
// enrich failure. It reports how many tracks it wrote and whether a failure was
// recorded.
func copyVideo(ctx context.Context, source *sql.DB, q *generated.Queries, videoID string) (int, bool, error) {
	video := generated.UpsertVideoParams{VideoID: videoID}
	var uploadDate sql.NullString
	err := source.QueryRowContext(ctx, `
		SELECT title, channel, duration_seconds, description, upload_date, is_unavailable, enriched_ts
		FROM videos WHERE video_id = ?`, videoID,
	).Scan(&video.Title, &video.ChannelTitle, &video.DurationSeconds, &video.Description, &uploadDate, &video.IsUnavailable, &video.EnrichedTs)
	if err != nil {
		return 0, false, fmt.Errorf("read video %s: %w", videoID, err)
	}
	if uploadDate.Valid {
		iso, err := isoDate(uploadDate.String)
		if err != nil {
			return 0, false, fmt.Errorf("video %s: %w", videoID, err)
		}
		video.UploadDate = sql.NullString{String: iso, Valid: true}
	}
	if err := q.UpsertVideo(ctx, video); err != nil {
		return 0, false, fmt.Errorf("write video %s: %w", videoID, err)
	}

	tracks, err := tracksOf(ctx, source, videoID)
	if err != nil {
		return 0, false, err
	}
	if err := store.ReplaceTracks(ctx, q, videoID, tracks); err != nil {
		return 0, false, err
	}

	failure := generated.UpsertEnrichFailureParams{VideoID: videoID}
	err = source.QueryRowContext(ctx, "SELECT attempted_ts, reason FROM enrich_failures WHERE video_id = ?", videoID).
		Scan(&failure.AttemptedTs, &failure.Reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return len(tracks), false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read the enrich failure of %s: %w", videoID, err)
	}
	if err := q.UpsertEnrichFailure(ctx, failure); err != nil {
		return 0, false, fmt.Errorf("write the enrich failure of %s: %w", videoID, err)
	}
	return len(tracks), true, nil
}

func tracksOf(ctx context.Context, source *sql.DB, videoID string) ([]generated.InsertTrackParams, error) {
	rows, err := source.QueryContext(ctx, `
		SELECT position, start_seconds, end_seconds, artist, title, raw_text, source
		FROM tracks WHERE video_id = ? ORDER BY position`, videoID)
	if err != nil {
		return nil, fmt.Errorf("read the tracks of %s: %w", videoID, err)
	}
	var tracks []generated.InsertTrackParams
	for rows.Next() {
		track := generated.InsertTrackParams{VideoID: videoID}
		err := rows.Scan(&track.Position, &track.StartSeconds, &track.EndSeconds, &track.Artist, &track.Title, &track.RawText, &track.Source)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read the tracks of %s: %w", videoID, err)
		}
		tracks = append(tracks, track)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read the tracks of %s: %w", videoID, err)
	}
	return tracks, nil
}

// isoDate turns yt-dlp's YYYYMMDD upload date into YYYY-MM-DD, and passes an
// ISO date through.
func isoDate(value string) (string, error) {
	for _, layout := range []string{"20060102", time.DateOnly} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("upload date %q is neither YYYYMMDD nor YYYY-MM-DD", value)
}
