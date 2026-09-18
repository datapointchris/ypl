package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

func (a *app) playCommand() *cobra.Command {
	var (
		audio bool
		limit int
		extra []string
	)
	cmd := &cobra.Command{
		Use:     "play [playlist]",
		Short:   "Play a playlist, or a draw of the mixes heard least recently",
		GroupID: groupPlaying,
		Long: "Runs in the foreground and exits when mpv does. Tab completes the playlist.\n" +
			"\n" +
			"Named, a playlist plays in its own order. With no playlist, it plays a draw of\n" +
			"up to 100 mixes, made the way `ypl next` makes one: never-played mixes first,\n" +
			"in a new order each time, then the ones heard least recently.\n" +
			"\n" +
			"A video YouTube will not serve is left out. mpv would stop on it, and the\n" +
			"server already knows which ones those are.\n" +
			"\n" +
			"Playback opens mpv's IPC socket, which is what lets `ypl now` say which track\n" +
			"of a two-hour mix is on. It is also how a listen is recorded: once a mix has\n" +
			"played for 20 minutes, or half its length when that is shorter, the server is\n" +
			"told, and `ypl next` stops offering it first. A seek forward is not listening,\n" +
			"so it does not count toward that.",
		Example: "  ypl play sunday-morning             the whole playlist, in its order\n" +
			"  ypl play                            a draw, least recently heard first\n" +
			"  ypl play sunday-morning --audio     no video window\n" +
			"  ypl play sunday-morning --limit 3   the first three of it",
		Args:              usageArgs(cobra.MaximumNArgs(1)),
		ValidArgsFunction: onlyFirst(a.completePlaylists),
		RunE: func(cmd *cobra.Command, args []string) error {
			// An unset --limit is no ceiling. An explicit --limit 0 is a
			// request for nothing, which is what it means on every other verb
			// of this binary.
			var ceiling *int
			if cmd.Flags().Changed("limit") {
				ceiling = &limit
			}
			// The cheapest and most certain refusals run first: what was typed,
			// then whether mpv is here at all. Reaching the second inside
			// mpv.Play would put it after the config load, the keychain read
			// and a network round trip, so a machine with neither mpv nor a
			// reachable server would report the wrong one of the two.
			switch {
			case len(args) == 0 && ceiling != nil && *ceiling > api.MaxSuggestions:
				return goclikit.UsageError(fmt.Errorf("at most %d can be drawn at once, and this asks for %d", api.MaxSuggestions, *ceiling))
			case ceiling != nil && *ceiling == 0:
				// Asking for no videos is answered by playing none. It is what
				// the caller asked for, so it is not a failure.
				nothing(cmd, "A limit of 0 asks for no videos, so nothing was played.")
				return nil
			}
			if err := mpv.Available(); err != nil {
				return reported(err)
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			var urls mpv.WatchURLs
			if len(args) == 0 {
				urls, err = drawn(cmd, client, ceiling)
			} else {
				urls, err = listed(cmd, client, args[0], ceiling)
			}
			if err != nil {
				return err
			}

			arguments := append(mpv.Arguments{}, extra...)
			if audio {
				arguments = append(arguments, "--no-video")
			}
			socket := mpv.SocketPath()
			if !mpv.Addressable(socket) {
				// mpv logs this and plays on regardless, which leaves `ypl now`
				// quietly reporting nothing with no way to tell why.
				nothing(cmd, fmt.Sprintf("%s is too long for a unix socket, so `ypl now` will not see this and no listen is recorded.", socket))
				socket = ""
			}

			// The recorder reads the socket mpv opens and stops when mpv
			// exits. What it has to say waits for the terminal to come back,
			// because mpv is drawing on it until then.
			listening, stop := context.WithCancel(cmd.Context())
			heard := make(chan listened, 1)
			if socket != "" {
				ticks := time.NewTicker(listenEvery)
				defer ticks.Stop()
				go func() {
					heard <- recordListens(listening, client, func() (mpv.State, error) { return mpv.Read(socket) }, ticks.C)
				}()
			} else {
				heard <- listened{}
			}
			outcome, err := mpv.Play(cmd.Context(), socket, arguments, urls)
			stop()
			reportListens(cmd, <-heard)
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
	addLimit(cmd, &limit, 0, "Play at most this many videos, from the start of the playlist or the draw")
	// No --json. This one takes the terminal and hands it to mpv, and the flag
	// would then have to decide whether it plays at all, which is a verb's job
	// and never a rendering flag's. `ypl playlists show --json` and
	// `ypl next --json` are the reads.
	return cmd
}

// reportListens says what a playback recorded, and names each play the server
// was not told about, since `ypl next` will offer that mix again as if unheard.
func reportListens(cmd *cobra.Command, heard listened) {
	if len(heard.recorded) > 0 {
		titles := make([]string, len(heard.recorded))
		for i, play := range heard.recorded {
			titles[i] = play.Video.Title
		}
		nothing(cmd, fmt.Sprintf("Recorded %s: %s.", count(int64(len(titles)), "listen"), strings.Join(titles, ", ")))
	}
	for _, err := range heard.unsent {
		nothing(cmd, fmt.Sprintf("Could not record a listen, so `ypl next` may offer it again. %v", err))
	}
}

// drawn is a draw of the library made the way `ypl next` makes one, up to
// ceiling or as much as one draw holds. The server leaves out what YouTube will
// not serve.
func drawn(cmd *cobra.Command, client *api.Client, ceiling *int) (mpv.WatchURLs, error) {
	most := api.MaxSuggestions
	if ceiling != nil {
		most = *ceiling
	}
	suggestions, err := client.ListSuggestions(cmd.Context(), "", most)
	if err != nil {
		return nil, reported(err)
	}
	if len(suggestions) == 0 {
		nothing(cmd, "The library has nothing playable in it. `ypl status` says what the server holds.")
		return nil, exitCode(1)
	}
	urls := make(mpv.WatchURLs, len(suggestions))
	for i, suggestion := range suggestions {
		urls[i] = youtube.WatchURL(suggestion.ID)
	}
	nothing(cmd, fmt.Sprintf("Playing %s drawn from the library, least recently heard first.", count(int64(len(urls)), "video")))
	// A draw that came back as large as a draw can be may have left mixes out,
	// and nothing else on the screen would say so.
	if len(urls) == api.MaxSuggestions {
		nothing(cmd, fmt.Sprintf("A draw holds at most %d, so the library may hold more than this.", api.MaxSuggestions))
	}
	return urls, nil
}

// listed is the playlist reference names, in its own order.
func listed(cmd *cobra.Command, client *api.Client, reference string, ceiling *int) (mpv.WatchURLs, error) {
	playlist, err := client.GetPlaylist(cmd.Context(), api.Reference(reference))
	if err != nil {
		return nil, reported(namingPlaylists(cmd.Context(), client, err))
	}
	urls, left := playable(playlist, ceiling)
	if len(urls) == 0 {
		nothing(cmd, fmt.Sprintf("%s has nothing playable in it. `ypl playlists show` says what is in it.", playlist.Title))
		return nil, exitCode(1)
	}
	if left > 0 {
		nothing(cmd, fmt.Sprintf("Leaving out %s YouTube will not serve.", count(left, "video")))
	}
	// Named, because a slug or part of a title is what was typed, and this is
	// the one place that says which playlist it reached.
	nothing(cmd, fmt.Sprintf("Playing %s, %s.", playlist.Title, count(int64(len(urls)), "video")))
	return urls, nil
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
