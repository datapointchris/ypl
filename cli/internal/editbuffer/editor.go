package editbuffer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// editorVariables are read in this order. The convention is that $EDITOR may be
// a line editor for dumb terminals and $VISUAL is the full-screen one, and a
// playlist is not something to rearrange in ed.
var editorVariables = []string{"VISUAL", "EDITOR"}

// defaultEditor is what runs where neither variable is set. POSIX requires it,
// so it is the one editor a machine can be assumed to have.
const defaultEditor = "vi"

// suffix is what the buffer's file is named, so an editor configured for a file
// type has something to match on.
const suffix = ".ypl"

// Command is what an edit runs, split the way a shell would split it.
//
// Split rather than executed through a shell so `EDITOR="code --wait"` works,
// which is the form half the editors on a machine are configured with. Split on
// whitespace alone, so an editor whose path holds a space has to be reached
// through a wrapper on $PATH — running it through a shell instead would take
// whatever else the variable held with it.
func Command() []string {
	for _, variable := range editorVariables {
		if configured := strings.TrimSpace(os.Getenv(variable)); configured != "" {
			return strings.Fields(configured)
		}
	}
	return []string{defaultEditor}
}

// Open edits text in this machine's editor, and returns what came back and
// whether it differs.
//
// Whether it differs is carried rather than left to the caller to compare,
// because "you opened it and closed it again" is a different outcome from "you
// rearranged it back to how it was", and only the first should reach nothing.
func Open(text string) (string, bool, error) {
	command := Command()
	directory, err := os.MkdirTemp("", "ypl")
	if err != nil {
		return "", false, fmt.Errorf("make somewhere to edit: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()

	path := filepath.Join(directory, "playlist"+suffix)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", false, fmt.Errorf("write the buffer: %w", err)
	}

	// The editor inherits this process's terminal, which is the whole of what
	// it needs and the opposite of how the browser launcher is run. An editor
	// whose stdout is a pipe draws its interface into a string.
	//
	// No context bounds it, and none should: a person deciding what a playlist
	// holds takes as long as they take, and the editor owns the terminal while
	// they do. A deadline here would take the screen back mid-edit.
	editor := exec.Command(command[0], append(command[1:], path)...)
	editor.Stdin, editor.Stdout, editor.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := editor.Run(); err != nil {
		return "", false, fmt.Errorf("%s exited without saving, so nothing was changed: %w", command[0], err)
	}

	edited, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read the buffer back: %w", err)
	}
	return string(edited), string(edited) != text, nil
}
