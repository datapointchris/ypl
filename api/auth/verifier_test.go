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

// testProvider is an identity provider serving a discovery document and a JWKS,
// recording each request's User-Agent and counting the JWKS reads.
type testProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu sync.Mutex
	// agents is every request's User-Agent.
	agents []string
	// published is the JWKS the provider serves.
	published jose.JSONWebKeySet
	// jwksStatus, when not 200, answers the JWKS with that status.
	jwksStatus int
	// jwksBody, when set, is served in place of published.
	jwksBody string
	// jwksGate, when set, holds every JWKS answer until it is closed.
	jwksGate chan struct{}
	// jwksReads counts JWKS requests.
	jwksReads int
	// discoveryFailures answers that many discovery requests with 503.
	discoveryFailures int
}

func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	p := &testProvider{key: newKey(t), jwksStatus: http.StatusOK}
	p.published = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicKey(p.key, testKeyID)}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.agents = append(p.agents, r.UserAgent())
		failing := p.discoveryFailures > 0
		if failing {
			p.discoveryFailures--
		}
		p.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.server.URL, "jwks_uri": p.server.URL + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.agents = append(p.agents, r.UserAgent())
		p.jwksReads++
		gate, status, body, published := p.jwksGate, p.jwksStatus, p.jwksBody, p.published
		p.mu.Unlock()
		if gate != nil {
			<-gate
		}
		// An error status still carries the published keys, so only the status
		// tells the answer is not the provider's key set.
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(published)
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func publicKey(key *rsa.PrivateKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: key.Public(), KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}
}

func (p *testProvider) set(edit func(*testProvider)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	edit(p)
}

func (p *testProvider) reads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jwksReads
}

// sign is a token with the header type typ and claims, signed by key under the
// key id kid.
func sign(t *testing.T, typ string, claims map[string]any, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType(jose.ContentType(typ))
	opts.WithHeader("kid", kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, opts)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return serialize(t, signer, claims)
}

func serialize(t *testing.T, signer jose.Signer, claims map[string]any) string {
	t.Helper()
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

// token is an access token for this product signed by the provider's key.
func (p *testProvider) token(t *testing.T) string {
	t.Helper()
	return sign(t, "at+jwt", p.claims(), p.key, testKeyID)
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

	identity, err := p.verifier(t).Verify(context.Background(), p.token(t))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity != (Identity{Subject: "user-uuid", ClientID: "ypl-cli-desk"}) {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestEveryBadTokenIsUnauthorized(t *testing.T) {
	p := newTestProvider(t)
	foreign := newKey(t)
	with := func(edit func(map[string]any)) map[string]any {
		claims := p.claims()
		edit(claims)
		return claims
	}
	hmacSigner, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: make([]byte, 32)}, (&jose.SignerOptions{}).WithType("at+jwt"))
	if err != nil {
		t.Fatalf("new HMAC signer: %v", err)
	}
	cases := map[string]string{
		// An id_token carries the same issuer and signature, and is handed to the
		// client. Only its header type tells it apart.
		"an id_token":                 sign(t, "JWT", p.claims(), p.key, testKeyID),
		"a foreign signing key":       sign(t, "at+jwt", p.claims(), foreign, testKeyID),
		"a foreign key under no kid":  sign(t, "at+jwt", p.claims(), foreign, ""),
		"another signing algorithm":   serialize(t, hmacSigner, p.claims()),
		"an expired token":            sign(t, "at+jwt", with(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), p.key, testKeyID),
		"a token with no expiry":      sign(t, "at+jwt", with(func(c map[string]any) { delete(c, "exp") }), p.key, testKeyID),
		"another issuer":              sign(t, "at+jwt", with(func(c map[string]any) { c["iss"] = "https://other.example" }), p.key, testKeyID),
		"another product's client":    sign(t, "at+jwt", with(func(c map[string]any) { c["client_id"] = "other-cli-desk" }), p.key, testKeyID),
		"a token with no subject":     sign(t, "at+jwt", with(func(c map[string]any) { delete(c, "sub") }), p.key, testKeyID),
		"text that is not a JWT":      "not-a-jwt",
		"a header that is not JSON":   "bm90LWpzb24.e30.sig",
		"a JWT with a fourth segment": p.token(t) + ".extra",
	}
	v := p.verifier(t)
	for name, raw := range cases {
		if _, err := v.Verify(context.Background(), raw); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Verify = %v, want ErrUnauthorized", name, err)
		}
	}
}

// A token under a key id the verifier does not hold makes it read the keys
// again, which is when a provider that has gone away is found.
func TestAProviderWhoseKeysCannotBeReadIsUnavailable(t *testing.T) {
	p := newTestProvider(t)
	v := p.verifier(t)
	rotated := sign(t, "at+jwt", p.claims(), newKey(t), "rotated")
	p.server.Close()

	if _, err := v.Verify(context.Background(), rotated); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Verify with the provider down = %v, want ErrProviderUnavailable", err)
	}
}

func TestKeysTheProviderCannotServeLeaveItUnavailable(t *testing.T) {
	cases := map[string]func(*testProvider){
		"an error status":    func(p *testProvider) { p.jwksStatus = http.StatusInternalServerError },
		"a body not JSON":    func(p *testProvider) { p.jwksBody = "<html>" },
		"no signing key":     func(p *testProvider) { p.jwksBody = `{"keys": []}` },
		"only an encryption": func(p *testProvider) { p.published.Keys[0].Use = "enc" },
	}
	for name, edit := range cases {
		p := newTestProvider(t)
		p.set(edit)
		if _, err := NewVerifier(context.Background(), p.server.URL, "ypl-cli-"); !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("%s: NewVerifier = %v, want ErrProviderUnavailable", name, err)
		}
	}
}

func TestAKeyTheProviderRotatesInIsReadAndAccepted(t *testing.T) {
	p := newTestProvider(t)
	v := p.verifier(t)
	next := newKey(t)
	p.set(func(p *testProvider) {
		p.published.Keys = append(p.published.Keys, publicKey(next, "next"))
	})

	if _, err := v.Verify(context.Background(), sign(t, "at+jwt", p.claims(), next, "next")); err != nil {
		t.Fatalf("Verify under a rotated-in key: %v", err)
	}
	if _, err := v.Verify(context.Background(), sign(t, "at+jwt", p.claims(), next, "next")); err != nil {
		t.Fatalf("Verify under the held rotated-in key: %v", err)
	}
	if got := p.reads(); got != 2 {
		t.Fatalf("JWKS reads = %d, want 2: one at construction and one for the new key", got)
	}
}

// A caller that stops waiting is not the provider going away, and the read it
// was waiting on still finishes for the callers that remain.
func TestACallerThatStopsWaitingIsNotAnUnavailableProvider(t *testing.T) {
	p := newTestProvider(t)
	v := p.verifier(t)
	gate := make(chan struct{})
	p.set(func(p *testProvider) { p.jwksGate = gate })
	defer close(gate)
	rotated := sign(t, "at+jwt", p.claims(), newKey(t), "rotated")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := v.Verify(ctx, rotated)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrProviderUnavailable) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Verify after its caller stopped waiting = %v, want the context's error alone", err)
	}
}

func TestCallersWaitingTogetherShareOneRead(t *testing.T) {
	p := newTestProvider(t)
	v := p.verifier(t)
	gate := make(chan struct{})
	p.set(func(p *testProvider) { p.jwksGate = gate })
	next := newKey(t)
	p.set(func(p *testProvider) { p.published.Keys = append(p.published.Keys, publicKey(next, "next")) })
	raw := sign(t, "at+jwt", p.claims(), next, "next")

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			_, err := v.Verify(context.Background(), raw)
			errs <- err
		})
	}
	// The read in flight holds the gate, so a caller that joins it waits here.
	// Every caller has had time to reach the read before the gate opens.
	for p.reads() < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Verify: %v", err)
		}
	}
	if got := p.reads(); got != 2 {
		t.Fatalf("JWKS reads = %d, want 2: one at construction and one shared by %d callers", got, callers)
	}
}

func TestAnIssuerAdvertisingAnotherNameIsRefused(t *testing.T) {
	p := newTestProvider(t)
	_, err := NewVerifier(context.Background(), p.server.URL+"/", "ypl-cli-")
	if err == nil || errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("NewVerifier with a mismatched issuer = %v, want a refusal that is not an outage", err)
	}
}

// A proxy in front of a provider can refuse a client that names none, so the
// JWKS read names itself as well as discovery.
func TestEveryRequestToTheProviderNamesThisAPI(t *testing.T) {
	p := newTestProvider(t)
	_, _ = p.verifier(t).Verify(context.Background(), p.token(t))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.agents) < 2 {
		t.Fatalf("the provider saw %d requests, want discovery and a JWKS read", len(p.agents))
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
