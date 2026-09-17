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
// A read sent within ReadLag of a write YouTube answered can return the
// playlist as it was before the write.
//
// A write that returns ErrRefused was not applied. A write that returns any
// other error may or may not have been: a request can reach YouTube and its
// answer still be lost. Only a read afterwards says which.
type Channel struct {
	service  *ytapi.Service
	requests atomic.Int64
	units    atomic.Int64
	// pause waits between attempts at a request YouTube aborted, and returns
	// early with ctx's error when ctx ends.
	pause func(ctx context.Context, d time.Duration) error
}

// ReadLag is how long after YouTube answers a write a read can still return the
// playlist as it was before it. Reads after a create first showed the playlist
// 1.9 seconds on by id and 2.1 seconds on in the channel's list, after a rename
// showed the new title 2.8 seconds on, and after a delete stopped showing the
// playlist 2.7 seconds on. A list of a new playlist's items sent as the create
// answered was refused with playlistNotFound. ReadLag is about twenty times the
// longest of those.
const ReadLag = time.Minute

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

// The names of the Data API methods this package calls, as its quota calculator
// names them.
const (
	MethodPlaylistsList       = "playlists.list"
	MethodPlaylistsInsert     = "playlists.insert"
	MethodPlaylistsUpdate     = "playlists.update"
	MethodPlaylistsDelete     = "playlists.delete"
	MethodPlaylistItemsList   = "playlistItems.list"
	MethodPlaylistItemsInsert = "playlistItems.insert"
	MethodPlaylistItemsUpdate = "playlistItems.update"
	MethodPlaylistItemsDelete = "playlistItems.delete"
)

// The methods this package calls, priced as the Data API's quota calculator
// prices them.
var (
	playlistsList   = method{name: MethodPlaylistsList, units: 1}
	playlistsInsert = method{name: MethodPlaylistsInsert, units: 50}
	// An update sent right after its playlist was created was aborted, and the
	// playlist kept its title through reads 5, 10 and 15 seconds later.
	playlistsUpdate = method{name: MethodPlaylistsUpdate, units: 50, retryAborted: true}
	playlistsDelete = method{
		name: MethodPlaylistsDelete, units: 50,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	playlistItemsList = method{
		name: MethodPlaylistItemsList, units: 1,
		refusals: map[string]error{"playlistNotFound": ErrPlaylistNotFound},
	}
	// An insert sent right after its playlist was created was aborted twice,
	// and each playlist afterwards held only the copies from inserts that
	// returned 200.
	playlistItemsInsert = method{
		name: MethodPlaylistItemsInsert, units: 50,
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
		name: MethodPlaylistItemsUpdate, units: 50,
		refusals: map[string]error{
			"invalidSnippet":     ErrItemNotFound,
			"manualSortRequired": ErrManualSortRequired,
		},
	}
	playlistItemsDelete = method{
		name: MethodPlaylistItemsDelete, units: 50,
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
// attempts in all, only when m retries aborts. Every refusal comes back as
// ErrRefused, and a refusal m names, and YouTube's quota refusal, as their
// sentinels too.
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

// refusal is err as the caller sees it: a 4xx answer as a refusedError, and
// anything else unchanged.
func refusal(m method, err error) error {
	var google *googleapi.Error
	if !errors.As(err, &google) || google.Code < 400 || google.Code > 499 {
		return err
	}
	refused := &refusedError{answer: err}
	if hasReason(err, http.StatusForbidden, "quotaExceeded") {
		refused.named = ErrQuotaSpent
	}
	for _, item := range google.Errors {
		if named, ok := m.refusals[item.Reason]; ok {
			refused.named = named
			break
		}
	}
	return refused
}

// refusedError is YouTube answering a request with a 4xx status. It is
// ErrRefused, and named as well when YouTube's reason has a sentinel.
type refusedError struct {
	named  error
	answer error
}

func (e *refusedError) Error() string {
	if e.named == nil {
		return e.answer.Error()
	}
	return e.named.Error() + ": " + e.answer.Error()
}

func (e *refusedError) Is(target error) bool {
	return target == ErrRefused
}

func (e *refusedError) Unwrap() []error {
	if e.named == nil {
		return []error{e.answer}
	}
	return []error{e.named, e.answer}
}

// RefusalMessage is the message YouTube gave with the refusal err carries, and
// false when err carries none.
func RefusalMessage(err error) (string, bool) {
	var google *googleapi.Error
	if !errors.Is(err, ErrRefused) || !errors.As(err, &google) {
		return "", false
	}
	return google.Message, true
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
