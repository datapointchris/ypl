package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
			"is printed here, and the files it writes land where -o names them under this\n" +
			"directory. The server adds its own sleeps between requests, and keeps a longer\n" +
			"one given here.\n\n" +
			"This exits 0 where yt-dlp ended well or stopped at a --max-downloads or\n" +
			"--break-on limit, and 1 naming yt-dlp's exit code where it failed. It exits 4\n" +
			"where the server's door stayed busy through every attempt.\n\n" +
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
			run, err := atTheDoor(cmd.Context(), func(ctx context.Context) (*api.Run, error) { return client.RunYtdlp(ctx, args) },
				false, func(waiting string) { nothing(cmd, waiting) })
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
			// yt-dlp's code is said rather than returned, since it spends 2
			// on what yt-dlp itself refused and -1 on a signal.
			switch {
			case code == 0, code == ytdlpStoppedAtLimit:
				return nil
			case code < 0:
				nothing(cmd, "yt-dlp was stopped by a signal on the server.")
			default:
				nothing(cmd, fmt.Sprintf("yt-dlp exited %d.", code))
			}
			return exitCode(1)
		},
	}
}

// ytdlpStoppedAtLimit is yt-dlp's exit code for a run that stopped at
// --max-downloads or a --break-on option, as it was asked to.
const ytdlpStoppedAtLimit = 101

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
