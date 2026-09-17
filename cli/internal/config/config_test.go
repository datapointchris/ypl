package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFile points the config at a directory of its own holding contents, and
// clears every override so a test starts from the layer it means to test.
func withFile(t *testing.T, contents string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, d := range declared {
		t.Setenv(d.env, "")
	}
	if contents == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "ypl"), 0o700); err != nil {
		t.Fatalf("make the config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ypl", "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}
}

func load(t *testing.T) Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// A first run has no config file, and the environment may carry everything, so
// an absent file is a layer with nothing in it rather than a failure.
func TestNoConfigFileIsNotAFailure(t *testing.T) {
	withFile(t, "")

	cfg := load(t)
	if len(cfg.Settings) != len(declared) {
		t.Fatalf("resolved %d settings, want %d", len(cfg.Settings), len(declared))
	}
	if cfg.APIBase() != "" {
		t.Errorf("api_base = %q with nothing to read it from", cfg.APIBase())
	}
}

// A file that cannot be parsed was written to be used, so reading past it would
// run the CLI against a config nobody wrote.
func TestAConfigFileThatCannotBeParsedIsAFailure(t *testing.T) {
	withFile(t, "api_base = \n")

	if _, err := Load(); err == nil {
		t.Fatal("a malformed config file loaded without complaint")
	}
}

func TestTheEnvironmentOutranksTheFileAndTheFileOutranksTheDefault(t *testing.T) {
	withFile(t, "api_base = \"https://from-file.test\"\nclient_id = \"from-file\"\n")
	t.Setenv("YPL_API_BASE", "https://from-env.test")

	cfg := load(t)
	for _, c := range []struct {
		key   string
		value string
		layer Layer
	}{
		{KeyAPIBase, "https://from-env.test", LayerEnv},
		{KeyClientID, "from-file", LayerFile},
		{KeyIssuer, "", LayerUnset},
	} {
		setting := settingOf(t, cfg, c.key)
		if setting.Value != c.value || setting.Layer != c.layer {
			t.Errorf("%s = %q from %q, want %q from %q", c.key, setting.Value, setting.Layer, c.value, c.layer)
		}
	}
}

// Nothing overrides the client id, so it falls back to a name derived from this
// machine rather than being left for someone to set by hand on every host.
func TestTheClientIDFallsBackToOneNamingThisMachine(t *testing.T) {
	withFile(t, "")

	setting := settingOf(t, load(t), KeyClientID)
	if setting.Layer != LayerDefault || !strings.HasPrefix(setting.Value, "ypl-cli") {
		t.Fatalf("client_id = %q from %q, want a ypl-cli name by default", setting.Value, setting.Layer)
	}
}

// The Python tool keeps its own settings in this file until it is retired, and
// a key this CLI does not know is that tool's rather than a mistake.
func TestAKeyThisCLIDoesNotReadIsLeftAlone(t *testing.T) {
	withFile(t, "api_base = \"https://a.test\"\ncookies_from_browser = \"firefox\"\nenrich_batch_size = 50\n")

	if got := load(t).APIBase(); got != "https://a.test" {
		t.Fatalf("api_base = %q beside keys this CLI does not read", got)
	}
}

// A path is joined onto the base once, so a base written with a trailing slash
// does not produce a request to //api/v1/....
func TestATrailingSlashOnTheServerIsDropped(t *testing.T) {
	withFile(t, "")
	t.Setenv("YPL_API_BASE", "https://a.test/")

	if got := load(t).APIBase(); got != "https://a.test" {
		t.Fatalf("api_base = %q, want the slash dropped", got)
	}
}

// A reader holding an empty value cannot see which of the two places they
// forgot, so the refusal names each setting and both. It names commands rather
// than a path, since a path is something to reconstruct and a command is
// something to run.
func TestTheRefusalNamesEveryMissingSettingAndTheCommandsThatFixIt(t *testing.T) {
	withFile(t, "")

	err := load(t).Check()
	if err == nil {
		t.Fatal("a config with no server and no issuer passed its own check")
	}
	for _, want := range []string{
		KeyAPIBase, "YPL_API_BASE", KeyIssuer, "YPL_OIDC_ISSUER",
		"ypl config example", "ypl config path",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %s", want, err)
		}
	}
}

// The example is generated from the same declarations Load resolves, so a
// setting cannot be added without appearing in the file someone copies.
func TestTheExampleNamesEverySettingAndItsEnvironmentVariable(t *testing.T) {
	written := Example()
	for _, d := range declared {
		if !strings.Contains(written, d.key) {
			t.Errorf("the example does not name %s", d.key)
		}
		if !strings.Contains(written, d.env) {
			t.Errorf("the example does not name %s", d.env)
		}
	}
}

// The client id has a default, so it is never what stops a command running.
func TestAConfigWithAServerAndAnIssuerPassesItsOwnCheck(t *testing.T) {
	withFile(t, "api_base = \"https://a.test\"\nissuer = \"https://b.test\"\n")

	if err := load(t).Check(); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func settingOf(t *testing.T, cfg Config, key string) Setting {
	t.Helper()
	for _, setting := range cfg.Settings {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("no setting named %s among %+v", key, cfg.Settings)
	return Setting{}
}
