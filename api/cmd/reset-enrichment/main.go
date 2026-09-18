// Command reset-enrichment shows the videos enrichment has stopped reading and
// puts them back in its queue.
//
// Enrichment stops reading a video when YouTube answers that no signed-out
// session will ever read it, and when enough reads in a row stored no
// tracklist. Both verdicts come from classifying another program's message
// text, and both are open-ended: whether yt-dlp can read an age-gated video
// changes between its releases, a country block changes if the host moves, and
// a tracklist can be posted at any time. So neither is left with no way out.
//
//	go run ./cmd/reset-enrichment            # what is held back, and why
//	go run ./cmd/reset-enrichment -clear     # put all of it back in the queue
//
// It reads the same database the server does, at DATABASE_PATH or the default
// path. Clearing forgets how many reads each video has had, so a video put back
// is read again from the first retry wait.
//
// It prints the held-back videos as JSON on stdout, exits 0 and on -h, 2 on a
// usage error, and 1 otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/datapointchris/ypl/api/store"
)

// ErrUsage marks a mistake in how the command was invoked, as distinct from a
// run that failed.
var ErrUsage = errors.New("usage")

// held is one video enrichment has stopped reading.
type held struct {
	VideoID     string `json:"video_id"`
	AttemptedTs string `json:"attempted_ts"`
	Attempts    int64  `json:"attempts"`
	Reason      string `json:"reason"`
}

// report is what one invocation found and did.
type report struct {
	Held []held `json:"held"`
	// Cleared is how many videos were put back in the queue, and 0 for a run
	// that only looked.
	Cleared int64 `json:"cleared"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	switch {
	case err == nil:
	case errors.Is(err, ErrUsage):
		slog.Error("reset enrichment", "err", err)
		os.Exit(2)
	default:
		slog.Error("reset enrichment", "err", err)
		os.Exit(1)
	}
}

// run shows and clears as args describe. Flag usage and parse errors go to
// usage.
func run(ctx context.Context, args []string, stdout, usage io.Writer) error {
	flags := flag.NewFlagSet("reset-enrichment", flag.ContinueOnError)
	flags.SetOutput(usage)
	clear := flags.Bool("clear", false, "put every held-back video back in the enrichment queue")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: reset-enrichment takes no arguments, and was given %q", ErrUsage, flags.Args())
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
	return reset(ctx, st, *clear, stdout)
}

// reset prints the videos enrichment has stopped reading, and clears them when
// clear. The listing is read before the clearing, so what a clearing run prints
// is what it put back.
func reset(ctx context.Context, st *store.Store, clear bool, stdout io.Writer) error {
	rows, err := st.Queries.ListEnrichFailuresHeld(ctx)
	if err != nil {
		return fmt.Errorf("list the videos enrichment has stopped reading: %w", err)
	}
	rep := report{Held: []held{}}
	for _, row := range rows {
		rep.Held = append(rep.Held, held{
			VideoID:     row.VideoID,
			AttemptedTs: row.AttemptedTs,
			Attempts:    row.Attempts,
			Reason:      row.Reason,
		})
	}
	if clear {
		err := st.InTx(ctx, func(tx *store.Tx) error {
			if _, err := tx.ForgetReadsOfEnrichFailuresHeld(ctx); err != nil {
				return err
			}
			rep.Cleared, err = tx.ClearEnrichFailuresHeld(ctx)
			return err
		})
		if err != nil {
			return fmt.Errorf("put the held-back videos back in the queue: %w", err)
		}
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(rep)
}
