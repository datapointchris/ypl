package store

import (
	"context"
	"fmt"
	"time"

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// Spent is the units recorded against the Pacific date: every write sent that
// day, a write not yet answered at its full cost, and every sync pass's reads.
func Spent(ctx context.Context, q *generated.Queries, date string) (int64, error) {
	written, err := q.SumWriteUnits(ctx, date)
	if err != nil {
		return 0, err
	}
	read, err := q.SumRunReadUnits(ctx, date)
	if err != nil {
		return 0, err
	}
	return written.Units + written.Pending*youtube.WriteUnits + read, nil
}

// PendingPush is a playlist whose server order YouTube does not yet hold, and
// how many writes a push plans to reach it from what YouTube held at the last
// read. Held is whether the push waits on a write YouTube refused this Pacific
// day.
type PendingPush struct {
	PlaylistID string
	Writes     int
	Held       bool
}

// PendingPushes is every playlist whose server order YouTube does not yet hold,
// as of now. It reads only the store, so the count is what a push would plan
// if YouTube has not changed since the last read.
func PendingPushes(ctx context.Context, q *generated.Queries, now time.Time) ([]PendingPush, error) {
	ids, err := q.ListPlaylistIDs(ctx)
	if err != nil {
		return nil, err
	}
	var pending []PendingPush
	for _, id := range ids {
		state, err := q.GetPlaylistState(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("read the state of %s: %w", id, err)
		}
		base, err := Base(ctx, q, id)
		if err != nil {
			return nil, err
		}
		stored, err := Entries(ctx, q, id)
		if err != nil {
			return nil, err
		}
		read := make([]merge.Item, len(base))
		for i, item := range base {
			read[i] = merge.Item{ID: item.ItemID, VideoID: item.VideoID}
		}
		entries := make([]merge.Entry, len(stored))
		for i, entry := range stored {
			entries[i] = merge.Entry(entry)
		}
		writes := merge.PlanAppends(read, entries)
		if state.Sort == SortManual {
			writes = merge.Plan(read, entries)
		}
		if len(writes) == 0 {
			continue
		}
		held := false
		if state.RefusedWriteID.Valid {
			refused, err := q.GetYouTubeWrite(ctx, state.RefusedWriteID.Int64)
			if err != nil {
				return nil, fmt.Errorf("read the refused write %d to %s: %w", state.RefusedWriteID.Int64, id, err)
			}
			held = refused.QuotaDate == youtube.QuotaDate(now)
		}
		pending = append(pending, PendingPush{PlaylistID: id, Writes: len(writes), Held: held})
	}
	return pending, nil
}
