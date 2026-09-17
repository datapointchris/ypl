// Command check-youtube-writes makes each playlist write the server makes,
// through the Data API with the credentials in the environment, on a private
// playlist it creates for the check. It reads the playlist back after the
// writes, deletes it, and prints the playlist, whether it was deleted, any
// playlist an earlier check left behind that it deleted, the requests the check
// made, the quota units they cost and any error as JSON on stdout.
//
//	YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=… YOUTUBE_REFRESH_TOKEN=… go run ./cmd/check-youtube-writes
//
// It adds the first two available videos in the channel's own playlists. It
// creates the playlist, inserts the first video at position 0 and then the
// second at 0, and renames the playlist; moves the first video's item to 0;
// deletes the second video's item; and deletes the playlist. That is seven
// writes at 50 units each. It reads the playlist back after the rename, the
// move and the delete, each time comparing its title, description, privacy and
// items in order. A read made within seconds of a write can miss it, so each
// read-back waits 10 seconds first. Each page it reads costs 1 unit.
//
// Before it creates its playlist, it deletes any private playlist carrying the
// check's description and either of its titles: what an earlier check left when
// its playlist was never deleted. The create is sent on a context an interrupt
// does not cancel, so its id comes back. A create that fails is followed by the
// same sweep, since YouTube may have applied it.
//
// It exits 0 when every read-back matched and the playlist was deleted, and on
// -h, 2 on a usage error, and 1 otherwise.
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
	// detachedTime bounds the create and the deletions, which run on a context
	// the check's own ending does not cancel.
	detachedTime = 30 * time.Second
)

// channel is what the check writes and reads through.
type channel interface {
	Playlists(ctx context.Context) ([]youtube.Playlist, error)
	Items(ctx context.Context, playlistID youtube.PlaylistID) ([]youtube.Item, error)
	CreatePlaylist(ctx context.Context, details youtube.PlaylistDetails) (youtube.Playlist, error)
	UpdatePlaylist(ctx context.Context, id youtube.PlaylistID, details youtube.PlaylistDetails) (youtube.PlaylistDetails, error)
	DeletePlaylist(ctx context.Context, id youtube.PlaylistID) error
	InsertItem(ctx context.Context, playlist youtube.PlaylistID, video youtube.VideoID, position int64) (youtube.ItemID, error)
	MoveItem(ctx context.Context, item youtube.Item, position int64) error
	DeleteItem(ctx context.Context, id youtube.ItemID) error
	Requests() int64
	Units() int64
}

// report is what one check did.
type report struct {
	// Playlist is the id of the playlist the check created, and null when no
	// create returned one.
	Playlist *youtube.PlaylistID `json:"playlist"`
	Deleted  bool                `json:"deleted"`
	// Swept holds each playlist an earlier check, or this check's failed create,
	// left behind that this check deleted.
	Swept    []youtube.PlaylistID `json:"swept"`
	Requests int64                `json:"requests"`
	Units    int64                `json:"units"`
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

// check writes through ch, waiting in settle before each read-back, and prints
// the report. Once it has found its videos it prints the report on every path.
func check(ctx context.Context, ch channel, settle func(context.Context) error, stdout io.Writer) error {
	videos, err := availableVideos(ctx, ch, 2)
	if err != nil {
		return err
	}

	rep := report{Swept: []youtube.PlaylistID{}}
	failure := sweep(ctx, ch, &rep)
	if failure == nil {
		failure = createAndCheck(ctx, ch, settle, videos, &rep)
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

// createAndCheck creates the check's playlist, writes and reads it back, and
// deletes it.
func createAndCheck(ctx context.Context, ch channel, settle func(context.Context) error, videos []youtube.VideoID, rep *report) error {
	created, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedTime)
	playlist, err := ch.CreatePlaylist(created, youtube.PlaylistDetails{Title: title, Description: description})
	cancel()
	if err != nil {
		return errors.Join(err, sweep(ctx, ch, rep))
	}
	id := playlist.ID
	rep.Playlist = &id

	failure := writeAndReadBack(ctx, ch, settle, id, videos)
	deletion, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedTime)
	defer cancel()
	if err := ch.DeletePlaylist(deletion, id); err != nil {
		return errors.Join(failure, fmt.Errorf("%w: %w", ErrNotDeleted, err))
	}
	rep.Deleted = true
	return failure
}

// writeAndReadBack makes the check's writes on the playlist and reads it back
// after the rename, the move and the delete.
func writeAndReadBack(ctx context.Context, ch channel, settle func(context.Context) error, playlist youtube.PlaylistID, videos []youtube.VideoID) error {
	first, err := ch.InsertItem(ctx, playlist, videos[0], 0)
	if err != nil {
		return err
	}
	second, err := ch.InsertItem(ctx, playlist, videos[1], 0)
	if err != nil {
		return err
	}
	if _, err := ch.UpdatePlaylist(ctx, playlist, youtube.PlaylistDetails{Title: renamed, Description: description}); err != nil {
		return err
	}
	// An insert that ignored its position would leave the first video first.
	if err := readBack(ctx, ch, settle, playlist, second, first); err != nil {
		return err
	}
	if err := ch.MoveItem(ctx, youtube.Item{ID: first, PlaylistID: playlist, VideoID: videos[0]}, 0); err != nil {
		return err
	}
	if err := readBack(ctx, ch, settle, playlist, first, second); err != nil {
		return err
	}
	if err := ch.DeleteItem(ctx, second); err != nil {
		return err
	}
	return readBack(ctx, ch, settle, playlist, first)
}

// readBack waits in settle, then fails with ErrReadBack unless the playlist is
// listed renamed, described and private, and holds exactly items, in order.
func readBack(ctx context.Context, ch channel, settle func(context.Context) error, playlist youtube.PlaylistID, items ...youtube.ItemID) error {
	if err := settle(ctx); err != nil {
		return err
	}
	playlists, err := ch.Playlists(ctx)
	if err != nil {
		return err
	}
	want := youtube.Playlist{ID: playlist, Title: renamed, Description: description, Privacy: "private"}
	if !slices.Contains(playlists, want) {
		return fmt.Errorf("%w: no listed playlist is %+v", ErrReadBack, want)
	}
	listed, err := ch.Items(ctx, playlist)
	if err != nil {
		return err
	}
	var held []youtube.ItemID
	for _, item := range listed {
		held = append(held, item.ID)
	}
	if !slices.Equal(held, items) {
		return fmt.Errorf("%w: playlist %s holds items %q, want %q", ErrReadBack, playlist, held, items)
	}
	return nil
}

// sweep deletes every private playlist carrying the check's description and
// either of its titles, and records each it deleted in rep. One YouTube reports
// already gone is passed over.
func sweep(ctx context.Context, ch channel, rep *report) error {
	playlists, err := ch.Playlists(ctx)
	if err != nil {
		return err
	}
	for _, p := range playlists {
		if p.Privacy != "private" || p.Description != description || (p.Title != title && p.Title != renamed) {
			continue
		}
		deletion, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedTime)
		err := ch.DeletePlaylist(deletion, p.ID)
		cancel()
		switch {
		case errors.Is(err, youtube.ErrPlaylistNotFound):
			// A list made seconds after a delete can still show the playlist.
		case err != nil:
			return fmt.Errorf("%w: %s left by an earlier check: %w", ErrNotDeleted, p.ID, err)
		default:
			rep.Swept = append(rep.Swept, p.ID)
		}
	}
	return nil
}

// availableVideos is the first n distinct available videos in the channel's
// playlists, in the order the playlists and their items are listed.
func availableVideos(ctx context.Context, ch channel, n int) ([]youtube.VideoID, error) {
	playlists, err := ch.Playlists(ctx)
	if err != nil {
		return nil, err
	}
	var videos []youtube.VideoID
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
