package ytdlp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// LeaseHold is how long a lease holds the door. It covers the read that a
// desk then makes itself: mpv's yt-dlp resolving one stream, which takes a few
// seconds.
const LeaseHold = 15 * time.Second

// Lease is a turn at the door, held while a desk reads YouTube itself: its id,
// which EndLease takes, and when its hold ends.
type Lease struct {
	ID        string
	HeldUntil time.Time
}

// Lease waits for the door's turn for class, as a read does, and holds the
// door for LeaseHold from then, or until EndLease names it. The gap after it
// runs from the end of the hold, so a read made on another machine keeps the
// door's pace and stops at its breaker. It returns the door's own refusal where
// no turn came.
func (d *Door) Lease(ctx context.Context, class Class) (Lease, error) {
	release, err := d.enter(ctx, readTerms{class: class, subject: "a lease"})
	if err != nil {
		return Lease{}, err
	}
	lease := Lease{ID: rand.Text(), HeldUntil: d.now().Add(d.leaseHold)}
	ended := make(chan error, 1)
	d.mu.Lock()
	d.leases[lease.ID] = ended
	d.mu.Unlock()
	go func() {
		held := time.NewTimer(d.leaseHold)
		defer held.Stop()
		outcome, told := errNoVerdict, false
		select {
		case outcome = <-ended:
			told = true
		case <-held.C:
		case <-d.stopped.Done():
		}
		d.mu.Lock()
		if _, open := d.leases[lease.ID]; open {
			delete(d.leases, lease.ID)
		} else if !told {
			// EndLease took the lease as the hold ended, and its outcome waits.
			outcome = <-ended
		}
		d.mu.Unlock()
		release(outcome)
	}()
	return lease, nil
}

// ansiEscape is a terminal's color code, which a desk's yt-dlp writes where
// its own config asks for color.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// EndLease ends the lease id, whose read exited with exitCode after writing
// stderr. Where YouTube refused the read, the door pauses as it does for a
// read of its own, even once the hold has ended. Any other end moves no
// failures in a row. The desk's yt-dlp reads with its own options and config,
// which the server never saw. A lease still held is let go at once.
func (d *Door) EndLease(id string, exitCode int, stderr string) {
	outcome := errNoVerdict
	if exitCode != 0 {
		refused := refusal(ansiEscape.ReplaceAllString(stderr, ""), fmt.Errorf("yt-dlp exited %d", exitCode))
		if errors.Is(refused, ErrRateLimited) {
			outcome = refused
		}
	}
	d.mu.Lock()
	if ended, held := d.leases[id]; held {
		delete(d.leases, id)
		ended <- outcome
		d.mu.Unlock()
		return
	}
	if outcome == errNoVerdict {
		d.mu.Unlock()
		return
	}
	d.judgeAndUnlock(readTerms{class: Person, subject: "a lease"}, outcome)
}
