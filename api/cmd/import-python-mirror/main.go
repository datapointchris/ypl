// Command import-python-mirror copies videos and their tracklists from a Python
// ypl mirror into the API's database, for the playlists listed in a file. It
// writes through the store, so running it again overwrites the same rows, and
// it is not a migration: it carries data.
//
//	DATABASE_PATH=/data/api.db go run ./cmd/import-python-mirror -from ypl.db -playlists owned.txt
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/datapointchris/ypl/api/pyimport"
	"github.com/datapointchris/ypl/api/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("import python mirror", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("import-python-mirror", flag.ContinueOnError)
	from := flags.String("from", "", "the Python tool's mirror database, ypl.db")
	playlists := flags.String("playlists", "", "a file of playlist ids, one per line")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *playlists == "" {
		return errors.New("-from and -playlists are both required")
	}

	ids, err := readIDs(*playlists)
	if err != nil {
		return err
	}

	source, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: *from, RawQuery: "mode=ro"}).String())
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
		return nil, fmt.Errorf("%s names no playlists", path)
	}
	return ids, nil
}
