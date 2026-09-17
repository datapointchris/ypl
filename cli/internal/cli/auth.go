package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/datapointchris/goclilogin"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/datapointchris/ypl/cli/internal/config"
)

// loginTimeout backs up the device code's own expiry, which the poll stops at
// first. It is here so a login left open overnight ends rather than holding the
// terminal.
const loginTimeout = 15 * time.Minute

func (a *app) authCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Log this machine in and out of the server",
		GroupID: groupSetup,
		Long: "Authenticate with the identity provider using the OAuth 2.0 device\n" +
			"authorization grant. The CLI prints a code and a URL; approving it in a\n" +
			"browser on any device logs this machine in, which is what makes this work\n" +
			"over SSH on a machine with no browser of its own.",
		RunE: requireSubcommand,
	}
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
		Short:   "Log in by approving a code in a browser",
		Example: "  ypl auth login",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
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
			browser.Stdout = io.Discard
			browser.Stderr = io.Discard

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
		Short:   "Remove this machine's stored token",
		Example: "  ypl auth logout",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
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
		Use:   "token",
		Short: "Print a valid access token to stdout",
		Long: "Print an access token, refreshing it first if it has expired. This is for\n" +
			"driving the API with something else: curl -H \"Authorization: Bearer $(ypl\n" +
			"auth token)\". It exits non-zero rather than printing nothing when this\n" +
			"machine is not logged in.",
		Example: "  ypl auth token",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			login := cfg.Login()
			source, err := goclilogin.TokenSource(cmd.Context(), login, a.tokens(login))
			if errors.Is(err, goclilogin.ErrNotLoggedIn) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not logged in as %s. Run `ypl auth login`.\n", cfg.ClientID())
				return exitCode(1)
			}
			if err != nil {
				return err
			}
			token, err := source.Token()
			if err != nil {
				return fmt.Errorf("get a valid token, which may mean the refresh failed — try `ypl auth login`: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), token.AccessToken)
			return nil
		},
	}
}

// authStatus is what `ypl auth status --json` writes. The token is opaque to
// this CLI — the server is what reads its claims — so there is no identity to
// report, only whether one is held and whether it has expired.
type authStatus struct {
	LoggedIn  bool               `json:"logged_in"`
	ClientID  string             `json:"client_id"`
	Issuer    string             `json:"issuer"`
	ExpiresAt string             `json:"expires_at,omitempty"`
	Expired   bool               `json:"expired"`
	Backend   goclilogin.Backend `json:"backend,omitempty"`
}

func (a *app) authStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Say whether this machine is logged in",
		Example: "  ypl auth status\n  ypl auth status --json",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loginConfig()
			if err != nil {
				return err
			}
			status := authStatus{ClientID: cfg.ClientID(), Issuer: cfg.Issuer()}
			token, backend, err := a.tokens(cfg.Login()).Load(cfg.ClientID())
			switch {
			case errors.Is(err, goclilogin.ErrNotLoggedIn):
				// Logged out is a state this command reports, not a failure.
			case err != nil:
				return fmt.Errorf("read the stored token: %w", err)
			default:
				status.LoggedIn = true
				if !token.Expiry.IsZero() {
					status.ExpiresAt = token.Expiry.Format(time.RFC3339)
					status.Expired = time.Now().After(token.Expiry)
					status.Backend = backend
				}
			}
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			} else {
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

func printAuthStatus(out io.Writer, status authStatus) {
	if !status.LoggedIn {
		_, _ = fmt.Fprintf(out, "Not logged in as %s.\nRun `ypl auth login` to authenticate.\n", status.ClientID)
		return
	}
	_, _ = fmt.Fprintln(out, "Logged in")
	_, _ = fmt.Fprintf(out, "  client   %s\n", status.ClientID)
	_, _ = fmt.Fprintf(out, "  issuer   %s\n", status.Issuer)
	if status.ExpiresAt != "" {
		state := "valid"
		if status.Expired {
			state = "expired, and refreshed on the next command"
		}
		_, _ = fmt.Fprintf(out, "  token    %s until %s\n", state, status.ExpiresAt)
	}
	if status.Backend == goclilogin.BackendFile {
		_, _ = fmt.Fprintf(out, "  stored   in a %s, since this host has no OS keyring\n", status.Backend)
	}
}
