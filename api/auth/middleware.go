package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/datapointchris/ypl/api/wire"
)

// TokenVerifier is what RequireBearer checks a token with, as *Verifier and
// *Connecting do.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Identity, error)
}

// exempt holds the paths answered without a token: the liveness and readiness
// probes a container runtime calls without credentials.
var exempt = []string{"/health", "/ready"}

type contextKey struct{}

// RequireBearer passes a request to next only when it carries a bearer token
// verifier accepts, or asks for an exempt path.
//
// A missing or rejected token is a 401 carrying the RFC 6750 Bearer challenge,
// and a provider that cannot be read is a 503. Each is logged with its reason,
// which the body does not carry. A request whose caller went away before its
// token was verified gets no answer.
func RequireBearer(verifier TokenVerifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.Contains(exempt, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			raw := BearerToken(r.Header.Get("Authorization"))
			if raw == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				wire.Refuse(w, http.StatusUnauthorized, wire.CodeMissingToken, "a bearer token is required")
				return
			}
			identity, err := verifier.Verify(ctx, raw)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, contextKey{}, identity)))
			case ctx.Err() != nil:
				log.InfoContext(ctx, "request ended before its token was verified", "err", err, "path", r.URL.Path)
			case errors.Is(err, ErrProviderUnavailable):
				log.ErrorContext(ctx, "identity provider unavailable", "err", err, "path", r.URL.Path)
				wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeIdentityProviderUnavailable, "the identity provider is unavailable")
			default:
				log.WarnContext(ctx, "bearer token rejected", "err", err, "path", r.URL.Path)
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				wire.Refuse(w, http.StatusUnauthorized, wire.CodeInvalidToken, "the bearer token is not valid")
			}
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
