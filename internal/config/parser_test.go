package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
)

// The integration-test configs must stay valid under the parser's validation rules.
func TestRepoConfigsParse(t *testing.T) {
	files, err := filepath.Glob("../../tests/*/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no integration configs found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.ParseBytes(data); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestParseBytes_RejectsRouteToUndefinedChain(t *testing.T) {
	_, err := config.ParseBytes([]byte(`
version: v1
chains:
  public: []
router:
  default_chain: public
  routes:
    - name: admin
      target_chain: admin-chain
      matches:
        - path_prefix: /admin
`))
	if err == nil || !strings.Contains(err.Error(), "undefined chain") {
		t.Fatalf("expected undefined chain error, got %v", err)
	}
}

func TestParseBytes_RejectsUndefinedDefaultChain(t *testing.T) {
	_, err := config.ParseBytes([]byte(`
version: v1
chains:
  public: []
router:
  default_chain: missing
`))
	if err == nil || !strings.Contains(err.Error(), "default_chain") {
		t.Fatalf("expected default_chain error, got %v", err)
	}
}

func TestParseBytes_Defaults(t *testing.T) {
	cfg, err := config.ParseBytes([]byte("version: v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.HealthAddress != ":9003" {
		t.Fatalf("health address default: %q", cfg.Server.HealthAddress)
	}
	if cfg.Server.PprofAddress != "" {
		t.Fatalf("pprof must be disabled by default, got %q", cfg.Server.PprofAddress)
	}
}
