package jarseal

import (
	"errors"
	"testing"
)

// A jar sealed before the server's key changed is one the new key cannot
// open, and the server answers that as a missing session.
func TestAJarSealedUnderAnotherKeyIsUnopenable(t *testing.T) {
	before, err := New("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	after, err := New("ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := before.Seal("# Netscape HTTP Cookie File\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Open(sealed); !errors.Is(err, ErrUnopenable) {
		t.Fatalf("opening under another key returned %v, want ErrUnopenable", err)
	}
}
