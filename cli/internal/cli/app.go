package cli

import (
	"context"

	"github.com/datapointchris/goclilogin"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// app is what the command tree reaches the world through: the ypl server, and
// this machine's own stored token.
//
// Both are fields rather than calls, so a test drives the whole tree against a
// server and a keychain of its own. A command tested any other way is one that
// cannot run without a live server and the machine's real keychain, which is
// shared state a test may not write.
type app struct {
	client func(context.Context) (*api.Client, error)
	tokens func(goclilogin.Config) *goclilogin.TokenStore
}
