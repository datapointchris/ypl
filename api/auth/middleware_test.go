package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubVerifier struct {
	identity Identity
	err      error
}

func (s stubVerifier) Verify(context.Context, string) (Identity, error) {
	return s.identity, s.err
}

// serve sends a request for path with the Authorization header authorization
// through RequireBearer, and reports the response and the identity the next
// handler saw, if it was reached.
func serve(verifier TokenVerifier, path, authorization string) (*httptest.ResponseRecorder, *Identity) {
	var reached *Identity
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := IdentityFrom(r.Context())
		reached = &identity
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	RequireBearer(verifier, slog.New(slog.NewTextHandler(io.Discard, nil)))(next).ServeHTTP(rec, req)
	return rec, reached
}

func TestTheProbesNeedNoToken(t *testing.T) {
	for _, path := range []string{"/health", "/ready"} {
		rec, reached := serve(stubVerifier{err: ErrUnauthorized}, path, "")
		if reached == nil || rec.Code != http.StatusOK {
			t.Errorf("%s answered %d without reaching the handler, want it reached", path, rec.Code)
		}
	}
}

func TestARequestWithoutAVerifiedTokenIsRefused(t *testing.T) {
	cases := map[string]struct {
		verifier      TokenVerifier
		authorization string
		code          int
	}{
		"no token":                  {stubVerifier{identity: Identity{Subject: "s"}}, "", http.StatusUnauthorized},
		"another scheme":            {stubVerifier{identity: Identity{Subject: "s"}}, "Basic dXNlcjpwdw==", http.StatusUnauthorized},
		"a rejected token":          {stubVerifier{err: fmt.Errorf("%w: expired", ErrUnauthorized)}, "Bearer a.b.c", http.StatusUnauthorized},
		"an unexpected error":       {stubVerifier{err: errors.New("decode")}, "Bearer a.b.c", http.StatusUnauthorized},
		"an unreachable provider":   {stubVerifier{err: fmt.Errorf("%w: fetching keys", ErrProviderUnavailable)}, "Bearer a.b.c", http.StatusServiceUnavailable},
		"a path near a probe's one": {stubVerifier{err: ErrUnauthorized}, "", http.StatusUnauthorized},
	}
	for name, c := range cases {
		path := "/api/v1/playlists"
		if name == "a path near a probe's one" {
			path = "/health/extra"
		}
		rec, reached := serve(c.verifier, path, c.authorization)
		if reached != nil || rec.Code != c.code {
			t.Errorf("%s: answered %d and reached the handler %v, want %d and not reached", name, rec.Code, reached != nil, c.code)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type %q, want application/json", name, got)
		}
	}
}

func TestAVerifiedTokensIdentityReachesTheHandler(t *testing.T) {
	want := Identity{Subject: "user-uuid", ClientID: "ypl-cli-desk"}
	rec, reached := serve(stubVerifier{identity: want}, "/api/v1/playlists", "Bearer a.b.c")
	if rec.Code != http.StatusOK || reached == nil || *reached != want {
		t.Fatalf("answered %d with identity %v, want 200 and %+v", rec.Code, reached, want)
	}
}
