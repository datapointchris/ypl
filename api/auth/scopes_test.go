package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/datapointchris/ypl/api/wire"
)

func limited(identity *Identity, method, path string) (*httptest.ResponseRecorder, bool) {
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/playlists", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	routes := map[string][]string{"ypl.status.read": {"GET /api/v1/status"}}

	req := httptest.NewRequest(method, path, http.NoBody)
	if identity != nil {
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, *identity))
	}
	rec := httptest.NewRecorder()
	LimitServices(mux, routes, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(rec, req)
	return rec, reached
}

func TestAServiceReachesOnlyTheRoutesItsScopesList(t *testing.T) {
	status := &Identity{ClientID: "ypl-svc-worker", Service: true, Scopes: []string{"ypl.status.read"}}
	unknown := &Identity{ClientID: "ypl-svc-worker", Service: true, Scopes: []string{"ypl.playlists.write"}}
	person := &Identity{Subject: "user", ClientID: "ypl-cli-desk"}

	cases := map[string]struct {
		identity     *Identity
		method, path string
		reached      bool
	}{
		"a service on its route":                {status, http.MethodGet, "/api/v1/status", true},
		"a service sending HEAD to its route":   {status, http.MethodHead, "/api/v1/status", true},
		"a service on another route":            {status, http.MethodGet, "/api/v1/playlists", false},
		"a service writing to its route":        {status, http.MethodPost, "/api/v1/status", false},
		"a service on a route nothing serves":   {status, http.MethodGet, "/api/v1/nothing", false},
		"a service with a scope nothing lists":  {unknown, http.MethodGet, "/api/v1/status", false},
		"a person on a route no scope lists":    {person, http.MethodGet, "/api/v1/playlists", true},
		"a request RequireBearer left unproven": {nil, http.MethodGet, "/api/v1/playlists", true},
	}
	for name, c := range cases {
		rec, reached := limited(c.identity, c.method, c.path)
		if reached != c.reached {
			t.Errorf("%s: reached the route %v, want %v", name, reached, c.reached)
		}
		if c.reached {
			continue
		}
		var body wire.Refusal
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusForbidden || body.Code != wire.CodeOutsideServiceScope {
			t.Errorf("%s: answered %d %s, want 403 %s", name, rec.Code, rec.Body, wire.CodeOutsideServiceScope)
		}
	}
}
