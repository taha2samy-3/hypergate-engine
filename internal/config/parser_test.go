package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/filters"
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

// docsCodeBlock returns the YAML code block with the given title from a docs page.
func docsCodeBlock(t *testing.T, page, title string) []byte {
	t.Helper()
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	start := "```yaml title=\"" + title + "\"\n"
	_, rest, ok := strings.Cut(string(data), start)
	if !ok {
		t.Fatalf("%s: no code block titled %q", page, title)
	}
	block, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatalf("%s: unterminated code block %q", page, title)
	}
	return []byte(block)
}

// The engine example on the filter chains page must stay a valid configuration.
func TestDocsFilterChainExampleParses(t *testing.T) {
	cfg, err := config.ParseBytes(docsCodeBlock(t, "../../website/docs/concepts/filter-chains.md", "config.yaml"))
	if err != nil {
		t.Fatalf("docs example does not parse: %v", err)
	}
	var types []string
	for _, f := range cfg.Chains["web-api"] {
		types = append(types, f.Type)
	}
	want := "cors,correlation_id,jwt_auth,redis_metadata_enricher,embedded_rate_limiter,header_modifier"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("web-api chain = %s, want %s", got, want)
	}
	if cfg.Router.DefaultChain != "public" || len(cfg.Router.Routes) != 2 {
		t.Fatalf("router = %+v", cfg.Router)
	}
	// Filters that need neither Redis nor the network are built for real.
	for name, chain := range cfg.Chains {
		for _, f := range chain {
			switch f.Type {
			case "cors", "correlation_id", "header_modifier", "deny":
				if _, err := filters.CreateFilter(f.Type, f.Options, nil); err != nil {
					t.Errorf("chain %s: %s options rejected: %v", name, f.Type, err)
				}
			}
		}
	}
}

func TestParseBytes_IdentityAndWorkloadSelectors(t *testing.T) {
	base := "version: v1\nchains:\n  c: []\n"
	route := "router:\n  routes:\n    - name: r\n      target_chain: c\n      matches:\n        - sources: [\"service:shop/checkout\", \"sa:shop/checkout\"]\n          destinations: [\"service:payments/ledger\"]\n"

	if _, err := config.ParseBytes([]byte(base + route)); err == nil || !strings.Contains(err.Error(), "identity map") {
		t.Fatalf("workload selectors without identity must be rejected, got %v", err)
	}

	withID := base + "identity:\n  enabled: true\n  address: hyper-operator-identity.hyper-system.svc:9444\n  ca_file: /etc/hypergate/identity/ca.crt\n" + route
	cfg, err := config.ParseBytes([]byte(withID))
	if err != nil {
		t.Fatalf("workload selectors with identity rejected: %v", err)
	}
	if cfg.Identity.TokenFile != config.DefaultIdentityTokenFile || cfg.Router.UnknownSource != config.UnknownSourceDeny {
		t.Fatalf("defaults not applied: %+v %q", cfg.Identity, cfg.Router.UnknownSource)
	}
	if !cfg.Router.Routes[0].Matches[0].CompiledSources.UsesWorkloads() {
		t.Fatal("sources not compiled as workload selectors")
	}

	for name, tt := range map[string]struct{ doc, errPart string }{
		"no address":     {"identity:\n  enabled: true\n  ca_file: /ca\n", "identity.address"},
		"no CA":          {"identity:\n  enabled: true\n  address: x:1\n", "identity.ca_file"},
		"bad unknown":    {"router:\n  unknown_source: allow\n", "router.unknown_source"},
		"insecure is ok": {"identity:\n  enabled: true\n  address: x:1\n  insecure: true\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.ParseBytes([]byte(base + tt.doc))
			if tt.errPart == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("want %q, got %v", tt.errPart, err)
			}
		})
	}
}

func TestParseBytes_ShutdownDefaultsAndValidation(t *testing.T) {
	cfg, err := config.ParseBytes([]byte("version: v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	sh := cfg.Server.Shutdown
	if sh.QuietPeriodDuration != config.DefaultShutdownQuietPeriod || sh.MinDelayDuration != config.DefaultShutdownMinDelay ||
		sh.MaxDelayDuration != config.DefaultShutdownMaxDelay || sh.DrainTimeoutDuration != config.DefaultShutdownDrainTimeout {
		t.Fatalf("defaults not applied: %+v", sh)
	}

	cfg, err = config.ParseBytes([]byte("version: v1\nserver:\n  shutdown:\n    quiet_period: 1s\n    min_delay: 0s\n    max_delay: 5s\n    drain_timeout: 7s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Shutdown.MaxDelayDuration.Seconds() != 5 || cfg.Server.Shutdown.MinDelayDuration != 0 {
		t.Fatalf("explicit values not parsed: %+v", cfg.Server.Shutdown)
	}

	for doc, want := range map[string]string{
		"version: v1\nserver:\n  shutdown:\n    quiet_period: soon\n":             "quiet_period",
		"version: v1\nserver:\n  shutdown:\n    drain_timeout: -1s\n":             "drain_timeout",
		"version: v1\nserver:\n  shutdown:\n    min_delay: 10s\n    max_delay: 5s\n": "must not exceed",
	} {
		if _, err := config.ParseBytes([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", doc, want, err)
		}
	}
}
