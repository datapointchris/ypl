package cli

import (
	"fmt"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/youtube"
)

func (a *app) transcriptCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "transcript <video> [language]",
		Short:   "Print any YouTube video's captions as one line of text",
		GroupID: groupVideos,
		Long: "The server reads the captions from YouTube, signed in as nobody, one read at a\n" +
			"time behind every other caller's, so a read can wait. The video is any public\n" +
			"one, by its link or its id. The language is a code such as en or de, and en when\n" +
			"absent. A track someone wrote wins over YouTube's automatic one.",
		Example: "  ypl transcript https://youtu.be/<id>     the English captions, as one line\n" +
			"  ypl transcript <id> de                   the German ones\n" +
			"  ypl transcript <id> --json               with the track's language and kind",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := youtube.VideoID(args[0])
			if id == "" {
				return goclikit.UsageError(fmt.Errorf("%q is not a YouTube video's link or id", args[0]))
			}
			var lang string
			if len(args) == 2 {
				lang = args[1]
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			transcript, err := client.ReadTranscript(cmd.Context(), id, lang)
			if err != nil {
				return reported(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), transcript)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), transcript.Text)
			return err
		},
	}
	addJSON(cmd, &asJSON, "the transcript")
	return cmd
}
