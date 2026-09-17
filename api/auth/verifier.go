// Package auth verifies the RFC 9068 JWT access tokens the identity provider
// issues to the ypl CLI, and refuses every other request to the API.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/sync/singleflight"
)

// ErrUnauthorized is returned for every rejected token. The reason is logged
// rather than returned to the caller, so a probe cannot learn from the error
// which check it failed.
var ErrUnauthorized = errors.New("unauthorized")

// ErrProviderUnavailable is the identity provider not answering, or answering
// with no usable keys, as distinct from a token that is bad. A caller holding a
// good token cannot fix it by logging in again.
var ErrProviderUnavailable = errors.New("identity provider unavailable")

// providerTimeout bounds each request to the provider. Without it a provider
// that accepts the connection and never replies holds every caller waiting on
// it.
const providerTimeout = 10 * time.Second

// userAgent names this API to the provider. A proxy in front of a provider can
// refuse a client that names none.
const userAgent = "ypl-api"

// signingAlgorithm is the one algorithm the provider signs CLI access tokens
// with. A token naming another is refused before any key is tried.
const signingAlgorithm = jose.RS256

// Identity is what a verified token establishes about the caller.
type Identity struct {
	Subject  string
	ClientID string
}

// Verifier checks token signatures against the issuer's published keys and the
// claims against this API's expectations.
type Verifier struct {
	issuer         string
	clientIDPrefix string
	keys           *keySet
	now            func() time.Time
}

// namedAgent sets the User-Agent on every request to the provider.
type namedAgent struct{ base http.RoundTripper }

func (n namedAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", userAgent)
	return n.base.RoundTrip(req)
}

func identifiedClient() *http.Client {
	return &http.Client{Timeout: providerTimeout, Transport: namedAgent{base: http.DefaultTransport}}
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// NewVerifier reads the issuer's discovery document and then the keys it
// publishes, so a Verifier that exists can verify a token. clientIDPrefix is the
// per-product half of a CLI client's id, `ypl-cli-` for `ypl-cli-<host>`, and is
// what keeps a token issued to another product's CLI from being accepted: the
// device authorization grant leaves the audience claim empty, so it cannot carry
// that isolation. A provider that cannot be read is ErrProviderUnavailable.
func NewVerifier(ctx context.Context, issuer, clientIDPrefix string) (*Verifier, error) {
	if issuer == "" || clientIDPrefix == "" {
		return nil, errors.New("auth: issuer and clientIDPrefix are both required")
	}
	doc, err := readDiscovery(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if doc.Issuer != issuer {
		return nil, fmt.Errorf("auth: issuer %q advertises itself as %q", issuer, doc.Issuer)
	}
	if doc.JWKSURI == "" {
		return nil, fmt.Errorf("auth: issuer %q advertises no jwks_uri", issuer)
	}
	keys := &keySet{uri: doc.JWKSURI, client: identifiedClient()}
	if _, err := keys.fetch(); err != nil {
		return nil, err
	}
	return &Verifier{issuer: issuer, clientIDPrefix: clientIDPrefix, keys: keys, now: time.Now}, nil
}

func readDiscovery(ctx context.Context, issuer string) (discoveryDocument, error) {
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("auth: %w", err)
	}
	resp, err := identifiedClient().Do(req)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("%w: reach %s: %w", ErrProviderUnavailable, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return discoveryDocument{}, fmt.Errorf("%w: %s answered %s", ErrProviderUnavailable, url, resp.Status)
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return discoveryDocument{}, fmt.Errorf("%w: decode %s: %w", ErrProviderUnavailable, url, err)
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
// relies on. A bad token is ErrUnauthorized and a provider whose keys cannot be
// read is ErrProviderUnavailable, each with the reason wrapped for logging. ctx
// ending while the keys are read returns ctx's own error.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	signed, err := parseAccessToken(raw)
	if err != nil {
		return Identity{}, err
	}
	payload, err := v.keys.verify(ctx, signed)
	if err != nil {
		return Identity{}, err
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

// parseAccessToken reads raw as a compact JWS signed with signingAlgorithm and
// typed as an RFC 9068 access token. An id_token from the same issuer carries a
// valid signature and issuer, and it is handed to the client, so without the
// type check it would authenticate.
func parseAccessToken(raw string) (*jose.JSONWebSignature, error) {
	if strings.Count(raw, ".") != 2 {
		return nil, fmt.Errorf("%w: not a compact JWT", ErrUnauthorized)
	}
	signed, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{signingAlgorithm})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	typ, _ := signed.Signatures[0].Protected.ExtraHeaders[jose.HeaderType].(string)
	if !strings.EqualFold(typ, "at+jwt") {
		return nil, fmt.Errorf("%w: token type %q is not at+jwt", ErrUnauthorized, typ)
	}
	return signed, nil
}

// keySet holds the signing keys the provider publishes at uri. A token no held
// key verifies makes it read them again, which covers a key the provider has
// rotated in. Callers waiting at the same moment share one read.
type keySet struct {
	uri     string
	client  *http.Client
	fetches singleflight.Group

	mu   sync.RWMutex
	keys []jose.JSONWebKey
}

func (s *keySet) held() []jose.JSONWebKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys
}

func (s *keySet) verify(ctx context.Context, signed *jose.JSONWebSignature) ([]byte, error) {
	if payload, ok := verifyWith(s.held(), signed); ok {
		return payload, nil
	}
	read := s.fetches.DoChan("keys", func() (any, error) {
		keys, err := s.fetch()
		return keys, err
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("auth: read the provider's keys: %w", ctx.Err())
	case result := <-read:
		if result.Err != nil {
			return nil, result.Err
		}
		if payload, ok := verifyWith(result.Val.([]jose.JSONWebKey), signed); ok {
			return payload, nil
		}
		return nil, fmt.Errorf("%w: no key the provider publishes verifies the signature", ErrUnauthorized)
	}
}

// fetch reads the keys the provider publishes and holds them. It runs on a
// deadline of its own rather than a caller's, since every caller waiting shares
// it and any of them can stop waiting.
func (s *keySet) fetch() ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), providerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.uri, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: reach %s: %w", ErrProviderUnavailable, s.uri, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %s", ErrProviderUnavailable, s.uri, resp.Status)
	}
	var published jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&published); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrProviderUnavailable, s.uri, err)
	}
	var signing []jose.JSONWebKey
	for _, key := range published.Keys {
		if key.Use == "" || key.Use == "sig" {
			signing = append(signing, key)
		}
	}
	if len(signing) == 0 {
		return nil, fmt.Errorf("%w: %s publishes no signing key", ErrProviderUnavailable, s.uri)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = signing
	return signing, nil
}

// verifyWith is the payload of signed as one of keys verifies it. Every key is
// the provider's, so the key id the token names decides nothing a signature
// does not.
func verifyWith(keys []jose.JSONWebKey, signed *jose.JSONWebSignature) ([]byte, bool) {
	for i := range keys {
		if payload, err := signed.Verify(&keys[i]); err == nil {
			return payload, true
		}
	}
	return nil, false
}
