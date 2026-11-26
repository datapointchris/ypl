package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
	"github.com/datapointchris/ypl/api/ytdlp"
)

// doorPauses keeps the yt-dlp door's pauses in the store, so a server
// restarted during a pause takes it up again.
type doorPauses struct {
	st *store.Store
}

func (p doorPauses) LatestPause(ctx context.Context) (ytdlp.Pause, bool, error) {
	row, err := p.st.Queries.LatestYtdlpPause(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ytdlp.Pause{}, false, nil
	}
	if err != nil {
		return ytdlp.Pause{}, false, err
	}
	at, err := time.Parse(time.RFC3339, row.PausedTs)
	if err != nil {
		return ytdlp.Pause{}, false, fmt.Errorf("the latest pause began at %q: %w", row.PausedTs, err)
	}
	until, err := time.Parse(time.RFC3339, row.UntilTs)
	if err != nil {
		return ytdlp.Pause{}, false, fmt.Errorf("the latest pause ends at %q: %w", row.UntilTs, err)
	}
	return ytdlp.Pause{At: at, Until: until, Cause: ytdlp.PauseCause(row.Cause), Error: row.Error}, true, nil
}

func (p doorPauses) RecordPause(ctx context.Context, pause ytdlp.Pause) error {
	return p.st.InTx(ctx, func(tx *store.Tx) error {
		return tx.InsertYtdlpPause(ctx, generated.InsertYtdlpPauseParams{
			PausedTs: store.Timestamp(pause.At),
			UntilTs:  store.Timestamp(pause.Until),
			Cause:    string(pause.Cause),
			Error:    pause.Error,
		})
	})
}
