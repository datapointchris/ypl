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
// at an aborted request included.
func (c *Channel) Requests() int64 {
	return c.requests.Load()
}

// Units is the quota those requests cost at their methods' published prices.
// A request that failed is counted at its full price.
func (c *Channel) Units() int64 {
	return c.units.Load()
}

// method is one Data API method: its name, the quota units a request to it
// costs, and the sentinel for each refusal reason YouTube answers it with that a
// caller can act on.
type method struct {
	name     string
	units    int64
	refusals map[string]error
}

// The methods this package calls, priced as the Data API's quota calculator
// prices them.
var (
	playlistsList   = method{name: "playlists.list", units: 1}
	playlistsInsert = method{name: "playlists.insert", units: 50}
	playlistsUpdate = method{name: "playlists.update", units: 50}
	playlistsDelete = method{
		name: "playlists.delete", units: 50,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	playlistItemsList = method{
		name: "playlistItems.list", units: 1,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	playlistItemsInsert = method{
		name: "playlistItems.insert", units: 50,
		refusals: map[string]error{
			"playlistNotFound":   ErrPlaylistNotFound,
			"videoNotFound":      ErrVideoNotFound,
			"failedPrecondition": ErrVideoRefused,
		},
	}
	playlistItemsUpdate = method{name: "playlistItems.update", units: 50}
	playlistItemsDelete = method{
		name: "playlistItems.delete", units: 50,
		refusals: map[string]error{"playlistItemNotFound": ErrItemNotFound},
	}
)

const (
	// attempts is how many times send makes a request YouTube keeps aborting.
	attempts = 4
	// firstPause is the wait before the second attempt, doubled before each one
	// after. YouTube aborts an insert into a playlist created under a second
	// before, and accepts the same insert a second or more after.
	firstPause = time.Second
)

// send is the one place this package makes a request to YouTube. It counts each
// attempt and the units m costs. A request YouTube aborts wrote nothing, so it
// is sent again, up to attempts in all. A refusal m names, and YouTube's quota
// refusal, come back wrapped in their sentinels.
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
		if !aborted(err) || attempt == attempts {
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

// aborted is whether YouTube answered 409 SERVICE_UNAVAILABLE, which it does
// without applying the request.
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
