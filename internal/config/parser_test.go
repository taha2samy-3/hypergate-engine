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

func TestParseBytes_LowercasesRouteHeaderNames(t *testing.T) {
	cfg, err := config.ParseBytes([]byte(`
version: v1
chains:
  internal: []
router:
  routes:
    - name: internal
      target_chain: internal
      matches:
        - headers:
            X-Internal: "true"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Router.Routes[0].Matches[0].Headers["x-internal"]; !ok {
		t.Fatalf("route header names must be lower-cased, got %v", cfg.Router.Routes[0].Matches[0].Headers)
	}
}

func TestParseBytes_CompilesTrafficAndSelectors(t *testing.T) {
	cfg, err := config.ParseBytes([]byte(`
version: v1
chains:
  public: []
  partner: []
  internal: []
router:
  routes:
    - name: partner-api
      target_chain: partner
      matches:
        - traffic: north_south
          sources: ["cidr:203.0.113.0/24", "ip:192.0.2.7"]
          destinations: ["host:api.example.com", "host:*.partner.example.com"]
  default_chains:
    north_south: public
    east_west: internal
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := cfg.Router.Routes[0].Matches[0]
	if m.CompiledTraffic.String() != "north_south" {
		t.Errorf("traffic = %v", m.CompiledTraffic)
	}
	if got := m.CompiledSources.String(); got != "cidr:203.0.113.0/24,ip:192.0.2.7" {
		t.Errorf("sources = %q", got)
	}
	if got := m.CompiledDestinations.String(); got != "host:api.example.com,host:*.partner.example.com" {
		t.Errorf("destinations = %q", got)
	}
	if cfg.Router.DefaultChains.NorthSouth != "public" || cfg.Router.DefaultChains.EastWest != "internal" {
		t.Errorf("default_chains = %+v", cfg.Router.DefaultChains)
	}
}

func TestParseBytes_RejectsInvalidRoutingConfig(t *testing.T) {
	tests := map[string]struct {
		match   string
		router  string
		errPart string
	}{
		"bad traffic":        {match: `traffic: internal`, errPart: "invalid traffic"},
		"unknown prefix":     {match: `sources: ["pod:shop/a"]`, errPart: "unknown prefix"},
		"wrong side":         {match: `sources: ["host:api.example.com"]`, errPart: "cannot be used as a source"},
		"malformed cidr":     {match: `destinations: ["cidr:10.0.0.1/8"]`, errPart: "host bits"},
		"workload selector":  {match: `sources: ["service:shop/checkout"]`, errPart: "identity map"},
		"undefined ns chain": {router: "default_chains:\n    north_south: missing", errPart: "default_chains.north_south"},
		"undefined ew chain": {router: "default_chains:\n    east_west: missing", errPart: "default_chains.east_west"},
		"error names route":  {match: `sources: ["ip:nope"]`, errPart: `route "r" match index 0`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			match := tt.match
			if match == "" {
				match = "path_prefix: /"
			}
			doc := "version: v1\nchains:\n  c: []\nrouter:\n  routes:\n    - name: r\n      target_chain: c\n      matches:\n        - " + match + "\n"
			if tt.router != "" {
				doc += "  " + tt.router + "\n"
			}
			_, err := config.ParseBytes([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("want error containing %q, got %v", tt.errPart, err)
			}
		})
	}
}
