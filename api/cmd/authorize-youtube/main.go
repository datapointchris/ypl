// Command authorize-youtube asks the channel's owner to grant this service
// youtube.Scope through Google's device flow, and prints the refresh token the
// grant issues as JSON on stdout.
//
//	YOUTUBE_CLIENT_ID=… YOUTUBE_CLIENT_SECRET=… go run ./cmd/authorize-youtube > token.json
//
// It prints the page to open and the code to enter there on stderr, then waits
// for the grant. Choose the channel's own account in the account chooser. The
// token is YOUTUBE_REFRESH_TOKEN. It exits 0 once the grant arrives and on -h, 2
// on a usage error, and 1 otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/datapointchris/ypl/api/youtube"
)

// ErrUsage marks a mistake in how the command was invoked, as distinct from an
// authorization that ran and failed.
var ErrUsage = errors.New("usage")

// authorizer runs the device flow for a client, as youtube.Authorize does.
type authorizer func(ctx context.Context, client youtube.Client, show func(verificationURL, userCode string) error) (string, error)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, youtube.Authorize)
	stop()
	switch {
	case err == nil:
	case errors.Is(err, ErrUsage):
		slog.Error("authorize youtube", "err", err)
		os.Exit(2)
	default:
		slog.Error("authorize youtube", "err", err)
		os.Exit(1)
	}
}

// run authorizes as args describe. The prompt, flag usage and parse errors go to
// stderr.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, authorize authorizer) error {
	flags := flag.NewFlagSet("authorize-youtube", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: authorize-youtube takes no arguments, and was given %q", ErrUsage, flags.Args())
	}

	client, err := youtube.ClientFromEnv()
	if err != nil {
		return err
	}
	refreshToken, err := authorize(ctx, client, func(verificationURL, userCode string) error {
		_, err := fmt.Fprintf(stderr, "Open %s, enter %s, and choose the channel's own account.\n", verificationURL, userCode)
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"refresh_token": refreshToken})
}
