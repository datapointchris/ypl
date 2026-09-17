package store

import (
	"context"
	"database/sql"
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

func TestOpenAppliesMigrationsAndSeedsTheSources(t *testing.T) {
	st, _ := open(t)
	if got, want := countSources(t, st), len(trackSources); got != want {
		t.Fatalf("track sources = %d, want %d", got, want)
	}
}

func TestReopeningAnExistingDatabaseChangesNothing(t *testing.T) {
	ctx := context.Background()
	first, path := open(t)
	if err := first.Queries.UpsertVideo(ctx, video("v1")); err != nil {
		t.Fatalf("upsert: %v", err)
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
	if n, err := again.Queries.CountVideos(ctx); err != nil || n != 1 {
		t.Fatalf("videos after reopen = %d, %v; want 1", n, err)
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
	err := st.Queries.InsertTrack(context.Background(), track("missing", 1))
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
	if err := st.Queries.UpsertVideo(ctx, video("v1")); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	replace := func(tracks ...generated.InsertTrackParams) {
		t.Helper()
		err := st.InTx(ctx, func(q *generated.Queries) error {
			return ReplaceTracks(ctx, q, "v1", tracks)
		})
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
	}
	replace(track("", 1), track("", 2), track("", 3))
	replace(track("", 1))

	tracks, err := st.Queries.ListTracks(ctx, "v1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Position != 1 {
		t.Fatalf("tracks after the second replace = %+v, want one at position 1", tracks)
	}
}

func TestAFailedTransactionLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	err := st.InTx(ctx, func(q *generated.Queries) error {
		if err := q.UpsertVideo(ctx, video("v1")); err != nil {
			return err
		}
		return q.InsertTrack(ctx, track("missing", 1))
	})
	if err == nil {
		t.Fatal("the transaction committed a track for a missing video")
	}
	if n, _ := st.Queries.CountVideos(ctx); n != 0 {
		t.Fatalf("videos after a rolled-back transaction = %d, want 0", n)
	}
}

func video(id string) generated.UpsertVideoParams {
	return generated.UpsertVideoParams{VideoID: id, Title: "A Mix", ChannelTitle: "A Channel"}
}

func track(videoID string, position int64) generated.InsertTrackParams {
	return generated.InsertTrackParams{
		VideoID:  videoID,
		Position: position,
		Title:    "A Track",
		RawText:  "00:00 A Track",
		Source:   "chapter",
	}
}
