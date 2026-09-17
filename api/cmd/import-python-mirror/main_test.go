package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

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
	if _, err := readIDs(path); err == nil {
		t.Fatal("readIDs accepted a list naming no playlists")
	}
}

func TestBothInputsAreRequired(t *testing.T) {
	if err := run(context.Background(), []string{"-from", "ypl.db"}); err == nil {
		t.Fatal("run started without -playlists")
	}
}
