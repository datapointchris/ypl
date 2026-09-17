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

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/datapointchris/ypl/api/store/generated"
)

//go:embed migrations/*.sql
var migrations embed.FS

// trackSources is the vocabulary tracks.source draws from. It is upserted on
// every open, so no database is left unable to take a track.
var trackSources = []generated.UpsertTrackSourceParams{
	{Source: "chapter", Label: "Chapter", Description: "A YouTube chapter marker, with real timestamps"},
	{Source: "description", Label: "Description", Description: "Parsed from the video description"},
}

// Store is a database with its migrations applied and its lookups seeded.
type Store struct {
	db *sql.DB

	// Queries runs against the connection pool. InTx hands out a copy bound to
	// one transaction.
	Queries *generated.Queries
}

// Open opens the database at path, creating it if needed, then applies every
// pending migration and seeds the lookup tables.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
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
func (s *Store) InTx(ctx context.Context, fn func(*generated.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(s.Queries.WithTx(tx)); err != nil {
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
func ReplaceTracks(ctx context.Context, q *generated.Queries, videoID string, tracks []generated.InsertTrackParams) error {
	if err := q.DeleteTracks(ctx, videoID); err != nil {
		return fmt.Errorf("delete tracks of %s: %w", videoID, err)
	}
	for _, track := range tracks {
		track.VideoID = videoID
		if err := q.InsertTrack(ctx, track); err != nil {
			return fmt.Errorf("insert track %d of %s: %w", track.Position, videoID, err)
		}
	}
	return nil
}

// dsn names the file and its pragmas together. A pragma set with a statement
// configures only the pooled connection that ran it, while the driver applies
// these to every connection as it opens.
func dsn(path string) string {
	pragmas := "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	return (&url.URL{Scheme: "file", Path: path, RawQuery: pragmas}).String()
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
	return nil
}
