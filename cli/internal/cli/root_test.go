package cli

import (
	"bytes"
	"slices"
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
	words := strings.Fields(out)
	for _, want := range []string{"ypl", "dev"} {
		if !slices.Contains(words, want) {
			t.Errorf("--version = %q, want it to name %q", out, want)
		}
	}
}
