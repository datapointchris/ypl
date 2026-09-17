package youtube

import "errors"

// ErrMissingCredentials is the refusal for an environment that lacks a
// credential variable the caller needs.
var ErrMissingCredentials = errors.New("missing YouTube credentials")

// ErrIncompleteGrant is the refusal for a device grant that carries no refresh
// token, or does not grant Scope.
var ErrIncompleteGrant = errors.New("the grant does not give this service what it needs")

// ErrQuotaSpent is YouTube refusing a request because the Cloud project's daily
// quota is spent. The quota resets at midnight Pacific.
var ErrQuotaSpent = errors.New("YouTube reports the project's daily quota spent")

// ErrInconsistentRead is the refusal for a paged read whose pages disagree: they
// report different totals, an id appears twice, or the items do not match the
// total or their positions. That is what an edit between two page requests
// leaves behind. It is never a shorter list, so a caller must not take what is
// missing as removed.
var ErrInconsistentRead = errors.New("a paged read changed while it was read")

// ErrUnexpectedResponse is the refusal for a response that lacks a part the
// request named, or carries a value this package does not know.
var ErrUnexpectedResponse = errors.New("YouTube returned a response this package does not understand")

// ErrPlaylistNotFound is YouTube reporting that no playlist has the id a list,
// an insert or a delete named. It answers this for a playlist already deleted.
var ErrPlaylistNotFound = errors.New("YouTube has no playlist with that id")

// ErrItemNotFound is YouTube reporting that the playlist item a delete or a move
// named is gone. A delete answers it for an item already deleted and for an item
// of a deleted playlist, and a move answers it for an item already deleted.
var ErrItemNotFound = errors.New("YouTube has no playlist item with that id")

// ErrVideoNotFound is YouTube refusing to add a video it has no record of. It
// answers this for a deleted video and for an id no video has.
var ErrVideoNotFound = errors.New("YouTube has no video with that id")

// ErrVideoRefused is YouTube refusing to add a video it has. It answers this for
// a private video another channel owns.
var ErrVideoRefused = errors.New("YouTube refuses to add that video to a playlist")

// ErrManualSortRequired is YouTube refusing a write that names a position in a
// playlist not sorted manually. The Data API reference documents it for inserts
// and moves; no request here has drawn it. An append names no position.
var ErrManualSortRequired = errors.New("the playlist is not sorted manually, so a write cannot name a position in it")
