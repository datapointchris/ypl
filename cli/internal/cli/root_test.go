package cli

import (
	"bytes"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestVersionNamesTheToolAndTheBuild(t *testing.T) {
	out, err := execute(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if got, want := strings.TrimSpace(out), "ypl version dev"; got != want {
		t.Fatalf("--version = %q, want %q", got, want)
	}
}
