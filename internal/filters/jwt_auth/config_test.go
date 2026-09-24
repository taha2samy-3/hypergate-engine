package jwt_auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/taha2samy/hypergate/internal/engine"
)

// The operator emits durations as strings (e.g. "5m"); they must decode.
func TestConfigDecodesDurationStrings(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("jwks_refresh_interval: 5m\nintrospection_timeout: 1500ms\n"), &cfg); err != nil {
		t.Fatalf("duration strings must decode: %v", err)
	}
	if cfg.JWKSRefreshInterval != 5*time.Minute || cfg.IntrospectionTimeout != 1500*time.Millisecond {
		t.Fatalf("unexpected durations: %v %v", cfg.JWKSRefreshInterval, cfg.IntrospectionTimeout)
	}
}

func TestNewFilter_LocalSecretFileDefaultsToHMAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := NewFilter(Config{LocalSecretFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if f.cfg.LocalSecret != "file-secret" || f.cfg.Algorithm != "HS256" {
		t.Fatalf("secret file not loaded or wrong default algorithm: secret=%q alg=%q", f.cfg.LocalSecret, f.cfg.Algorithm)
	}
}

func TestNewFilter_RequiresKeyMaterial(t *testing.T) {
	if _, err := NewFilter(Config{}); err == nil {
		t.Fatal("a filter without JWKS, secret or introspection must be rejected")
	}
}

func newTestCtx(token string) *engine.RequestContext {
	return &engine.RequestContext{
		Headers:          map[string]string{"authorization": "Bearer " + token},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
}

func TestFailOpen_NeverAcceptsInvalidToken(t *testing.T) {
	f, err := NewFilter(Config{LocalSecret: "s3cret", FailOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ctx := newTestCtx("forged.token.value")
	_ = f.Execute(ctx)
	if !ctx.Blocked || ctx.ResponseStatus != 401 {
		t.Fatal("fail_open must not let an invalid token through")
	}
}

func TestFailOpen_InactiveIntrospectionIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"active": false}`))
	}))
	defer srv.Close()
	f, err := NewFilter(Config{IntrospectionEndpoint: srv.URL, FailOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ctx := newTestCtx("opaque")
	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("an inactive token must be rejected even with fail_open")
	}
}

func TestFailOpen_IntrospectionOutageAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	f, err := NewFilter(Config{IntrospectionEndpoint: srv.URL, FailOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ctx := newTestCtx("opaque")
	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("an introspection outage with fail_open should allow the request")
	}

	f2, _ := NewFilter(Config{IntrospectionEndpoint: srv.URL})
	defer func() { _ = f2.Close() }()
	ctx2 := newTestCtx("opaque")
	_ = f2.Execute(ctx2)
	if !ctx2.Blocked {
		t.Fatal("without fail_open an outage must reject")
	}
}
