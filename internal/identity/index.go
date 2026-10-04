// Package identity is the engine side of workload identity distribution
// (docs/design/identity-distribution.md): a client that receives the IP →
// workload map from the hyper-operator leader over delta xDS, and the local
// index requests are matched against.
package identity

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	identityv1 "github.com/taha2samy/hypergate/internal/identityapi/v1"
)

// Type URLs of the identity resources.
const (
	WorkloadTypeURL = "type.googleapis.com/hypergate.identity.v1.Workload"
	ServiceTypeURL  = "type.googleapis.com/hypergate.identity.v1.Service"
)

// Kind says what an IP belongs to.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindPod
	KindNode
)

func (k Kind) String() string {
	switch k {
	case KindPod:
		return "pod"
	case KindNode:
		return "node"
	}
	return "unknown"
}

// Workload is the identity behind one IP. Values are immutable once stored, so
// pointers returned by the index are safe to keep.
type Workload struct {
	IP               netip.Addr
	Kind             Kind
	Namespace        string
	Pod              string
	PodUID           string
	ServiceAccount   string
	SPIFFEID         string
	Node             string
	Labels           map[string]string
	Services         []string // "<namespace>/<name>", sorted
	SecurityIdentity uint32
}

// IsPod reports whether the workload is a pod (not a node address).
func (w *Workload) IsPod() bool { return w != nil && w.Kind == KindPod }

// Service is a Service and its cluster IPs.
type Service struct {
	Namespace  string
	Name       string
	ClusterIPs []netip.Addr
	Ports      []uint32
}

// Key returns "<namespace>/<name>".
func (s *Service) Key() string { return s.Namespace + "/" + s.Name }

// Index is the engine's local copy of the identity map. It is safe for
// concurrent use; lookups take a read lock only.
type Index struct {
	mu          sync.RWMutex
	workloads   map[netip.Addr]*Workload
	services    map[string]*Service
	byClusterIP map[netip.Addr]*Service
	versions    map[string]map[string]string // type URL -> resource name -> version
	synced      map[string]bool
	lastUpdate  time.Time
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		workloads:   map[netip.Addr]*Workload{},
		services:    map[string]*Service{},
		byClusterIP: map[netip.Addr]*Service{},
		versions:    map[string]map[string]string{WorkloadTypeURL: {}, ServiceTypeURL: {}},
		synced:      map[string]bool{},
	}
}

// Lookup returns the workload behind ip, or nil.
func (x *Index) Lookup(ip netip.Addr) *Workload {
	if x == nil || !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.workloads[ip]
}

// ServiceByClusterIP returns the Service that owns a cluster IP, or nil.
func (x *Index) ServiceByClusterIP(ip netip.Addr) *Service {
	if x == nil || !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.byClusterIP[ip]
}

// Service returns a Service by namespace and name, or nil.
func (x *Index) Service(namespace, name string) *Service {
	if x == nil {
		return nil
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.services[namespace+"/"+name]
}

// Synced reports whether the first full state of both resource types arrived.
func (x *Index) Synced() bool {
	if x == nil {
		return false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.synced[WorkloadTypeURL] && x.synced[ServiceTypeURL]
}

// Status summarises the index for debugging.
type Status struct {
	Synced     bool      `json:"synced"`
	Workloads  int       `json:"workloads"`
	Services   int       `json:"services"`
	LastUpdate time.Time `json:"lastUpdate"`
}

// Status returns a summary of the index.
func (x *Index) Status() Status {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return Status{
		Synced:     x.synced[WorkloadTypeURL] && x.synced[ServiceTypeURL],
		Workloads:  len(x.workloads),
		Services:   len(x.services),
		LastUpdate: x.lastUpdate,
	}
}

// versionsOf returns a copy of the known resource versions of a type, sent as
// initial_resource_versions when reconnecting.
func (x *Index) versionsOf(typeURL string) map[string]string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make(map[string]string, len(x.versions[typeURL]))
	for k, v := range x.versions[typeURL] {
		out[k] = v
	}
	return out
}

// applyWorkloads stores updated workloads and drops removed ones.
func (x *Index) applyWorkloads(updated map[string]versioned[*identityv1.Workload], removed []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, name := range removed {
		if ip, err := netip.ParseAddr(name); err == nil {
			delete(x.workloads, ip.Unmap())
		}
		delete(x.versions[WorkloadTypeURL], name)
	}
	for name, v := range updated {
		w := workloadFromProto(v.res)
		if !w.IP.IsValid() {
			continue
		}
		x.workloads[w.IP] = w
		x.versions[WorkloadTypeURL][name] = v.version
	}
	x.synced[WorkloadTypeURL] = true
	x.lastUpdate = time.Now()
}

// applyServices stores updated Services and drops removed ones.
func (x *Index) applyServices(updated map[string]versioned[*identityv1.Service], removed []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, name := range removed {
		x.dropServiceLocked(name)
		delete(x.versions[ServiceTypeURL], name)
	}
	for name, v := range updated {
		x.dropServiceLocked(name)
		s := serviceFromProto(v.res)
		x.services[name] = s
		for _, ip := range s.ClusterIPs {
			x.byClusterIP[ip] = s
		}
		x.versions[ServiceTypeURL][name] = v.version
	}
	x.synced[ServiceTypeURL] = true
	x.lastUpdate = time.Now()
}

func (x *Index) dropServiceLocked(name string) {
	old, ok := x.services[name]
	if !ok {
		return
	}
	for _, ip := range old.ClusterIPs {
		if x.byClusterIP[ip] == old {
			delete(x.byClusterIP, ip)
		}
	}
	delete(x.services, name)
}

type versioned[T any] struct {
	res     T
	version string
}

func workloadFromProto(p *identityv1.Workload) *Workload {
	ip, _ := netip.ParseAddr(p.GetIp())
	w := &Workload{
		IP:               ip.Unmap(),
		Namespace:        p.GetNamespace(),
		Pod:              p.GetPod(),
		PodUID:           p.GetPodUid(),
		ServiceAccount:   p.GetServiceAccount(),
		SPIFFEID:         p.GetSpiffeId(),
		Node:             p.GetNode(),
		Labels:           p.GetLabels(),
		SecurityIdentity: p.GetSecurityIdentity(),
	}
	switch p.GetKind() {
	case identityv1.WorkloadKind_WORKLOAD_KIND_POD:
		w.Kind = KindPod
	case identityv1.WorkloadKind_WORKLOAD_KIND_NODE:
		w.Kind = KindNode
	}
	for _, s := range p.GetServices() {
		w.Services = append(w.Services, s.GetNamespace()+"/"+s.GetName())
	}
	sort.Strings(w.Services)
	return w
}

func serviceFromProto(p *identityv1.Service) *Service {
	s := &Service{Namespace: p.GetNamespace(), Name: p.GetName(), Ports: p.GetPorts()}
	for _, c := range p.GetClusterIps() {
		if ip, err := netip.ParseAddr(c); err == nil {
			s.ClusterIPs = append(s.ClusterIPs, ip.Unmap())
		}
	}
	return s
}

// ServiceFromHost returns the Service named by a cluster-local host name
// (<svc>.<ns>, <svc>.<ns>.svc or <svc>.<ns>.svc.cluster.local) when that Service
// exists in the index. host must already be lower-cased and without port.
func (x *Index) ServiceFromHost(host string) *Service {
	if x == nil || host == "" {
		return nil
	}
	h := strings.TrimSuffix(strings.TrimSuffix(host, ".cluster.local"), ".svc")
	name, ns, ok := strings.Cut(h, ".")
	if !ok || name == "" || ns == "" || strings.Contains(ns, ".") {
		return nil
	}
	return x.Service(ns, name)
}

// Replace sets the whole content of the index and marks it synced. It is meant
// for tests and static setups; the client updates the index incrementally.
func (x *Index) Replace(workloads []*Workload, services []*Service) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.workloads = make(map[netip.Addr]*Workload, len(workloads))
	for _, w := range workloads {
		x.workloads[w.IP.Unmap()] = w
	}
	x.services = make(map[string]*Service, len(services))
	x.byClusterIP = make(map[netip.Addr]*Service)
	for _, s := range services {
		x.services[s.Key()] = s
		for _, ip := range s.ClusterIPs {
			x.byClusterIP[ip.Unmap()] = s
		}
	}
	x.versions = map[string]map[string]string{WorkloadTypeURL: {}, ServiceTypeURL: {}}
	x.synced[WorkloadTypeURL], x.synced[ServiceTypeURL] = true, true
	x.lastUpdate = time.Now()
}
