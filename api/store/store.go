// Package store is the API's SQLite database: its schema migrations, the lookup
// rows every open seeds, and the typed queries sqlc generates from queries.sql.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/datapointchris/ypl/api/store/generated"
)

//go:embed migrations/*.sql
var migrations embed.FS

// trackSources is the vocabulary tracks.source draws from, upserted on every
// open. It matches the Python tool's seed, so every track a Python mirror can
// hold has its source here.
var trackSources = []generated.UpsertTrackSourceParams{
	{Source: "chapter", Label: "Chapter", Description: "YouTube chapter marker, carries real timestamps"},
	{Source: "description", Label: "Description", Description: "Parsed from the video description"},
	{Source: "llm", Label: "Claude", Description: "Extracted by Claude from unstructured text"},
	{Source: "manual", Label: "Manual", Description: "Entered by hand"},
}

// quotaMethods is every YouTube Data API method this service calls, with its
// quota cost from developers.google.com/youtube/v3/determine_quota_cost,
// upserted on every open.
var quotaMethods = []generated.UpsertQuotaMethodParams{
	{Method: "playlists.list", Units: 1},
	{Method: "playlistItems.list", Units: 1},
}

// Store is a database with its migrations applied and its lookups seeded.
type Store struct {
	db *sql.DB

	// Queries runs against the connection pool, one statement at a time. Writes
	// that have to land together go through InTx.
	Queries *generated.Queries
}

// Tx is one transaction's queries, with the writes that are only correct
// inside a transaction.
type Tx struct {
	*generated.Queries
}

// Path is DATABASE_PATH, or api.db in ypl's directory under $XDG_DATA_HOME.
// The database is data rather than state: the server keeps the only copy of
// what it stores.
func Path() (string, error) {
	if path := os.Getenv("DATABASE_PATH"); path != "" {
		return path, nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the data directory: %w", err)
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "ypl", "api.db"), nil
}

// URI is the file: URI naming path, with query as its parameters. It carries no
// host, so a relative path stays a path: `file://api.db` would make SQLite read
// api.db as the URI's authority.
func URI(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, OmitHost: true, RawQuery: query}).String()
}

// Open opens the database at path, creating it and its directory if needed,
// then applies every pending migration and seeds the lookup tables.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create the database directory: %w", err)
	}
	// Pragmas go in the URI because a pragma set with a statement configures
	// only the pooled connection that ran it. The driver applies these to every
	// connection as it opens.
	db, err := sql.Open("sqlite", URI(path, "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	st := &Store{db: db, Queries: generated.New(db)}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := st.seed(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

// Close closes the connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// InTx runs fn inside one transaction, committing when fn returns nil and
// rolling back otherwise.
func (s *Store) InTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(&Tx{Queries: s.Queries.WithTx(tx)}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReplaceTracks sets a video's tracklist to tracks, removing every track it
// held before. Each track's VideoID is set from videoID.
func (tx *Tx) ReplaceTracks(ctx context.Context, videoID string, tracks []generated.InsertTrackParams) error {
	if err := tx.DeleteTracks(ctx, videoID); err != nil {
		return fmt.Errorf("delete tracks of %s: %w", videoID, err)
	}
	for _, track := range tracks {
		track.VideoID = videoID
		if err := tx.InsertTrack(ctx, track); err != nil {
			return fmt.Errorf("insert track %d of %s from source %q: %w", track.Position, videoID, track.Source, err)
		}
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func (s *Store) seed(ctx context.Context) error {
	for _, source := range trackSources {
		if err := s.Queries.UpsertTrackSource(ctx, source); err != nil {
			return fmt.Errorf("seed track source %s: %w", source.Source, err)
		}
	}
	for _, method := range quotaMethods {
		if err := s.Queries.UpsertQuotaMethod(ctx, method); err != nil {
			return fmt.Errorf("seed quota method %s: %w", method.Method, err)
		}
	}
	return nil
}
