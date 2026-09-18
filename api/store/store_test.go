package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

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
	st, path := open(t)
	if got, want := countSources(t, st), len(trackSources); got != want {
		t.Fatalf("track sources = %d, want %d", got, want)
	}

	// Two programs open this store and both migrate on the way in, so the
	// migration is serialized by a lock beside the database. Opening a second
	// store on the same path while the first is live is what a seed command run
	// beside a starting server does, and it has to find the schema whole rather
	// than half applied.
	beside, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open a second store on the same path: %v", err)
	}
	defer func() { _ = beside.Close() }()
	if got, want := countSources(t, beside), len(trackSources); got != want {
		t.Fatalf("the second open sees %d track sources, want %d", got, want)
	}
	// The lock is released rather than held for the life of the store, or the
	// second open above would block until the first one closed.
	if _, err := os.Stat(path + ".migrate.lock"); err != nil {
		t.Errorf("the migration lock file is not beside the database: %v", err)
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

func TestAReadTransactionReadsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	err := st.InReadTx(ctx, func(q *generated.Queries) error {
		before, err := q.CountVideos(ctx)
		if err != nil {
			return err
		}
		if err := st.Queries.ImportVideo(ctx, video("v1")); err != nil {
			return err
		}
		after, err := q.CountVideos(ctx)
		if err != nil {
			return err
		}
		if before != 0 || after != 0 {
			t.Errorf("videos read inside the transaction = %d then %d, want 0 both times", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InReadTx: %v", err)
	}
	if n := countVideos(t, st); n != 1 {
		t.Fatalf("videos after the read transaction = %d, want the 1 committed meanwhile", n)
	}
}

// A second connection that does not wait for a lock shows whether the
// transaction holds the write lock before its first write.
func TestAWriteTransactionHoldsTheWriteLockFromItsStart(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	other, err := sql.Open("sqlite", URI(path, "_pragma=foreign_keys(1)&_pragma=busy_timeout(0)"))
	if err != nil {
		t.Fatalf("open a second connection: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	err = st.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.CountVideos(ctx); err != nil {
			return err
		}
		if err := generated.New(other).ImportVideo(ctx, video("v2")); sqliteCode(err) != sqlite3.SQLITE_BUSY {
			t.Errorf("another connection's write while a write transaction was open = %v, want SQLITE_BUSY", err)
		}
		return tx.ImportVideo(ctx, video("v1"))
	})
	if err != nil {
		t.Fatalf("a transaction that read before it wrote: %v", err)
	}
	if n := countVideos(t, st); n != 1 {
		t.Fatalf("videos = %d, want the transaction's 1", n)
	}
}

// sqliteCode is the primary SQLite result code err carries, or -1 when it
// carries none.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return -1
	}
	return se.Code() & 0xff
}

// With one connection in the pool, the write after the read transaction runs
// on the connection the transaction used.
func TestAReadTransactionRefusesAWriteAndLeavesItsConnectionWritable(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	st.db.SetMaxOpenConns(1)

	err := st.InReadTx(ctx, func(q *generated.Queries) error {
		return q.ImportVideo(ctx, video("v1"))
	})
	if sqliteCode(err) != sqlite3.SQLITE_READONLY {
		t.Fatalf("a write inside a read transaction = %v, want SQLITE_READONLY", err)
	}
	if err := st.Queries.ImportVideo(ctx, video("v2")); err != nil {
		t.Fatalf("a write after the read transaction: %v", err)
	}
	if n := countVideos(t, st); n != 1 {
		t.Fatalf("videos = %d, want only the write made after the read transaction", n)
	}
}

func TestPlaysTakeHandlesInOrderAndARepeatTakesNone(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.ImportVideo(ctx, video("v1")); err != nil {
		t.Fatalf("import video: %v", err)
	}
	for _, id := range []string{"p1", "p2", "p1", "p3"} {
		if _, err := st.Queries.InsertPlay(ctx, generated.InsertPlayParams{PlayID: id, VideoID: "v1", PlayedTs: "2026-09-01T10:00:00Z"}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	for id, want := range map[string]int64{"p1": 1, "p2": 2, "p3": 3} {
		if got, err := st.Queries.GetPlay(ctx, id); err != nil || got.Handle != want {
			t.Errorf("%s handle = %d, %v, want %d", id, got.Handle, err, want)
		}
	}
}

func TestAPlayTimeNotInUTCToTheSecondIsRefused(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if err := st.Queries.ImportVideo(ctx, video("v1")); err != nil {
		t.Fatalf("import video: %v", err)
	}
	for _, ts := range []string{"2026-09-01T10:00:00.5Z", "2026-09-01T10:00:00+02:00", "2026-09-01 10:00:00", "2026-09-01T10:00:00z"} {
		if _, err := st.Queries.InsertPlay(ctx, generated.InsertPlayParams{PlayID: ts, VideoID: "v1", PlayedTs: ts}); err == nil {
			t.Errorf("a play at %q was stored", ts)
		}
	}
	if _, err := st.Queries.InsertPlay(ctx, generated.InsertPlayParams{PlayID: "p", VideoID: "v1", PlayedTs: "2026-09-01T10:00:00Z"}); err != nil {
		t.Fatalf("a play at 2026-09-01T10:00:00Z: %v", err)
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
