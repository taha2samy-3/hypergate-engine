// Package selector parses and matches the typed source and destination
// references used by routes (`prefix:value`, see
// docs/design/identity-aware-routing.md section 4).
//
// Network selectors (ip:, cidr:, host:, any) are matched directly from the
// request. Workload selectors (service:, namespace:, sa:, spiffe:, labels:,
// external) are parsed and validated, but compiling them is rejected until the
// engine receives the workload identity map.
package selector

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Side says whether a selector constrains the caller or the target of a request.
type Side uint8

const (
	Source Side = iota
	Destination
)

func (s Side) String() string {
	if s == Destination {
		return "destination"
	}
	return "source"
}

// Kind is the prefix of a selector.
type Kind uint8

const (
	KindAny Kind = iota
	KindIP
	KindCIDR
	KindHost
	KindExternal
	KindService
	KindNamespace
	KindSA
	KindSPIFFE
	KindLabels
)

type kindInfo struct {
	name        string
	source      bool
	destination bool
	needsValue  bool
	workload    bool
}

var kinds = map[Kind]kindInfo{
	KindAny:       {name: "any", source: true, destination: true},
	KindIP:        {name: "ip", source: true, destination: true, needsValue: true},
	KindCIDR:      {name: "cidr", source: true, destination: true, needsValue: true},
	KindHost:      {name: "host", destination: true, needsValue: true},
	KindExternal:  {name: "external", source: true, workload: true},
	KindService:   {name: "service", source: true, destination: true, needsValue: true, workload: true},
	KindNamespace: {name: "namespace", source: true, destination: true, needsValue: true, workload: true},
	KindSA:        {name: "sa", source: true, needsValue: true, workload: true},
	KindSPIFFE:    {name: "spiffe", source: true, needsValue: true, workload: true},
	KindLabels:    {name: "labels", source: true, needsValue: true, workload: true},
}

var kindByName = func() map[string]Kind {
	m := make(map[string]Kind, len(kinds))
	for k, info := range kinds {
		m[info.name] = k
	}
	return m
}()

func (k Kind) String() string { return kinds[k].name }

// Workload reports whether the selector needs the workload identity map.
func (k Kind) Workload() bool { return kinds[k].workload }

// Selector is one parsed `prefix:value` reference.
type Selector struct {
	Kind Kind
	// Value is the normalized value (empty for any and external).
	Value string

	addr   netip.Addr
	prefix netip.Prefix
	// host is the lower-cased host; for wildcards it is the suffix including the leading dot.
	host     string
	wildcard bool
}

// String returns the canonical `prefix:value` form.
func (s Selector) String() string {
	if s.Value == "" {
		return s.Kind.String()
	}
	return s.Kind.String() + ":" + s.Value
}

// Parse parses one selector for the given side. An unknown prefix, a malformed
// value, or a prefix that is not allowed on that side is an error.
func Parse(raw string, side Side) (Selector, error) {
	raw = strings.TrimSpace(raw)
	name, value, hasValue := strings.Cut(raw, ":")
	kind, ok := kindByName[name]
	if !ok {
		return Selector{}, fmt.Errorf("selector %q: unknown prefix %q (expected one of any, ip, cidr, host, external, service, namespace, sa, spiffe, labels)", raw, name)
	}
	info := kinds[kind]
	if side == Source && !info.source || side == Destination && !info.destination {
		return Selector{}, fmt.Errorf("selector %q: %s: cannot be used as a %s", raw, name, side)
	}
	if !info.needsValue {
		if hasValue {
			return Selector{}, fmt.Errorf("selector %q: %s takes no value", raw, name)
		}
		return Selector{Kind: kind}, nil
	}
	if value == "" {
		return Selector{}, fmt.Errorf("selector %q: %s: requires a value", raw, name)
	}

	sel := Selector{Kind: kind, Value: value}
	var err error
	switch kind {
	case KindIP:
		err = sel.parseIP(value)
	case KindCIDR:
		err = sel.parseCIDR(value)
	case KindHost:
		err = sel.parseHost(value)
	case KindService, KindSA:
		err = validateNamespacedName(value)
	case KindNamespace:
		err = validateDNSLabel(value)
	case KindSPIFFE:
		err = validateSPIFFE(value)
	case KindLabels:
		err = validateLabels(value)
	}
	if err != nil {
		return Selector{}, fmt.Errorf("selector %q: %w", raw, err)
	}
	return sel, nil
}

func (s *Selector) parseIP(v string) error {
	addr, err := netip.ParseAddr(v)
	if err != nil || addr.Zone() != "" {
		return fmt.Errorf("invalid IP address %q", v)
	}
	s.addr = addr.Unmap()
	s.Value = s.addr.String()
	return nil
}

func (s *Selector) parseCIDR(v string) error {
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q", v)
	}
	if p.Masked() != p {
		return fmt.Errorf("CIDR %q has host bits set, did you mean %s?", v, p.Masked())
	}
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		if p.Bits() < 0 {
			return fmt.Errorf("invalid CIDR %q", v)
		}
	}
	s.prefix = p
	s.Value = p.String()
	return nil
}

func (s *Selector) parseHost(v string) error {
	h := strings.TrimSuffix(strings.ToLower(v), ".")
	name := h
	if strings.HasPrefix(h, "*.") {
		s.wildcard = true
		name = h[2:]
	}
	if name == "" || strings.ContainsAny(name, "*:/") {
		return fmt.Errorf("invalid host %q (use a host name without port, optionally starting with \"*.\")", v)
	}
	for _, label := range strings.Split(name, ".") {
		if err := validateHostLabel(label); err != nil {
			return fmt.Errorf("invalid host %q: %w", v, err)
		}
	}
	if s.wildcard {
		s.host = "." + name
	} else {
		s.host = name
	}
	s.Value = h
	return nil
}

func validateHostLabel(l string) error {
	if l == "" || len(l) > 63 {
		return fmt.Errorf("label %q must be 1-63 characters", l)
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !isLowerAlnum(c) && c != '-' && c != '_' {
			return fmt.Errorf("label %q contains %q", l, c)
		}
	}
	return nil
}

// validateDNSLabel checks a Kubernetes namespace or object name segment (RFC 1123 label).
func validateDNSLabel(v string) error {
	if len(v) == 0 || len(v) > 63 {
		return fmt.Errorf("%q must be 1-63 characters", v)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !isLowerAlnum(c) && c != '-' {
			return fmt.Errorf("%q is not a valid Kubernetes name (lower-case letters, digits and '-')", v)
		}
	}
	if v[0] == '-' || v[len(v)-1] == '-' {
		return fmt.Errorf("%q must start and end with a letter or digit", v)
	}
	return nil
}

func isLowerAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

func validateNamespacedName(v string) error {
	ns, name, ok := strings.Cut(v, "/")
	if !ok {
		return fmt.Errorf("%q must be <namespace>/<name>", v)
	}
	if err := validateDNSLabel(ns); err != nil {
		return err
	}
	return validateDNSLabel(name)
}

func validateSPIFFE(v string) error {
	rest, ok := strings.CutPrefix(v, "//")
	if !ok {
		return fmt.Errorf("%q must be spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>", "spiffe:"+v)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || parts[0] == "" || parts[1] != "ns" || parts[3] != "sa" {
		return fmt.Errorf("%q must be spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>", "spiffe:"+v)
	}
	if err := validateDNSLabel(parts[2]); err != nil {
		return err
	}
	return validateDNSLabel(parts[4])
}

func validateLabels(v string) error {
	if ns, rest, ok := strings.Cut(v, "/"); ok && !strings.Contains(ns, "=") {
		if err := validateDNSLabel(ns); err != nil {
			return err
		}
		v = rest
	}
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return fmt.Errorf("label %q must be key=value", pair)
		}
		if strings.ContainsAny(k+val, " \t") {
			return fmt.Errorf("label %q must not contain spaces", pair)
		}
	}
	return nil
}

// Set is a compiled list of selectors for one side. Entries are alternatives:
// the set matches when any entry matches. A nil Set has no constraint.
type Set struct {
	any      bool
	addrs    map[netip.Addr]struct{}
	prefixes []netip.Prefix
	hosts    map[string]struct{}
	suffixes []string
	raw      []string
}

// Compile parses and compiles a selector list. Workload selectors are rejected
// until the engine receives the workload identity map.
func Compile(list []string, side Side) (*Set, error) {
	if len(list) == 0 {
		return nil, nil
	}
	set := &Set{}
	for _, raw := range list {
		sel, err := Parse(raw, side)
		if err != nil {
			return nil, err
		}
		if sel.Kind.Workload() {
			return nil, fmt.Errorf("selector %q: %s selectors require the workload identity map, which this engine version does not receive yet; use ip:, cidr:, host: or any", raw, sel.Kind)
		}
		set.raw = append(set.raw, sel.String())
		switch sel.Kind {
		case KindAny:
			set.any = true
		case KindIP:
			if set.addrs == nil {
				set.addrs = make(map[netip.Addr]struct{})
			}
			set.addrs[sel.addr] = struct{}{}
		case KindCIDR:
			set.prefixes = append(set.prefixes, sel.prefix)
		case KindHost:
			if sel.wildcard {
				set.suffixes = append(set.suffixes, sel.host)
			} else {
				if set.hosts == nil {
					set.hosts = make(map[string]struct{})
				}
				set.hosts[sel.host] = struct{}{}
			}
		}
	}
	return set, nil
}

// String returns the canonical selectors of the set.
func (s *Set) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.raw, ",")
}

// Match reports whether addr (an invalid Addr when unknown) or host (normalized
// with NormalizeHost, empty when unknown) satisfies any selector of the set.
// A nil set always matches.
func (s *Set) Match(addr netip.Addr, host string) bool {
	if s == nil || s.any {
		return true
	}
	if addr.IsValid() {
		if _, ok := s.addrs[addr]; ok {
			return true
		}
		for _, p := range s.prefixes {
			if p.Contains(addr) {
				return true
			}
		}
	}
	if host != "" {
		if _, ok := s.hosts[host]; ok {
			return true
		}
		for _, suffix := range s.suffixes {
			if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// ParseAddr parses an address as reported by Envoy ("10.0.0.1", "10.0.0.1:8080",
// "[2001:db8::1]:443") into a comparable form. It returns an invalid Addr when
// the input is empty or not an IP address.
func ParseAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap()
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	if err != nil {
		return netip.Addr{}
	}
	return addr.WithZone("").Unmap()
}

// NormalizeHost lower-cases an :authority value and strips the port and a trailing dot.
func NormalizeHost(authority string) string {
	h := strings.TrimSpace(authority)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else {
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}
