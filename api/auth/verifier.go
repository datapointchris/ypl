// Package auth verifies the RFC 9068 JWT access tokens the identity provider
// issues to the ypl CLI, and refuses every other request to the API.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrUnauthorized is returned for every rejected token. The reason is logged
// rather than returned to the caller, so a probe cannot learn from the error
// which check it failed.
var ErrUnauthorized = errors.New("unauthorized")

// ErrProviderUnavailable is the identity provider not answering, as distinct
// from a token that is bad. A caller holding a good token cannot fix a network
// failure by logging in again.
var ErrProviderUnavailable = errors.New("identity provider unavailable")

// discoveryTimeout bounds each request to the provider. Without it a provider
// that accepts the connection and never replies wedges the process before it
// binds a port.
const discoveryTimeout = 10 * time.Second

// userAgent names this API to the provider. A proxy in front of a provider can
// refuse a client that names none.
const userAgent = "ypl-api"

// Identity is what a verified token establishes about the caller.
type Identity struct {
	Subject  string
	ClientID string
}

// Verifier checks token signatures against the issuer's JWKS and the claims
// against this API's expectations.
type Verifier struct {
	issuer         string
	clientIDPrefix string
	keySet         *oidc.RemoteKeySet
	now            func() time.Time
}

// namedAgent sets the User-Agent on every request the OIDC client makes,
// including the JWKS refreshes go-oidc makes long after startup.
type namedAgent struct{ base http.RoundTripper }

func (n namedAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", userAgent)
	return n.base.RoundTrip(req)
}

func identifiedClient() *http.Client {
	return &http.Client{Timeout: discoveryTimeout, Transport: namedAgent{base: http.DefaultTransport}}
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// NewVerifier resolves the issuer's JWKS endpoint from its discovery document.
// clientIDPrefix is the per-product half of a CLI client's id, `ypl-cli-` for
// `ypl-cli-<host>`, and is what keeps a token issued to another product's CLI
// from being accepted: the device authorization grant leaves the audience claim
// empty, so it cannot carry that isolation.
func NewVerifier(ctx context.Context, issuer, clientIDPrefix string) (*Verifier, error) {
	if issuer == "" || clientIDPrefix == "" {
		return nil, errors.New("auth: issuer and clientIDPrefix are both required")
	}
	doc, err := fetchDiscovery(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if doc.Issuer != issuer {
		return nil, fmt.Errorf("auth: issuer %q advertises itself as %q", issuer, doc.Issuer)
	}
	if doc.JWKSURI == "" {
		return nil, fmt.Errorf("auth: issuer %q advertises no jwks_uri", issuer)
	}
	return &Verifier{
		issuer:         issuer,
		clientIDPrefix: clientIDPrefix,
		keySet:         oidc.NewRemoteKeySet(oidc.ClientContext(ctx, identifiedClient()), doc.JWKSURI),
		now:            time.Now,
	}, nil
}

func fetchDiscovery(ctx context.Context, issuer string) (discoveryDocument, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return discoveryDocument{}, err
	}
	resp, err := identifiedClient().Do(req)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("auth: reach %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return discoveryDocument{}, fmt.Errorf("auth: %s returned %s", url, resp.Status)
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return discoveryDocument{}, fmt.Errorf("auth: decode %s: %w", url, err)
	}
	return doc, nil
}

type accessTokenClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	ClientID string `json:"client_id"`
	Expiry   int64  `json:"exp"`
}

// Verify checks the header type, the signature, and then every claim this API
// relies on. It returns ErrUnauthorized for a bad token and
// ErrProviderUnavailable when the keys cannot be fetched, with the reason
// wrapped for logging.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if err := requireAccessTokenType(raw); err != nil {
		return Identity{}, err
	}

	payload, err := v.keySet.VerifySignature(ctx, raw)
	if err != nil {
		if isRetrievalFailure(err) {
			return Identity{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
		}
		return Identity{}, fmt.Errorf("%w: signature: %w", ErrUnauthorized, err)
	}

	var claims accessTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, fmt.Errorf("%w: claims are not JSON: %w", ErrUnauthorized, err)
	}
	switch {
	case claims.Issuer != v.issuer:
		return Identity{}, fmt.Errorf("%w: issuer %q", ErrUnauthorized, claims.Issuer)
	case claims.Subject == "":
		return Identity{}, fmt.Errorf("%w: no subject", ErrUnauthorized)
	case !strings.HasPrefix(claims.ClientID, v.clientIDPrefix):
		return Identity{}, fmt.Errorf("%w: client %q is not a %s* client", ErrUnauthorized, claims.ClientID, v.clientIDPrefix)
	case claims.Expiry == 0 || v.now().After(time.Unix(claims.Expiry, 0)):
		return Identity{}, fmt.Errorf("%w: expired", ErrUnauthorized)
	}
	return Identity{Subject: claims.Subject, ClientID: claims.ClientID}, nil
}

// requireAccessTokenType refuses anything not typed as an RFC 9068 access
// token. An id_token from the same issuer carries a valid signature and issuer,
// and it is handed to the client, so without this check it would authenticate.
func requireAccessTokenType(raw string) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: not a JWT", ErrUnauthorized)
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("%w: header is not base64url", ErrUnauthorized)
	}
	var header struct {
		Type string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return fmt.Errorf("%w: header is not JSON", ErrUnauthorized)
	}
	if !strings.EqualFold(header.Type, "at+jwt") {
		return fmt.Errorf("%w: token type %q is not at+jwt", ErrUnauthorized, header.Type)
	}
	return nil
}

// isRetrievalFailure is whether the key set could not be fetched, as opposed to
// the token failing against keys that were. go-oidc does not type the two
// apart, so its message is all there is to tell them by.
func isRetrievalFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "fetching keys") || strings.Contains(msg, "oidc: get keys failed")
}
