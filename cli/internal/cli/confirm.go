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
func (a *app) confirm(cmd *cobra.Command, question string) (bool, error) {
	if err := a.confirmable(cmd); err != nil {
		return false, err
	}
	return readConfirmation(cmd.ErrOrStderr(), cmd.InOrStdin(), question)
}

// confirmable reports whether a prompt could be offered, which is what a verb
// checks before it reads anything the prompt would name. The refusal names
// what stopped it, since at a terminal a sentence blaming the terminal is false.
func (a *app) confirmable(cmd *cobra.Command) error {
	switch {
	case forbidsInput(cmd):
		return goclikit.UsageError(errors.New("refusing to prompt under --no-input; pass --yes to confirm"))
	case !a.terminalIn(cmd):
		return goclikit.UsageError(errors.New("refusing to prompt without an interactive terminal; pass --yes to confirm"))
	}
	return nil
}

// editable reports whether a verb may open an editor, which it checks before
// the token and before its reads.
//
// A pipe is not a prompt, so a buffer piped in is read whatever --no-input
// says. The refusal is for the one case --no-input forbids: a terminal, with
// nothing else to read a buffer from.
func (a *app) editable(cmd *cobra.Command) error {
	if forbidsInput(cmd) && a.terminalIn(cmd) {
		return goclikit.UsageError(errors.New("refusing to open an editor with --no-input; pipe a buffer in instead"))
	}
	return nil
}

// forbidsInput reports whether --no-input was passed.
//
// Read off cmd.Flags() rather than the root's, because that resolves the flag
// wherever it is declared. Reading the root's persistent set finds nothing once
// the declaration moves down the tree, and a lookup that finds nothing reports
// false — which is --no-input silently doing nothing.
func forbidsInput(cmd *cobra.Command) bool {
	forbidden, err := cmd.Flags().GetBool(noInput)
	return err == nil && forbidden
}

// terminalIn reports whether this command's stdin is a terminal rather than
// something a caller piped in.
//
// It is separate from forbidsInput because the two answer different
// questions. A verb reading a document from stdin wants this one: --no-input
// says not to take the terminal, and says nothing about a pipe.
func (a *app) terminalIn(cmd *cobra.Command) bool {
	return a.terminal(cmd.InOrStdin())
}

// isTerminal is the binary's answer to whether in is a terminal somebody is
// at. A reader that is not an *os.File was piped in.
func isTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
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

// addNoInput binds --no-input on a verb that asks a question or opens an
// editor, and on no other, so it never shows on a verb it would not change.
// instead is what the verb does in place of taking the terminal. The flag is
// read back off the flag set by forbidsInput.
func addNoInput(cmd *cobra.Command, instead string) {
	cmd.Flags().Bool(noInput, false, "Never take the terminal; "+instead)
}
