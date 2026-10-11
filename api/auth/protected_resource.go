package auth

import (
	"net/http"

	"github.com/datapointchris/ypl/api/wire"
)

// ProtectedResourcePath is where RFC 9728 puts this API's metadata, for an API
// served at the root of its host. It is public: a CLI reads it before it holds
// a token, to learn which identity provider to log in through.
const ProtectedResourcePath = "/.well-known/oauth-protected-resource"

// protectedResourceMetadata is the RFC 9728 § 2 document, carrying only the two
// members a client needs to log in.
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// ProtectedResourceMetadata serves the RFC 9728 document naming resource, the
// URL clients reach this API at, and issuer, the provider whose tokens it
// accepts. A client refuses the document unless resource equals the URL it
// fetched it for (§ 3.3), so resource has to be the public URL exactly.
func ProtectedResourceMetadata(resource, issuer string) http.Handler {
	document := protectedResourceMetadata{
		Resource:             resource,
		AuthorizationServers: []string{issuer},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wire.JSON(w, http.StatusOK, document)
	})
}
