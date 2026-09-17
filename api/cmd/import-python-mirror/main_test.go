package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runChild makes this test binary run the real main when a test starts it as a
// child process.
const runChild = "YPL_IMPORT_TEST_RUN"

func TestMain(m *testing.M) {
	if os.Getenv(runChild) == "1" {
		// The test binary's own flags come first; the command sees what follows --.
		if i := slices.Index(os.Args, "--"); i >= 0 {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
		}
		main()
		return
	}
	os.Exit(m.Run())
}

func TestHelpIsNotAnError(t *testing.T) {
	if err := run(context.Background(), []string{"-h"}, io.Discard); err != nil {
		t.Fatalf("run -h = %v, want nil", err)
	}
}

func TestAnUnknownFlagIsAUsageError(t *testing.T) {
	err := run(context.Background(), []string{"-bogus"}, io.Discard)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("run -bogus = %v, want ErrUsage", err)
	}
}

func TestBothInputsAreRequired(t *testing.T) {
	err := run(context.Background(), []string{"-from", "ypl.db"}, io.Discard)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("run without -playlists = %v, want ErrUsage", err)
	}
}

func TestReadIDsSkipsBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.txt")
	if err := os.WriteFile(path, []byte("PLA\n\n  PLB  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ids, err := readIDs(path)
	if err != nil {
		t.Fatalf("readIDs: %v", err)
	}
	if !slices.Equal(ids, []string{"PLA", "PLB"}) {
		t.Fatalf("ids = %q, want PLA and PLB", ids)
	}
}

func TestAnEmptyPlaylistListIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.txt")
	if err := os.WriteFile(path, []byte("\n\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readIDs(path); !errors.Is(err, ErrNoPlaylists) {
		t.Fatalf("readIDs of an empty list = %v, want ErrNoPlaylists", err)
	}
}

// The process contract, run as the real binary: exit codes, and nothing but
// data on stdout.
func TestExitCodesAndStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the command as a child process")
	}
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"-h"}, 0},
		{"unknown flag", []string{"-bogus"}, 2},
		{"missing input", []string{"-from", "ypl.db"}, 2},
		{"missing mirror", []string{"-from", filepath.Join(t.TempDir(), "absent", "ypl.db"), "-playlists", playlistFile(t)}, 1},
	}
	for _, tc := range cases {
		child := exec.Command(os.Args[0], append([]string{"-test.run=^$", "--"}, tc.args...)...)
		child.Env = append(os.Environ(), runChild+"=1", "DATABASE_PATH="+filepath.Join(t.TempDir(), "api.db"))
		var stdout, stderr bytes.Buffer
		child.Stdout, child.Stderr = &stdout, &stderr
		_ = child.Run()

		if got := child.ProcessState.ExitCode(); got != tc.code {
			t.Errorf("%s: exit %d, want %d; stderr: %s", tc.name, got, tc.code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: wrote to stdout: %q", tc.name, stdout.String())
		}
		if tc.code != 0 && !strings.Contains(stderr.String(), `"level":"ERROR"`) {
			t.Errorf("%s: no error record on stderr: %q", tc.name, stderr.String())
		}
	}
}

func playlistFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "owned.txt")
	if err := os.WriteFile(path, []byte("PLA\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}
