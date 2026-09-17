// Command check-youtube-writes makes each playlist write the server makes,
// through the Data API with the credentials in the environment, on a private
// playlist it creates for the check. It reads the playlist back, deletes it,
// and prints the playlist, whether it was deleted, the requests the check made,
// the quota units they cost and any error as JSON on stdout.
//
//	YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=… YOUTUBE_REFRESH_TOKEN=… go run ./cmd/check-youtube-writes
//
// It adds the first two available videos in the channel's own playlists. The
// check makes seven writes at 50 units each: it creates the playlist, inserts
// both videos at position 0, moves the first back to 0 and renames the
// playlist, then deletes the second video's item, and finally deletes the
// playlist. It reads the playlist back after the rename and after the item's
// deletion, and each page it reads costs 1 unit more. A read made within
// seconds of a write can miss it, so each read-back waits 10 seconds first.
//
// The playlist is deleted even when a write fails or the check is interrupted.
// It exits 0 when the playlist read back as written and was deleted, and on -h,
// 2 on a usage error, and 1 otherwise.
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
	"slices"
	"syscall"
	"time"

	"github.com/datapointchris/ypl/api/youtube"
)

// ErrUsage marks a mistake in how the command was invoked, as distinct from a
// check that ran and failed.
var ErrUsage = errors.New("usage")

// ErrNoVideos is the refusal for a channel whose playlists hold fewer than two
// available videos, which the check needs before it writes anything.
var ErrNoVideos = errors.New("the channel's playlists hold fewer than two available videos")

// ErrReadBack is the failure of a check whose playlist, read back, is not what
// its writes should have left.
var ErrReadBack = errors.New("the playlist did not read back as written")

// ErrNotDeleted is the failure of a check that could not delete its playlist,
// which is left on the channel.
var ErrNotDeleted = errors.New("the check's playlist was not deleted")

const (
	title       = "ypl write check"
	renamed     = "ypl write check (renamed)"
	description = "Created by check-youtube-writes, which deletes it when the check ends."
	// settleTime is how long the check waits after its writes before reading
	// them back. A rename read 10 seconds after it was made came back renamed.
	settleTime = 10 * time.Second
	// cleanupTime bounds the playlist's deletion after the check's context ends.
	cleanupTime = 30 * time.Second
)

// channel is what the check writes and reads through.
type channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Items(ctx context.Context, playlistID string) ([]youtube.Item, error)
	CreatePlaylist(ctx context.Context, title, description string) (youtube.Playlist, error)
	UpdatePlaylist(ctx context.Context, playlist youtube.Playlist) error
	DeletePlaylist(ctx context.Context, playlistID string) error
	InsertItem(ctx context.Context, playlistID, videoID string, position int64) (youtube.Item, error)
	MoveItem(ctx context.Context, item youtube.Item, position int64) error
	DeleteItem(ctx context.Context, itemID string) error
	Requests() int64
	Units() int64
}

// report is what one check did.
type report struct {
	// Playlist is the id of the playlist the check created, and null when it
	// created none.
	Playlist *string `json:"playlist"`
	Deleted  bool    `json:"deleted"`
	Requests int64   `json:"requests"`
	Units    int64   `json:"units"`
	// Error is why the check failed, and null when it passed.
	Error *string `json:"error"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	switch {
	case err == nil:
	case errors.Is(err, ErrUsage):
		slog.Error("check youtube writes", "err", err)
		os.Exit(2)
	default:
		slog.Error("check youtube writes", "err", err)
		os.Exit(1)
	}
}

// run checks as args describe. Flag usage and parse errors go to usage.
func run(ctx context.Context, args []string, stdout, usage io.Writer) error {
	flags := flag.NewFlagSet("check-youtube-writes", flag.ContinueOnError)
	flags.SetOutput(usage)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: check-youtube-writes takes no arguments, and was given %q", ErrUsage, flags.Args())
	}

	creds, err := youtube.CredentialsFromEnv()
	if err != nil {
		return err
	}
	ch, err := youtube.NewChannel(ctx, creds)
	if err != nil {
		return err
	}
	return check(ctx, ch, settle, stdout)
}

// check writes through ch, waits in settle, reads the playlist back, deletes it
// and prints the report. The report is printed whenever a playlist was created.
func check(ctx context.Context, ch channel, settle func(context.Context) error, stdout io.Writer) error {
	videos, err := availableVideos(ctx, ch, 2)
	if err != nil {
		return err
	}
	created, err := ch.CreatePlaylist(ctx, title, description)
	if err != nil {
		return err
	}

	rep := report{Playlist: &created.ID}
	failure := writeAndReadBack(ctx, ch, settle, created, videos)
	// The deletion outlives ctx, so an interrupted check still removes its
	// playlist.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
	defer cancel()
	if err := ch.DeletePlaylist(cleanup, created.ID); err != nil {
		failure = errors.Join(failure, fmt.Errorf("%w: %w", ErrNotDeleted, err))
	} else {
		rep.Deleted = true
	}
	rep.Requests, rep.Units = ch.Requests(), ch.Units()
	if failure != nil {
		message := failure.Error()
		rep.Error = &message
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rep); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

// writeAndReadBack makes the check's writes on playlist, adding the two videos,
// and reads the playlist back after the move and rename and again after the
// delete.
func writeAndReadBack(ctx context.Context, ch channel, settle func(context.Context) error, playlist youtube.Playlist, videos []string) error {
	first, err := ch.InsertItem(ctx, playlist.ID, videos[0], 0)
	if err != nil {
		return err
	}
	second, err := ch.InsertItem(ctx, playlist.ID, videos[1], 0)
	if err != nil {
		return err
	}
	if err := ch.MoveItem(ctx, first, 0); err != nil {
		return err
	}
	if err := ch.UpdatePlaylist(ctx, youtube.Playlist{ID: playlist.ID, Title: renamed, Description: description}); err != nil {
		return err
	}
	if err := readBack(ctx, ch, settle, playlist.ID, first.ID, second.ID); err != nil {
		return err
	}
	if err := ch.DeleteItem(ctx, second.ID); err != nil {
		return err
	}
	return readBack(ctx, ch, settle, playlist.ID, first.ID)
}

// readBack waits in settle, then fails with ErrReadBack unless the playlist
// playlistID is listed as renamed and holds exactly itemIDs, in order.
func readBack(ctx context.Context, ch channel, settle func(context.Context) error, playlistID string, itemIDs ...string) error {
	if err := settle(ctx); err != nil {
		return err
	}
	playlists, err := ch.Playlists(ctx)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(playlists, func(p youtube.Playlist) bool { return p.ID == playlistID })
	if index < 0 || playlists[index].Title != renamed {
		return fmt.Errorf("%w: playlist %s is not listed as %q", ErrReadBack, playlistID, renamed)
	}
	items, err := ch.Items(ctx, playlistID)
	if err != nil {
		return err
	}
	var held []string
	for _, item := range items {
		held = append(held, item.ID)
	}
	if !slices.Equal(held, itemIDs) {
		return fmt.Errorf("%w: playlist %s holds items %q, want %q", ErrReadBack, playlistID, held, itemIDs)
	}
	return nil
}

// availableVideos is the first n distinct available videos in the channel's
// playlists, in the order the playlists and their items are listed.
func availableVideos(ctx context.Context, ch channel, n int) ([]string, error) {
	playlists, err := ch.Playlists(ctx)
	if err != nil {
		return nil, err
	}
	var videos []string
	for _, playlist := range playlists {
		items, err := ch.Items(ctx, playlist.ID)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !item.Unavailable && !slices.Contains(videos, item.VideoID) {
				videos = append(videos, item.VideoID)
			}
			if len(videos) == n {
				return videos, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: found %d", ErrNoVideos, len(videos))
}

func settle(ctx context.Context) error {
	timer := time.NewTimer(settleTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
