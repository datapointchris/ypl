package cli

import (
	"errors"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"
)

// exitCode is an answer that is not a success and not a failure: the command
// ran, said what it found, and the finding is worth an exit code of its own.
// `ypl auth status` on a machine that is not logged in is the case. It carries
// no message, so nothing prints an "error:" line over what the command already
// said.
type exitCode int

func (e exitCode) Error() string { return "" }

// requireSubcommand is what a namespace runs. A namespace expects another word
// after it, so a bare invocation is ambiguous and shows help; a word that names
// no subcommand is a mistake, answered with the subcommands near it, and cobra
// would otherwise show help for that too.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	return goclikit.UnknownCommand(cmd, args[0])
}

// usageArgs is validate, with what it refuses marked as a usage mistake.
// Cobra's own validators return a plain error, which would exit 1 and tell a
// caller the command failed rather than that it was typed wrong.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return goclikit.UsageError(err)
		}
		return nil
	}
}

// exitCodeFor is the process exit code for err: 0 for success, 2 for a usage
// mistake, an exitCode's own value, and 1 for anything else.
//
// One marker covers both what this tree refuses and what cobra refuses before
// any RunE runs, because goclikit.UsageError wraps to the same sentinel cobra's
// own failures carry.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	if errors.Is(err, goclikit.ErrUsage) {
		return 2
	}
	return 1
}
