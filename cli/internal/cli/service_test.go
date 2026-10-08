package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/datapointchris/ypl/cli/internal/config"
)

const (
	serviceClientID = "ypl-svc-worker"
	serviceSecret   = "service-secret"
	serviceToken    = "service-access-token"
	statusBody      = `{"library": {}, "last_run": null, "last_ok_run": null}`
)

// freshStatus is a status whose last ok sync finished a minute ago, which
// `ypl server status` exits 0 on.
func freshStatus() string {
	ok := strings.ReplaceAll(run(1, "ok"), "2026-09-01T00:01:00Z", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	return `{"library": {}, "last_run": ` + ok + `, "last_ok_run": ` + ok + `}`
}

// serviceIDP grants a token only to serviceClientID presenting serviceSecret in
// a Basic header and naming exactly the scope ypl asks for. Anything else gets
// the refusal the provider sends.
func serviceIDP(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "token_endpoint": srv.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, secret, ok := r.BasicAuth()
		if !ok || id != serviceClientID || secret != serviceSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		if r.FormValue("grant_type") != "client_credentials" || r.FormValue("scope") != "ypl.status.read" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_scope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + serviceToken + `","token_type":"bearer","expires_in":300}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// asService is f run as serviceClientID holding secret against issuer, with
// the real API client, so the grant the tree picks is the one that runs.
func (f *fixture) asService(issuer, secret string) {
	f.t.Helper()
	f.t.Setenv("YPL_OIDC_ISSUER", issuer)
	f.t.Setenv("YPL_CLIENT_ID", serviceClientID)
	f.t.Setenv("YPL_CLIENT_SECRET", secret)
	f.app.client = newAPIClient
}

func TestAServiceReadsTheStatusWithTheClientCredentialsToken(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/status": freshStatus()}))
	f.asService(serviceIDP(t).URL, serviceSecret)

	got := f.run("server", "status", "--json")
	if got.code != 0 {
		t.Fatalf("exited %d: %s%s", got.code, got.out, got.err)
	}
	if authorization := f.sent[len(f.sent)-1].Header.Get("Authorization"); authorization != "Bearer "+serviceToken {
		t.Errorf("the server saw Authorization %q, want the service token", authorization)
	}
	entries, _ := os.ReadDir(os.Getenv("XDG_STATE_HOME"))
	if len(entries) != 0 {
		t.Errorf("a service run wrote %v under its state directory, want nothing stored", entries)
	}
}

func TestARefusedSecretNamesTheSecretRatherThanALogin(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/status": statusBody}))
	f.asService(serviceIDP(t).URL, "wrong-secret")

	got := f.run("server", "status")
	if got.code != 1 || !strings.Contains(got.err, "YPL_CLIENT_SECRET") || strings.Contains(got.err, "auth login") {
		t.Errorf("exited %d with %q, want 1 and the secret named", got.code, got.err)
	}
	if len(f.sent) != 0 {
		t.Errorf("the server was asked %d times with no token granted", len(f.sent))
	}
}

func TestAServicesStatusAsksTheProvider(t *testing.T) {
	idp := serviceIDP(t)
	f := newFixture(t, serves(nil))

	f.asService(idp.URL, serviceSecret)
	got := f.run("auth", "status", "--json")
	var status authStatus
	decodeInto(t, got.out, &status)
	if got.code != 0 || !status.LoggedIn || keyIn(t, got.out, "type") != "service_account" || status.ExpiresAt == "" || keyIn(t, got.out, "session") != "live" {
		t.Errorf("status with a good secret: exit %d, %s, want logged in as a service until an expiry, session live", got.code, got.out)
	}

	f.asService(idp.URL, "wrong-secret")
	got = f.run("auth", "status")
	if got.code != 1 || !strings.Contains(got.out, "YPL_CLIENT_SECRET") || strings.Contains(got.out, "auth login") {
		t.Errorf("status with a refused secret: exit %d, %q, want 1 and the secret named", got.code, got.out)
	}
	got = f.run("auth", "status", "--json")
	if got.code != 1 || keyIn(t, got.out, "session") != "rejected" {
		t.Errorf("status --json with a refused secret: exit %d, %s, want 1 and session rejected", got.code, got.out)
	}
}

// A service stores no token, so with the provider down its next command fails
// too. A job gating on status has to see that, and has to tell it from a
// refused secret.
func TestAServicesStatusWithTheProviderDownExitsOne(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.asService("http://127.0.0.1:9", serviceSecret)

	got := f.run("auth", "status", "--json")
	var status authStatus
	decodeInto(t, got.out, &status)
	if got.code != 1 || status.LoggedIn || keyIn(t, got.out, "type") != "service_account" || keyIn(t, got.out, "session") != "unverified" {
		t.Errorf("status with the provider down: exit %d, %s, want 1, not logged in, session unverified", got.code, got.out)
	}
}

// keyIn is one key of a status document, read by name so a renamed tag fails
// here rather than round-tripping through authStatus.
func keyIn(t *testing.T, out, key string) any {
	t.Helper()
	var keys map[string]any
	decodeInto(t, out, &keys)
	return keys[key]
}

func TestAPersonsStatusIsAnAuthorizedUser(t *testing.T) {
	f := newFixture(t, serves(nil))

	if got := keyIn(t, f.run("auth", "status", "--json").out, "type"); got != "authorized_user" {
		t.Errorf("type = %v, want authorized_user", got)
	}
}

func TestAServiceIsRefusedALoginAndALogout(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.asService(serviceIDP(t).URL, serviceSecret)

	for _, verb := range []string{"login", "logout"} {
		got := f.run("auth", verb)
		if got.code == 0 || !strings.Contains(got.err, "YPL_CLIENT_SECRET") {
			t.Errorf("auth %s: exit %d with %q, want it refused with the secret named", verb, got.code, got.err)
		}
	}
}

// Without YPL_CLIENT_ID the client would be the person's ypl-cli-<host>.
func TestASecretWithoutItsClientIsRefused(t *testing.T) {
	f := newFixture(t, serves(map[string]string{"/api/v1/status": statusBody}))
	f.asService(serviceIDP(t).URL, serviceSecret)
	t.Setenv("YPL_CLIENT_ID", "")

	for _, args := range [][]string{{"server", "status"}, {"auth", "status"}, {"auth", "token"}, {"auth", "login"}, {"auth", "logout"}} {
		got := f.run(args...)
		if got.code == 0 || !strings.Contains(got.err, "YPL_CLIENT_ID") {
			t.Errorf("%v: exit %d with %q, want it refused naming YPL_CLIENT_ID", args, got.code, got.err)
		}
	}
	if len(f.sent) != 0 {
		t.Errorf("the server was asked %d times", len(f.sent))
	}
}

// `config show` is pasted into issues and chat, and `config example` becomes a
// file on disk.
func TestTheSecretIsNeverPrintedOrAskedForInTheFile(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.asService(serviceIDP(t).URL, serviceSecret)

	for _, args := range [][]string{{"config", "show"}, {"config", "show", "--json"}, {"config", "example"}} {
		got := f.run(args...)
		if strings.Contains(got.out+got.err, serviceSecret) || strings.Contains(got.out, "YPL_CLIENT_SECRET") {
			t.Errorf("%v printed the secret or asked for it: %s%s", args, got.out, got.err)
		}
	}
}

func TestARouteOutsideTheScopeReportsTheServersSentence(t *testing.T) {
	says := "client ypl-svc-worker may not reach GET /api/v1/playlists: its scopes reach only GET /api/v1/status"
	f := newFixture(t, refuses(http.StatusForbidden, "outside_service_scope", says))
	f.asService(serviceIDP(t).URL, serviceSecret)

	got := f.run("playlists", "list")
	if got.code != 1 || !strings.Contains(got.err, says) || strings.Contains(got.err, "auth login") {
		t.Errorf("exited %d with %q, want the server's sentence whole and no login suggested", got.code, got.err)
	}
}

// The fake provider grants whatever scope ypl asks for, so a scope the server
// renamed, or a route it moved, passes every other test here and answers 403
// to every scheduled run.
func TestServerStatusCallsARouteTheRequestedScopeReaches(t *testing.T) {
	raw, err := os.ReadFile("../../../api/handlers/testdata/wire/service-scopes.json")
	if err != nil {
		t.Fatalf("read the server's service scopes — run `go test ./handlers -update` in the api module: %v", err)
	}
	var reaches map[string][]string
	if err := json.Unmarshal(raw, &reaches); err != nil {
		t.Fatalf("decode the server's service scopes: %v", err)
	}

	f := newFixture(t, serves(map[string]string{"/api/v1/status": freshStatus()}))
	f.asService(serviceIDP(t).URL, serviceSecret)
	if got := f.run("server", "status"); got.code != 0 {
		t.Fatalf("exited %d: %s", got.code, got.err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load the config: %v", err)
	}

	scopes := cfg.Service().Scopes
	listed := http.NewServeMux()
	registered := make(map[string]bool)
	for _, scope := range scopes {
		for _, pattern := range reaches[scope] {
			if !registered[pattern] {
				registered[pattern] = true
				listed.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
			}
		}
	}
	if len(f.sent) == 0 {
		t.Fatal("`ypl server status` sent nothing to hold against the scopes")
	}
	for _, sent := range f.sent {
		if _, pattern := listed.Handler(httptest.NewRequest(sent.Method, sent.URL.Path, http.NoBody)); pattern == "" {
			t.Errorf("`ypl server status` sent %s %s, which the server lists for none of %v", sent.Method, sent.URL.Path, scopes)
		}
	}
}
