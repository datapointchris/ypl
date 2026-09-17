package youtube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	ytapi "google.golang.org/api/youtube/v3"
)

// Channel reads and edits the playlists a channel owns, acting as that channel.
//
// A read that spans several pages is not a snapshot. The checks that refuse
// one as ErrInconsistentRead cannot see a delete and an add between the same
// two page requests, so a playlist or item absent from a read is not known to
// be gone until a read that names its id says so.
//
// A read made within seconds of a write can return the playlist as it was
// before the write.
//
// A write that returns an error this package does not name may or may not have
// been applied: a request can reach YouTube and its answer still be lost. Only a
// read afterwards says which.
type Channel struct {
	service  *ytapi.Service
	requests atomic.Int64
	units    atomic.Int64
	// pause waits between attempts at a request YouTube aborted, and returns
	// early with ctx's error when ctx ends.
	pause func(ctx context.Context, d time.Duration) error
}

// NewChannel acts as the channel whose owner granted creds. opts apply after
// the credentials, so option.WithEndpoint and option.WithHTTPClient point it
// elsewhere.
func NewChannel(ctx context.Context, creds Credentials, opts ...option.ClientOption) (*Channel, error) {
	source := oauthConfig(creds.Client).TokenSource(ctx, &oauth2.Token{RefreshToken: creds.RefreshToken})
	service, err := ytapi.NewService(ctx, append([]option.ClientOption{option.WithTokenSource(source)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("create the YouTube client: %w", err)
	}
	return &Channel{service: service, pause: sleep}, nil
}

// Requests is how many requests this channel has sent to YouTube, each attempt
// at an aborted insert included.
func (c *Channel) Requests() int64 {
	return c.requests.Load()
}

// Units is the quota those requests cost at their methods' published prices.
// A request that failed is counted at its full price.
func (c *Channel) Units() int64 {
	return c.units.Load()
}

// method is one Data API method: its name, the quota units a request to it
// costs, the sentinel for each refusal reason YouTube answers it with that a
// caller can act on, and whether an abort is sent again.
type method struct {
	name     string
	units    int64
	refusals map[string]error
	// retryAborted is set only where an aborted request was measured to leave
	// nothing behind, so sending it again cannot apply it twice.
	retryAborted bool
}

// ReadUnits is the quota one list request costs, and WriteUnits the quota one
// insert, update or delete costs, as the Data API's quota calculator prices them.
const (
	ReadUnits  = 1
	WriteUnits = 50
)

// The methods this package calls, at those prices.
var (
	playlistsList   = method{name: "playlists.list", units: ReadUnits}
	playlistsInsert = method{name: "playlists.insert", units: WriteUnits}
	playlistsUpdate = method{name: "playlists.update", units: WriteUnits}
	playlistsDelete = method{
		name: "playlists.delete", units: WriteUnits,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	playlistItemsList = method{
		name: "playlistItems.list", units: ReadUnits,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	// An insert sent right after its playlist was created was aborted twice,
	// and each playlist afterwards held only the copies from inserts that
	// returned 200.
	playlistItemsInsert = method{
		name: "playlistItems.insert", units: WriteUnits,
		refusals: map[string]error{
			"playlistNotFound":   ErrPlaylistNotFound,
			"videoNotFound":      ErrVideoNotFound,
			"failedPrecondition": ErrVideoRefused,
			"manualSortRequired": ErrManualSortRequired,
		},
		retryAborted: true,
	}
	// A move always sends the item's playlist, video and position, and YouTube
	// answered one naming a deleted item with invalidSnippet.
	playlistItemsUpdate = method{
		name: "playlistItems.update", units: WriteUnits,
		refusals: map[string]error{
			"invalidSnippet":     ErrItemNotFound,
			"manualSortRequired": ErrManualSortRequired,
		},
	}
	playlistItemsDelete = method{
		name: "playlistItems.delete", units: WriteUnits,
		refusals: map[string]error{"playlistItemNotFound": ErrItemNotFound},
	}
)

const (
	// attempts is how many times send makes a request YouTube keeps aborting,
	// for a method that retries one.
	attempts = 4
	// firstPause is the wait before the second attempt, doubled before each one
	// after, so the pauses total 7 seconds. A second insert sent at once after
	// an abort landed in one measurement, and one sent 5 seconds after in
	// another.
	firstPause = time.Second
)

// send is the one place this package makes a request to YouTube. It counts each
// attempt and the units m costs. A request YouTube aborts is sent again, up to
// attempts in all, only when m retries aborts. A refusal m names, and YouTube's
// quota refusal, come back wrapped in their sentinels.
func send[T any](ctx context.Context, c *Channel, m method, do func(...googleapi.CallOption) (T, error)) (T, error) {
	var none T
	pause := firstPause
	for attempt := 1; ; attempt++ {
		c.requests.Add(1)
		c.units.Add(m.units)
		response, err := do()
		if err == nil {
			return response, nil
		}
		if !m.retryAborted || !aborted(err) || attempt == attempts {
			return none, refusal(m, err)
		}
		if err := c.pause(ctx, pause); err != nil {
			return none, err
		}
		pause *= 2
	}
}

// noContent adapts a call whose response has no body to send.
func noContent(do func(...googleapi.CallOption) error) func(...googleapi.CallOption) (struct{}, error) {
	return func(opts ...googleapi.CallOption) (struct{}, error) {
		return struct{}{}, do(opts...)
	}
}

// aborted is whether YouTube answered 409 SERVICE_UNAVAILABLE.
func aborted(err error) bool {
	return hasReason(err, http.StatusConflict, "SERVICE_UNAVAILABLE")
}

func refusal(m method, err error) error {
	if hasReason(err, http.StatusForbidden, "quotaExceeded") {
		return fmt.Errorf("%w: %w", ErrQuotaSpent, err)
	}
	var google *googleapi.Error
	if errors.As(err, &google) {
		for _, item := range google.Errors {
			if named, ok := m.refusals[item.Reason]; ok {
				return fmt.Errorf("%w: %w", named, err)
			}
		}
	}
	return err
}

func hasReason(err error, code int, reason string) bool {
	var google *googleapi.Error
	if !errors.As(err, &google) || google.Code != code {
		return false
	}
	for _, item := range google.Errors {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
