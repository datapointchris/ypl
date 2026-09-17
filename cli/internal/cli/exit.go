package cli

import (
	"errors"
	"fmt"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"
)

// usageError marks an invocation a caller got wrong — a bad flag, the wrong
// number of arguments, a value outside its range. It exits 2, which is the only
// answer that tells a caller to try different arguments rather than to try
// again later.
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }

func (u usageError) Unwrap() error { return u.err }

// exitCode is an answer that is not a success and not a failure: the command
// ran, said what it found, and the finding is worth an exit code of its own.
// `ypl auth status` on a machine that is not logged in is the case. It carries
// no message, so nothing prints an "error:" line over what the command already
// said.
type exitCode int

func (e exitCode) Error() string { return "" }

// requireSubcommand is what a namespace runs. A namespace expects another word
// after it, so a bare invocation is ambiguous and shows help; a word that names
// no subcommand is a mistake, and cobra would otherwise show help for that too.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	return usageError{fmt.Errorf("unknown command %q for %q\nRun '%s --help' for usage",
		args[0], cmd.CommandPath(), cmd.CommandPath())}
}

// usageArgs is validate, with what it refuses marked as a usage mistake.
// Cobra's own validators return a plain error, which would exit 1 and tell a
// caller the command failed rather than that it was typed wrong.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// exitCodeFor is the process exit code for err: 0 for success, 2 for a usage
// mistake, an exitCode's own value, and 1 for anything else.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	var usage usageError
	if errors.As(err, &usage) {
		return 2
	}
	// What cobra refuses before any RunE runs, which the tree never reaches to
	// mark itself.
	if errors.Is(err, goclikit.ErrUsage) {
		return 2
	}
	return 1
}
