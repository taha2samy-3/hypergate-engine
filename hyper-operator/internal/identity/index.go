// Package identity keeps the IP → workload identity map that engines use to
// route and enforce policy by caller (docs/design/identity-distribution.md).
//
// Index is the data structure: it is fed with records derived from Pods,
// EndpointSlices, Services, Nodes and (with Cilium) CiliumEndpoints, and answers
// lookups by IP. Cache feeds it from informers on every operator replica.
package identity

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// WorkloadKind says what an IP belongs to.
type WorkloadKind uint8

const (
	KindPod WorkloadKind = iota + 1
	// KindNode is a node address; hostNetwork pods share it and are not told apart.
	KindNode
)

func (k WorkloadKind) String() string {
	switch k {
	case KindPod:
		return "pod"
	case KindNode:
		return "node"
	}
	return "unknown"
}

// ServiceRef names a Service.
type ServiceRef struct {
	Namespace string
	Name      string
}

func (s ServiceRef) String() string { return s.Namespace + "/" + s.Name }

func compareServiceRef(a, b ServiceRef) int {
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// Workload is the identity behind one IP. Values returned by Index are copies
// and safe to keep.
type Workload struct {
	Kind WorkloadKind
	IP   netip.Addr
	// Node is the node name (for pods: where the pod runs).
	Node string

	// Pod fields (empty for KindNode).
	PodUID         string
	Namespace      string
	Pod            string
	ServiceAccount string
	SPIFFEID       string
	// Labels holds the pod labels on the configured allow-list.
	Labels map[string]string
	// Services lists the Services that have the pod as an endpoint, sorted.
	Services []ServiceRef

	// SecurityIdentity and CiliumLabels come from the pod's CiliumEndpoint
	// (0 and empty without Cilium).
	SecurityIdentity uint32
	CiliumLabels     []string
}

// Service is a Service and its cluster IPs.
type Service struct {
	Namespace  string
	Name       string
	ClusterIPs []netip.Addr
	Ports      []uint32
}

// PodRecord is the part of a Pod the index needs.
type PodRecord struct {
	UID            string
	Namespace      string
	Name           string
	ServiceAccount string
	Node           string
	Labels         map[string]string
	IPs            []netip.Addr
	HostNetwork    bool
	// Finished is true for pods in phase Succeeded or Failed: their IPs have been
	// released and may already belong to another pod.
	Finished bool
	Created  time.Time
}

// SliceRecord is the membership an EndpointSlice contributes to its Service.
type SliceRecord struct {
	Namespace string
	Name      string
	Service   string
	// PodUIDs are endpoints whose targetRef is a Pod.
	PodUIDs []string
	// IPs are endpoint addresses without a Pod targetRef.
	IPs []netip.Addr
}

// CiliumEndpointRecord is the identity Cilium assigned to a pod. Cilium names
// the CiliumEndpoint after the pod.
type CiliumEndpointRecord struct {
	Namespace string
	Name      string
	Identity  uint32
	Labels    []string
}

// NodeRecord holds a node's addresses.
type NodeRecord struct {
	Name string
	IPs  []netip.Addr
}

// Options configure how identities are derived.
type Options struct {
	// TrustDomain of SPIFFE IDs. Default cluster.local.
	TrustDomain string
	// LabelKeys is the allow-list of pod labels kept in the index.
	LabelKeys []string
}

// DefaultLabelKeys are kept when Options.LabelKeys is empty.
var DefaultLabelKeys = []string{
	"app", "version",
	"app.kubernetes.io/name", "app.kubernetes.io/instance",
	"app.kubernetes.io/component", "app.kubernetes.io/version",
}

type sliceKey struct{ namespace, name string }

type nsName struct{ namespace, name string }

// Index maps IPs to workloads. It is safe for concurrent use.
type Index struct {
	trustDomain string
	labelKeys   map[string]struct{}

	mu sync.RWMutex
	// pods by UID, and the pods claiming each IP (several during IP reuse).
	pods   map[string]*PodRecord
	claims map[netip.Addr][]string

	// Service membership, reference-counted across a Service's slices.
	slices      map[sliceKey]SliceRecord
	podServices map[string]map[ServiceRef]int
	ipServices  map[netip.Addr]map[ServiceRef]int

	services    map[ServiceRef]Service
	byClusterIP map[netip.Addr]ServiceRef

	cilium map[nsName]CiliumEndpointRecord

	nodes  map[string]NodeRecord
	nodeIP map[netip.Addr]string

	generation uint64
	// changed receives a value after changes (coalesced, never blocks writers).
	changed chan struct{}
}

// NewIndex returns an empty index.
func NewIndex(opts Options) *Index {
	td := opts.TrustDomain
	if td == "" {
		td = "cluster.local"
	}
	keys := opts.LabelKeys
	if len(keys) == 0 {
		keys = DefaultLabelKeys
	}
	allow := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allow[k] = struct{}{}
	}
	return &Index{
		trustDomain: td,
		labelKeys:   allow,
		pods:        map[string]*PodRecord{},
		claims:      map[netip.Addr][]string{},
		slices:      map[sliceKey]SliceRecord{},
		podServices: map[string]map[ServiceRef]int{},
		ipServices:  map[netip.Addr]map[ServiceRef]int{},
		services:    map[ServiceRef]Service{},
		byClusterIP: map[netip.Addr]ServiceRef{},
		cilium:      map[nsName]CiliumEndpointRecord{},
		nodes:       map[string]NodeRecord{},
		nodeIP:      map[netip.Addr]string{},
		changed:     make(chan struct{}, 1),
	}
}

// Changed returns a channel that receives a value after the index changes.
// Several changes may be coalesced into one notification. It is meant for a
// single consumer.
func (x *Index) Changed() <-chan struct{} { return x.changed }

// bump records a change. Callers hold x.mu.
func (x *Index) bump() {
	x.generation++
	select {
	case x.changed <- struct{}{}:
	default:
	}
}

// LabelAllowed reports whether a pod label is kept (used to trim informer objects).
func (x *Index) LabelAllowed(key string) bool {
	_, ok := x.labelKeys[key]
	return ok
}

// Generation increases on every change.
func (x *Index) Generation() uint64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.generation
}

// UpsertPod adds or replaces a pod. hostNetwork pods are not indexed: their IP
// is the node's. Pods that finished release their IPs.
func (x *Index) UpsertPod(p PodRecord) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removePodLocked(p.UID)
	if p.HostNetwork || p.Finished || p.UID == "" {
		x.bump()
		return
	}
	rec := p
	rec.IPs = uniqueAddrs(p.IPs)
	rec.Labels = x.filterLabels(p.Labels)
	x.pods[p.UID] = &rec
	for _, ip := range rec.IPs {
		x.claims[ip] = append(x.claims[ip], p.UID)
	}
	x.bump()
}

// DeletePod removes a pod by UID. An IP is released only by the pod that
// claimed it, so a late delete of an old pod never removes the new pod that
// reused its IP.
func (x *Index) DeletePod(uid string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removePodLocked(uid)
	x.bump()
}

func (x *Index) removePodLocked(uid string) {
	old, ok := x.pods[uid]
	if !ok {
		return
	}
	for _, ip := range old.IPs {
		claims := slices.DeleteFunc(x.claims[ip], func(u string) bool { return u == uid })
		if len(claims) == 0 {
			delete(x.claims, ip)
		} else {
			x.claims[ip] = claims
		}
	}
	delete(x.pods, uid)
}

// owner picks the pod an IP belongs to. With several claimants (an old pod's
// delete has not arrived yet) the most recently created pod wins.
func (x *Index) owner(ip netip.Addr) *PodRecord {
	var best *PodRecord
	for _, uid := range x.claims[ip] {
		p := x.pods[uid]
		if p == nil {
			continue
		}
		if best == nil || p.Created.After(best.Created) || p.Created.Equal(best.Created) && p.UID > best.UID {
			best = p
		}
	}
	return best
}

// UpsertSlice adds or replaces an EndpointSlice's contribution.
func (x *Index) UpsertSlice(s SliceRecord) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key := sliceKey{s.Namespace, s.Name}
	x.removeSliceLocked(key)
	if s.Service == "" {
		x.bump()
		return
	}
	svc := ServiceRef{Namespace: s.Namespace, Name: s.Service}
	rec := SliceRecord{Namespace: s.Namespace, Name: s.Name, Service: s.Service,
		PodUIDs: uniqueStrings(s.PodUIDs), IPs: uniqueAddrs(s.IPs)}
	for _, uid := range rec.PodUIDs {
		addRef(x.podServices, uid, svc)
	}
	for _, ip := range rec.IPs {
		addRef(x.ipServices, ip, svc)
	}
	x.slices[key] = rec
	x.bump()
}

// DeleteSlice removes an EndpointSlice's contribution.
func (x *Index) DeleteSlice(namespace, name string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeSliceLocked(sliceKey{namespace, name})
	x.bump()
}

func (x *Index) removeSliceLocked(key sliceKey) {
	old, ok := x.slices[key]
	if !ok {
		return
	}
	svc := ServiceRef{Namespace: old.Namespace, Name: old.Service}
	for _, uid := range old.PodUIDs {
		dropRef(x.podServices, uid, svc)
	}
	for _, ip := range old.IPs {
		dropRef(x.ipServices, ip, svc)
	}
	delete(x.slices, key)
}

// UpsertService adds or replaces a Service.
func (x *Index) UpsertService(s Service) {
	x.mu.Lock()
	defer x.mu.Unlock()
	ref := ServiceRef{Namespace: s.Namespace, Name: s.Name}
	x.removeServiceLocked(ref)
	s.ClusterIPs = uniqueAddrs(s.ClusterIPs)
	s.Ports = slices.Clone(s.Ports)
	x.services[ref] = s
	for _, ip := range s.ClusterIPs {
		x.byClusterIP[ip] = ref
	}
	x.bump()
}

// DeleteService removes a Service.
func (x *Index) DeleteService(namespace, name string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeServiceLocked(ServiceRef{Namespace: namespace, Name: name})
	x.bump()
}

func (x *Index) removeServiceLocked(ref ServiceRef) {
	old, ok := x.services[ref]
	if !ok {
		return
	}
	for _, ip := range old.ClusterIPs {
		if x.byClusterIP[ip] == ref {
			delete(x.byClusterIP, ip)
		}
	}
	delete(x.services, ref)
}

// UpsertCiliumEndpoint records the Cilium identity of the pod with the same name.
func (x *Index) UpsertCiliumEndpoint(c CiliumEndpointRecord) {
	x.mu.Lock()
	defer x.mu.Unlock()
	c.Labels = slices.Clone(c.Labels)
	sort.Strings(c.Labels)
	x.cilium[nsName{c.Namespace, c.Name}] = c
	x.bump()
}

// DeleteCiliumEndpoint forgets a CiliumEndpoint.
func (x *Index) DeleteCiliumEndpoint(namespace, name string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.cilium, nsName{namespace, name})
	x.bump()
}

// UpsertNode adds or replaces a node's addresses.
func (x *Index) UpsertNode(n NodeRecord) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeNodeLocked(n.Name)
	n.IPs = uniqueAddrs(n.IPs)
	x.nodes[n.Name] = n
	for _, ip := range n.IPs {
		x.nodeIP[ip] = n.Name
	}
	x.bump()
}

// DeleteNode removes a node.
func (x *Index) DeleteNode(name string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeNodeLocked(name)
	x.bump()
}

func (x *Index) removeNodeLocked(name string) {
	old, ok := x.nodes[name]
	if !ok {
		return
	}
	for _, ip := range old.IPs {
		if x.nodeIP[ip] == name {
			delete(x.nodeIP, ip)
		}
	}
	delete(x.nodes, name)
}

// Lookup returns the workload behind ip: a pod, otherwise a node.
func (x *Index) Lookup(ip netip.Addr) (Workload, bool) {
	ip = ip.Unmap()
	x.mu.RLock()
	defer x.mu.RUnlock()
	if p := x.owner(ip); p != nil {
		return x.podWorkloadLocked(p, ip), true
	}
	if node, ok := x.nodeIP[ip]; ok {
		return Workload{Kind: KindNode, IP: ip, Node: node}, true
	}
	return Workload{}, false
}

// ServiceByClusterIP returns the Service that owns a cluster IP.
func (x *Index) ServiceByClusterIP(ip netip.Addr) (Service, bool) {
	ip = ip.Unmap()
	x.mu.RLock()
	defer x.mu.RUnlock()
	ref, ok := x.byClusterIP[ip]
	if !ok {
		return Service{}, false
	}
	return cloneService(x.services[ref]), true
}

// Service returns a Service by name.
func (x *Index) Service(namespace, name string) (Service, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	s, ok := x.services[ServiceRef{Namespace: namespace, Name: name}]
	return cloneService(s), ok
}

// Workloads returns every indexed IP's workload, sorted by IP.
func (x *Index) Workloads() []Workload {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]Workload, 0, len(x.claims)+len(x.nodeIP))
	for ip := range x.claims {
		if p := x.owner(ip); p != nil {
			out = append(out, x.podWorkloadLocked(p, ip))
		}
	}
	for ip, node := range x.nodeIP {
		if _, claimed := x.claims[ip]; !claimed {
			out = append(out, Workload{Kind: KindNode, IP: ip, Node: node})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP.Less(out[j].IP) })
	return out
}

// Services returns every Service, sorted by namespace and name.
func (x *Index) Services() []Service {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]Service, 0, len(x.services))
	for _, s := range x.services {
		out = append(out, cloneService(s))
	}
	sort.Slice(out, func(i, j int) bool {
		return compareServiceRef(ServiceRef{out[i].Namespace, out[i].Name}, ServiceRef{out[j].Namespace, out[j].Name}) < 0
	})
	return out
}

// Counts returns the number of indexed pod IPs, node IPs and Services.
func (x *Index) Counts() (podIPs, nodeIPs, services int) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.claims), len(x.nodeIP), len(x.services)
}

func (x *Index) podWorkloadLocked(p *PodRecord, ip netip.Addr) Workload {
	sa := p.ServiceAccount
	if sa == "" {
		sa = "default"
	}
	w := Workload{
		Kind:           KindPod,
		IP:             ip,
		Node:           p.Node,
		PodUID:         p.UID,
		Namespace:      p.Namespace,
		Pod:            p.Name,
		ServiceAccount: sa,
		SPIFFEID:       fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", x.trustDomain, p.Namespace, sa),
		Labels:         cloneMap(p.Labels),
	}
	refs := map[ServiceRef]struct{}{}
	for ref := range x.podServices[p.UID] {
		refs[ref] = struct{}{}
	}
	for ref := range x.ipServices[ip] {
		refs[ref] = struct{}{}
	}
	for ref := range refs {
		w.Services = append(w.Services, ref)
	}
	slices.SortFunc(w.Services, compareServiceRef)
	if c, ok := x.cilium[nsName{p.Namespace, p.Name}]; ok {
		w.SecurityIdentity = c.Identity
		w.CiliumLabels = slices.Clone(c.Labels)
	}
	return w
}

func (x *Index) filterLabels(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if _, ok := x.labelKeys[k]; ok {
			if out == nil {
				out = make(map[string]string, len(x.labelKeys))
			}
			out[k] = v
		}
	}
	return out
}

func addRef[K comparable](m map[K]map[ServiceRef]int, key K, ref ServiceRef) {
	refs := m[key]
	if refs == nil {
		refs = map[ServiceRef]int{}
		m[key] = refs
	}
	refs[ref]++
}

func dropRef[K comparable](m map[K]map[ServiceRef]int, key K, ref ServiceRef) {
	refs := m[key]
	if refs[ref]--; refs[ref] <= 0 {
		delete(refs, ref)
	}
	if len(refs) == 0 {
		delete(m, key)
	}
}

func uniqueAddrs(in []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(in))
	for _, a := range in {
		if !a.IsValid() {
			continue
		}
		a = a.Unmap()
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneService(s Service) Service {
	s.ClusterIPs = slices.Clone(s.ClusterIPs)
	s.Ports = slices.Clone(s.Ports)
	return s
}
