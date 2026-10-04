// Package selector parses and matches the typed source and destination
// references used by routes (`prefix:value`, see
// docs/design/identity-aware-routing.md section 4).
//
// Network selectors (ip:, cidr:, host:, any) are matched directly from the
// request. Workload selectors (service:, namespace:, sa:, spiffe:, labels:,
// external) are matched against the workload identity map, so compiling them
// requires the engine to receive that map.
package selector

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/taha2samy/hypergate/internal/identity"
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
	// labels: optional namespace and the required key/value pairs.
	labelNS string
	labelKV map[string]string
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
	case KindService:
		err = validateNamespacedName(value, validateDNSLabel)
	case KindSA:
		err = validateNamespacedName(value, validateDNSSubdomain)
	case KindNamespace:
		err = validateDNSLabel(value)
	case KindSPIFFE:
		err = validateSPIFFE(value)
	case KindLabels:
		sel.labelNS, sel.labelKV, err = parseLabels(value)
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

// validateDNSSubdomain checks a name that may contain dots (RFC 1123 subdomain),
// such as a ServiceAccount name.
func validateDNSSubdomain(v string) error {
	if len(v) == 0 || len(v) > 253 {
		return fmt.Errorf("%q must be 1-253 characters", v)
	}
	for _, label := range strings.Split(v, ".") {
		if err := validateDNSLabel(label); err != nil {
			return fmt.Errorf("%q is not a valid Kubernetes name: %w", v, err)
		}
	}
	return nil
}

func validateNamespacedName(v string, validateName func(string) error) error {
	ns, name, ok := strings.Cut(v, "/")
	if !ok {
		return fmt.Errorf("%q must be <namespace>/<name>", v)
	}
	if err := validateDNSLabel(ns); err != nil {
		return err
	}
	return validateName(name)
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
	return validateDNSSubdomain(parts[4])
}

func parseLabels(v string) (string, map[string]string, error) {
	var ns string
	if before, rest, ok := strings.Cut(v, "/"); ok && !strings.Contains(before, "=") {
		if err := validateDNSLabel(before); err != nil {
			return "", nil, err
		}
		ns, v = before, rest
	}
	kv := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return "", nil, fmt.Errorf("label %q must be key=value", pair)
		}
		if strings.ContainsAny(k+val, " \t") {
			return "", nil, fmt.Errorf("label %q must not contain spaces", pair)
		}
		kv[k] = val
	}
	return ns, kv, nil
}

// Set is a compiled list of selectors for one side. Entries are alternatives:
// the set matches when any entry matches. A nil Set has no constraint.
type Set struct {
	any      bool
	addrs    map[netip.Addr]struct{}
	prefixes []netip.Prefix
	hosts    map[string]struct{}
	suffixes []string

	// Workload selectors.
	services   map[string]struct{} // "<ns>/<name>"
	namespaces map[string]struct{}
	sas        map[string]struct{} // "<ns>/<name>"
	spiffe     map[string]struct{}
	labels     []labelSelector
	external   bool

	raw []string
}

type labelSelector struct {
	namespace string
	kv        map[string]string
}

// UsesWorkloads reports whether the set has a workload selector.
func (s *Set) UsesWorkloads() bool {
	return s != nil && (s.external || len(s.services) > 0 || len(s.namespaces) > 0 ||
		len(s.sas) > 0 || len(s.spiffe) > 0 || len(s.labels) > 0)
}

// Compile parses and compiles a selector list. Workload selectors are accepted
// only when allowWorkloads is true, that is when the engine receives the
// workload identity map.
func Compile(list []string, side Side, allowWorkloads bool) (*Set, error) {
	if len(list) == 0 {
		return nil, nil
	}
	set := &Set{}
	for _, raw := range list {
		sel, err := Parse(raw, side)
		if err != nil {
			return nil, err
		}
		if sel.Kind.Workload() && !allowWorkloads {
			return nil, fmt.Errorf("selector %q: %s selectors require the workload identity map; enable identity (engine: identity.enabled, operator: the identity server) or use ip:, cidr:, host: or any", raw, sel.Kind)
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
				set.hosts = addKey(set.hosts, sel.host)
			}
		case KindService:
			set.services = addKey(set.services, sel.Value)
		case KindNamespace:
			set.namespaces = addKey(set.namespaces, sel.Value)
		case KindSA:
			set.sas = addKey(set.sas, sel.Value)
		case KindSPIFFE:
			set.spiffe = addKey(set.spiffe, "spiffe:"+sel.Value)
		case KindLabels:
			set.labels = append(set.labels, labelSelector{namespace: sel.labelNS, kv: sel.labelKV})
		case KindExternal:
			set.external = true
		}
	}
	return set, nil
}

func addKey(m map[string]struct{}, k string) map[string]struct{} {
	if m == nil {
		m = make(map[string]struct{})
	}
	m[k] = struct{}{}
	return m
}

// MatchSource reports whether the caller satisfies any selector. w is the
// caller's workload from the identity map (nil when unknown). external matches
// only when w is nil and externalAllowed is true; the router disallows it for
// east-west callers that are merely missing from the map.
func (s *Set) MatchSource(addr netip.Addr, w *identity.Workload, externalAllowed bool) bool {
	if s == nil || s.any || s.Match(addr, "") {
		return true
	}
	if w == nil {
		return s.external && externalAllowed
	}
	if !w.IsPod() {
		return false
	}
	if _, ok := s.namespaces[w.Namespace]; ok {
		return true
	}
	if _, ok := s.sas[w.Namespace+"/"+w.ServiceAccount]; ok {
		return true
	}
	if _, ok := s.spiffe[w.SPIFFEID]; ok && w.SPIFFEID != "" {
		return true
	}
	for _, svc := range w.Services {
		if _, ok := s.services[svc]; ok {
			return true
		}
	}
	for _, l := range s.labels {
		if l.matches(w) {
			return true
		}
	}
	return false
}

func (l labelSelector) matches(w *identity.Workload) bool {
	if l.namespace != "" && l.namespace != w.Namespace {
		return false
	}
	for k, v := range l.kv {
		if got, ok := w.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// MatchDestination reports whether the target satisfies any selector. services
// are the destination Services ("<ns>/<name>") the engine resolved.
func (s *Set) MatchDestination(addr netip.Addr, host string, services []string) bool {
	if s == nil || s.any || s.Match(addr, host) {
		return true
	}
	for _, svc := range services {
		if _, ok := s.services[svc]; ok {
			return true
		}
		ns, _, _ := strings.Cut(svc, "/")
		if _, ok := s.namespaces[ns]; ok {
			return true
		}
	}
	return false
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
