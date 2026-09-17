package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/datapointchris/ypl/api/youtube"
)

// stubReader serves playlists, and for each playlist id either its items or an
// error.
type stubReader struct {
	playlists []youtube.Playlist
	items     map[string][]youtube.Item
	failures  map[string]error
	read      []string
}

func (s *stubReader) Playlists(context.Context) ([]youtube.Playlist, error) {
	return s.playlists, nil
}

func (s *stubReader) Items(_ context.Context, playlistID string) ([]youtube.Item, error) {
	s.read = append(s.read, playlistID)
	if err := s.failures[playlistID]; err != nil {
		return nil, err
	}
	return s.items[playlistID], nil
}

func (s *stubReader) Requests() int64 { return int64(1 + len(s.read)) }

func noCredentials(t *testing.T) {
	t.Setenv("YOUTUBE_CLIENT_ID", "")
	t.Setenv("YOUTUBE_CLIENT_SECRET", "")
	t.Setenv("YOUTUBE_REFRESH_TOKEN", "")
}

func TestHelpReadsNothing(t *testing.T) {
	noCredentials(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &out, io.Discard); err != nil {
		t.Fatalf("run -h = %v, want nil before any credential is read", err)
	}
	if out.Len() != 0 {
		t.Fatalf("run -h wrote a report: %s", out.String())
	}
}

func TestAnythingButNoArgumentsIsAUsageError(t *testing.T) {
	noCredentials(t)
	for _, args := range [][]string{{"-bogus"}, {"extra"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, ErrUsage) {
			t.Errorf("run %q = %v, want ErrUsage before any credential is read", args, err)
		}
	}
}

func TestMissingCredentialsStopTheCheckBeforeAnyRequest(t *testing.T) {
	noCredentials(t)
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out, io.Discard); !errors.Is(err, youtube.ErrMissingCredentials) {
		t.Fatalf("run without credentials = %v, want ErrMissingCredentials", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote a report without credentials: %s", out.String())
	}
}

func TestEveryPlaylistIsReadAndEachUnreadOneReported(t *testing.T) {
	stub := &stubReader{
		playlists: []youtube.Playlist{{ID: "PLA"}, {ID: "PLB"}, {ID: "PLC"}, {ID: "PLD"}},
		items: map[string][]youtube.Item{
			"PLA": {{ID: "a1"}, {ID: "a2", Unavailable: true}},
			"PLD": {{ID: "d1"}},
		},
		failures: map[string]error{
			"PLB": fmt.Errorf("%w: moved", youtube.ErrInconsistentRead),
			"PLC": fmt.Errorf("%w: new status", youtube.ErrUnexpectedResponse),
		},
	}
	var out bytes.Buffer

	err := check(context.Background(), stub, &out)
	if !errors.Is(err, ErrUnreadPlaylists) {
		t.Fatalf("check = %v, want ErrUnreadPlaylists", err)
	}
	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("report %q: %v", out.String(), err)
	}
	if got.Playlists != 4 || got.Items != 3 || got.Unavailable != 1 || got.Requests != 5 {
		t.Fatalf("report = %+v, want 4 playlists, 3 items, 1 unavailable and 5 requests", got)
	}
	if len(got.Unread) != 2 || got.Unread[0].Playlist != "PLB" || got.Unread[1].Playlist != "PLC" {
		t.Fatalf("unread = %+v, want PLB and PLC", got.Unread)
	}
}

func TestASpentQuotaStopsTheCheck(t *testing.T) {
	stub := &stubReader{
		playlists: []youtube.Playlist{{ID: "PLA"}, {ID: "PLB"}},
		failures:  map[string]error{"PLA": fmt.Errorf("%w: refused", youtube.ErrQuotaSpent)},
	}
	var out bytes.Buffer

	if err := check(context.Background(), stub, &out); !errors.Is(err, youtube.ErrQuotaSpent) {
		t.Fatalf("check = %v, want ErrQuotaSpent", err)
	}
	if len(stub.read) != 1 || out.Len() != 0 {
		t.Fatalf("read %v and wrote %q, want PLA alone and no report", stub.read, out.String())
	}
}

func TestACleanCheckReportsNoUnreadPlaylists(t *testing.T) {
	stub := &stubReader{playlists: []youtube.Playlist{{ID: "PLA"}}, items: map[string][]youtube.Item{"PLA": {{ID: "a1"}}}}
	var out bytes.Buffer

	if err := check(context.Background(), stub, &out); err != nil {
		t.Fatalf("check: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("report %q: %v", out.String(), err)
	}
	if unread, ok := got["unread"].([]any); !ok || len(unread) != 0 {
		t.Fatalf("unread = %#v, want an empty list", got["unread"])
	}
}
