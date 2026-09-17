package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/datapointchris/ypl/api/youtube"
)

// neverAuthorize fails the test if the command reaches the device flow.
func neverAuthorize(t *testing.T) authorizer {
	return func(context.Context, youtube.Client, func(string, string) error) (string, error) {
		t.Error("the device flow started")
		return "", nil
	}
}

func TestHelpStartsNoDeviceFlow(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	if err := run(context.Background(), []string{"-h"}, io.Discard, io.Discard, neverAuthorize(t)); err != nil {
		t.Fatalf("run -h = %v, want nil before any credential is read", err)
	}
}

func TestAnythingButNoArgumentsIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"-bogus"}, {"extra"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard, neverAuthorize(t)); !errors.Is(err, ErrUsage) {
			t.Errorf("run %q = %v, want ErrUsage", args, err)
		}
	}
}

func TestAMissingClientStartsNoDeviceFlow(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "id")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	if err := run(context.Background(), nil, io.Discard, io.Discard, neverAuthorize(t)); !errors.Is(err, youtube.ErrMissingCredentials) {
		t.Fatalf("run = %v, want ErrMissingCredentials", err)
	}
}

func TestTheGrantedTokenIsPrintedAndThePromptKeptOffStdout(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "id")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "secret")
	var stdout, stderr bytes.Buffer
	authorize := func(_ context.Context, client youtube.Client, show func(string, string) error) (string, error) {
		if client != (youtube.Client{ID: "id", Secret: "secret"}) {
			t.Errorf("authorized as %+v", client)
		}
		if err := show("https://www.google.com/device", "ABCD-EFGH"); err != nil {
			return "", err
		}
		return "refresh", nil
	}

	if err := run(context.Background(), nil, &stdout, &stderr, authorize); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := stdout.String(); got != "{\"refresh_token\":\"refresh\"}\n" {
		t.Fatalf("stdout = %q, want the refresh token alone", got)
	}
	if want := "Open https://www.google.com/device, enter ABCD-EFGH, and choose the channel's own account.\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}
