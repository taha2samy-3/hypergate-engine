// Package clientip resolves the real downstream client address for a request
// without trusting client-controlled headers.
//
// The address chain is built from X-Forwarded-For followed by the peer address
// Envoy observed on the downstream connection (the ext_proc "source.address"
// attribute). The client is the entry TrustedHops positions from the right, so
// with TrustedHops = 0 the client is the direct peer of Envoy, and with
// TrustedHops = 1 it is the address appended by one trusted proxy/load balancer
// in front of Envoy. Entries further left can be forged by the client and are
// never used.
package clientip

import (
	"net"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// SourceAddressAttribute is the ext_proc attribute carrying Envoy's downstream peer.
// Configure it on the Envoy ext_proc filter with `request_attributes: ["source.address"]`.
const SourceAddressAttribute = "source.address"

// Resolve returns the client IP given the raw X-Forwarded-For header value, the
// peer address reported by Envoy (may be empty) and the number of trusted proxy hops.
//
// When the peer address is unknown (Envoy not configured to send source.address)
// the rightmost X-Forwarded-For entry is used as the peer. That entry is
// trustworthy only when Envoy runs with `use_remote_address: true`, which makes
// Envoy append the address it observed.
func Resolve(xff, peer string, trustedHops int) string {
	if trustedHops < 0 {
		trustedHops = 0
	}

	chain := splitXFF(xff)
	if peer = stripPort(peer); peer != "" {
		// Envoy with use_remote_address already appended the peer; don't count it twice.
		if len(chain) == 0 || chain[len(chain)-1] != peer {
			chain = append(chain, peer)
		}
	}
	if len(chain) == 0 {
		return ""
	}

	idx := len(chain) - 1 - trustedHops
	if idx < 0 {
		idx = 0
	}
	return chain[idx]
}

// PeerFromAttributes extracts source.address from the ext_proc attributes map.
// Envoy keys the map by filter namespace, so every entry is searched.
func PeerFromAttributes(attrs map[string]*structpb.Struct) string {
	for _, s := range attrs {
		if s == nil {
			continue
		}
		if v, ok := s.GetFields()[SourceAddressAttribute]; ok {
			if str := v.GetStringValue(); str != "" {
				return str
			}
		}
	}
	return ""
}

func splitXFF(xff string) []string {
	if xff == "" {
		return nil
	}
	parts := strings.Split(xff, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = stripPort(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripPort removes an optional port (and IPv6 brackets) from an address.
func stripPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
}
