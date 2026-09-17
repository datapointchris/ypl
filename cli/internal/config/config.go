// Package config is what the ypl CLI has to be told before it can reach a
// server: where that server is, and which identity provider signs the tokens
// the server takes.
//
// Neither is compiled in. Each names one installation, so a value shipped in
// the binary would be a guess at somebody else's, and a guess is worse than
// nothing here — it points the CLI somewhere plausible instead of saying it was
// never told. An unset one resolves as unset, and the commands that need it
// refuse naming the two places it can be set.
//
// A value comes from the environment, from the config file, or from a default,
// in that order. Every one is printed with the layer that set it, because an
// export made in October and a line in the config file read identically from
// outside.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/datapointchris/goclilogin"
)

// keyringService namespaces the CLI's entries in the OS keychain. It is a
// deployed identifier rather than one derived from the module or binary name,
// so it keeps its spelling however those are renamed.
const keyringService = "ypl-cli"

// The key of every setting, spelled as the config file spells it.
const (
	KeyAPIBase  = "api_base"
	KeyIssuer   = "issuer"
	KeyClientID = "client_id"
)

// Layer is where a resolved value came from.
type Layer string

const (
	LayerEnv     Layer = "environment"
	LayerFile    Layer = "config file"
	LayerDefault Layer = "default"
	LayerUnset   Layer = "unset"
)

// Setting is one value the CLI resolves, with the layer that set it and the
// two places it can be set.
type Setting struct {
	Key      string
	Env      string
	Value    string
	Layer    Layer
	Required bool
}

// declaration is one setting before it is resolved: its key, the environment
// variable that outranks the file, where the file holds it, and what it falls
// back to.
type declaration struct {
	key      string
	env      string
	inFile   func(file) string
	fallback func() string
	required bool
}

// declared is every value the CLI resolves. The rows `ypl config show` prints
// are built from it, so a setting added here cannot be one the command leaves
// out — and a row nobody can see missing is the row nobody notices is wrong.
var declared = []declaration{
	{
		key:      KeyAPIBase,
		env:      "YPL_API_BASE",
		inFile:   func(f file) string { return f.APIBase },
		required: true,
	},
	{
		key:      KeyIssuer,
		env:      "YPL_OIDC_ISSUER",
		inFile:   func(f file) string { return f.Issuer },
		required: true,
	},
	{
		key:      KeyClientID,
		env:      "YPL_CLIENT_ID",
		inFile:   func(f file) string { return f.ClientID },
		fallback: func() string { return goclilogin.ClientID("ypl") },
	},
}

// file is the part of the config file this CLI reads. A key it does not name is
// ignored rather than refused, so the settings of anything else sharing the file
// are left alone.
type file struct {
	APIBase  string `toml:"api_base"`
	Issuer   string `toml:"issuer"`
	ClientID string `toml:"client_id"`
}

// Config is every setting resolved, and the file they were resolved against.
type Config struct {
	// Path is the config file, whether or not one is there to read.
	Path string
	// Settings is every value the CLI resolves, in the order they are declared.
	Settings []Setting
}

// Load resolves every setting. A config file that is not there is not a
// failure: the environment may carry everything, and a first run has no file.
// One that cannot be read or parsed is, because it was written to be used.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Path: path}
	var stored file
	switch _, err := toml.DecodeFile(path, &stored); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, d := range declared {
		cfg.Settings = append(cfg.Settings, d.resolve(stored))
	}
	return cfg, nil
}

// resolve is d as one Setting, taking the environment over the file and the
// file over the fallback.
func (d declaration) resolve(stored file) Setting {
	setting := Setting{Key: d.key, Env: d.env, Layer: LayerUnset, Required: d.required}
	switch value := os.Getenv(d.env); {
	case value != "":
		setting.Value, setting.Layer = value, LayerEnv
	case d.inFile(stored) != "":
		setting.Value, setting.Layer = d.inFile(stored), LayerFile
	case d.fallback != nil:
		setting.Value, setting.Layer = d.fallback(), LayerDefault
	}
	return setting
}

// Path is the config file the CLI reads, whether or not it is there.
func Path() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "ypl", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve the config directory: %w", err)
	}
	return filepath.Join(home, ".config", "ypl", "config.toml"), nil
}

// Value is the resolved value of key, and "" for a setting that is unset.
func (c Config) Value(key string) string {
	i := slices.IndexFunc(c.Settings, func(s Setting) bool { return s.Key == key })
	if i < 0 {
		return ""
	}
	return c.Settings[i].Value
}

// APIBase is the ypl server's base URL, without a trailing slash.
func (c Config) APIBase() string { return strings.TrimRight(c.Value(KeyAPIBase), "/") }

// Issuer is the identity provider's base URL.
func (c Config) Issuer() string { return c.Value(KeyIssuer) }

// ClientID is what this machine calls itself to the identity provider.
func (c Config) ClientID() string { return c.Value(KeyClientID) }

// Login is the goclilogin view of this config: which provider to authenticate
// against, as which client, and where this tool's own state lives. Naming the
// state directory after the tool keeps the refresh lock and the fallback token
// file beside its other state, rather than under the keyring service name.
func (c Config) Login() goclilogin.Config {
	return goclilogin.Config{
		Issuer:         c.Issuer(),
		ClientID:       c.ClientID(),
		KeyringService: keyringService,
		StateDir:       goclilogin.StateDir("ypl"),
	}
}

// Missing is every required setting that resolved to nothing.
func (c Config) Missing() []Setting {
	var missing []Setting
	for _, setting := range c.Settings {
		if setting.Required && setting.Value == "" {
			missing = append(missing, setting)
		}
	}
	return missing
}

// ErrNotConfigured is returned where a command needs a setting the CLI was
// never given. It names each one and both places it can be set, since a reader
// holding an empty value cannot see which of the two they forgot.
type ErrNotConfigured struct {
	Missing []Setting
	Path    string
}

func (e *ErrNotConfigured) Error() string {
	named := make([]string, len(e.Missing))
	for i, setting := range e.Missing {
		named[i] = fmt.Sprintf("%s (or %s)", setting.Key, setting.Env)
	}
	return fmt.Sprintf("ypl has not been told %s — set each in %s, or in the environment",
		strings.Join(named, " or "), e.Path)
}

// Check is the refusal for a command that cannot run on this config, and nil
// where every required setting resolved.
func (c Config) Check() error {
	missing := c.Missing()
	if len(missing) == 0 {
		return nil
	}
	return &ErrNotConfigured{Missing: missing, Path: c.Path}
}
