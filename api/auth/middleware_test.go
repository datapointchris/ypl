package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/datapointchris/ypl/api/wire"
)

type stubVerifier struct {
	identity Identity
	err      error
}

func (s stubVerifier) Verify(context.Context, string) (Identity, error) {
	return s.identity, s.err
}

// serve sends req through RequireBearer, and reports the response and the
// identity the next handler saw, if it was reached.
func serve(verifier TokenVerifier, req *http.Request) (*httptest.ResponseRecorder, *Identity) {
	var reached *Identity
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := IdentityFrom(r.Context())
		reached = &identity
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	RequireBearer(verifier, slog.New(slog.NewTextHandler(io.Discard, nil)))(next).ServeHTTP(rec, req)
	return rec, reached
}

func request(path, authorization string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req
}

func TestTheProbesNeedNoToken(t *testing.T) {
	for _, path := range []string{"/health", "/ready"} {
		rec, reached := serve(stubVerifier{err: ErrUnauthorized}, request(path, ""))
		if reached == nil || rec.Code != http.StatusOK {
			t.Errorf("%s answered %d without reaching the handler, want it reached", path, rec.Code)
		}
	}
}

func TestARequestWithoutAVerifiedTokenIsRefused(t *testing.T) {
	cases := map[string]struct {
		verifier      TokenVerifier
		path          string
		authorization string
		status        int
		code          wire.Code
		challenge     string
	}{
		"no token":                  {stubVerifier{identity: Identity{Subject: "s"}}, "/api/v1/playlists", "", http.StatusUnauthorized, wire.CodeMissingToken, "Bearer"},
		"another scheme":            {stubVerifier{identity: Identity{Subject: "s"}}, "/api/v1/playlists", "Basic dXNlcjpwdw==", http.StatusUnauthorized, wire.CodeMissingToken, "Bearer"},
		"a rejected token":          {stubVerifier{err: fmt.Errorf("%w: expired", ErrUnauthorized)}, "/api/v1/playlists", "Bearer a.b.c", http.StatusUnauthorized, wire.CodeInvalidToken, `Bearer error="invalid_token"`},
		"an unexpected error":       {stubVerifier{err: errors.New("decode")}, "/api/v1/playlists", "Bearer a.b.c", http.StatusUnauthorized, wire.CodeInvalidToken, `Bearer error="invalid_token"`},
		"an unreachable provider":   {stubVerifier{err: fmt.Errorf("%w: reading keys", ErrProviderUnavailable)}, "/api/v1/playlists", "Bearer a.b.c", http.StatusServiceUnavailable, wire.CodeIdentityProviderUnavailable, ""},
		"a path near a probe's one": {stubVerifier{err: ErrUnauthorized}, "/health/extra", "", http.StatusUnauthorized, wire.CodeMissingToken, "Bearer"},
	}
	for name, c := range cases {
		rec, reached := serve(c.verifier, request(c.path, c.authorization))
		if reached != nil || rec.Code != c.status {
			t.Errorf("%s: answered %d and reached the handler %v, want %d and not reached", name, rec.Code, reached != nil, c.status)
		}
		var body wire.Refusal
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != c.code {
			t.Errorf("%s: body %s, want code %s", name, rec.Body, c.code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != c.challenge {
			t.Errorf("%s: WWW-Authenticate %q, want %q", name, got, c.challenge)
		}
	}
}

func TestARequestWhoseCallerWentAwayGetsNoAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verifier := stubVerifier{err: fmt.Errorf("auth: read the provider's keys: %w", context.Canceled)}

	rec, reached := serve(verifier, request("/api/v1/playlists", "Bearer a.b.c").WithContext(ctx))
	if reached != nil || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("answered %d with %q and reached the handler %v, want no answer", rec.Code, rec.Body, reached != nil)
	}
}

func TestAVerifiedTokensIdentityReachesTheHandler(t *testing.T) {
	want := Identity{Subject: "user-uuid", ClientID: "ypl-cli-desk"}
	rec, reached := serve(stubVerifier{identity: want}, request("/api/v1/playlists", "Bearer a.b.c"))
	if rec.Code != http.StatusOK || reached == nil || *reached != want {
		t.Fatalf("answered %d with identity %v, want 200 and %+v", rec.Code, reached, want)
	}
}
