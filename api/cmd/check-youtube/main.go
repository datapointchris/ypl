// Command check-youtube reads every playlist the channel owns, and every item in
// each, through the Data API with the credentials in the environment. It prints
// what it read, the requests the read made, and each playlist it could not read
// as JSON on stdout.
//
//	YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=… YOUTUBE_REFRESH_TOKEN=… go run ./cmd/check-youtube
//
// Each request is one page of up to 50 playlists or 50 items, and costs one unit
// of the project's daily quota. An empty playlist still takes a request. It
// exits 0 when every playlist reads and on -h, 2 on a usage error, and 1
// otherwise.
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

	"github.com/datapointchris/ypl/api/youtube"
)

// ErrUsage marks a mistake in how the command was invoked, as distinct from a
// check that ran and failed.
var ErrUsage = errors.New("usage")

// ErrUnreadPlaylists is the failure of a check that read every playlist it
// could and reported the ones it could not.
var ErrUnreadPlaylists = errors.New("some playlists could not be read")

// reader is what the check reads through.
type reader interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Items(ctx context.Context, playlistID string) ([]youtube.Item, error)
	Requests() int64
}

// report is what one check read.
type report struct {
	Playlists   int      `json:"playlists"`
	Items       int      `json:"items"`
	Unavailable int      `json:"unavailable"`
	Requests    int64    `json:"requests"`
	Unread      []unread `json:"unread"`
}

// unread is a playlist whose items could not be read, and why.
type unread struct {
	Playlist string `json:"playlist"`
	Error    string `json:"error"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, ErrUsage):
		slog.Error("check youtube", "err", err)
		os.Exit(2)
	default:
		slog.Error("check youtube", "err", err)
		os.Exit(1)
	}
}

// run checks as args describe. Flag usage and parse errors go to usage.
func run(ctx context.Context, args []string, stdout, usage io.Writer) error {
	flags := flag.NewFlagSet("check-youtube", flag.ContinueOnError)
	flags.SetOutput(usage)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: check-youtube takes no arguments, and was given %q", ErrUsage, flags.Args())
	}

	creds, err := youtube.CredentialsFromEnv()
	if err != nil {
		return err
	}
	r, err := youtube.NewReader(ctx, creds)
	if err != nil {
		return err
	}
	return check(ctx, r, stdout)
}

// check reads every playlist through r and prints the report. A playlist whose
// items cannot be read is reported and the check goes on to the next. A spent
// quota, or a failure that is not about one playlist, stops it with no report.
func check(ctx context.Context, r reader, stdout io.Writer) error {
	playlists, err := r.Playlists(ctx)
	if err != nil {
		return err
	}
	rep := report{Playlists: len(playlists), Unread: []unread{}}
	for _, playlist := range playlists {
		items, err := r.Items(ctx, playlist.ID)
		if errors.Is(err, youtube.ErrInconsistentRead) || errors.Is(err, youtube.ErrUnexpectedResponse) {
			rep.Unread = append(rep.Unread, unread{Playlist: playlist.ID, Error: err.Error()})
			continue
		}
		if err != nil {
			return err
		}
		rep.Items += len(items)
		for _, item := range items {
			if item.Unavailable {
				rep.Unavailable++
			}
		}
	}
	rep.Requests = r.Requests()

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rep); err != nil {
		return err
	}
	if len(rep.Unread) > 0 {
		return fmt.Errorf("%w: %d of %d", ErrUnreadPlaylists, len(rep.Unread), rep.Playlists)
	}
	return nil
}
