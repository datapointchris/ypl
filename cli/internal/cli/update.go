package cli

import (
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/goselfupdate"
	"github.com/spf13/cobra"
)

func newUpdateCommand() *cobra.Command {
	cmd := goclikit.UpdateCommand(updateConfig())
	cmd.GroupID = groupSetup
	return cmd
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
