package pyimport

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

// pythonSchema is the Python tool's own mirror schema, so the source here has
// the shape a real mirror has, its seeded track sources included.
const pythonSchema = "../../src/ypl/schema.sql"

// The fixture: v2 is in both playlists, v1 and v4 are enriched and v4's
// description was read as empty, v2 and v3 were never enriched.
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

	exec(t, db,
		string(schema),
		`INSERT INTO playlists (playlist_id, title, channel, synced_ts) VALUES
			('PLA', 'Owned', 'Owner', '2026-09-01T00:00:00+00:00'),
			('PLB', 'Also Owned', 'Owner', '2026-09-01T00:00:00+00:00')`,
		`INSERT INTO videos (video_id, title, channel, duration_seconds, description, upload_date, is_unavailable, enriched_ts) VALUES
			('v1', 'Mix One', 'DJ One', 3600, 'tracklist below', '20240115', 0, '2026-09-02T00:00:00+00:00'),
			('v2', 'Gone', 'DJ Two', NULL, '', NULL, 1, NULL),
			('v3', 'Elsewhere', 'DJ Three', 1800, '', '20230301', 0, NULL),
			('v4', 'Quiet', 'DJ Four', 600, '', NULL, 0, '2026-09-04T00:00:00+00:00')`,
		`INSERT INTO playlist_videos (playlist_id, video_id, position) VALUES
			('PLA', 'v1', 0), ('PLA', 'v2', 1), ('PLB', 'v2', 0), ('PLB', 'v3', 1), ('PLB', 'v4', 2)`,
		`INSERT INTO tracks (video_id, position, start_seconds, end_seconds, artist, title, raw_text, source) VALUES
			('v1', 1, 0, 300, 'Artist', 'First', '00:00 Artist - First', 'chapter'),
			('v1', 2, 300, NULL, NULL, 'Intro', '05:00 Intro', 'description'),
			('v3', 1, 0, NULL, 'Other', 'Only', '00:00 Other - Only', 'chapter'),
			('v4', 1, 60, 120, 'Someone', 'Named', 'Someone - Named', 'llm')`,
		`INSERT INTO enrich_failures (video_id, attempted_ts, reason) VALUES
			('v2', '2026-09-03T00:00:00+00:00', 'Video unavailable')`,
	)
	return db
}

func exec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("build source mirror: %v", err)
		}
	}
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

func importOK(t *testing.T, source *sql.DB, st *store.Store, playlistIDs ...string) Counts {
	t.Helper()
	counts, err := Import(context.Background(), source, st, playlistIDs)
	if err != nil {
		t.Fatalf("Import(%v): %v", playlistIDs, err)
	}
	return counts
}

func getVideo(t *testing.T, st *store.Store, videoID string) generated.Video {
	t.Helper()
	got, err := st.Queries.GetVideo(context.Background(), videoID)
	if err != nil {
		t.Fatalf("get %s: %v", videoID, err)
	}
	return got
}

func text(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func number(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

func TestImportCopiesOnlyTheNamedPlaylistsVideos(t *testing.T) {
	st := target(t)
	counts := importOK(t, sourceMirror(t), st, "PLA")

	if want := (Counts{Videos: 2, Tracks: 2, EnrichFailures: 1}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if _, err := st.Queries.GetVideo(context.Background(), "v3"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get v3 = %v, want no row: v3 is only in PLB", err)
	}
}

func TestAVideoInSeveralPlaylistsIsCopiedOnce(t *testing.T) {
	st := target(t)
	counts := importOK(t, sourceMirror(t), st, "PLA", "PLB")

	if want := (Counts{Videos: 4, Tracks: 4, EnrichFailures: 1}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	videos, err := st.Queries.CountVideos(context.Background())
	if err != nil || videos != 4 {
		t.Fatalf("videos in the store = %d, %v; want 4", videos, err)
	}
}

func TestImportCarriesEveryFieldAcross(t *testing.T) {
	ctx := context.Background()
	st := target(t)
	importOK(t, sourceMirror(t), st, "PLA", "PLB")

	wantVideos := []generated.Video{
		{
			VideoID: "v1", Title: "Mix One", ChannelTitle: "DJ One", DurationSeconds: number(3600),
			Description: text("tracklist below"), UploadDate: text("2024-01-15"), EnrichedTs: text("2026-09-02T00:00:00+00:00"),
		},
		{VideoID: "v2", Title: "Gone", ChannelTitle: "DJ Two", IsUnavailable: true},
		{VideoID: "v3", Title: "Elsewhere", ChannelTitle: "DJ Three", DurationSeconds: number(1800), UploadDate: text("2023-03-01")},
		{
			VideoID: "v4", Title: "Quiet", ChannelTitle: "DJ Four", DurationSeconds: number(600),
			Description: text(""), EnrichedTs: text("2026-09-04T00:00:00+00:00"),
		},
	}
	for _, want := range wantVideos {
		if got := getVideo(t, st, want.VideoID); !reflect.DeepEqual(got, want) {
			t.Errorf("video %s\n got %+v\nwant %+v", want.VideoID, got, want)
		}
	}

	wantTracks := map[string][]generated.Track{
		"v1": {
			{VideoID: "v1", Position: 1, StartSeconds: number(0), EndSeconds: number(300), Artist: text("Artist"), Title: "First", RawText: "00:00 Artist - First", Source: "chapter"},
			{VideoID: "v1", Position: 2, StartSeconds: number(300), Title: "Intro", RawText: "05:00 Intro", Source: "description"},
		},
		"v4": {
			{VideoID: "v4", Position: 1, StartSeconds: number(60), EndSeconds: number(120), Artist: text("Someone"), Title: "Named", RawText: "Someone - Named", Source: "llm"},
		},
	}
	for videoID, want := range wantTracks {
		got, err := st.Queries.ListTracks(ctx, videoID)
		if err != nil {
			t.Fatalf("list tracks of %s: %v", videoID, err)
		}
		for i := range got {
			got[i].TrackID = 0
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tracks of %s\n got %+v\nwant %+v", videoID, got, want)
		}
	}

	failure, err := st.Queries.GetEnrichFailure(ctx, "v2")
	if err != nil {
		t.Fatalf("get the enrich failure of v2: %v", err)
	}
	if want := (generated.EnrichFailure{VideoID: "v2", AttemptedTs: "2026-09-03T00:00:00+00:00", Reason: "Video unavailable"}); failure != want {
		t.Errorf("enrich failure\n got %+v\nwant %+v", failure, want)
	}
}

func TestReimportingWritesWhatTheMirrorHoldsNow(t *testing.T) {
	source := sourceMirror(t)
	st := target(t)
	importOK(t, source, st, "PLA")

	exec(t, source,
		`UPDATE videos SET title = 'Mix One, Remastered', description = 'a longer tracklist' WHERE video_id = 'v1'`,
		`UPDATE videos SET description = 'read at last', enriched_ts = '2026-09-05T00:00:00+00:00' WHERE video_id = 'v2'`,
	)
	importOK(t, source, st, "PLA")

	if v1 := getVideo(t, st, "v1"); v1.Title != "Mix One, Remastered" || v1.Description != text("a longer tracklist") {
		t.Fatalf("v1 after re-import = %+v, want the new title and description", v1)
	}
	if v2 := getVideo(t, st, "v2"); v2.Description != text("read at last") || v2.EnrichedTs != text("2026-09-05T00:00:00+00:00") {
		t.Fatalf("v2 after re-import = %+v, want its enrichment", v2)
	}
}

func TestAPlaylistTheMirrorLacksStopsTheImport(t *testing.T) {
	st := target(t)
	_, err := Import(context.Background(), sourceMirror(t), st, []string{"PLA", "PLMISSING"})
	if !errors.Is(err, ErrPlaylistNotInMirror) {
		t.Fatalf("Import with a missing playlist = %v, want ErrPlaylistNotInMirror", err)
	}
	videos, err := st.Queries.CountVideos(context.Background())
	if err != nil {
		t.Fatalf("count videos: %v", err)
	}
	if videos != 0 {
		t.Fatalf("videos after a refused import = %d, want 0", videos)
	}
}

// Every source the Python schema declares has to import, so a source added on
// the Python side fails here until the store seeds it too.
func TestATrackFromEverySourceTheMirrorDeclaresImports(t *testing.T) {
	ctx := context.Background()
	source := sourceMirror(t)
	rows, err := source.QueryContext(ctx, "SELECT source FROM track_sources ORDER BY source")
	if err != nil {
		t.Fatalf("read the mirror's track sources: %v", err)
	}
	var sources []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		sources = append(sources, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("read the mirror's track sources: %v", err)
	}

	for i, name := range sources {
		_, err := source.ExecContext(ctx,
			"INSERT INTO tracks (video_id, position, title, raw_text, source) VALUES ('v1', ?, 'T', 'T', ?)", 100+i, name)
		if err != nil {
			t.Fatalf("add a %s track: %v", name, err)
		}
	}

	st := target(t)
	importOK(t, source, st, "PLA")
	tracks, err := st.Queries.ListTracks(ctx, "v1")
	if err != nil {
		t.Fatalf("list tracks: %v", err)
	}
	imported := map[string]bool{}
	for _, track := range tracks {
		imported[track.Source] = true
	}
	for _, name := range sources {
		if !imported[name] {
			t.Errorf("no %s track was imported", name)
		}
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
