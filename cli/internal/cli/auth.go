package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/datapointchris/goclikit"
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
			"Where ypl is pointed comes from one config file. `ypl auth status` prints\n" +
			"its path and its api, issuer and client_id, and `ypl auth login <url>`\n" +
			"fills in whatever it lacks.\n\n" +
			"With YPL_CLIENT_SECRET set, ypl is a service: it authenticates as the\n" +
			"confidential client the config file's client_id names, through the\n" +
			"client-credentials grant, requests a token per run, and stores nothing.\n" +
			"There is no login.",
	}
	goclikit.AsNamespace(cmd)
	splitReadingFromChanging(cmd)
	cmd.AddCommand(withAPIFlag(a.authLoginCommand()), a.authLogoutCommand(), withAPIFlag(a.authStatusCommand()), withAPIFlag(a.authTokenCommand()))
	return cmd
}

func (a *app) authLoginCommand() *cobra.Command {
	var clientID string
	cmd := &cobra.Command{
		Use:     "login [url]",
		GroupID: groupChanging,
		Short:   "Log in by approving a code in any browser",
		Long: "Log in to the ypl server at url, or to the config file's api when url is\n" +
			"omitted. Whatever the file lacks is filled in: api from url, the issuer from\n" +
			"the server's RFC 9728 metadata, and the client id from --client-id or this\n" +
			"machine's short hostname. Each is printed with where it came from before the\n" +
			"login starts, and written to the file once it succeeds. A value already in\n" +
			"the file is never rewritten.",
		Example: "  ypl auth login                        log this machine in, once per machine\n" +
			"  ypl auth login https://ypl.example.com  the same, on a machine with no config file",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if config.IsService() {
				return fmt.Errorf("%s is set, so ypl authenticates as a service client with no login — unset it to log in as a person", config.ClientSecretEnv)
			}
			url := apiOverride
			if len(args) == 1 {
				if apiOverride != "" && apiOverride != args[0] {
					return goclikit.UsageError(fmt.Errorf("--api %s and the url %s name different servers; give one", apiOverride, args[0]))
				}
				url = args[0]
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()

			deployment, err := config.ForLogin(ctx, url, clientID)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Logging in to:")
			goclilogin.WriteDeployment(cmd.ErrOrStderr(), deployment)
			_, _ = fmt.Fprintln(cmd.ErrOrStderr())

			login := config.Login(deployment)
			store := a.tokens(login)

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
			backend, err := store.Save(login.ClientID, token)
			if err != nil {
				return fmt.Errorf("save the token: %w", err)
			}
			if backend == goclilogin.BackendFile {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"\nThis host has no OS keyring, so the token is in %s, readable by this user alone.\n",
					store.FilePath())
			}
			if err := deployment.Save(); err != nil {
				return fmt.Errorf("logged in as %s, but the config file was not written: %w", login.ClientID, err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\nLogged in as %s\n", login.ClientID)
			return nil
		},
	}
	cmd.Flags().StringVar(&clientID, "client-id", "",
		"Log in as this OAuth client when the config file names none (default ypl-cli-<short hostname>)")
	return cmd
}

func (a *app) authLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		GroupID: groupChanging,
		Short:   "Remove the stored token from this machine's keychain",
		Example: "  ypl auth logout  forget the token, before handing the machine on",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if config.IsService() {
				return fmt.Errorf("%s is set, so ypl authenticates as a service client and stores no token to remove — unset it to log out as a person", config.ClientSecretEnv)
			}
			// Only the client id finds the token, so a file lacking the other
			// two keys still logs out. A file naming no client falls back to
			// ypl-cli-<short hostname>, the id login uses when the file names
			// none, so a token stored by a login whose file was never written
			// is still removed. No server is asked, so logout takes no --api.
			deployment, err := config.Load(cmd.Context(), "")
			if err != nil && !errors.Is(err, goclilogin.ErrNotConfigured) {
				return err
			}
			if deployment.ClientID.Value == "" {
				deployment.ClientID.Value = config.DefaultClientID()
			}
			clientID := deployment.ClientID.Value
			err = a.tokens(config.Login(deployment)).Delete(clientID)
			if errors.Is(err, goclilogin.ErrNotLoggedIn) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Not logged in — there is nothing to remove.")
				return nil
			}
			if err != nil {
				return fmt.Errorf("remove the token: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged out (%s).\n", clientID)
			return nil
		},
	}
}

func (a *app) authTokenCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "token",
		GroupID: groupReading,
		Short:   "Print the current access token to stdout",
		Long: "Exits 1 when this machine is not logged in. With YPL_CLIENT_SECRET set, it\n" +
			"requests a new service token instead.",
		Example: "  # the server's status, read with curl rather than ypl\n" +
			"  base=$(ypl config show --json | jq -r .api.value)\n" +
			"  curl -s -H \"Authorization: Bearer $(ypl auth token)\" \"$base/api/v1/status\"",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			deployment, err := config.Load(cmd.Context(), apiOverride)
			if err != nil {
				return err
			}
			source, err := tokenSource(cmd.Context(), deployment, a.tokens)
			if errors.Is(err, errNeedsLogin) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not logged in as %s. Run `ypl auth login`.\n", deployment.ClientID.Value)
				return exitCode(1)
			}
			if err != nil {
				return err
			}
			token, err := source.Token()
			if err != nil && config.IsService() {
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
	LoggedIn bool `json:"logged_in"`
	// Configured is false when the config file lacks a value, which ConfigFile
	// names and the empty fields below show.
	Configured bool                    `json:"configured"`
	ConfigFile string                  `json:"config_file"`
	Type       credentialType          `json:"type"`
	API        string                  `json:"api"`
	ClientID   string                  `json:"client_id"`
	Issuer     string                  `json:"issuer"`
	ExpiresAt  string                  `json:"expires_at,omitempty"`
	Expired    bool                    `json:"expired"`
	Session    goclilogin.SessionState `json:"session"`
	Backend    goclilogin.Backend      `json:"backend,omitempty"`
}

func (a *app) authStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		GroupID: groupReading,
		Short:   "Show where the CLI is pointed and whether it is logged in",
		Long: "Shows the api, issuer and client_id this machine uses, where each came from,\n" +
			"the config file's path, and whether a token is held for the client. Exits 1\n" +
			"when the file lacks a value or no token is held.",
		Example: "  ypl auth status         is this machine logged in, and for how much longer\n" +
			"  ypl auth status --json  the same, for a prompt or a status bar",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			deployment, loadErr := config.Load(cmd.Context(), apiOverride)
			if loadErr != nil && !errors.Is(loadErr, goclilogin.ErrNotConfigured) {
				return loadErr
			}
			status := authStatus{
				Configured: loadErr == nil,
				ConfigFile: deployment.Path,
				Type:       authorizedUser,
				API:        deployment.API.Value,
				ClientID:   deployment.ClientID.Value,
				Issuer:     deployment.Issuer.Value,
			}
			if config.IsService() {
				status.Type = serviceAccount
			}
			switch {
			case !status.Configured:
			case config.IsService():
				serviceStatus(cmd.Context(), deployment, &status)
			default:
				if err := a.loginStatus(deployment, &status); err != nil {
					return err
				}
			}
			switch {
			case asJSON:
				if err := emitJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			case !status.Configured:
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%v\n", loadErr)
				goclilogin.WriteDeployment(cmd.OutOrStdout(), deployment)
			case config.IsService():
				printServiceStatus(cmd.OutOrStdout(), status, deployment)
			default:
				printAuthStatus(cmd.OutOrStdout(), status, deployment)
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
func (a *app) loginStatus(d goclilogin.Deployment, status *authStatus) error {
	token, backend, err := a.tokens(config.Login(d)).Load(d.ClientID.Value)
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
func serviceStatus(ctx context.Context, d goclilogin.Deployment, status *authStatus) {
	source, err := goclilogin.ClientCredentialsTokenSource(ctx, config.Service(d), config.ClientSecret())
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

// The token rows below pad their key to the column goclilogin.WriteDeployment
// starts its values in: its widest key is client_id, padded by two.

func printServiceStatus(out io.Writer, status authStatus, d goclilogin.Deployment) {
	switch status.Session {
	case goclilogin.SessionRejected:
		_, _ = fmt.Fprintf(out, "Service client refused by %s. Check client_id in the config file, and %s.\n", status.Issuer, config.ClientSecretEnv)
	case goclilogin.SessionUnverified:
		_, _ = fmt.Fprintf(out, "Service client configured, but %s could not be reached.\n", status.Issuer)
	default:
		_, _ = fmt.Fprintln(out, "Service client (client credentials)")
	}
	goclilogin.WriteDeployment(out, d)
	if status.ExpiresAt != "" {
		_, _ = fmt.Fprintf(out, "  %-11sgranted, valid until %s\n", "token", status.ExpiresAt)
	}
}

func printAuthStatus(out io.Writer, status authStatus, d goclilogin.Deployment) {
	if !status.LoggedIn {
		_, _ = fmt.Fprintf(out, "Not logged in as %s. Run `ypl auth login` to authenticate.\n", status.ClientID)
	} else {
		_, _ = fmt.Fprintln(out, "Logged in")
	}
	goclilogin.WriteDeployment(out, d)
	if status.ExpiresAt != "" {
		state := "valid"
		if status.Expired {
			state = "expired — will refresh on next use"
		}
		_, _ = fmt.Fprintf(out, "  %-11s%s (expires %s)\n", "token", state, status.ExpiresAt)
	}
	if status.Backend == goclilogin.BackendFile {
		_, _ = fmt.Fprintf(out, "  %-11s%s — no OS keyring on this host\n", "storage", status.Backend)
	}
}
