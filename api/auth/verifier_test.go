package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const testKeyID = "main"

// testProvider is an identity provider serving a discovery document and the
// JWKS for one RSA key, recording each request's User-Agent.
type testProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu     sync.Mutex
	agents []string
}

func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &testProvider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.record(r)
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.server.URL, "jwks_uri": p.server.URL + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		p.record(r)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: testKeyID, Algorithm: string(jose.RS256), Use: "sig",
		}}})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *testProvider) record(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents = append(p.agents, r.UserAgent())
}

// sign is a token with the header type typ and claims, signed by key or, when
// key is nil, by the provider's own key.
func (p *testProvider) sign(t *testing.T, typ string, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = p.key
	}
	opts := (&jose.SignerOptions{}).WithType(jose.ContentType(typ))
	opts.WithHeader("kid", testKeyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, opts)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

func (p *testProvider) claims() map[string]any {
	return map[string]any{
		"iss":       p.server.URL,
		"sub":       "user-uuid",
		"client_id": "ypl-cli-desk",
		"iat":       time.Now().Add(-time.Minute).Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
}

func (p *testProvider) verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(context.Background(), p.server.URL, "ypl-cli-")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestAnAccessTokenForThisProductIsAccepted(t *testing.T) {
	p := newTestProvider(t)

	identity, err := p.verifier(t).Verify(context.Background(), p.sign(t, "at+jwt", p.claims(), nil))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity != (Identity{Subject: "user-uuid", ClientID: "ypl-cli-desk"}) {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestEveryBadTokenIsUnauthorized(t *testing.T) {
	p := newTestProvider(t)
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	with := func(edit func(map[string]any)) map[string]any {
		claims := p.claims()
		edit(claims)
		return claims
	}
	cases := map[string]string{
		// An id_token carries the same issuer and signature, and is handed to the
		// client. Only its header type tells it apart.
		"an id_token":               p.sign(t, "JWT", p.claims(), nil),
		"a foreign signing key":     p.sign(t, "at+jwt", p.claims(), foreign),
		"an expired token":          p.sign(t, "at+jwt", with(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), nil),
		"a token with no expiry":    p.sign(t, "at+jwt", with(func(c map[string]any) { delete(c, "exp") }), nil),
		"another issuer":            p.sign(t, "at+jwt", with(func(c map[string]any) { c["iss"] = "https://other.example" }), nil),
		"another product's client":  p.sign(t, "at+jwt", with(func(c map[string]any) { c["client_id"] = "other-cli-desk" }), nil),
		"a token with no subject":   p.sign(t, "at+jwt", with(func(c map[string]any) { delete(c, "sub") }), nil),
		"text that is not a JWT":    "not-a-jwt",
		"a header that is not JSON": "bm90LWpzb24.e30.sig",
	}
	v := p.verifier(t)
	for name, raw := range cases {
		if _, err := v.Verify(context.Background(), raw); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Verify = %v, want ErrUnauthorized", name, err)
		}
	}
}

func TestAProviderWhoseKeysCannotBeFetchedIsUnavailable(t *testing.T) {
	p := newTestProvider(t)
	v := p.verifier(t)
	raw := p.sign(t, "at+jwt", p.claims(), nil)
	p.server.Close()

	if _, err := v.Verify(context.Background(), raw); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Verify with the provider down = %v, want ErrProviderUnavailable", err)
	}
}

func TestAnIssuerAdvertisingAnotherNameIsRefused(t *testing.T) {
	p := newTestProvider(t)
	if _, err := NewVerifier(context.Background(), p.server.URL+"/", "ypl-cli-"); err == nil {
		t.Fatal("NewVerifier accepted an issuer whose discovery document names a different issuer")
	}
}

// A proxy in front of a provider can refuse a client that names none, so the
// JWKS fetch made on the first verification names itself as well as discovery.
func TestEveryRequestToTheProviderNamesThisAPI(t *testing.T) {
	p := newTestProvider(t)
	_, _ = p.verifier(t).Verify(context.Background(), p.sign(t, "at+jwt", p.claims(), nil))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.agents) < 2 {
		t.Fatalf("the provider saw %d requests, want discovery and a JWKS fetch", len(p.agents))
	}
	for _, agent := range p.agents {
		if agent != userAgent {
			t.Fatalf("User-Agent = %q, want %q", agent, userAgent)
		}
	}
}

func TestBearerTokenReadsOnlyTheBearerScheme(t *testing.T) {
	cases := map[string]string{
		"Bearer abc.def.ghi": "abc.def.ghi",
		"bearer abc.def.ghi": "abc.def.ghi",
		"Basic dXNlcjpwdw==": "",
		"":                   "",
		"Bearer":             "",
	}
	for header, want := range cases {
		if got := BearerToken(header); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
