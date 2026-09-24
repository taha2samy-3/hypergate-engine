package clientip_test

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/taha2samy/hypergate/internal/clientip"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		xff  string
		peer string
		hops int
		want string
	}{
		{"peer only", "", "10.0.0.5:4242", 0, "10.0.0.5"},
		{"spoofed xff ignored with zero hops", "6.6.6.6", "10.0.0.5:4242", 0, "10.0.0.5"},
		{"one trusted proxy", "6.6.6.6, 1.2.3.4", "10.0.0.9:1000", 1, "1.2.3.4"},
		{"peer already appended by envoy", "1.2.3.4, 10.0.0.9", "10.0.0.9:1000", 1, "1.2.3.4"},
		{"hops beyond chain clamp to leftmost", "1.2.3.4", "10.0.0.9:1", 5, "1.2.3.4"},
		{"no peer uses rightmost xff", "6.6.6.6, 1.2.3.4", "", 0, "1.2.3.4"},
		{"ipv6 peer", "", "[2001:db8::1]:443", 0, "2001:db8::1"},
		{"nothing known", "", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientip.Resolve(tc.xff, tc.peer, tc.hops); got != tc.want {
				t.Fatalf("Resolve(%q, %q, %d) = %q, want %q", tc.xff, tc.peer, tc.hops, got, tc.want)
			}
		})
	}
}

func TestPeerFromAttributes(t *testing.T) {
	s, err := structpb.NewStruct(map[string]interface{}{"source.address": "10.1.2.3:5555"})
	if err != nil {
		t.Fatal(err)
	}
	attrs := map[string]*structpb.Struct{"envoy.filters.http.ext_proc": s}
	if got := clientip.PeerFromAttributes(attrs); got != "10.1.2.3:5555" {
		t.Fatalf("got %q", got)
	}
	if got := clientip.PeerFromAttributes(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
