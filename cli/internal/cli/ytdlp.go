package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

func (a *app) ytdlpCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ytdlp -- <yt-dlp arguments>",
		Short:   "Run yt-dlp on the server, at the pace of its other YouTube reads",
		GroupID: groupVideos,
		Long: "Runs yt-dlp on the server, through the one door every read of YouTube takes, so\n" +
			"reads from here and the server's own never come close together. What it prints\n" +
			"is printed here, the files it writes land where -o names them under this\n" +
			"directory, and this exits with its exit code. The server adds its own sleeps\n" +
			"between requests, and keeps a longer one given here.\n\n" +
			"Only YouTube links, video ids and ytsearch: are read. Cookies, logins, and any\n" +
			"option that runs or reads something on the server are refused, naming why.\n\n" +
			"A URL the run prints, such as a stream's from -g, is bound to the server's\n" +
			"address, and plays nowhere else.",
		Example: "  ypl ytdlp -- --skip-download --write-auto-subs -o 'x.%(ext)s' <link>\n" +
			"  ypl ytdlp -- -J <link>             the video's details, as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 {
				return goclikit.UsageError(errors.New("yt-dlp's arguments go after --, as in `ypl ytdlp -- --skip-download <link>`"))
			}
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("find this directory: %w", err)
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			run, err := runWhenTheDoorOpens(cmd.Context(), client, args, func(waiting string) { nothing(cmd, waiting) })
			var refusal *api.Refusal
			if errors.As(err, &refusal) && refusal.Code == api.CodeArgumentRefused {
				return goclikit.UsageError(err)
			}
			if err != nil {
				return reported(err)
			}
			defer func() { _ = run.Close() }()
			code, err := run.Streams(cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			for {
				file, err := run.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				if err := writeRunFile(dir, file); err != nil {
					return err
				}
			}
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}

// runWhenTheDoorOpens runs args on the server, sending them again while the
// door is busy, which leaves the run unstarted. It says once why it waits. A
// paused door is answered as a failure naming when it opens, since a pause can
// last a day.
func runWhenTheDoorOpens(ctx context.Context, client *api.Client, args []string, waiting func(string)) (*api.Run, error) {
	said := false
	for {
		run, err := client.RunYtdlp(ctx, args)
		var refusal *api.Refusal
		if !errors.As(err, &refusal) || refusal.Code != api.CodeReadsBusy {
			return run, err
		}
		if !said {
			waiting(fmt.Sprintf("Waiting for a turn at the server's door: %s.", refusal.Message))
			said = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(max(refusal.RetryAfter, time.Second)):
		}
	}
}

// writeRunFile writes file under dir at the path yt-dlp gave it, with its
// time, replacing a file already there. A path that is absolute or climbs out
// of dir is refused, so no answer writes outside dir.
func writeRunFile(dir string, file api.RunFile) error {
	relative := filepath.FromSlash(file.Path)
	if !filepath.IsLocal(relative) {
		return fmt.Errorf("the run wrote %q, which is outside this directory, so it was not written", file.Path)
	}
	path := filepath.Join(dir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make the directory for %s: %w", file.Path, err)
	}
	// Written beside the path and renamed over it, so an answer cut short
	// leaves no half a file under the name.
	written, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", file.Path, err)
	}
	defer func() { _ = os.Remove(written.Name()) }()
	_, err = io.Copy(written, file)
	err = errors.Join(err, written.Chmod(0o644), written.Close())
	if err == nil && !file.Modified.IsZero() {
		err = os.Chtimes(written.Name(), file.Modified, file.Modified)
	}
	if err == nil {
		err = os.Rename(written.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", file.Path, err)
	}
	return nil
}
