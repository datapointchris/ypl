package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/datapointchris/ypl/api/store/generated"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func countSources(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(), "SELECT count(*) FROM track_sources").Scan(&n); err != nil {
		t.Fatalf("count track sources: %v", err)
	}
	return n
}

func countVideos(t *testing.T, st *Store) int64 {
	t.Helper()
	n, err := st.Queries.CountVideos(context.Background())
	if err != nil {
		t.Fatalf("count videos: %v", err)
	}
	return n
}

func TestPathPrefersTheEnvironmentThenDataHome(t *testing.T) {
	t.Setenv("DATABASE_PATH", "/data/ypl.db")
	if got, err := Path(); err != nil || got != "/data/ypl.db" {
		t.Fatalf("Path with DATABASE_PATH set = %q, %v", got, err)
	}

	t.Setenv("DATABASE_PATH", "")
	t.Setenv("XDG_DATA_HOME", "/share")
	if got, err := Path(); err != nil || got != "/share/ypl/api.db" {
		t.Fatalf("Path under XDG_DATA_HOME = %q, %v", got, err)
	}
}

func TestARelativePathOpensAtThatPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, path := range []string{"api.db", filepath.Join("rel", "api.db")} {
		st, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open(%q): %v", path, err)
		}
		_ = st.Close()
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("Open(%q) did not create the file at that path: %v", path, err)
		}
	}
}

func TestOpenCreatesAMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ypl", "api.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open under a missing directory: %v", err)
	}
	_ = st.Close()
}

func TestOpenAppliesMigrationsAndSeedsTheSources(t *testing.T) {
	st, _ := open(t)
	if got, want := countSources(t, st), len(trackSources); got != want {
		t.Fatalf("track sources = %d, want %d", got, want)
	}
}

func TestReopeningAnExistingDatabaseChangesNothing(t *testing.T) {
	ctx := context.Background()
	first, path := open(t)
	if err := first.Queries.ImportVideo(ctx, video("v1")); err != nil {
		t.Fatalf("import video: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	if got := countSources(t, again); got != len(trackSources) {
		t.Fatalf("track sources after reopen = %d, want %d", got, len(trackSources))
	}
	if n := countVideos(t, again); n != 1 {
		t.Fatalf("videos after reopen = %d, want 1", n)
	}
}

// Pragmas set with a statement reach one pooled connection, so the check holds
// several open at once.
func TestEveryPooledConnectionEnforcesForeignKeysInWALMode(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)

	var conns []*sql.Conn
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range 4 {
		conn, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		conns = append(conns, conn)

		var foreignKeys int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("foreign_keys: %v", err)
		}
		var journal string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("journal_mode: %v", err)
		}
		if foreignKeys != 1 || journal != "wal" {
			t.Fatalf("connection %d: foreign_keys=%d journal_mode=%s, want 1 and wal", len(conns), foreignKeys, journal)
		}
	}
}

func TestATrackNamingAnUnknownVideoIsRefused(t *testing.T) {
	st, _ := open(t)
	err := st.Queries.InsertTrack(context.Background(), track("missing", 1, "chapter"))
	if err == nil {
		t.Fatal("inserted a track for a video the database does not hold")
	}
}

func TestUnavailabilityIsABoolean(t *testing.T) {
	st, _ := open(t)
	_, err := st.db.ExecContext(context.Background(),
		"INSERT INTO videos (video_id, title, channel_title, is_unavailable) VALUES ('v1', 't', 'c', 2)")
	if err == nil {
		t.Fatal("stored is_unavailable = 2")
	}
}

func TestReplaceTracksReplacesTheWholeTracklist(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.ImportVideo(ctx, video("v1")); err != nil {
		t.Fatalf("import video: %v", err)
	}
	replace(t, st, track("", 1, "chapter"), track("", 2, "chapter"), track("", 3, "chapter"))
	replace(t, st, track("", 1, "chapter"))

	tracks, err := st.Queries.ListTracks(ctx, "v1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Position != 1 {
		t.Fatalf("tracks after the second replace = %+v, want one at position 1", tracks)
	}
}

// A failure partway through a replacement rolls the whole transaction back,
// so the video keeps the tracklist it had.
func TestAFailedReplacementKeepsThePreviousTracklist(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.ImportVideo(ctx, video("v1")); err != nil {
		t.Fatalf("import video: %v", err)
	}
	replace(t, st, track("", 1, "chapter"), track("", 2, "chapter"))

	err := st.InTx(ctx, func(tx *Tx) error {
		return tx.ReplaceTracks(ctx, "v1", []generated.InsertTrackParams{track("", 1, "chapter"), track("", 2, "unseeded")})
	})
	if err == nil {
		t.Fatal("replaced a tracklist with a track naming an unseeded source")
	}

	tracks, err := st.Queries.ListTracks(ctx, "v1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks after a failed replacement = %d, want the previous 2", len(tracks))
	}
}

func TestAFailedTransactionLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	err := st.InTx(ctx, func(tx *Tx) error {
		if err := tx.ImportVideo(ctx, video("v1")); err != nil {
			return err
		}
		return tx.InsertTrack(ctx, track("missing", 1, "chapter"))
	})
	if err == nil {
		t.Fatal("the transaction committed a track for a missing video")
	}
	if n := countVideos(t, st); n != 0 {
		t.Fatalf("videos after a rolled-back transaction = %d, want 0", n)
	}
}

func replace(t *testing.T, st *Store, tracks ...generated.InsertTrackParams) {
	t.Helper()
	ctx := context.Background()
	err := st.InTx(ctx, func(tx *Tx) error {
		return tx.ReplaceTracks(ctx, "v1", tracks)
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
}

func video(id string) generated.ImportVideoParams {
	return generated.ImportVideoParams{VideoID: id, Title: "A Mix", ChannelTitle: "A Channel"}
}

func track(videoID string, position int64, source string) generated.InsertTrackParams {
	return generated.InsertTrackParams{
		VideoID:  videoID,
		Position: position,
		Title:    "A Track",
		RawText:  "00:00 A Track",
		Source:   source,
	}
}
