// Package store is the API's SQLite database: its schema migrations, the lookup
// rows every open seeds, and the typed queries sqlc generates from queries.sql.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
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

// How a sync run ended, the vocabulary sync_runs.outcome draws from.
const (
	OutcomeOK         = "ok"
	OutcomePartial    = "partial"
	OutcomeQuotaSpent = "quota_spent"
	OutcomeFailed     = "failed"
	OutcomeCanceled   = "canceled"
)

// syncOutcomes is the sync_outcomes vocabulary, upserted on every open.
var syncOutcomes = []generated.UpsertSyncOutcomeParams{
	{Outcome: OutcomeOK, Label: "Synced", Description: "Every listed playlist was read and stored"},
	{Outcome: OutcomePartial, Label: "Partly synced", Description: "The run finished, and at least one playlist was not stored or the interval's runs read more than a day's quota"},
	{Outcome: OutcomeQuotaSpent, Label: "Quota spent", Description: "YouTube refused a request for the day's quota, in this run or an earlier one on the same Pacific date"},
	{Outcome: OutcomeFailed, Label: "Failed", Description: "An error ended the run before it finished"},
	{Outcome: OutcomeCanceled, Label: "Canceled", Description: "The run was canceled before it finished"},
}

// How a write sent to YouTube ended, the vocabulary youtube_writes.outcome draws
// from.
const (
	WritePending    = "pending"
	WriteApplied    = "applied"
	WriteAbsent     = "absent"
	WriteRefused    = "refused"
	WriteQuotaSpent = "quota_spent"
	WriteUnanswered = "unanswered"
)

// youtubeWriteOutcomes is the youtube_write_outcomes vocabulary, upserted on
// every open.
var youtubeWriteOutcomes = []generated.UpsertYouTubeWriteOutcomeParams{
	{Outcome: WritePending, Label: "Pending", Description: "Recorded before the write was sent, with no answer recorded: the write is in flight, or the server stopped before recording its answer"},
	{Outcome: WriteApplied, Label: "Applied", Description: "YouTube answered that it made the write"},
	{Outcome: WriteAbsent, Label: "Target absent", Description: "YouTube answered that the playlist or item the write named does not exist"},
	{Outcome: WriteRefused, Label: "Refused", Description: "YouTube refused the write and did not make it"},
	{Outcome: WriteQuotaSpent, Label: "Quota spent", Description: "YouTube refused the write because the day's quota is spent"},
	{Outcome: WriteUnanswered, Label: "Unanswered", Description: "No answer arrived, or none that says what YouTube did, so YouTube may or may not have made the write"},
}

// youtubeWriteMethods is the youtube_write_methods vocabulary, upserted on every
// open. It holds each write the youtube package makes.
var youtubeWriteMethods = []generated.UpsertYouTubeWriteMethodParams{
	{Method: youtube.MethodPlaylistsInsert, Label: "Create a playlist", Description: "Creates a private playlist with a title and a description"},
	{Method: youtube.MethodPlaylistsUpdate, Label: "Change a playlist's details", Description: "Sets a playlist's title and description together"},
	{Method: youtube.MethodPlaylistsDelete, Label: "Delete a playlist", Description: "Deletes a playlist and every item in it"},
	{Method: youtube.MethodPlaylistItemsInsert, Label: "Add a video", Description: "Adds a video to a playlist, at a position or at its end"},
	{Method: youtube.MethodPlaylistItemsUpdate, Label: "Move an item", Description: "Moves an item to a position in its playlist"},
	{Method: youtube.MethodPlaylistItemsDelete, Label: "Remove an item", Description: "Removes an item from its playlist"},
}

// playlistPrivacies is the playlist_privacies vocabulary, upserted on every
// open. It holds each privacy YouTube reports for a playlist.
var playlistPrivacies = []generated.UpsertPlaylistPrivacyParams{
	{Privacy: "public", Label: "Public", Description: "Anyone can find and watch the playlist"},
	{Privacy: "unlisted", Label: "Unlisted", Description: "Anyone with the link can watch the playlist"},
	{Privacy: "private", Label: "Private", Description: "Only the channel can see the playlist"},
}

// PlaylistItem is one item of a playlist as YouTube holds it: its playlistItem id
// and the video in it.
type PlaylistItem struct {
	ItemID  string
	VideoID string
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
	//
	// _txlock=immediate makes a write transaction take the write lock as it
	// begins. One that reads before it writes otherwise holds a snapshot that a
	// commit on another connection makes stale, and its first write then fails
	// with SQLITE_BUSY without busy_timeout waiting. A read-only transaction
	// begins deferred whatever this says.
	db, err := sql.Open("sqlite", URI(path, "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"))
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

// InReadTx runs fn inside one read transaction and rolls it back when fn
// returns. Every statement fn runs reads the database as it stood at fn's first
// read, whatever commits on another connection meanwhile, and a write fn makes
// fails with SQLITE_READONLY.
//
// The driver does not enforce a read-only transaction, so the connection is set
// query_only for the transaction and reset before it returns to the pool. A
// connection whose reset fails is closed rather than returned.
func (s *Store) InReadTx(ctx context.Context, fn func(*generated.Queries) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("take a connection: %w", err)
	}
	defer release(conn)
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return fmt.Errorf("make the connection read-only: %w", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(s.Queries.WithTx(tx))
}

// release returns conn to the pool with writes allowed again, or closes it when
// they cannot be.
func release(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
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

// ReplacePlaylistItems sets the items of the playlist playlistID to items, in
// order, removing every item it held before.
func (tx *Tx) ReplacePlaylistItems(ctx context.Context, playlistID string, items []PlaylistItem) error {
	if err := tx.DeletePlaylistItems(ctx, playlistID); err != nil {
		return fmt.Errorf("delete the items of %s: %w", playlistID, err)
	}
	for position, item := range items {
		row := generated.InsertPlaylistItemParams{ItemID: item.ItemID, PlaylistID: playlistID, Position: int64(position), VideoID: item.VideoID}
		if err := tx.InsertPlaylistItem(ctx, row); err != nil {
			return fmt.Errorf("insert item %s at %d in %s: %w", item.ItemID, position, playlistID, err)
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
	for _, outcome := range syncOutcomes {
		if err := s.Queries.UpsertSyncOutcome(ctx, outcome); err != nil {
			return fmt.Errorf("seed sync outcome %s: %w", outcome.Outcome, err)
		}
	}
	for _, privacy := range playlistPrivacies {
		if err := s.Queries.UpsertPlaylistPrivacy(ctx, privacy); err != nil {
			return fmt.Errorf("seed playlist privacy %s: %w", privacy.Privacy, err)
		}
	}
	for _, outcome := range youtubeWriteOutcomes {
		if err := s.Queries.UpsertYouTubeWriteOutcome(ctx, outcome); err != nil {
			return fmt.Errorf("seed YouTube write outcome %s: %w", outcome.Outcome, err)
		}
	}
	for _, method := range youtubeWriteMethods {
		if err := s.Queries.UpsertYouTubeWriteMethod(ctx, method); err != nil {
			return fmt.Errorf("seed YouTube write method %s: %w", method.Method, err)
		}
	}
	return nil
}

// Timestamp is t as the store holds a time: UTC, to the second, in RFC 3339.
// Times held this way order as text.
func Timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// BeginWrite records a pending write of method to the playlist playlistID, or
// to no playlist yet when playlistID is empty, sent at sentAt, and returns its
// id. It is recorded before the write is sent.
func (s *Store) BeginWrite(ctx context.Context, method, playlistID string, sentAt time.Time) (int64, error) {
	id, err := s.Queries.InsertYouTubeWrite(ctx, generated.InsertYouTubeWriteParams{
		Method:     method,
		PlaylistID: sql.NullString{String: playlistID, Valid: playlistID != ""},
		SentTs:     Timestamp(sentAt),
		QuotaDate:  youtube.QuotaDate(sentAt),
	})
	if err != nil {
		return 0, fmt.Errorf("record the %s write before sending it: %w", method, err)
	}
	return id, nil
}

// Settlement is how a pending write ended.
type Settlement struct {
	WriteID int64
	// PlaylistID is the playlist the write named or created, and empty for a
	// create that returned none.
	PlaylistID string
	Outcome    string
	SettledAt  time.Time
	// Requests and Units are what the write's attempts cost.
	Requests, Units int64
	// Err is why the write did not apply, and nil for an applied one.
	Err error
}

// SettleWrite records how a pending write ended. It fails for a write that is
// not pending.
func (tx *Tx) SettleWrite(ctx context.Context, s Settlement) error {
	params := generated.SettleYouTubeWriteParams{
		WriteID:    s.WriteID,
		PlaylistID: sql.NullString{String: s.PlaylistID, Valid: s.PlaylistID != ""},
		Outcome:    s.Outcome,
		SettledTs:  sql.NullString{String: Timestamp(s.SettledAt), Valid: true},
		Requests:   sql.NullInt64{Int64: s.Requests, Valid: true},
		Units:      sql.NullInt64{Int64: s.Units, Valid: true},
	}
	if s.Err != nil {
		params.Error = sql.NullString{String: s.Err.Error(), Valid: true}
	}
	settled, err := tx.SettleYouTubeWrite(ctx, params)
	switch {
	case err != nil:
		return fmt.Errorf("settle write %d as %s: %w", s.WriteID, s.Outcome, err)
	case settled != 1:
		return fmt.Errorf("settle write %d as %s: no pending write has that id", s.WriteID, s.Outcome)
	}
	return nil
}

// WriteNewerThanRead is the method of the latest write to the playlist
// playlistID that a read sent at readAt may not show: one YouTube answered by
// making it, or by reporting the playlist absent, and that settled later than
// youtube.ReadLag before readAt. ok is false when no write did.
func WriteNewerThanRead(ctx context.Context, q *generated.Queries, playlistID string, readAt time.Time) (method string, ok bool, err error) {
	row, err := q.LatestPlaylistWriteSettledAfter(ctx, generated.LatestPlaylistWriteSettledAfterParams{
		PlaylistID:   sql.NullString{String: playlistID, Valid: true},
		SettledAfter: sql.NullString{String: Timestamp(readAt.Add(-youtube.ReadLag)), Valid: true},
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read the latest write to playlist %s: %w", playlistID, err)
	}
	return row.Method, true, nil
}
