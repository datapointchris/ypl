package cli

import (
	"context"
	"io"

	"github.com/datapointchris/goclilogin"

	"github.com/datapointchris/ypl/cli/internal/api"
)

// app is what the command tree reaches the world through: the ypl server, this
// machine's own stored token, and whether somebody is at the terminal.
//
// Each is a field rather than a call, so a test drives the whole tree against a
// server, a keychain and a terminal of its own. A command tested any other way
// is one that cannot run without a live server and the machine's real
// keychain, which is shared state a test may not write, and one whose question
// no test reaches, because a suite's stdin is never a terminal.
type app struct {
	client   func(context.Context) (*api.Client, error)
	tokens   func(goclilogin.Config) *goclilogin.TokenStore
	terminal func(io.Reader) bool
}
