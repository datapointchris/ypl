// Package config says where the ypl CLI is pointed and how it authenticates
// there. Where it is pointed is one file, $XDG_CONFIG_HOME/ypl/config.toml,
// read through goclilogin's config-file contract: its api, issuer and
// client_id keys, with --api the only thing that outranks them.
//
// None of the three is compiled in. Each names one installation, so a value
// shipped in the binary would point somebody else's copy at this one.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/datapointchris/goclilogin"
)

// product names the config file's directory and the state directory.
const product = "ypl"

// keyringService namespaces the CLI's entries in the OS keychain. It is a
// deployed identifier rather than one derived from the module or binary name,
// so it keeps its spelling however those are renamed.
const keyringService = "ypl-cli"

// serviceScope is what a service client asks for. The provider grants only the
// scopes a request names, and the server admits this one to what `ypl server
// status` reads.
const serviceScope = "ypl.status.read"

// ClientSecretEnv selects the client-credentials grant, for the client the
// config file names. It is read from the environment alone, so it is never
// written to the file and no command prints it.
const ClientSecretEnv = "YPL_CLIENT_SECRET"

// Load reads the deployment every command talks to. api is the --api flag's
// value, and outranks the file's when it is not empty.
//
// A service missing a value is told which key to write rather than to log in,
// since a service has no login.
func Load(ctx context.Context, api string) (goclilogin.Deployment, error) {
	d, err := goclilogin.LoadDeployment(ctx, product, api)
	if d.HandsTokens() && IsService() {
		return d, fmt.Errorf("%s sets token_command and %s is set, so ypl has two credentials; keep one",
			d.Path, ClientSecretEnv)
	}
	if !IsService() || !errors.Is(err, goclilogin.ErrNotConfigured) {
		return d, err
	}
	var missing []string
	for _, key := range []struct {
		name string
		set  goclilogin.Setting
	}{{"api", d.API}, {"issuer", d.Issuer}, {"client_id", d.ClientID}} {
		if key.set.Value == "" {
			missing = append(missing, key.name)
		}
	}
	return d, fmt.Errorf("%w: %s sets no %s; a service names its server, provider and client there, and only %s in its environment",
		goclilogin.ErrNotConfigured, d.Path, strings.Join(missing, " or "), ClientSecretEnv)
}

// ForLogin is the deployment `ypl auth login` logs in to: the file's values,
// with whatever it lacks filled in from api, the API's metadata and clientID.
func ForLogin(ctx context.Context, api, clientID string) (goclilogin.Deployment, error) {
	return goclilogin.DeploymentForLogin(ctx, product, api, clientID)
}

// Path is the config file, whether or not one is there, and "" where no home
// directory resolves.
func Path() string { return goclilogin.ConfigPath(product) }

// DefaultClientID is the client id login derives when neither the file nor
// --client-id names one: ypl-cli-<short hostname>.
func DefaultClientID() string { return goclilogin.ClientID(product) }

// Login is the goclilogin view of a deployment: which provider to authenticate
// against, as which client, and where this tool's own state lives. Naming the
// state directory after the tool keeps the refresh lock and the fallback token
// file beside its other state, rather than under the keyring service name.
func Login(d goclilogin.Deployment) goclilogin.Config {
	return goclilogin.Config{
		Issuer:         d.Issuer.Value,
		ClientID:       d.ClientID.Value,
		KeyringService: keyringService,
		StateDir:       goclilogin.StateDir(product),
	}
}

// ClientSecret is YPL_CLIENT_SECRET, set only where a service runs ypl with
// nobody present to approve a device login.
func ClientSecret() string { return os.Getenv(ClientSecretEnv) }

// IsService reports whether ypl authenticates as a confidential service client
// rather than as the person who logged this machine in.
func IsService() bool { return ClientSecret() != "" }

// Service is the goclilogin view of a deployment for the client-credentials
// grant. Nothing it obtains is stored, so it names no keyring or state
// directory.
func Service(d goclilogin.Deployment) goclilogin.ServiceClient {
	return goclilogin.ServiceClient{Issuer: d.Issuer.Value, ClientID: d.ClientID.Value, Scopes: []string{serviceScope}}
}
