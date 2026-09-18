package cli

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/mpv"
	"github.com/datapointchris/ypl/cli/internal/youtube"
)

// heardAfter is how long a mix has to play before it counts as heard. A mix
// shorter than twice this counts at half its length instead, so a short one is
// not held to a bar it can barely reach.
const heardAfter = 20 * time.Minute

// readEvery is how often playback asks mpv where it is. It bounds how much of
// a listen the last read before mpv exits can miss.
const readEvery = 10 * time.Second

// sendWithin bounds one attempt to tell the server about a play. The reads
// wait on it, so a server that never answers delays the next read by this much
// rather than stopping them.
const sendWithin = 5 * time.Second

// flushWithin bounds the last attempt to send what could not be sent while
// playing, which runs after mpv has exited and the command is otherwise done.
const flushWithin = 5 * time.Second

// errOtherPlayer is an mpv answering on the socket that is not the one this
// playback started. Another `ypl play` takes the same path, and what that
// player plays is not this playback's to record.
var errOtherPlayer = errors.New("another player answers on the socket")

// delivered is what one playback told the server: the plays it stored, and the
// ones it could not.
type delivered struct {
	stored []api.Play
	unsent []unsentPlay
}

// unsentPlay is a play the server was never told about, with why.
type unsentPlay struct {
	videoID string
	err     error
}

// tally counts how long each video has actually played.
//
// Only ground covered at the speed of playing is counted. Between two reads
// the position can move as far as the time between them allows at mpv's
// speed; further than that is a seek. A move backwards is a rewind and a move
// of nothing is a pause, and neither is listening. The bound is the time that
// passed rather than the interval asked for, because a read can land late.
type tally struct {
	heard    map[string]time.Duration
	counted  map[string]bool
	lastPath string
	lastAt   *int64
	lastRead time.Time
}

func newTally() *tally {
	return &tally{heard: map[string]time.Duration{}, counted: map[string]bool{}}
}

// observe takes what mpv answered at the moment it answered, and returns the
// video that has just crossed into counting as heard, or "" where none has.
func (t *tally) observe(state mpv.State, at time.Time) string {
	videoID := youtube.VideoID(state.Path)
	was, wasAt, wasRead := t.lastPath, t.lastAt, t.lastRead
	t.lastPath, t.lastAt, t.lastRead = state.Path, state.Position, at
	if videoID == "" || state.Position == nil || was != state.Path || wasAt == nil {
		return ""
	}
	moved := time.Duration(*state.Position-*wasAt) * time.Second
	if moved <= 0 || moved > reachable(at.Sub(wasRead), state.Speed) {
		return ""
	}
	t.heard[videoID] += moved
	if t.counted[videoID] || t.heard[videoID] < heardBar(state.Duration) {
		return ""
	}
	t.counted[videoID] = true
	return videoID
}

// reachable is the furthest playing moves the position in elapsed at speed:
// that far, half again for a read answered a little late, and two seconds more
// because positions arrive in whole seconds.
func reachable(elapsed time.Duration, speed *float64) time.Duration {
	rate := 1.0
	if speed != nil && *speed > 0 {
		rate = *speed
	}
	return time.Duration(float64(elapsed)*rate*1.5) + 2*time.Second
}

// heardBar is how long a video of this length has to play to count as heard.
func heardBar(duration *int64) time.Duration {
	if duration != nil && *duration > 0 {
		if half := time.Duration(*duration) * time.Second / 2; half < heardAfter {
			return half
		}
	}
	return heardAfter
}

// fromPlayer is read, answering only for the mpv whose process id pid reports.
// Until the player has started, pid reports zero and nothing is this
// playback's.
func fromPlayer(read func() (mpv.State, error), pid func() int64) func() (mpv.State, error) {
	return func() (mpv.State, error) {
		state, err := read()
		if err != nil {
			return state, err
		}
		if state.PID == nil || *state.PID != pid() {
			return mpv.State{}, errOtherPlayer
		}
		return state, nil
	}
}

// recordListens reads mpv on every tick until ctx ends, and tells the server
// about each video once it has played long enough to count. now is when a read
// was answered.
//
// A play is sent the moment it counts rather than when mpv exits, so a
// playback killed partway still records what was heard. Its id is made when it
// counts and kept, so one the server did not answer is sent again under the
// same id and stored once. What is still unsent when playback ends gets one
// more attempt, bounded, on a context of its own.
func recordListens(ctx context.Context, client *api.Client, read func() (mpv.State, error), ticks <-chan time.Time, now func() time.Time) delivered {
	var done delivered
	counting := newTally()
	pending := map[string]api.PlayID{}
	// attempt tells the server about every pending play, each under a bound of
	// its own, and returns why each it could not was refused.
	attempt := func(ctx context.Context, within time.Duration) map[string]error {
		refused := map[string]error{}
		for videoID, id := range pending {
			bounded, cancel := context.WithTimeout(ctx, within)
			play, err := client.CreatePlay(bounded, id, api.VideoID(videoID))
			cancel()
			// A play is deleted while it waits to be sent again when its first
			// send landed unanswered and somebody took it back. It is settled,
			// and sending it on would only be refused again.
			if api.PlayDeleted(err) {
				delete(pending, videoID)
				continue
			}
			if err != nil {
				refused[videoID] = err
				continue
			}
			done.stored = append(done.stored, play)
			delete(pending, videoID)
		}
		return refused
	}

	for {
		select {
		case <-ctx.Done():
			for videoID, err := range attempt(context.WithoutCancel(ctx), flushWithin) {
				done.unsent = append(done.unsent, unsentPlay{videoID: videoID, err: reported(err)})
			}
			return done
		case <-ticks:
			// Nothing playing yet, or a socket held by another player, is the
			// same answer every tick until it is not.
			state, err := read()
			if err != nil {
				continue
			}
			if videoID := counting.observe(state, now()); videoID != "" {
				id, err := uuid.NewV7()
				if err != nil {
					done.unsent = append(done.unsent, unsentPlay{videoID: videoID, err: err})
					continue
				}
				pending[videoID] = api.PlayID(id.String())
			}
			attempt(ctx, sendWithin)
		}
	}
}
