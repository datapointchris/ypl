// Command import-python-mirror copies videos and their tracklists from a Python
// ypl mirror into the API's database, for the playlists listed in a file. It
// writes through the store, so running it again overwrites the same rows.
//
//	DATABASE_PATH=/data/api.db go run ./cmd/import-python-mirror -from ypl.db -playlists owned.txt
//
// It exits 0 on success and on -h, 2 on a usage error, and 1 when the import
// fails.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/datapointchris/ypl/api/pyimport"
	"github.com/datapointchris/ypl/api/store"
)

// ErrUsage marks a mistake in how the command was invoked, as distinct from an
// import that ran and failed.
var ErrUsage = errors.New("usage")

// ErrNoPlaylists is the refusal for a playlist file that names no playlist.
var ErrNoPlaylists = errors.New("the playlist file names no playlists")

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := run(context.Background(), os.Args[1:], os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, ErrUsage):
		slog.Error("import python mirror", "err", err)
		os.Exit(2)
	default:
		slog.Error("import python mirror", "err", err)
		os.Exit(1)
	}
}

// run imports as args describe. Flag usage and parse errors go to usage.
func run(ctx context.Context, args []string, usage io.Writer) error {
	flags := flag.NewFlagSet("import-python-mirror", flag.ContinueOnError)
	flags.SetOutput(usage)
	from := flags.String("from", "", "the Python tool's mirror database, ypl.db")
	playlists := flags.String("playlists", "", "a file of playlist ids, one per line")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if *from == "" || *playlists == "" {
		return fmt.Errorf("%w: -from and -playlists are both required", ErrUsage)
	}

	ids, err := readIDs(*playlists)
	if err != nil {
		return err
	}

	source, err := sql.Open("sqlite", store.URI(*from, "mode=ro"))
	if err != nil {
		return fmt.Errorf("open %s: %w", *from, err)
	}
	defer func() { _ = source.Close() }()
	if err := source.PingContext(ctx); err != nil {
		return fmt.Errorf("open %s: %w", *from, err)
	}

	path, err := store.Path()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	counts, err := pyimport.Import(ctx, source, st, ids)
	if err != nil {
		return err
	}
	slog.Info("imported",
		"playlists", len(ids),
		"videos", counts.Videos,
		"tracks", counts.Tracks,
		"enrich_failures", counts.EnrichFailures,
		"database", path,
	)
	return nil
}

// readIDs reads one playlist id per line, skipping blank lines.
func readIDs(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the playlist list: %w", err)
	}
	defer func() { _ = file.Close() }()

	var ids []string
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		if id := strings.TrimSpace(lines.Text()); id != "" {
			ids = append(ids, id)
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read the playlist list: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoPlaylists, path)
	}
	return ids, nil
}
