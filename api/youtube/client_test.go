package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
	ytapi "google.golang.org/api/youtube/v3"
)

// fakeGoogle is Google's device authorization and token endpoints, granting
// whatever scope and refresh token it holds as soon as it is polled.
type fakeGoogle struct {
	t            *testing.T
	scope        string
	refreshToken string
}

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		g.t.Errorf("parse %s: %v", r.URL.Path, err)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/device/code":
		if got := r.PostForm.Get("scope"); got != Scope {
			g.t.Errorf("device code request asked for scope %q, want %q", got, Scope)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "device-code",
			"user_code":        "ABCD-EFGH",
			"verification_url": "https://www.google.com/device",
			"expires_in":       1800,
			"interval":         1,
		})
	case "/token":
		if got := r.PostForm.Get("device_code"); got != "device-code" {
			g.t.Errorf("token request sent device code %q", got)
		}
		response := map[string]any{"access_token": "access", "expires_in": 3599, "scope": g.scope, "token_type": "Bearer"}
		if g.refreshToken != "" {
			response["refresh_token"] = g.refreshToken
		}
		_ = json.NewEncoder(w).Encode(response)
	default:
		http.NotFound(w, r)
	}
}

func authorizeAgainst(t *testing.T, google *fakeGoogle) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(google)
	t.Cleanup(server.Close)
	config := oauthConfig(Client{ID: "id", Secret: "secret"})
	config.Endpoint = oauth2.Endpoint{DeviceAuthURL: server.URL + "/device/code", TokenURL: server.URL + "/token"}

	var shown string
	token, err := authorize(context.Background(), config, func(verificationURL, userCode string) error {
		shown = verificationURL + " " + userCode
		return nil
	})
	return token, shown, err
}

func TestAuthorizeAsksForTheScopeAndReturnsTheRefreshToken(t *testing.T) {
	t.Parallel()
	token, shown, err := authorizeAgainst(t, &fakeGoogle{t: t, scope: Scope, refreshToken: "refresh"})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if token != "refresh" || shown != "https://www.google.com/device ABCD-EFGH" {
		t.Fatalf("authorize returned %q and showed %q", token, shown)
	}
}

func TestAuthorizeRefusesAGrantThisServiceCannotUse(t *testing.T) {
	t.Parallel()
	cases := map[string]*fakeGoogle{
		"a narrower scope":   {scope: ytapi.YoutubeReadonlyScope, refreshToken: "refresh"},
		"no refresh token":   {scope: Scope},
		"no scope in answer": {refreshToken: "refresh"},
	}
	for name, google := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			google.t = t
			if _, _, err := authorizeAgainst(t, google); !errors.Is(err, ErrIncompleteGrant) {
				t.Fatalf("authorize = %v, want ErrIncompleteGrant", err)
			}
		})
	}
}
