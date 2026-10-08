package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/datapointchris/goclilogin"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/datapointchris/ypl/cli/internal/config"
)

// loginTimeout backs up the device code's own expiry, which the poll stops at
// first. It is here so a login left open overnight ends rather than holding the
// terminal.
const loginTimeout = 15 * time.Minute

func (a *app) authCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Log this machine in to the server",
		GroupID: groupSetup,
		Long: "Logging in prints a code and a link. Approve it in a browser on any device,\n" +
			"so it works over SSH too.\n\n" +
			"With YPL_CLIENT_SECRET set, ypl is a service: it authenticates as the\n" +
			"confidential client YPL_CLIENT_ID names through the client-credentials\n" +
			"grant, requests a token per run, and stores nothing. There is no login.",
		RunE: requireSubcommand,
	}
	splitReadingFromChanging(cmd)
	cmd.AddCommand(a.authLoginCommand(), a.authLogoutCommand(), a.authStatusCommand(), a.authTokenCommand())
	return cmd
}

// loginConfig is the resolved login settings, refusing first where the CLI has
// not been told which provider to authenticate against.
func loginConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	return cfg, cfg.Check()
}

func (a *app) authLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "login",
		GroupID: groupChanging,
		Short:   "Log in by approving a code in a browser",
		Example: "  ypl auth login  log this machine in, once per machine",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			if err := cfg.CheckService(); err != nil {
				return err
			}
			if cfg.IsService() {
				return fmt.Errorf("YPL_CLIENT_SECRET is set, so ypl authenticates as service client %s with no login — unset it to log in as a person", cfg.ClientID())
			}
			login := cfg.Login()
			store := a.tokens(login)

			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()

			// The browser launcher inherits these streams, so whatever the
			// browser writes on startup — a GPU warning, a dbus complaint —
			// lands in the middle of the code and the URL being read off the
			// screen. A launcher that fails is reported by OpenURL's error
			// instead, so nothing diagnostic is lost by dropping the stream.
			//
			// It has to be an *os.File and not io.Discard. os/exec passes a
			// file through as a descriptor and gives anything else a pipe plus
			// a copying goroutine, and then Wait blocks until every process
			// holding the write end exits — which is the browser xdg-open
			// spawned. That leaves OpenURL blocked for as long as the browser
			// is open, and goclilogin calls it before the device-code poll
			// starts, so nothing is polling while the screen says it is.
			if quiet, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
				defer func() { _ = quiet.Close() }()
				browser.Stdout = quiet
				browser.Stderr = quiet
			}

			token, err := goclilogin.Login(ctx, login, func(prompt goclilogin.DevicePrompt) {
				goclilogin.WriteInstructions(cmd.ErrOrStderr(), login.ClientID, prompt)
				if openErr := browser.OpenURL(prompt.BrowserURL()); openErr != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "(no browser could be opened here: %v)\n", openErr)
				}
			})
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("nothing approved the code within %s — run `ypl auth login` again", loginTimeout)
				}
				return err
			}
			backend, err := store.Save(cfg.ClientID(), token)
			if err != nil {
				return fmt.Errorf("save the token: %w", err)
			}
			if backend == goclilogin.BackendFile {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"\nThis host has no OS keyring, so the token is in %s, readable by this user alone.\n",
					store.FilePath())
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\nLogged in as %s\n", cfg.ClientID())
			return nil
		},
	}
	return cmd
}

func (a *app) authLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		GroupID: groupChanging,
		Short:   "Log out, forgetting this machine's token",
		Example: "  ypl auth logout  forget the token, before handing the machine on",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			if err := cfg.CheckService(); err != nil {
				return err
			}
			if cfg.IsService() {
				return fmt.Errorf("YPL_CLIENT_SECRET is set, so ypl authenticates as service client %s and stores no token to remove — unset it to log out as a person", cfg.ClientID())
			}
			err = a.tokens(cfg.Login()).Delete(cfg.ClientID())
			if errors.Is(err, goclilogin.ErrNotLoggedIn) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Not logged in — there is nothing to remove.")
				return nil
			}
			if err != nil {
				return fmt.Errorf("remove the token: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged out (%s).\n", cfg.ClientID())
			return nil
		},
	}
}

func (a *app) authTokenCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "token",
		GroupID: groupReading,
		Short:   "Print an access token for calling the API directly",
		Long: "Exits 1 when this machine is not logged in. With YPL_CLIENT_SECRET set, it\n" +
			"requests a new service token instead.",
		Example: "  # the server's status, read with curl rather than ypl\n" +
			"  base=$(ypl config show --json |\n" +
			"    jq -r '.settings[] | select(.key == \"api_base\").value')\n" +
			"  curl -s -H \"Authorization: Bearer $(ypl auth token)\" \"$base/api/v1/status\"",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			source, err := tokenSource(cmd.Context(), cfg, a.tokens)
			if errors.Is(err, errNeedsLogin) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not logged in as %s. Run `ypl auth login`.\n", cfg.ClientID())
				return exitCode(1)
			}
			if err != nil {
				return err
			}
			token, err := source.Token()
			if err != nil && cfg.IsService() {
				return reported(err)
			}
			if err != nil {
				return fmt.Errorf("get a valid token, which may mean the refresh failed — try `ypl auth login`: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), token.AccessToken)
			return nil
		},
	}
}

// credentialType is whose credential `ypl auth status` checked, spelled as
// Google's credential files spell a person's OAuth login and a service's.
type credentialType string

const (
	// authorizedUser is the device-grant login a person made on this machine.
	authorizedUser credentialType = "authorized_user"
	// serviceAccount is the client-credentials grant YPL_CLIENT_SECRET selects.
	serviceAccount credentialType = "service_account"
)

// authStatus is what `ypl auth status --json` writes. The token is opaque to
// this CLI — the server is what reads its claims — so there is no identity to
// report, only whether one is held and whether it has expired.
//
// For a service, logged_in is true only when the provider granted a token just
// now. A service stores none, so nothing else says the next command will work.
// session says why it is false: rejected is a secret to rotate, unverified a
// provider to retry. A person's status never asks the provider, so its session
// is empty.
type authStatus struct {
	LoggedIn  bool                    `json:"logged_in"`
	Type      credentialType          `json:"type"`
	ClientID  string                  `json:"client_id"`
	Issuer    string                  `json:"issuer"`
	ExpiresAt string                  `json:"expires_at,omitempty"`
	Expired   bool                    `json:"expired"`
	Session   goclilogin.SessionState `json:"session"`
	Backend   goclilogin.Backend      `json:"backend,omitempty"`
}

func (a *app) authStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		GroupID: groupReading,
		Short:   "Show whether this machine is logged in",
		Example: "  ypl auth status         is this machine logged in, and for how much longer\n" +
			"  ypl auth status --json  the same, for a prompt or a status bar",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			if err := cfg.CheckService(); err != nil {
				return err
			}
			status := authStatus{Type: authorizedUser, ClientID: cfg.ClientID(), Issuer: cfg.Issuer()}
			if cfg.IsService() {
				status.Type = serviceAccount
				serviceStatus(cmd.Context(), cfg, &status)
			} else if err := a.loginStatus(cfg, &status); err != nil {
				return err
			}
			switch {
			case asJSON:
				if err := emitJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			case cfg.IsService():
				printServiceStatus(cmd.OutOrStdout(), status)
			default:
				printAuthStatus(cmd.OutOrStdout(), status)
			}
			if !status.LoggedIn {
				return exitCode(1)
			}
			return nil
		},
	}
	addJSON(cmd, &asJSON, "the status")
	return cmd
}

// loginStatus fills status from the token this machine logged in for.
func (a *app) loginStatus(cfg config.Config, status *authStatus) error {
	token, backend, err := a.tokens(cfg.Login()).Load(cfg.ClientID())
	switch {
	case errors.Is(err, goclilogin.ErrNotLoggedIn):
		// Logged out is a state this command reports, not a failure.
	case err != nil:
		return fmt.Errorf("read the stored token: %w", err)
	default:
		status.LoggedIn = true
		// The backend is a fact about where the token was stored, not about
		// whether it expires. A provider may omit expires_in — RFC 6749 only
		// recommends it — and on a host with no keyring this is the one line
		// saying the token is in a plain file.
		status.Backend = backend
		if !token.Expiry.IsZero() {
			status.ExpiresAt = token.Expiry.Format(time.RFC3339)
			status.Expired = time.Now().After(token.Expiry)
		}
	}
	return nil
}

// serviceStatus requests a token, because a service stores none that could say
// whether its credentials still work, and reports what the provider answered.
func serviceStatus(ctx context.Context, cfg config.Config, status *authStatus) {
	source, err := goclilogin.ClientCredentialsTokenSource(ctx, cfg.Service(), cfg.ClientSecret)
	var token *oauth2.Token
	if err == nil {
		token, err = source.Token()
	}
	status.Session, token = goclilogin.ClassifySession(token, err)
	status.LoggedIn = status.Session == goclilogin.SessionLive
	if token != nil && !token.Expiry.IsZero() {
		status.ExpiresAt = token.Expiry.Format(time.RFC3339)
	}
}

func printServiceStatus(out io.Writer, status authStatus) {
	switch status.Session {
	case goclilogin.SessionRejected:
		_, _ = fmt.Fprintf(out, "Service client refused by %s.\nCheck YPL_CLIENT_ID and YPL_CLIENT_SECRET.\n", status.Issuer)
	case goclilogin.SessionUnverified:
		_, _ = fmt.Fprintf(out, "Service client configured, but %s could not be reached.\n", status.Issuer)
	default:
		_, _ = fmt.Fprintln(out, "Service client (client credentials)")
	}
	_, _ = fmt.Fprintf(out, "  client   %s\n", status.ClientID)
	_, _ = fmt.Fprintf(out, "  issuer   %s\n", status.Issuer)
	if status.ExpiresAt != "" {
		_, _ = fmt.Fprintf(out, "  token    granted, valid until %s\n", status.ExpiresAt)
	}
}

func printAuthStatus(out io.Writer, status authStatus) {
	if !status.LoggedIn {
		_, _ = fmt.Fprintf(out, "Not logged in as %s.\nRun `ypl auth login` to authenticate.\n", status.ClientID)
		return
	}
	_, _ = fmt.Fprintln(out, "Logged in")
	_, _ = fmt.Fprintf(out, "  client   %s\n", status.ClientID)
	_, _ = fmt.Fprintf(out, "  issuer   %s\n", status.Issuer)
	// Two sentences rather than one with a swapped clause: "expired … until"
	// reads as valid-until, which is the opposite of what it says.
	switch {
	case status.ExpiresAt != "" && status.Expired:
		_, _ = fmt.Fprintf(out, "  token    expired at %s, and is refreshed on the next command\n", status.ExpiresAt)
	case status.ExpiresAt != "":
		_, _ = fmt.Fprintf(out, "  token    valid until %s\n", status.ExpiresAt)
	}
	if status.Backend == goclilogin.BackendFile {
		_, _ = fmt.Fprintf(out, "  stored   in a %s, since this host has no OS keyring\n", status.Backend)
	}
}
