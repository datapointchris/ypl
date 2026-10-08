package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
	if got.code != 0 || !status.LoggedIn || status.Mode != modeService || status.ExpiresAt == "" {
		t.Errorf("status with a good secret: exit %d, %+v, want logged in as a service until an expiry", got.code, status)
	}

	f.asService(idp.URL, "wrong-secret")
	got = f.run("auth", "status")
	if got.code != 1 || !strings.Contains(got.out, "YPL_CLIENT_SECRET") || strings.Contains(got.out, "auth login") {
		t.Errorf("status with a refused secret: exit %d, %q, want 1 and the secret named", got.code, got.out)
	}
}

// A service stores no token, so a provider it cannot reach means the next
// command fails, and a job gating on status has to see that.
func TestAServicesStatusWithTheProviderDownExitsOne(t *testing.T) {
	f := newFixture(t, serves(nil))
	f.asService("http://127.0.0.1:9", serviceSecret)

	got := f.run("auth", "status", "--json")
	var status authStatus
	decodeInto(t, got.out, &status)
	if got.code != 1 || status.LoggedIn || status.Mode != modeService {
		t.Errorf("status with the provider down: exit %d, %+v, want 1 and not logged in", got.code, status)
	}
}

func TestAPersonsStatusNamesTheLoginMode(t *testing.T) {
	f := newFixture(t, serves(nil))

	var status authStatus
	decodeInto(t, f.run("auth", "status", "--json").out, &status)
	if status.Mode != modeLogin {
		t.Errorf("mode = %q, want %q", status.Mode, modeLogin)
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

	for _, args := range [][]string{{"server", "status"}, {"auth", "status"}, {"auth", "token"}} {
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

func TestARouteOutsideTheScopeSaysWhatTheScopeCovers(t *testing.T) {
	f := newFixture(t, refuses(http.StatusForbidden, "outside_service_scope", "client ypl-svc-worker may not reach GET /api/v1/playlists"))
	f.asService(serviceIDP(t).URL, serviceSecret)

	got := f.run("playlists", "list")
	if got.code != 1 || !strings.Contains(got.err, "ypl-svc-worker") || !strings.Contains(got.err, "ypl server status") {
		t.Errorf("exited %d with %q, want the server's sentence and what the scope covers", got.code, got.err)
	}
}
