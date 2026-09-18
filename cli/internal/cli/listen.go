package cli

import (
	"context"
	"fmt"
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

// listenEvery is how often playback asks mpv where it is. A listen is counted
// from how far the position moved between two asks, so this bounds how much
// of a listen the last ask before mpv exits can miss.
const listenEvery = 10 * time.Second

// flushWithin bounds the last attempt to send what could not be sent while
// playing, which runs after mpv has exited and the command is otherwise done.
const flushWithin = 5 * time.Second

// listened is what one playback recorded: the plays the server stored, and the
// ones it could not be told about.
type listened struct {
	recorded []api.Play
	unsent   []error
}

// listening counts how long each video has actually played.
//
// Only the ground covered at the speed of playing is counted. Between two asks
// the position moves by about the interval, or twice it at double speed; a
// jump further than that is a seek, and a move backwards is a rewind, so
// neither is time spent listening. A pause moves nothing.
type listening struct {
	every    time.Duration
	heard    map[string]time.Duration
	counted  map[string]bool
	lastPath string
	lastAt   *int64
}

func newListening(every time.Duration) *listening {
	return &listening{every: every, heard: map[string]time.Duration{}, counted: map[string]bool{}}
}

// observe takes one answer from mpv and returns the video that has just
// crossed into counting as heard, or "" where none has.
func (l *listening) observe(state mpv.State) string {
	videoID := youtube.VideoID(state.Path)
	was, wasAt := l.lastPath, l.lastAt
	l.lastPath, l.lastAt = state.Path, state.Position
	if videoID == "" || state.Position == nil || was != state.Path || wasAt == nil {
		return ""
	}
	moved := time.Duration(*state.Position-*wasAt) * time.Second
	if moved <= 0 || moved > 2*l.every {
		return ""
	}
	l.heard[videoID] += moved
	if l.counted[videoID] || l.heard[videoID] < heardBar(state.Duration) {
		return ""
	}
	l.counted[videoID] = true
	return videoID
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

// recordListens asks mpv where it is on every tick until ctx ends, and tells
// the server about each video once it has played long enough to count. The
// ticks come about listenEvery apart; a test hands them over one at a time.
//
// A play is sent the moment it counts rather than when mpv exits, so a
// playback killed partway still records what was heard. Its id is made when it
// counts and kept, so one the server did not answer is sent again under the
// same id and stored once. What is still unsent when playback ends gets one
// more attempt, bounded, on a context of its own.
func recordListens(ctx context.Context, client *api.Client, read func() (mpv.State, error), ticks <-chan time.Time) listened {
	var done listened
	heard := newListening(listenEvery)
	pending := map[string]api.PlayID{}
	// send tells the server about every pending play, and returns the error of
	// each it could not, by video.
	send := func(ctx context.Context) map[string]error {
		failed := map[string]error{}
		for videoID, id := range pending {
			play, err := client.CreatePlay(ctx, id, api.VideoID(videoID))
			if err != nil {
				failed[videoID] = err
				continue
			}
			done.recorded = append(done.recorded, play)
			delete(pending, videoID)
		}
		return failed
	}

	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushWithin)
			for videoID, err := range send(last) {
				done.unsent = append(done.unsent, fmt.Errorf("%s: %w", videoID, reported(err)))
			}
			cancel()
			return done
		case <-ticks:
			// Nothing playing yet, or a socket mpv has not opened, is the same
			// answer every interval until it is not.
			state, err := read()
			if err != nil {
				continue
			}
			if videoID := heard.observe(state); videoID != "" {
				id, err := uuid.NewV7()
				if err != nil {
					done.unsent = append(done.unsent, fmt.Errorf("%s: make an id for the play: %w", videoID, err))
					continue
				}
				pending[videoID] = api.PlayID(id.String())
			}
			send(ctx)
		}
	}
}
