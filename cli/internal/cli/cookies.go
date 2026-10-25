package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"
)

func (a *app) serverCookiesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cookies",
		Short: "The YouTube session the server reads the watch history with",
	}
	goclikit.AsNamespace(cmd)
	cmd.AddCommand(a.serverCookiesSetCommand())
	return cmd
}

func (a *app) serverCookiesSetCommand() *cobra.Command {
	var keep bool
	cmd := &cobra.Command{
		Use:   "set <file>",
		Short: "Send a cookie jar for the server to read the watch history with",
		Long: "The file is a Netscape cookies.txt holding youtube.com's cookies, signed in as\n" +
			"the account the players use. YouTube rotates the cookies of a session a browser\n" +
			"has open, which breaks an exported copy, so export them from a private window:\n\n" +
			"  1. Open a private window and sign in to YouTube.\n" +
			"  2. In that tab open https://www.youtube.com/robots.txt, the only private tab.\n" +
			"  3. Export the youtube.com cookies with a cookies.txt extension.\n" +
			"  4. Close the private window.\n\n" +
			"The server keeps the jar only once a read of the history with it answers\n" +
			"signed in. The file is then deleted, since it is a live login; --keep keeps it.",
		Example: "  ypl server cookies set ~/Downloads/cookies.txt  send it, then delete it",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jar, err := os.ReadFile(args[0])
			if err != nil {
				return goclikit.UsageError(fmt.Errorf("read the cookie jar: %w", err))
			}
			if len(jar) == 0 {
				return goclikit.UsageError(errors.New("the cookie jar is empty"))
			}
			client, err := a.client(cmd.Context())
			if err != nil {
				return reported(err)
			}
			session, err := client.SetSession(cmd.Context(), string(jar))
			if err != nil {
				return reported(err)
			}
			said := "The server reads the watch history signed in, as of " + session.StoredTs + "."
			if !keep {
				if err := os.Remove(args[0]); err != nil {
					return fmt.Errorf("%s It could not delete %s: %w", said, args[0], err)
				}
				said += " Deleted " + args[0] + "."
			}
			nothing(cmd, said)
			return nil
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "Keep the file after sending it")
	return cmd
}
