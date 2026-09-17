// Package cmd holds the ypl command tree.
package cmd

import (
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/goselfupdate"
	"github.com/datapointchris/goselfupdate/autoupdate"
	"github.com/spf13/cobra"
)

// version is set at build time with -ldflags.
var version = "dev"

// NewRootCommand returns the ypl command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "ypl",
		Short: "Organize YouTube playlists",
		Long: "ypl organizes the playlists on a YouTube channel through a ypl server,\n" +
			"which keeps them in sync with YouTube and reads each mix's tracklist.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(goclikit.UpdateCommand(updateConfig()))
	return root
}

// AutoUpdateConfig is the daily update check that runs beside a command.
func AutoUpdateConfig() autoupdate.Config {
	return autoupdate.Config{Update: updateConfig()}
}

// updateConfig resolves releases under the cli/ tag prefix. The CLI is a nested
// module tagged cli/v1.2.3, and GitHub's latest release is repository-wide, so
// without the prefix it would name a release of something else in this repo.
//
// A locally built binary refuses to update: git describe returns the prefixed
// tag, which is not a semantic version, so it reports as a development build.
func updateConfig() goselfupdate.Config {
	return goselfupdate.Config{
		Owner:     "datapointchris",
		Repo:      "ypl",
		Binary:    "ypl",
		Version:   version,
		TagPrefix: "cli/",
	}
}
