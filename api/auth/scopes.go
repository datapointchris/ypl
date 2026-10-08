package auth

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/datapointchris/ypl/api/wire"
)

// LimitServices passes a request to mux unless RequireBearer established a
// service client for it and none of that client's scopes lists the pattern mux
// routes it to. That request is a 403. routes maps each scope to the ServeMux
// patterns it reaches, written as they were registered: "GET /api/v1/status".
//
// The pattern comes from mux itself, so a HEAD is judged as the GET route it
// reaches. A path no listed pattern serves, a missing route included, is a 403
// to a service rather than a 404.
func LimitServices(mux *http.ServeMux, routes map[string][]string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := IdentityFrom(r.Context())
		if !ok || !identity.Service {
			mux.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		for _, scope := range identity.Scopes {
			if slices.Contains(routes[scope], pattern) {
				mux.ServeHTTP(w, r)
				return
			}
		}
		log.WarnContext(r.Context(), "service client outside its scopes", "client_id", identity.ClientID, "scopes", identity.Scopes, "pattern", pattern, "path", r.URL.Path)
		wire.Refuse(w, http.StatusForbidden, wire.CodeOutsideServiceScope, "client %s may not reach %s %s: %s", identity.ClientID, r.Method, r.URL.Path, reachable(routes, identity.Scopes))
	})
}

// reachable says which patterns scopes reach in routes, so a refused client
// reads what it may call from the table that refused it.
func reachable(routes map[string][]string, scopes []string) string {
	var patterns []string
	for _, scope := range scopes {
		patterns = append(patterns, routes[scope]...)
	}
	if len(patterns) == 0 {
		return "its scopes reach no route"
	}
	slices.Sort(patterns)
	return "its scopes reach only " + strings.Join(slices.Compact(patterns), ", ")
}
