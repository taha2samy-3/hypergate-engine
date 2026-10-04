package routes

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/selector"
)

func TestCompile_MapsFieldsToEngineConfig(t *testing.T) {
	spec := &hyperv1alpha1.HyperRouteSpec{
		TargetPolicy: "partner",
		Matches: []hyperv1alpha1.MatchRule{{
			Traffic:      hyperv1alpha1.TrafficNorthSouth,
			Sources:      []string{"cidr:203.0.113.0/24"},
			Destinations: []string{"host:api.example.com"},
			PathPrefix:   "/v1",
			Headers:      map[string]string{"X-Tenant": "acme"},
		}, {
			Traffic: hyperv1alpha1.TrafficEastWest,
		}, {
			Traffic: hyperv1alpha1.TrafficAny,
		}},
	}
	got, err := Compile(spec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Traffic != "north_south" || got[1].Traffic != "east_west" || got[2].Traffic != "" {
		t.Fatalf("traffic not mapped: %q %q %q", got[0].Traffic, got[1].Traffic, got[2].Traffic)
	}
	if got[0].Sources[0] != "cidr:203.0.113.0/24" || got[0].Destinations[0] != "host:api.example.com" {
		t.Fatalf("selectors not copied: %+v", got[0])
	}
	if got[0].Headers["x-tenant"].Exact != "acme" {
		t.Fatalf("header names must be lower-cased: %+v", got[0].Headers)
	}

	// The compiled entries must be accepted by the engine as they are.
	for i := range got {
		if err := got[i].Compile(false); err != nil {
			t.Fatalf("engine rejects compiled match %d: %v", i, err)
		}
	}
}

func TestCompile_ReportsEveryProblem(t *testing.T) {
	spec := &hyperv1alpha1.HyperRouteSpec{
		Matches: []hyperv1alpha1.MatchRule{
			{Traffic: "Internal"},
			{Sources: []string{"host:api.example.com"}},
			{Destinations: []string{"sa:shop/checkout"}},
			{Sources: []string{"service:shop/checkout"}},
			{PathRegexPattern: "("},
		},
	}
	_, err := Compile(spec, Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"targetPolicy must name a HyperChain",
		"matches[0]: traffic: invalid value",
		"matches[1]: sources:",
		"cannot be used as a source",
		"matches[2]: destinations:",
		"matches[3]: sources:",
		"identity map",
		"matches[4]: pathRegexPattern",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}

	if err := Validate(&hyperv1alpha1.HyperRouteSpec{TargetPolicy: "x"}, Options{}); err == nil || !strings.Contains(err.Error(), "at least one entry") {
		t.Fatalf("empty matches must be rejected, got %v", err)
	}
}

func TestDefaultChainsAndReferences(t *testing.T) {
	spec := &hyperv1alpha1.HyperConfigSpec{
		DefaultChain:  "public",
		DefaultChains: &hyperv1alpha1.DefaultChains{NorthSouth: "edge", EastWest: "public"},
	}
	if got := DefaultChains(spec); got != (config.DefaultChainsConfig{NorthSouth: "edge", EastWest: "public"}) {
		t.Fatalf("DefaultChains = %+v", got)
	}
	refs := ReferencedChains(spec)
	if len(refs) != 2 || refs["public"] != "defaultChain" || refs["edge"] != "defaultChains.northSouth" {
		t.Fatalf("ReferencedChains = %v", refs)
	}
	if got := DefaultChains(&hyperv1alpha1.HyperConfigSpec{}); got != (config.DefaultChainsConfig{}) {
		t.Fatalf("nil defaultChains must compile to empty, got %+v", got)
	}
}

// The CRD schema patterns must equal the Go constants (both CRD copies) and agree
// with the engine parser on which side each selector kind is allowed.
func TestCRDSelectorPatterns(t *testing.T) {
	for _, path := range []string{
		"../../../charts/hyper-operator/crds/hyperroute_crd.yaml",
		"../../deploy/CRDs/hyperroute_crd.yaml",
	} {
		src, dst := crdPatterns(t, path)
		if src != hyperv1alpha1.SourceSelectorPattern {
			t.Errorf("%s: sources pattern differs from SourceSelectorPattern", path)
		}
		if dst != hyperv1alpha1.DestinationSelectorPattern {
			t.Errorf("%s: destinations pattern differs from DestinationSelectorPattern", path)
		}
	}

	srcRe := regexp.MustCompile(hyperv1alpha1.SourceSelectorPattern)
	dstRe := regexp.MustCompile(hyperv1alpha1.DestinationSelectorPattern)
	corpus := []string{
		"any", "external", "ip:10.1.2.3", "ip:2001:db8::1", "cidr:10.0.0.0/8", "cidr:2001:db8::/32",
		"host:api.example.com", "host:*.example.com", "service:shop/checkout", "namespace:shop",
		"sa:shop/checkout", "sa:shop/checkout.v2", "spiffe://cluster.local/ns/shop/sa/checkout",
		"labels:app=checkout", "labels:shop/app=checkout,tier=web",
	}
	for _, raw := range corpus {
		for _, side := range []struct {
			side selector.Side
			re   *regexp.Regexp
		}{{selector.Source, srcRe}, {selector.Destination, dstRe}} {
			_, err := selector.Parse(raw, side.side)
			if parsed, matched := err == nil, side.re.MatchString(raw); parsed != matched {
				t.Errorf("%s as %s: engine parser accepts=%v, CRD pattern accepts=%v", raw, side.side, parsed, matched)
			}
		}
	}
	for _, bad := range []string{"pod:shop/a", "ip:", "host:a b", "service:checkout", "any:x", "spiffe:cluster.local"} {
		if srcRe.MatchString(bad) && dstRe.MatchString(bad) {
			t.Errorf("%q should fail at least one pattern", bad)
		}
	}
}

func crdPatterns(t *testing.T, path string) (src, dst string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]struct {
							Properties map[string]struct {
								Items struct {
									Properties map[string]struct {
										Items struct {
											Pattern string `yaml:"pattern"`
										} `yaml:"items"`
									} `yaml:"properties"`
								} `yaml:"items"`
							} `yaml:"properties"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	match := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["matches"].Items.Properties
	return match["sources"].Items.Pattern, match["destinations"].Items.Pattern
}

func TestCompile_WorkloadSelectorsNeedIdentity(t *testing.T) {
	spec := &hyperv1alpha1.HyperRouteSpec{TargetPolicy: "c", Matches: []hyperv1alpha1.MatchRule{{
		Sources: []string{"sa:shop/checkout", "labels:app=web"}, Destinations: []string{"service:payments/ledger"},
	}}}
	if err := Validate(spec, Options{}); err == nil || !strings.Contains(err.Error(), "identity map") {
		t.Fatalf("expected rejection without identity, got %v", err)
	}
	got, err := Compile(spec, Options{Workloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := got[0].Compile(true); err != nil {
		t.Fatalf("engine rejects compiled workload selectors: %v", err)
	}
	if UnknownSource(&hyperv1alpha1.HyperConfigSpec{}) != "deny" ||
		UnknownSource(&hyperv1alpha1.HyperConfigSpec{UnknownSource: hyperv1alpha1.UnknownSourceDefault}) != "default" {
		t.Fatal("UnknownSource mapping")
	}
}
