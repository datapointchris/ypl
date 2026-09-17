package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// TokenVerifier is what RequireBearer checks a token with, as *Verifier does.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Identity, error)
}

// exempt holds the paths answered without a token: the liveness and readiness
// probes a container runtime calls without credentials.
var exempt = []string{"/health", "/ready"}

type contextKey struct{}

// RequireBearer passes a request to next only when it carries a bearer token
// verifier accepts, or asks for an exempt path. A rejected token is a 401, and a
// provider that cannot be reached is a 503, each with the reason logged and a
// JSON body that does not carry it.
func RequireBearer(verifier TokenVerifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.Contains(exempt, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			raw := BearerToken(r.Header.Get("Authorization"))
			if raw == "" {
				refuse(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			identity, err := verifier.Verify(r.Context(), raw)
			switch {
			case errors.Is(err, ErrProviderUnavailable):
				log.ErrorContext(r.Context(), "identity provider unreachable", "err", err, "path", r.URL.Path)
				refuse(w, http.StatusServiceUnavailable, "identity provider unavailable")
				return
			case err != nil:
				log.WarnContext(r.Context(), "bearer token rejected", "err", err, "path", r.URL.Path)
				refuse(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, identity)))
		})
	}
}

// IdentityFrom is the identity RequireBearer established for the request.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}

// BearerToken is the credential in an Authorization header, or "" when the
// header is absent or names another scheme.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
