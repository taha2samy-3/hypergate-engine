package selector

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		raw     string
		side    Side
		want    string // canonical form; empty means an error is expected
		errPart string
	}{
		{raw: "any", side: Source, want: "any"},
		{raw: "any", side: Destination, want: "any"},
		{raw: "ip:10.1.2.3", side: Source, want: "ip:10.1.2.3"},
		{raw: "ip:2001:db8::1", side: Destination, want: "ip:2001:db8::1"},
		{raw: "ip:::ffff:10.0.0.1", side: Source, want: "ip:10.0.0.1"},
		{raw: " cidr:10.0.0.0/8 ", side: Source, want: "cidr:10.0.0.0/8"},
		{raw: "cidr:2001:db8::/32", side: Destination, want: "cidr:2001:db8::/32"},
		{raw: "cidr:::ffff:10.0.0.0/104", side: Source, want: "cidr:10.0.0.0/8"},
		{raw: "host:API.Example.com.", side: Destination, want: "host:api.example.com"},
		{raw: "host:*.example.com", side: Destination, want: "host:*.example.com"},
		{raw: "service:shop/checkout", side: Source, want: "service:shop/checkout"},
		{raw: "service:payments/ledger", side: Destination, want: "service:payments/ledger"},
		{raw: "namespace:shop", side: Destination, want: "namespace:shop"},
		{raw: "sa:shop/checkout", side: Source, want: "sa:shop/checkout"},
		{raw: "sa:shop/checkout.v2", side: Source, want: "sa:shop/checkout.v2"},
		{raw: "service:shop/checkout.v2", side: Source, errPart: "not a valid Kubernetes name"},
		{raw: "spiffe://cluster.local/ns/shop/sa/checkout", side: Source, want: "spiffe://cluster.local/ns/shop/sa/checkout"},
		{raw: "labels:shop/app=checkout,tier=web", side: Source, want: "labels:shop/app=checkout,tier=web"},
		{raw: "labels:app=checkout", side: Source, want: "labels:app=checkout"},
		{raw: "external", side: Source, want: "external"},

		{raw: "pod:shop/x", side: Source, errPart: "unknown prefix"},
		{raw: "", side: Source, errPart: "unknown prefix"},
		{raw: "ip:", side: Source, errPart: "requires a value"},
		{raw: "ip:10.1.2", side: Source, errPart: "invalid IP"},
		{raw: "ip:fe80::1%eth0", side: Source, errPart: "invalid IP"},
		{raw: "cidr:10.0.0.1/8", side: Source, errPart: "did you mean 10.0.0.0/8"},
		{raw: "cidr:10.0.0.0", side: Source, errPart: "invalid CIDR"},
		{raw: "host:api.example.com", side: Source, errPart: "cannot be used as a source"},
		{raw: "host:api.example.com:443", side: Destination, errPart: "invalid host"},
		{raw: "host:foo.*.com", side: Destination, errPart: "invalid host"},
		{raw: "host:*.", side: Destination, errPart: "invalid host"},
		{raw: "host:bad host", side: Destination, errPart: "invalid host"},
		{raw: "sa:shop/checkout", side: Destination, errPart: "cannot be used as a destination"},
		{raw: "spiffe://x/ns/a/sa/b", side: Destination, errPart: "cannot be used as a destination"},
		{raw: "labels:app=x", side: Destination, errPart: "cannot be used as a destination"},
		{raw: "external", side: Destination, errPart: "cannot be used as a destination"},
		{raw: "any:x", side: Source, errPart: "takes no value"},
		{raw: "service:checkout", side: Source, errPart: "<namespace>/<name>"},
		{raw: "service:Shop/checkout", side: Source, errPart: "not a valid Kubernetes name"},
		{raw: "namespace:-shop", side: Source, errPart: "start and end"},
		{raw: "spiffe:cluster.local/ns/a/sa/b", side: Source, errPart: "must be spiffe://"},
		{raw: "spiffe://cluster.local/ns/a", side: Source, errPart: "must be spiffe://"},
		{raw: "labels:app", side: Source, errPart: "key=value"},
	}
	for _, tt := range tests {
		t.Run(tt.side.String()+"/"+tt.raw, func(t *testing.T) {
			sel, err := Parse(tt.raw, tt.side)
			if tt.want == "" {
				if err == nil || !strings.Contains(err.Error(), tt.errPart) {
					t.Fatalf("want error containing %q, got %v (%v)", tt.errPart, err, sel)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := sel.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompileRejectsWorkloadSelectors(t *testing.T) {
	for _, raw := range []string{"service:shop/checkout", "namespace:shop", "sa:shop/a", "spiffe://cluster.local/ns/a/sa/b", "labels:app=x", "external"} {
		_, err := Compile([]string{"cidr:10.0.0.0/8", raw}, Source)
		if err == nil || !strings.Contains(err.Error(), "identity map") {
			t.Errorf("%s: want identity map error, got %v", raw, err)
		}
	}
}

func TestCompileEmptyIsNil(t *testing.T) {
	set, err := Compile(nil, Source)
	if err != nil || set != nil {
		t.Fatalf("got %v, %v", set, err)
	}
	if !set.Match(netip.Addr{}, "") {
		t.Fatal("a nil set must match everything")
	}
}

func TestSetMatch(t *testing.T) {
	mustCompile := func(side Side, list ...string) *Set {
		t.Helper()
		s, err := Compile(list, side)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	addr := netip.MustParseAddr

	src := mustCompile(Source, "ip:192.0.2.7", "cidr:10.0.0.0/8", "cidr:2001:db8::/32")
	dst := mustCompile(Destination, "host:api.example.com", "host:*.internal.example.com", "cidr:10.96.0.0/12")

	tests := []struct {
		name string
		set  *Set
		addr netip.Addr
		host string
		want bool
	}{
		{"exact ip", src, addr("192.0.2.7"), "", true},
		{"other ip", src, addr("192.0.2.8"), "", false},
		{"ipv4 cidr", src, addr("10.200.1.1"), "", true},
		{"ipv6 cidr", src, addr("2001:db8::42"), "", true},
		{"ipv6 outside", src, addr("2001:db9::1"), "", false},
		{"unknown address", src, netip.Addr{}, "", false},
		{"source ignores host", src, netip.Addr{}, "api.example.com", false},

		{"exact host", dst, netip.Addr{}, "api.example.com", true},
		{"exact host only", dst, netip.Addr{}, "www.api.example.com", false},
		{"wildcard one label", dst, netip.Addr{}, "a.internal.example.com", true},
		{"wildcard many labels", dst, netip.Addr{}, "a.b.internal.example.com", true},
		{"wildcard excludes apex", dst, netip.Addr{}, "internal.example.com", false},
		{"wildcard needs dot boundary", dst, netip.Addr{}, "xinternal.example.com", false},
		{"destination cidr", dst, addr("10.96.0.10"), "unrelated", true},
		{"destination nothing", dst, addr("10.0.0.1"), "other.example.com", false},

		{"any", mustCompile(Source, "any"), netip.Addr{}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Match(tt.addr, tt.host); got != tt.want {
				t.Fatalf("Match(%v, %q) = %v, want %v", tt.addr, tt.host, got, tt.want)
			}
		})
	}
}

func TestParseAddr(t *testing.T) {
	tests := map[string]string{
		"10.0.0.1":            "10.0.0.1",
		"10.0.0.1:8080":       "10.0.0.1",
		"[2001:db8::1]:443":   "2001:db8::1",
		"2001:db8::1":         "2001:db8::1",
		"[2001:db8::1]":       "2001:db8::1",
		"::ffff:10.0.0.1":     "10.0.0.1",
		"[::ffff:10.0.0.1]:1": "10.0.0.1",
		"":                    "invalid IP",
		"not-an-ip":           "invalid IP",
		"pipe:/tmp/sock":      "invalid IP",
	}
	for in, want := range tests {
		if got := ParseAddr(in).String(); got != want {
			t.Errorf("ParseAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"API.example.com":      "api.example.com",
		"api.example.com:8443": "api.example.com",
		"api.example.com.":     "api.example.com",
		"[2001:db8::1]:443":    "2001:db8::1",
		"10.0.0.1:80":          "10.0.0.1",
		"":                     "",
	}
	for in, want := range tests {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTraffic(t *testing.T) {
	ok := map[string]Traffic{
		"": TrafficAny, "any": TrafficAny, "ANY": TrafficAny,
		"north_south": TrafficNorthSouth, "north-south": TrafficNorthSouth, "NorthSouth": TrafficNorthSouth,
		"east_west": TrafficEastWest, "east-west": TrafficEastWest, "EastWest": TrafficEastWest,
	}
	for in, want := range ok {
		got, err := ParseTraffic(in)
		if err != nil || got != want {
			t.Errorf("ParseTraffic(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseTraffic("internal"); err == nil {
		t.Error("expected an error for an unknown class")
	}
}
