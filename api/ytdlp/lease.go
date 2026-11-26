package ytdlp

import (
	"context"
	"time"
)

// LeaseHold is how long a lease holds the door. It covers the read that a
// desk then makes itself: mpv's yt-dlp resolving one stream, which takes a few
// seconds.
const LeaseHold = 15 * time.Second

// Lease waits for the door's turn for class, as a read does, and holds the
// door for LeaseHold from then. The gap after it runs from the end of the
// hold, so a read made on another machine keeps the door's pace and stops at
// its breaker. The hold says nothing of how YouTube answered that read, so it
// moves no failures in a row. Lease returns when the hold ends, and the
// door's own refusal where no turn came.
func (d *Door) Lease(ctx context.Context, class Class) (time.Time, error) {
	release, err := d.enter(ctx, class)
	if err != nil {
		return time.Time{}, err
	}
	until := d.now().Add(d.leaseHold)
	go func() {
		held := time.NewTimer(d.leaseHold)
		defer held.Stop()
		select {
		case <-held.C:
		case <-d.stopped.Done():
		}
		release("a lease", errNoVerdict)
	}()
	return until, nil
}
