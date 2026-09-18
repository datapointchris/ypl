package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

func (a *app) playlistsPlayCommand() *cobra.Command {
	var (
		audio bool
		limit int
		extra []string
	)
	cmd := &cobra.Command{
		Use:     "play <playlist>",
		Short:   "Play a playlist through mpv",
		GroupID: groupPlaylistReading,
		Long: "Runs in the foreground and exits when mpv does.\n" +
			"\n" +
			"A video YouTube will not serve is left out. mpv would stop on it, and the\n" +
			"server already knows which ones those are.\n" +
			"\n" +
			"Playback opens mpv's IPC socket, which is what lets `ypl now` say which track\n" +
			"of a two-hour mix is on.",
		Example: "  ypl playlists play 'sunday morning'             the whole playlist, in its order\n" +
			"  ypl playlists play 'sunday morning' --audio     no video window\n" +
			"  ypl playlists play 'sunday morning' --limit 3   the first three of it",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The cheapest and most certain refusal runs first. Reaching it
			// inside mpv.Play would put it after the config load, the keychain
			// read and a network round trip, so a machine with neither mpv nor
			// a reachable server reports the wrong one of the two.
			if err := mpv.Available(); err != nil {
				return reported(err)
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			playlist, err := client.GetPlaylist(cmd.Context(), args[0])
			if err != nil {
				return reported(err)
			}
			// An unset --limit is no ceiling. An explicit --limit 0 is a
			// request for nothing, which is what it means on every other verb
			// of this binary.
			var ceiling *int
			if cmd.Flags().Changed("limit") {
				ceiling = &limit
			}
			urls, left := playable(playlist, ceiling)
			switch {
			case ceiling != nil && *ceiling == 0:
				// Asking for no videos is answered by playing none. It is what
				// the caller asked for, so it is not a failure.
				nothing(cmd, "A limit of 0 asks for no videos, so nothing was played.")
				return nil
			case len(urls) == 0:
				nothing(cmd, fmt.Sprintf("%s has nothing playable in it. `ypl playlists show` says what is in it.", playlist.Title))
				return exitCode(1)
			}
			if left > 0 {
				nothing(cmd, fmt.Sprintf("Leaving out %s YouTube will not serve.", count(left, "video")))
			}

			arguments := append(mpv.Arguments{}, extra...)
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
			outcome, err := mpv.Play(cmd.Context(), socket, arguments, urls)
			if err != nil {
				return reported(err)
			}
			// A player that failed is a command that failed, which is exit 1.
			// mpv's own status is said rather than returned: it spends 2 on a
			// file it cannot open and this binary spends 2 on an invocation it
			// would not accept.
			if outcome.Failed {
				nothing(cmd, outcome.Says)
				return exitCode(1)
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
// were left out because YouTube will not serve them. A nil limit is no ceiling;
// a limit of zero is a request for no videos.
//
// The limit is applied before the unavailable ones are counted, so the count
// reports what was dropped from the part that would have played rather than
// from the whole playlist.
func playable(playlist api.Playlist, limit *int) (mpv.WatchURLs, int64) {
	urls := mpv.WatchURLs{}
	var left int64
	for _, item := range playlist.Items {
		if limit != nil && len(urls) == *limit {
			break
		}
		if item.Video.IsUnavailable {
			left++
			continue
		}
		urls = append(urls, youtube.WatchURL(item.Video.ID))
	}
	return urls, left
}
