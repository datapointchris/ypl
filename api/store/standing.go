package store

import (
	"context"
	"fmt"
	"time"

	"github.com/datapointchris/ypl/api/merge"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/youtube"
)

// Spent is the units recorded against the Pacific date, whoever spent them.
func Spent(ctx context.Context, q *generated.Queries, date string) (int64, error) {
	callers, err := SpentByCaller(ctx, q, date)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, caller := range callers {
		total += caller.Units
	}
	return total, nil
}

// EditReserve is the units no spender but an edit's push spends, so a long
// push running in the background leaves room for the next edit.
const EditReserve = 1_000

// Reserved is the units the day's quota keeps back at now for the sync: the
// reads of every tick of interval left before the quota resets, perTick each,
// and EditReserve for anything but an edit's push. Every other spender of the
// quota leaves it.
func Reserved(now time.Time, interval time.Duration, perTick int64, edit bool) int64 {
	var ticks int64
	if interval > 0 {
		ticks = int64(youtube.QuotaReset(now).Sub(now) / interval)
	}
	reserved := (ticks + 1) * perTick
	if !edit {
		reserved += EditReserve
	}
	return reserved
}

// ReadsPerTick is what one tick of the sync reads: the listing, and a sweep of
// a playlist of the library's average number of pages.
func ReadsPerTick(ctx context.Context, q *generated.Queries) (int64, error) {
	pages, err := q.CountPlaylistPages(ctx)
	if err != nil {
		return 0, err
	}
	return 1 + (pages.Pages+max(pages.Playlists, 1)-1)/max(pages.Playlists, 1), nil
}

// OwnCaller is the name SpentByCaller gives the server's own spending: its
// sync and the writes a person's requests make.
const OwnCaller = "ypl"

// CallerUnits is the units one caller spent on a Pacific date.
type CallerUnits struct {
	Caller string
	Units  int64
}

// SpentByCaller is the units recorded against the Pacific date by each caller
// that spent any, the server's own first and then each service client by id.
// The server's own are every write sent that day, a write not yet answered at
// its full cost, and every sync pass's reads.
func SpentByCaller(ctx context.Context, q *generated.Queries, date string) ([]CallerUnits, error) {
	written, err := q.SumWriteUnits(ctx, date)
	if err != nil {
		return nil, err
	}
	read, err := q.SumRunReadUnits(ctx, date)
	if err != nil {
		return nil, err
	}
	callers := []CallerUnits{{Caller: OwnCaller, Units: written.Units + written.Pending*youtube.WriteUnits + read}}
	services, err := q.SumServiceCallUnits(ctx, date)
	if err != nil {
		return nil, err
	}
	for _, service := range services {
		callers = append(callers, CallerUnits{Caller: service.ClientID, Units: service.Units})
	}
	return callers, nil
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
