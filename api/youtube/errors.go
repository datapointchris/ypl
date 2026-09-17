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
var ErrUnexpectedResponse = errors.New("YouTube returned a response this reader does not understand")
