package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/datapointchris/goclilogin"
	"golang.org/x/oauth2"

	"github.com/datapointchris/ypl/cli/internal/api"
	"github.com/datapointchris/ypl/cli/internal/config"
)

// errNeedsLogin is what a command gets where this machine holds no token. It is
// an ordinary state rather than a failure — the tool is installed and nobody
// has logged in yet — so it is kept apart from the errors that mean something
// broke.
var errNeedsLogin = errors.New("not logged in")

// newAPIClient is a client for the configured server, signed in as this
// machine or as the service client YPL_CLIENT_SECRET selects. The oauth2 client
// adds the bearer token to every request and renews it when it expires, so no
// command here holds one.
func newAPIClient(ctx context.Context) (*api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	source, err := tokenSource(ctx, cfg, goclilogin.NewTokenStore)
	if errors.Is(err, errNeedsLogin) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("prepare the API client: %w", err)
	}
	return api.New(cfg.APIBase(), oauth2.NewClient(ctx, source)), nil
}

// tokenSource is the client-credentials grant when YPL_CLIENT_SECRET is set,
// and otherwise the token this machine logged in for, read from tokens.
func tokenSource(ctx context.Context, cfg config.Config, tokens func(goclilogin.Config) *goclilogin.TokenStore) (oauth2.TokenSource, error) {
	if err := cfg.CheckService(); err != nil {
		return nil, err
	}
	if cfg.IsService() {
		return goclilogin.ClientCredentialsTokenSource(ctx, cfg.Service(), cfg.ClientSecret)
	}
	login := cfg.Login()
	source, err := goclilogin.TokenSource(ctx, login, tokens(login))
	if errors.Is(err, goclilogin.ErrNotLoggedIn) {
		return nil, errNeedsLogin
	}
	return source, err
}

// reported is err as the sentence a person reads. A session that has to be
// renewed says so and names the command that renews it, however it arrived:
// from no token at all, from a refresh the provider refused, or from the server
// refusing what was sent.
//
// A joined error is translated a part at a time and joined again. Translating
// the whole of one swaps a sentence in for every part of it, and the parts a
// caller joined on are the ones carrying what it did — an edit joins the file
// its buffer was kept in, and losing that leaves the file on disk with nothing
// naming it.
func reported(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts := joined.Unwrap()
		said := make([]error, len(parts))
		for i, part := range parts {
			said[i] = reported(part)
		}
		return errors.Join(said...)
	}
	if errors.Is(err, errNeedsLogin) {
		return errors.New("not logged in — run `ypl auth login`")
	}
	var refusal *api.Refusal
	// A service has no login to renew, so it is pointed at its id and secret.
	if cfg, loadErr := config.Load(); loadErr == nil && cfg.IsService() {
		switch {
		case goclilogin.IsSessionRejected(err):
			return fmt.Errorf("%s refused service client %s — check YPL_CLIENT_ID and YPL_CLIENT_SECRET", cfg.Issuer(), cfg.ClientID())
		case errors.As(err, &refusal) && refusal.Unauthorized():
			return fmt.Errorf("the server refused the token issued to service client %s", cfg.ClientID())
		}
		return err
	}
	switch {
	// A refused refresh arrives as the transport error of whatever request
	// triggered it, so without this the token endpoint's URL and its raw OAuth
	// description are what reach the terminal.
	case goclilogin.IsSessionRejected(err):
		return errors.New("the session has expired — run `ypl auth login`")
	case errors.As(err, &refusal) && refusal.Unauthorized():
		return errors.New("the server refused the session — run `ypl auth login`")
	}
	return err
}
