package jwt_auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters/jwt_auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newCtx() *engine.RequestContext {
	return &engine.RequestContext{
		Headers:          make(map[string]string),
		UpstreamShadow:   make(map[string]string),
		DownstreamShadow: make(map[string]string),
	}
}

// generateHS256Token creates a signed HS256 JWT with the given claims.
func generateHS256Token(secret string, claims map[string]interface{}, expiry time.Duration) string {
	tok, err := jwt.NewBuilder().
		Issuer("test-issuer").
		Subject("user-123").
		Expiration(time.Now().Add(expiry)).
		Claim("email", "user@example.com").
		Claim("role", "admin").
		Build()
	if err != nil {
		panic(err)
	}
	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			panic(err)
		}
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.HS256, []byte(secret)))
	if err != nil {
		panic(err)
	}
	return string(signed)
}

// generateRSAKeys returns a private key and a JWKS server for RS256 tests.
func generateRSAKeys(t *testing.T) (*rsa.PrivateKey, *httptest.Server) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	rawKey, err := jwk.FromRaw(privKey.Public())
	if err != nil {
		t.Fatalf("failed to create JWK from public key: %v", err)
	}
	if err := rawKey.Set(jwk.KeyIDKey, "test-key-id"); err != nil {
		t.Fatalf("failed to set kid: %v", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(rawKey); err != nil {
		t.Fatalf("failed to add key to set: %v", err)
	}

	buf, _ := json.Marshal(set)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf)
	}))
	return privKey, srv
}

func signRS256Token(privKey *rsa.PrivateKey, subject string, expiry time.Duration) string {
	tok, _ := jwt.NewBuilder().
		Issuer("test-issuer").
		Subject(subject).
		Expiration(time.Now().Add(expiry)).
		Claim("email", subject+"@example.com").
		Build()

	// Build a JWK with the kid set so the JWKS lookup can match by key ID.
	privJWK, err := jwk.FromRaw(privKey)
	if err != nil {
		panic(err)
	}
	if err := privJWK.Set(jwk.KeyIDKey, "test-key-id"); err != nil {
		panic(err)
	}

	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, privJWK))
	if err != nil {
		panic(err)
	}
	return string(signed)
}

// ─────────────────────────────────────────────────────────────────────────────
// HS256 local secret tests
// ─────────────────────────────────────────────────────────────────────────────

func TestJWTAuth_HS256_ValidToken(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
		Issuer:      "test-issuer",
		ClaimMappings: map[string]string{
			"sub":   "x-user-id",
			"email": "x-user-email",
			"role":  "x-user-role",
		},
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	if err := f.Execute(ctx); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if ctx.Blocked {
		t.Fatalf("request should not be blocked for valid token")
	}
	if ctx.UpstreamShadow["x-user-id"] != "user-123" {
		t.Fatalf("expected x-user-id=user-123, got %q", ctx.UpstreamShadow["x-user-id"])
	}
	if ctx.UpstreamShadow["x-user-email"] != "user@example.com" {
		t.Fatalf("expected x-user-email=user@example.com, got %q", ctx.UpstreamShadow["x-user-email"])
	}
}

func TestJWTAuth_HS256_ExpiredToken(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, -1*time.Minute) // already expired

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("expired token should result in blocked request")
	}
	if ctx.ResponseStatus != 401 {
		t.Fatalf("expected 401, got %d", ctx.ResponseStatus)
	}
}

func TestJWTAuth_HS256_WrongSecret(t *testing.T) {
	token := generateHS256Token("correct-secret-key-32-bytes-min!", nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: "wrong-secret-key-32-bytes-minXXX",
		Algorithm:   "HS256",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("wrong secret should block the request")
	}
}

func TestJWTAuth_HS256_MissingToken(t *testing.T) {
	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: "super-secret-key-32-bytes-min!!",
		Algorithm:   "HS256",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	// No authorization header

	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("missing token should block the request")
	}
	if ctx.ResponseStatus != 401 {
		t.Fatalf("expected 401, got %d", ctx.ResponseStatus)
	}
}

func TestJWTAuth_HS256_IssuerMismatch(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute) // iss = "test-issuer"

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
		Issuer:      "expected-other-issuer",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("issuer mismatch should block the request")
	}
}

func TestJWTAuth_HS256_StripToken(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
		StripToken:  true,
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("valid token should not be blocked")
	}
	// After stripping, the authorization key should be in HeadersToRemove
	found := false
	for _, k := range ctx.HeadersToRemove {
		if k == "authorization" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'authorization' to be in HeadersToRemove after StripToken=true")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Token extraction tests
// ─────────────────────────────────────────────────────────────────────────────

func TestJWTAuth_TokenFromQueryParam(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
		Source:      "query",
		QueryParam:  "access_token",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Path = "/api/v1/resource?access_token=" + token

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatalf("valid token in query param should not be blocked")
	}
}

func TestJWTAuth_TokenFromCookie(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
		Source:      "cookie",
		CookieName:  "auth_token",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["cookie"] = "session=abc; auth_token=" + token + "; other=xyz"

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatalf("valid token in cookie should not be blocked")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RS256 + JWKS tests
// ─────────────────────────────────────────────────────────────────────────────

func TestJWTAuth_RS256_ValidToken(t *testing.T) {
	privKey, jwksSrv := generateRSAKeys(t)
	defer jwksSrv.Close()

	token := signRS256Token(privKey, "rsa-user-456", 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		JWKSEndpoint:        jwksSrv.URL,
		JWKSRefreshInterval: 1 * time.Hour,
		Algorithm:           "RS256",
		Issuer:              "test-issuer",
		ClaimMappings:       map[string]string{"sub": "x-user-id"},
	})
	if err != nil {
		t.Fatalf("failed to create filter with JWKS: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatalf("RS256 valid token should not be blocked, response: %s", ctx.ResponseBody)
	}
	if ctx.UpstreamShadow["x-user-id"] != "rsa-user-456" {
		t.Fatalf("expected x-user-id=rsa-user-456, got %q", ctx.UpstreamShadow["x-user-id"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Introspection fallback tests
// ─────────────────────────────────────────────────────────────────────────────

func TestJWTAuth_IntrospectionFallback_ActiveToken(t *testing.T) {
	// Introspection server that returns active=true with a subject claim.
	introSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("token") == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true,"sub":"opaque-user-789","email":"opaque@example.com"}`))
	}))
	defer introSrv.Close()

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		// No LocalSecret and no JWKS — forces introspection path
		Algorithm:             "HS256",
		IntrospectionEndpoint: introSrv.URL,
		IntrospectionTimeout:  2 * time.Second,
		ClaimMappings:         map[string]string{"sub": "x-user-id", "email": "x-user-email"},
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer opaque-token-xyz"

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatalf("active introspection token should not be blocked: %s", ctx.ResponseBody)
	}
	if ctx.UpstreamShadow["x-user-id"] != "opaque-user-789" {
		t.Fatalf("expected x-user-id=opaque-user-789, got %q", ctx.UpstreamShadow["x-user-id"])
	}
}

func TestJWTAuth_IntrospectionFallback_InactiveToken(t *testing.T) {
	introSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":false}`))
	}))
	defer introSrv.Close()

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		Algorithm:             "HS256",
		IntrospectionEndpoint: introSrv.URL,
		IntrospectionTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer revoked-token"

	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("inactive token should be blocked")
	}
	if ctx.ResponseStatus != 401 {
		t.Fatalf("expected 401, got %d", ctx.ResponseStatus)
	}
}

func TestJWTAuth_FailOpen_NetworkError(t *testing.T) {
	// Point to a server that's not running
	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		Algorithm:             "HS256",
		IntrospectionEndpoint: "http://127.0.0.1:19999/introspect", // nothing listening here
		IntrospectionTimeout:  200 * time.Millisecond,
		FailOpen:              true, // should allow through on network error
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer some-token"

	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("fail_open=true should allow request through on network error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PhaseAware tests
// ─────────────────────────────────────────────────────────────────────────────

func TestJWTAuth_OnlyRunsOnRequestHeadersPhase(t *testing.T) {
	const secret = "super-secret-key-32-bytes-min!!"
	token := generateHS256Token(secret, nil, 10*time.Minute)

	f, err := jwt_auth.NewFilter(jwt_auth.Config{
		LocalSecret: secret,
		Algorithm:   "HS256",
	})
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	defer f.Close()

	phases := f.SupportedPhases()
	if len(phases) != 1 || phases[0] != engine.PhaseRequestHeaders {
		t.Fatalf("jwt_auth filter should only declare PhaseRequestHeaders, got %v", phases)
	}

	// Verify the executor also respects this
	exec := engine.NewChainExecutor()
	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer " + token
	chain := engine.Chain{f}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestBody) // Should be skipped
	if ctx.Blocked {
		// It was skipped, so no validation happened at all — no block
		t.Fatal("filter should not run on PhaseRequestBody at all")
	}
	// confirm not blocked because filter didn't run (no token validated)
	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)
	if ctx.Blocked {
		t.Fatalf("valid token on PhaseRequestHeaders should not block: %s", ctx.ResponseBody)
	}
}
