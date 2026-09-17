package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Connecting is a TokenVerifier that has not yet read the identity provider.
// Until Run has built its Verifier it refuses every token as
// ErrProviderUnavailable. A provider that is down when the service starts then
// holds back only the requests that need a token, and the rest of the process
// runs.
type Connecting struct {
	issuer, clientIDPrefix string
	firstRetry, lastRetry  time.Duration

	verifier atomic.Pointer[Verifier]
}

// NewConnecting is a Connecting for the provider issuer and the clients whose
// ids start with clientIDPrefix.
func NewConnecting(issuer, clientIDPrefix string) *Connecting {
	return &Connecting{issuer: issuer, clientIDPrefix: clientIDPrefix, firstRetry: time.Second, lastRetry: time.Minute}
}

// Run builds the Verifier, retrying after each failure to reach the provider
// with a wait that doubles from a second to a minute, until it succeeds or ctx
// ends. A provider that answers with something no retry changes, such as a
// discovery document naming another issuer, stops it: the service then refuses
// every token until it is configured again.
func (c *Connecting) Run(ctx context.Context, log *slog.Logger) {
	wait := c.firstRetry
	for {
		verifier, err := NewVerifier(ctx, c.issuer, c.clientIDPrefix)
		switch {
		case err == nil:
			c.verifier.Store(verifier)
			log.InfoContext(ctx, "identity provider read", "issuer", c.issuer)
			return
		case ctx.Err() != nil:
			return
		case !errors.Is(err, ErrProviderUnavailable):
			log.ErrorContext(ctx, "identity provider misconfigured", "issuer", c.issuer, "err", err)
			return
		}
		log.WarnContext(ctx, "identity provider not read", "issuer", c.issuer, "err", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, c.lastRetry)
	}
}

// Ready reports whether Run has read the provider, so a token can be verified.
func (c *Connecting) Ready() bool {
	return c.verifier.Load() != nil
}

// Verify verifies raw with the Verifier Run built, and refuses it as
// ErrProviderUnavailable before there is one.
func (c *Connecting) Verify(ctx context.Context, raw string) (Identity, error) {
	verifier := c.verifier.Load()
	if verifier == nil {
		return Identity{}, fmt.Errorf("%w: its discovery document and keys are not read yet", ErrProviderUnavailable)
	}
	return verifier.Verify(ctx, raw)
}
