package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
)

func (a *app) playCommand() *cobra.Command {
	var (
		audio bool
		limit int
		extra []string
	)
	cmd := &cobra.Command{
		Use:     "play <playlist>",
		Short:   "Play a playlist through mpv",
		GroupID: groupPlaying,
		Long: "Runs in the foreground and exits when mpv does, with mpv's own exit code.\n" +
			"The videos are handed to mpv as arguments rather than as a playlist file, so a\n" +
			"playlist the server holds plays without anything being written here first.\n" +
			"\n" +
			"A video YouTube will not serve is left out. mpv would stop on it, and the\n" +
			"server already knows which ones those are.\n" +
			"\n" +
			"Playback opens mpv's IPC socket, which is what lets `ypl now` say which track\n" +
			"of a two-hour mix is on.",
		Example: "  ypl play 'sunday morning'             the whole playlist, in its order\n" +
			"  ypl play 'sunday morning' --audio     no video window\n" +
			"  ypl play 'sunday morning' --limit 3   the first three of it",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			playlist, err := client.GetPlaylist(cmd.Context(), args[0])
			if err != nil {
				return reported(err)
			}
			urls, left := playable(playlist, limit)
			if len(urls) == 0 {
				nothing(cmd, fmt.Sprintf("%s has nothing playable in it. `ypl playlists show` says what is in it.", playlist.Title))
				return exitCode(1)
			}
			if left > 0 {
				nothing(cmd, fmt.Sprintf("Leaving out %s YouTube will not serve.", count(left, "video")))
			}

			arguments := append([]string{}, extra...)
			if audio {
				arguments = append(arguments, "--no-video")
			}
			socket := mpv.SocketPath()
			if !mpv.Addressable(socket) {
				// mpv logs this and plays on regardless, which leaves `ypl now`
				// quietly reporting nothing with no way to tell why.
				nothing(cmd, fmt.Sprintf("%s is too long for a unix socket, so `ypl now` will not see this.", socket))
				socket = ""
			}
			code, err := mpv.Play(cmd.Context(), socket, arguments, urls)
			if err != nil {
				return reported(err)
			}
			// mpv's own exit code, so a player that failed is a command that
			// failed. It carries no message of its own, because mpv has already
			// written whatever it had to say to the terminal it was holding.
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&audio, "audio", "a", false, "Play the sound alone, with no video window")
	cmd.Flags().StringArrayVar(&extra, "mpv", nil, "Pass this argument straight to mpv; repeat it for more than one")
	addLimit(cmd, &limit, 0, "Play at most this many videos, from the start of the playlist")
	// No --json. This one takes the terminal and hands it to mpv, and the flag
	// would then have to decide whether it plays at all, which is a verb's job
	// and never a rendering flag's. `ypl playlists show --json` is the read.
	return cmd
}

// playable is the watch URLs of a playlist, in its order, and how many videos
// were left out because YouTube will not serve them. A limit of zero is every
// video, since a limit is a ceiling rather than a count to reach.
//
// The limit is applied before the unavailable ones are counted, so the count
// reports what was dropped from the part that would have played rather than
// from the whole playlist.
func playable(playlist api.Playlist, limit int) ([]string, int64) {
	urls := []string{}
	var left int64
	for _, item := range playlist.Items {
		if limit > 0 && len(urls) == limit {
			break
		}
		if item.Video.IsUnavailable {
			left++
			continue
		}
		urls = append(urls, watchURL(item.Video.ID))
	}
	return urls, left
}
