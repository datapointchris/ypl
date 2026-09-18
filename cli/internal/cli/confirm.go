package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/datapointchris/goclikit"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// noInput is the flag that forbids every prompt, so a caller can take the
// non-interactive path from a terminal.
const noInput = "no-input"

// confirm asks question and reports whether the answer approved. A verb that
// destroys something calls it unless --yes has already answered.
//
// A prompt is only ever offered on an interactive stdin. Prompting a
// non-interactive caller blocks on a stdin that never closes, leaving it with no
// output and no exit code — the one failure a caller cannot recover from, and
// the reason the gate is here rather than at each call site.
//
// Refusing to prompt is a usage mistake rather than a failure, because the
// answer is to run it again with --yes. It is also made before anything is
// asked of the server, so the caller is told what they left out rather than
// told that a write did not happen.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	if err := confirmable(cmd); err != nil {
		return false, err
	}
	return readConfirmation(cmd.ErrOrStderr(), cmd.InOrStdin(), question)
}

// confirmable reports whether a prompt could be offered, which is what a verb
// checks before it reads anything the prompt would name.
func confirmable(cmd *cobra.Command) error {
	if interactive(cmd) {
		return nil
	}
	return goclikit.UsageError(errors.New("refusing to prompt without an interactive terminal; pass --yes to confirm"))
}

// editable reports whether a verb may open an editor, which it checks before
// the token and before its reads.
//
// A pipe is not a prompt, so a buffer piped in is read whatever --no-input
// says. The refusal is for the one case --no-input forbids: a terminal, with
// nothing else to read a buffer from.
func editable(cmd *cobra.Command) error {
	if !terminalIn(cmd) || interactive(cmd) {
		return nil
	}
	return goclikit.UsageError(errors.New("refusing to open an editor with --no-input; pipe a buffer in instead"))
}

// interactive reports whether the command may take the terminal: --no-input
// never may, and otherwise stdin has to be a terminal.
//
// Read off cmd.Flags() rather than the root's, because that resolves the flag
// wherever it is declared. Reading the root's persistent set finds nothing once
// the declaration moves down the tree, and a lookup that finds nothing reports
// false — which is --no-input silently doing nothing.
func interactive(cmd *cobra.Command) bool {
	if forbidden, err := cmd.Flags().GetBool(noInput); err == nil && forbidden {
		return false
	}
	return terminalIn(cmd)
}

// terminalIn reports whether this command's stdin is a terminal rather than
// something a caller piped in. A reader a test substituted is not an *os.File,
// so it reads as piped — which is what makes both gates testable without a pty.
//
// It is separate from interactive because the two answer different questions. A
// verb reading a document from stdin wants this one: --no-input says not to
// take the terminal, and says nothing about a pipe.
func terminalIn(cmd *cobra.Command) bool {
	file, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// readConfirmation writes question to out and reads an answer from in. Only "y"
// or "yes" approves, in any case; EOF and anything else decline. It is split
// from confirm so the parsing stays testable on plain buffers.
func readConfirmation(out io.Writer, in io.Reader, question string) (bool, error) {
	_, _ = fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// addYes binds --yes/-y, which answers the confirmation what does asks for.
func addYes(cmd *cobra.Command, yes *bool, does string) {
	cmd.Flags().BoolVarP(yes, "yes", "y", false, "Answer the confirmation, and "+does+" without asking")
}
