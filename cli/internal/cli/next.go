package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// watchURL is where a video is played from. It is built here rather than sent
// by the server, since it is a fact about YouTube rather than about the store.
func watchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

func (a *app) nextCommand() *cobra.Command {
	var (
		playlist string
		limit    = 1
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:     "next [flags]",
		Short:   "What to put on next",
		GroupID: groupPlaying,
		Long: "The mixes least recently listened to, never-played ones first. Videos last\n" +
			"played at the same moment come back in a new order each time, so this is a\n" +
			"draw rather than a page of a standing list.",
		Example: "  ypl next                                        one thing to put on now\n" +
			"  ypl next --playlist 'sunday morning' --limit 5  five to choose from, out of one playlist\n" +
			"  ypl next --json                                 for a status bar or a picker",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			suggestions, err := client.ListSuggestions(cmd.Context(), playlist, limit)
			if err != nil {
				return reported(err)
			}
			// Render first, decide the exit code after, in both modes. A status
			// bar is the caller that reads the empty draw off the code, and
			// --json is the rendering it parses — so returning inside the
			// branch would lose the signal for the only caller depending on it.
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), withURLs(suggestions)); err != nil {
					return err
				}
			} else {
				for _, suggestion := range suggestions {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", suggestion.Title, watchURL(suggestion.ID))
				}
			}
			if len(suggestions) == 0 {
				nothing(cmd, "Nothing to play. Check `ypl status` for what the server holds.")
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&playlist, "playlist", "p", "", "Draw from one playlist, by title or id")
	addLimit(cmd, &limit, api.MaxSuggestions, "How many to draw")
	addJSON(cmd, &asJSON, "the suggestions")
	return cmd
}

// suggestedVideo is a suggestion with the URL that plays it, which is what a
// caller of --json does with one.
type suggestedVideo struct {
	api.Suggestion
	URL string `json:"url"`
}

func withURLs(suggestions []api.Suggestion) []suggestedVideo {
	drawn := make([]suggestedVideo, len(suggestions))
	for i, suggestion := range suggestions {
		drawn[i] = suggestedVideo{Suggestion: suggestion, URL: watchURL(suggestion.ID)}
	}
	return drawn
}
