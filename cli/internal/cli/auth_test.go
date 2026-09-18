package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/goclilogin"
	"golang.org/x/oauth2"

	"github.com/datapointchris/ypl/cli/internal/config"
)

// logIn puts a token in the fixture's keychain, as a login would.
func (f *fixture) logIn(expiry time.Time) {
	f.t.Helper()
	login := goclilogin.Config{Issuer: "https://issuer.test", ClientID: "ypl-cli-test", KeyringService: "ypl-cli"}
	if _, err := f.app.tokens(login).Save("ypl-cli-test", &oauth2.Token{AccessToken: "a-token", Expiry: expiry}); err != nil {
		f.t.Fatalf("save a token: %v", err)
	}
}

// A status bar and a shell prompt both run this, so logged out has to be
// readable from the exit code without parsing anything.
func TestAuthStatusExitsOneWhenThisMachineIsNotLoggedIn(t *testing.T) {
	f := newFixture(t, serves(nil))

	got := f.run("auth", "status", "--json")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1: %s%s", got.code, got.out, got.err)
	}
	var status authStatus
	decodeInto(t, got.out, &status)
	switch {
	case status.LoggedIn:
		t.Error("reported logged in with nothing stored")
	case status.ClientID != "ypl-cli-test":
		t.Errorf("client = %q, want ypl-cli-test", status.ClientID)
	case status.Issuer != "https://issuer.test":
		t.Errorf("issuer = %q, want the configured one", status.Issuer)
	}
}

func TestAuthStatusReportsAStoredTokenAndWhetherItHasExpired(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.logIn(time.Now().Add(time.Hour))

	status := asJSON[authStatus](t, f.run("auth", "status", "--json"))
	if !status.LoggedIn || status.Expired || status.Backend == "" {
		t.Fatalf("status = %+v, want logged in with a live token, naming where it is stored", status)
	}
}

// An expired token is not a logged-out machine: the refresh token beside it is
// what the next command spends, so this reports expired and still exits 0.
func TestAuthStatusCallsAnExpiredTokenExpiredAndStaysLoggedIn(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.logIn(time.Now().Add(-time.Hour))

	status := asJSON[authStatus](t, f.run("auth", "status", "--json"))
	if !status.LoggedIn || !status.Expired {
		t.Fatalf("status = %+v, want logged in with an expired token", status)
	}
}

func TestAuthLogoutRemovesTheStoredToken(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.logIn(time.Now().Add(time.Hour))

	if got := f.run("auth", "logout"); got.code != 0 {
		t.Fatalf("logout exited %d: %s%s", got.code, got.out, got.err)
	}
	if got := f.run("auth", "status"); got.code != 1 {
		t.Fatalf("status after logout exited %d, want 1", got.code)
	}
}

// Logging out twice is the same request as logging out once, so the second is
// an ordinary success rather than something to report as broken.
func TestAuthLogoutOfAMachineThatIsNotLoggedInSucceeds(t *testing.T) {
	f := newFixture(t, serves(nil))

	if got := f.run("auth", "logout"); got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
}

func TestAuthTokenExitsOneRatherThanPrintingNothing(t *testing.T) {
	f := newFixture(t, serves(nil))

	got := f.run("auth", "token")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	if strings.TrimSpace(got.out) != "" {
		t.Errorf("wrote %q to stdout, want nothing a caller could mistake for a token", got.out)
	}
}

// The row set is counted against what the config package resolves rather than
// against a number written here, so a setting added there cannot be one this
// command silently stops printing.
func TestConfigShowNamesEveryResolvedSettingAndItsLayer(t *testing.T) {
	f := newFixture(t, serves(nil))
	resolved, err := config.Load()
	if err != nil {
		t.Fatalf("load the config: %v", err)
	}

	got := asJSON[resolvedConfig](t, f.run("config", "show", "--json"))
	if len(got.Settings) != len(resolved.Settings) {
		t.Fatalf("show printed %d rows for %d resolved settings", len(got.Settings), len(resolved.Settings))
	}
	for _, setting := range got.Settings {
		if setting.From != "environment" {
			t.Errorf("%s came from %q, want environment", setting.Key, setting.From)
		}
		if setting.Env == "" {
			t.Errorf("%s names no environment variable, so nothing says how to set it", setting.Key)
		}
	}
}

// A machine that has never been configured is told what to set, on stderr, with
// an exit code that says the command did not do its job.
func TestConfigShowSaysWhatIsMissingAndExitsOne(t *testing.T) {
	f := newFixture(t, serves(nil))
	t.Setenv("YPL_API_BASE", "")
	t.Setenv("YPL_OIDC_ISSUER", "")

	got := f.run("config", "show")
	if got.code != 1 {
		t.Fatalf("exited %d, want 1", got.code)
	}
	for _, want := range []string{"api_base", "YPL_API_BASE", "issuer", "YPL_OIDC_ISSUER"} {
		if !strings.Contains(got.err, want) {
			t.Errorf("the refusal does not name %s: %s", want, got.err)
		}
	}
}

func TestConfigPathPrintsThePathWhetherOrNotAFileIsThere(t *testing.T) {
	f := newFixture(t, serves(nil))

	got := f.run("config", "path")
	if got.code != 0 {
		t.Fatalf("exited %d: %s", got.code, got.err)
	}
	if !strings.HasSuffix(strings.TrimSpace(got.out), "/ypl/config.toml") {
		t.Fatalf("path = %q, want the ypl config file", got.out)
	}
}
