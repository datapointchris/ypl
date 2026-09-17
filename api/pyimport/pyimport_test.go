package pyimport

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/datapointchris/ypl/api/store"
)

// pythonSchema is the Python tool's own mirror schema, so the source here has
// the shape a real mirror has.
const pythonSchema = "../../src/ypl/schema.sql"

func sourceMirror(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(pythonSchema)
	if err != nil {
		t.Fatalf("read the Python schema: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ypl.db"))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	statements := []string{
		string(schema),
		`INSERT INTO playlists (playlist_id, title, channel, synced_ts) VALUES
			('PLA', 'Owned', 'Owner', '2026-09-01T00:00:00+00:00'),
			('PLB', 'Saved', 'Someone Else', '2026-09-01T00:00:00+00:00')`,
		`INSERT INTO videos (video_id, title, channel, duration_seconds, description, upload_date, is_unavailable, enriched_ts) VALUES
			('v1', 'Mix One', 'DJ One', 3600, 'tracklist below', '20240115', 0, '2026-09-02T00:00:00+00:00'),
			('v2', 'Gone', 'DJ Two', NULL, '', NULL, 1, NULL),
			('v3', 'Elsewhere', 'DJ Three', 1800, '', '20230301', 0, NULL)`,
		`INSERT INTO playlist_videos (playlist_id, video_id, position) VALUES
			('PLA', 'v1', 0), ('PLA', 'v2', 1), ('PLB', 'v2', 0), ('PLB', 'v3', 1)`,
		`INSERT INTO tracks (video_id, position, start_seconds, end_seconds, artist, title, raw_text, source) VALUES
			('v1', 1, 0, 300, 'Artist', 'First', '00:00 Artist - First', 'chapter'),
			('v1', 2, 300, NULL, NULL, 'Intro', '05:00 Intro', 'description'),
			('v3', 1, 0, NULL, 'Other', 'Only', '00:00 Other - Only', 'chapter')`,
		`INSERT INTO enrich_failures (video_id, attempted_ts, reason) VALUES
			('v2', '2026-09-03T00:00:00+00:00', 'Video unavailable')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("build source mirror: %v", err)
		}
	}
	return db
}

func target(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestImportCopiesOnlyTheNamedPlaylistsVideos(t *testing.T) {
	ctx := context.Background()
	st := target(t)

	counts, err := Import(ctx, sourceMirror(t), st, []string{"PLA"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if want := (Counts{Videos: 2, Tracks: 2, EnrichFailures: 1}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if _, err := st.Queries.GetVideo(ctx, "v3"); err == nil {
		t.Fatal("imported v3, which is only in a playlist that was not named")
	}
}

func TestImportCarriesEveryFieldAcross(t *testing.T) {
	ctx := context.Background()
	st := target(t)
	if _, err := Import(ctx, sourceMirror(t), st, []string{"PLA"}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	v1, err := st.Queries.GetVideo(ctx, "v1")
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.ChannelTitle != "DJ One" || v1.DurationSeconds.Int64 != 3600 || v1.Description != "tracklist below" ||
		v1.UploadDate.String != "2024-01-15" || v1.IsUnavailable || v1.EnrichedTs.String != "2026-09-02T00:00:00+00:00" {
		t.Fatalf("v1 = %+v", v1)
	}

	v2, err := st.Queries.GetVideo(ctx, "v2")
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if !v2.IsUnavailable || v2.UploadDate.Valid || v2.DurationSeconds.Valid || v2.EnrichedTs.Valid {
		t.Fatalf("v2 = %+v, want unavailable with no upload date, duration or enrichment", v2)
	}

	tracks, err := st.Queries.ListTracks(ctx, "v1")
	if err != nil {
		t.Fatalf("list tracks: %v", err)
	}
	if len(tracks) != 2 || tracks[0].Artist.String != "Artist" || tracks[1].Artist.Valid || tracks[1].Source != "description" {
		t.Fatalf("v1 tracks = %+v, want an artist on the first and none on the second", tracks)
	}
}

func TestImportingTwiceWritesTheSameRows(t *testing.T) {
	ctx := context.Background()
	source := sourceMirror(t)
	st := target(t)
	for range 2 {
		if _, err := Import(ctx, source, st, []string{"PLA"}); err != nil {
			t.Fatalf("Import: %v", err)
		}
	}
	videos, _ := st.Queries.CountVideos(ctx)
	tracks, _ := st.Queries.CountTracks(ctx)
	failures, _ := st.Queries.CountEnrichFailures(ctx)
	if videos != 2 || tracks != 2 || failures != 1 {
		t.Fatalf("after two imports: %d videos, %d tracks, %d failures; want 2, 2, 1", videos, tracks, failures)
	}
}

func TestAPlaylistTheMirrorLacksStopsTheImport(t *testing.T) {
	ctx := context.Background()
	st := target(t)
	if _, err := Import(ctx, sourceMirror(t), st, []string{"PLA", "PLMISSING"}); err == nil {
		t.Fatal("imported with a playlist id the mirror does not hold")
	}
	if n, _ := st.Queries.CountVideos(ctx); n != 0 {
		t.Fatalf("videos after a refused import = %d, want 0", n)
	}
}

func TestUploadDatesBecomeISO(t *testing.T) {
	cases := map[string]string{"20240115": "2024-01-15", "2024-01-15": "2024-01-15"}
	for in, want := range cases {
		if got, err := isoDate(in); err != nil || got != want {
			t.Errorf("isoDate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := isoDate("Jan 15"); err == nil {
		t.Error("isoDate accepted a date in neither format")
	}
}
