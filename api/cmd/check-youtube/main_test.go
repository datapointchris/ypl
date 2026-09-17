package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/datapointchris/ypl/api/youtube"
)

func TestMissingCredentialsStopTheCheckBeforeAnyRequest(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	t.Setenv("YOUTUBE_REFRESH_TOKEN", "")
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "api.db"))

	var out bytes.Buffer
	err := run(context.Background(), &out)
	if !errors.Is(err, youtube.ErrMissingCredentials) {
		t.Fatalf("run without credentials = %v, want ErrMissingCredentials", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote a report without credentials: %s", out.String())
	}
}
