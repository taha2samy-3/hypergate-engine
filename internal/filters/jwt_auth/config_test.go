package jwt_auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
