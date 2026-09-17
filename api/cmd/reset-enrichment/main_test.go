package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

// newStore is a store holding vheld, which enrichment has stopped reading, and
// vlater, which it reads again at the stored time.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	err = st.InTx(ctx, func(tx *store.Tx) error {
		for video, retry := range map[string]sql.NullString{
			"vheld":  {},
			"vlater": {String: "2026-09-18T00:00:00Z", Valid: true},
		} {
			if err := tx.UpsertAvailableVideo(ctx, generated.UpsertAvailableVideoParams{VideoID: video, Title: video, ChannelTitle: "Channel"}); err != nil {
				return err
			}
			if _, err := tx.SetVideoEnrichment(ctx, generated.SetVideoEnrichmentParams{
				VideoID:     video,
				Description: sql.NullString{Valid: true},
				EnrichedTs:  sql.NullString{String: "2026-09-17T00:00:00Z", Valid: true},
			}); err != nil {
				return err
			}
			failure := generated.UpsertEnrichFailureParams{
				VideoID: video, AttemptedTs: "2026-09-17T00:00:00Z", Reason: "sign in to confirm your age", Attempts: 6, RetryTs: retry,
			}
			if err := tx.UpsertEnrichFailure(ctx, failure); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store the videos: %v", err)
	}
	return st
}

func mustReset(t *testing.T, st *store.Store, clear bool) report {
	t.Helper()
	var out bytes.Buffer
	if err := reset(context.Background(), st, clear, &out); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var rep report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode %s: %v", out.String(), err)
	}
	return rep
}

// Only a video enrichment has stopped reading is shown, since a video waiting
// for its retry time needs nothing done to it.
func TestItShowsOnlyTheVideosEnrichmentStoppedReading(t *testing.T) {
	st := newStore(t)

	rep := mustReset(t, st, false)
	if len(rep.Held) != 1 || rep.Held[0].VideoID != "vheld" || rep.Held[0].Attempts != 6 || rep.Cleared != 0 {
		t.Fatalf("report %+v, want vheld alone, shown and not cleared", rep)
	}
	if _, err := st.Queries.GetEnrichFailure(context.Background(), "vheld"); err != nil {
		t.Fatalf("vheld's mark after a run that only looked: %v", err)
	}
}

// Putting a video back forgets both marks that keep it out of the queue: the
// failure, and that a read reached it.
func TestClearingPutsAHeldVideoBackInTheQueue(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	rep := mustReset(t, st, true)
	if len(rep.Held) != 1 || rep.Cleared != 1 {
		t.Fatalf("report %+v, want the one held video shown and cleared", rep)
	}
	if _, err := st.Queries.GetEnrichFailure(ctx, "vheld"); err == nil {
		t.Fatal("vheld kept its mark after it was cleared")
	}
	video, err := st.Queries.GetVideo(ctx, "vheld")
	if err != nil || video.EnrichedTs.Valid {
		t.Fatalf("vheld = %+v, %v, want the read forgotten so the queue offers it", video, err)
	}
	later, err := st.Queries.GetVideo(ctx, "vlater")
	if err != nil || !later.EnrichedTs.Valid {
		t.Fatalf("vlater = %+v, %v, want its read kept, since it was never held back", later, err)
	}
	if _, err := st.Queries.GetEnrichFailure(ctx, "vlater"); err != nil {
		t.Fatalf("vlater's mark: %v, want it kept", err)
	}
}

func TestItRefusesAnArgumentAndAnUnknownFlag(t *testing.T) {
	var out, usage bytes.Buffer
	for name, args := range map[string][]string{
		"an argument":    {"vheld"},
		"unknown a flag": {"-wipe"},
	} {
		if err := run(context.Background(), args, &out, &usage); err == nil {
			t.Errorf("run with %s succeeded, want a refusal", name)
		}
	}
}
