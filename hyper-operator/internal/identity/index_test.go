package identity

import (
	"net/netip"
	"reflect"
	"testing"
	"time"
)

var (
	t0  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ip  = netip.MustParseAddr
	ips = func(s ...string) []netip.Addr {
		out := make([]netip.Addr, len(s))
		for i, v := range s {
			out[i] = ip(v)
		}
		return out
	}
)

func pod(uid, ns, name string, created time.Time, addrs ...string) PodRecord {
	return PodRecord{
		UID: uid, Namespace: ns, Name: name, ServiceAccount: name, Node: "node-1",
		Labels: map[string]string{"app": name, "pod-template-hash": "abc"},
		IPs:    ips(addrs...), Created: created,
	}
}

func mustLookup(t *testing.T, x *Index, addr string) Workload {
	t.Helper()
	w, ok := x.Lookup(ip(addr))
	if !ok {
		t.Fatalf("%s not found", addr)
	}
	return w
}

func TestIndex_PodIdentity(t *testing.T) {
	x := NewIndex(Options{TrustDomain: "prod.example"})
	p := pod("u1", "shop", "checkout", t0, "10.0.0.5")
	p.ServiceAccount = "checkout-sa"
	x.UpsertPod(p)

	w := mustLookup(t, x, "10.0.0.5")
	if w.Kind != KindPod || w.Namespace != "shop" || w.Pod != "checkout" || w.PodUID != "u1" || w.Node != "node-1" {
		t.Fatalf("unexpected workload %+v", w)
	}
	if w.SPIFFEID != "spiffe://prod.example/ns/shop/sa/checkout-sa" {
		t.Fatalf("SPIFFE ID = %q", w.SPIFFEID)
	}
	if !reflect.DeepEqual(w.Labels, map[string]string{"app": "checkout"}) {
		t.Fatalf("labels must be filtered by the allow-list, got %v", w.Labels)
	}

	// IPv4-mapped IPv6 lookups resolve to the same entry.
	if _, ok := x.Lookup(ip("::ffff:10.0.0.5")); !ok {
		t.Fatal("IPv4-mapped lookup failed")
	}
	if _, ok := x.Lookup(ip("10.0.0.6")); ok {
		t.Fatal("unknown IP resolved")
	}
}

func TestIndex_DefaultServiceAccountAndTrustDomain(t *testing.T) {
	x := NewIndex(Options{})
	p := pod("u1", "shop", "web", t0, "10.0.0.5")
	p.ServiceAccount = ""
	x.UpsertPod(p)
	if w := mustLookup(t, x, "10.0.0.5"); w.ServiceAccount != "default" || w.SPIFFEID != "spiffe://cluster.local/ns/shop/sa/default" {
		t.Fatalf("got %q %q", w.ServiceAccount, w.SPIFFEID)
	}
}

func TestIndex_IPReuse(t *testing.T) {
	oldPod := pod("old", "shop", "a", t0, "10.0.0.5")
	newPod := pod("new", "shop", "b", t0.Add(time.Minute), "10.0.0.5")

	t.Run("delete arrives after the new pod", func(t *testing.T) {
		x := NewIndex(Options{})
		x.UpsertPod(oldPod)
		x.UpsertPod(newPod)
		if w := mustLookup(t, x, "10.0.0.5"); w.PodUID != "new" {
			t.Fatalf("newest pod must own the IP, got %s", w.PodUID)
		}
		x.DeletePod("old")
		if w := mustLookup(t, x, "10.0.0.5"); w.PodUID != "new" {
			t.Fatalf("late delete removed the new owner, got %s", w.PodUID)
		}
	})

	t.Run("late update of the old pod does not take the IP back", func(t *testing.T) {
		x := NewIndex(Options{})
		x.UpsertPod(newPod)
		x.UpsertPod(oldPod)
		if w := mustLookup(t, x, "10.0.0.5"); w.PodUID != "new" {
			t.Fatalf("got %s", w.PodUID)
		}
	})

	t.Run("deleting the owner falls back to the remaining claimant", func(t *testing.T) {
		x := NewIndex(Options{})
		x.UpsertPod(oldPod)
		x.UpsertPod(newPod)
		x.DeletePod("new")
		if w := mustLookup(t, x, "10.0.0.5"); w.PodUID != "old" {
			t.Fatalf("got %s", w.PodUID)
		}
		x.DeletePod("old")
		if _, ok := x.Lookup(ip("10.0.0.5")); ok {
			t.Fatal("IP still resolves after both pods are gone")
		}
	})

	t.Run("pod moving to a new IP releases the old one", func(t *testing.T) {
		x := NewIndex(Options{})
		x.UpsertPod(oldPod)
		moved := oldPod
		moved.IPs = ips("10.0.0.9")
		x.UpsertPod(moved)
		if _, ok := x.Lookup(ip("10.0.0.5")); ok {
			t.Fatal("old IP still resolves")
		}
		mustLookup(t, x, "10.0.0.9")
	})
}

func TestIndex_PodLifecycle(t *testing.T) {
	x := NewIndex(Options{})

	// Not yet assigned an IP: nothing to index.
	x.UpsertPod(pod("u1", "shop", "a", t0))
	if n, _, _ := x.Counts(); n != 0 {
		t.Fatalf("pod without IP indexed")
	}

	// IP assigned (before Ready): indexed.
	x.UpsertPod(pod("u1", "shop", "a", t0, "10.0.0.5"))
	mustLookup(t, x, "10.0.0.5")

	// Terminating pods stay until the object is deleted; the record is the same.
	x.UpsertPod(pod("u1", "shop", "a", t0, "10.0.0.5"))
	mustLookup(t, x, "10.0.0.5")

	// A finished pod (Succeeded/Failed) releases its IP even though the object remains.
	done := pod("u1", "shop", "a", t0, "10.0.0.5")
	done.Finished = true
	x.UpsertPod(done)
	if _, ok := x.Lookup(ip("10.0.0.5")); ok {
		t.Fatal("finished pod still owns its IP")
	}
}

func TestIndex_HostNetworkAndNodes(t *testing.T) {
	x := NewIndex(Options{})
	x.UpsertNode(NodeRecord{Name: "node-1", IPs: ips("192.168.1.10", "fd00::10")})
	hn := pod("u1", "kube-system", "agent", t0, "192.168.1.10")
	hn.HostNetwork = true
	x.UpsertPod(hn)

	w := mustLookup(t, x, "192.168.1.10")
	if w.Kind != KindNode || w.Node != "node-1" || w.Pod != "" {
		t.Fatalf("hostNetwork pod must resolve to its node, got %+v", w)
	}
	if w := mustLookup(t, x, "fd00::10"); w.Kind != KindNode {
		t.Fatalf("node IPv6 address: %+v", w)
	}

	x.UpsertNode(NodeRecord{Name: "node-1", IPs: ips("192.168.1.11")})
	if _, ok := x.Lookup(ip("192.168.1.10")); ok {
		t.Fatal("old node address still resolves")
	}
	x.DeleteNode("node-1")
	if _, ok := x.Lookup(ip("192.168.1.11")); ok {
		t.Fatal("deleted node still resolves")
	}
}

func TestIndex_DualStack(t *testing.T) {
	x := NewIndex(Options{})
	x.UpsertPod(pod("u1", "shop", "a", t0, "10.0.0.5", "fd00::5"))
	v4, v6 := mustLookup(t, x, "10.0.0.5"), mustLookup(t, x, "fd00::5")
	if v4.PodUID != "u1" || v6.PodUID != "u1" || v4.IP == v6.IP {
		t.Fatalf("dual-stack entries wrong: %+v %+v", v4, v6)
	}
	if n, _, _ := x.Counts(); n != 2 {
		t.Fatalf("expected one entry per IP, got %d", n)
	}
	x.DeletePod("u1")
	if n, _, _ := x.Counts(); n != 0 {
		t.Fatalf("entries left after delete: %d", n)
	}
}

func TestIndex_ServicesFromEndpointSlices(t *testing.T) {
	x := NewIndex(Options{})
	x.UpsertPod(pod("u1", "shop", "checkout", t0, "10.0.0.5"))

	// A Service split over two slices, plus a second Service selecting the same pod.
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "checkout-a", Service: "checkout", PodUIDs: []string{"u1"}})
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "checkout-b", Service: "checkout", PodUIDs: []string{"u1"}})
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "all-x", Service: "all", PodUIDs: []string{"u1"}})

	want := []ServiceRef{{"shop", "all"}, {"shop", "checkout"}}
	if got := mustLookup(t, x, "10.0.0.5").Services; !reflect.DeepEqual(got, want) {
		t.Fatalf("services = %v, want %v", got, want)
	}

	// Removing one of two slices keeps the membership; removing both drops it.
	x.DeleteSlice("shop", "checkout-a")
	if got := mustLookup(t, x, "10.0.0.5").Services; !reflect.DeepEqual(got, want) {
		t.Fatalf("after one slice delete: %v", got)
	}
	x.DeleteSlice("shop", "checkout-b")
	if got := mustLookup(t, x, "10.0.0.5").Services; !reflect.DeepEqual(got, []ServiceRef{{"shop", "all"}}) {
		t.Fatalf("after both slices deleted: %v", got)
	}

	// A slice update that drops the pod removes the membership.
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "all-x", Service: "all"})
	if got := mustLookup(t, x, "10.0.0.5").Services; len(got) != 0 {
		t.Fatalf("membership kept after slice update: %v", got)
	}

	// Endpoints without a Pod targetRef are matched by IP.
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "manual", Service: "legacy", IPs: ips("10.0.0.5")})
	if got := mustLookup(t, x, "10.0.0.5").Services; !reflect.DeepEqual(got, []ServiceRef{{"shop", "legacy"}}) {
		t.Fatalf("IP membership: %v", got)
	}
}

func TestIndex_ServiceRecords(t *testing.T) {
	x := NewIndex(Options{})
	x.UpsertService(Service{Namespace: "payments", Name: "ledger", ClusterIPs: ips("10.96.0.20", "fd00:96::20"), Ports: []uint32{8080}})

	s, ok := x.ServiceByClusterIP(ip("fd00:96::20"))
	if !ok || s.Name != "ledger" || len(s.ClusterIPs) != 2 {
		t.Fatalf("lookup by cluster IP: %+v %v", s, ok)
	}
	s.Ports[0] = 1 // copies must not alias the index
	if got, _ := x.Service("payments", "ledger"); got.Ports[0] != 8080 {
		t.Fatal("returned Service aliases the index")
	}

	x.UpsertService(Service{Namespace: "payments", Name: "ledger", ClusterIPs: ips("10.96.0.21")})
	if _, ok := x.ServiceByClusterIP(ip("10.96.0.20")); ok {
		t.Fatal("old cluster IP still resolves")
	}
	x.DeleteService("payments", "ledger")
	if _, ok := x.ServiceByClusterIP(ip("10.96.0.21")); ok {
		t.Fatal("deleted service still resolves")
	}
}

func TestIndex_CiliumIdentity(t *testing.T) {
	x := NewIndex(Options{})
	x.UpsertPod(pod("u1", "shop", "checkout", t0, "10.0.0.5"))
	x.UpsertCiliumEndpoint(CiliumEndpointRecord{Namespace: "shop", Name: "checkout", Identity: 51234,
		Labels: []string{"k8s:io.kubernetes.pod.namespace=shop", "k8s:app=checkout"}})

	w := mustLookup(t, x, "10.0.0.5")
	if w.SecurityIdentity != 51234 || len(w.CiliumLabels) != 2 || w.CiliumLabels[0] != "k8s:app=checkout" {
		t.Fatalf("cilium identity not attached: %+v", w)
	}

	// The CiliumEndpoint may arrive before the pod.
	x.UpsertCiliumEndpoint(CiliumEndpointRecord{Namespace: "shop", Name: "late", Identity: 7})
	x.UpsertPod(pod("u2", "shop", "late", t0, "10.0.0.6"))
	if w := mustLookup(t, x, "10.0.0.6"); w.SecurityIdentity != 7 {
		t.Fatalf("got %d", w.SecurityIdentity)
	}

	x.DeleteCiliumEndpoint("shop", "checkout")
	if w := mustLookup(t, x, "10.0.0.5"); w.SecurityIdentity != 0 || w.CiliumLabels != nil {
		t.Fatalf("identity kept after CiliumEndpoint delete: %+v", w)
	}
}

func TestIndex_SnapshotAndGeneration(t *testing.T) {
	x := NewIndex(Options{})
	g0 := x.Generation()
	x.UpsertPod(pod("u1", "shop", "b", t0, "10.0.0.9"))
	x.UpsertPod(pod("u2", "shop", "a", t0, "10.0.0.5"))
	x.UpsertNode(NodeRecord{Name: "n", IPs: ips("192.168.0.1")})
	x.UpsertService(Service{Namespace: "b", Name: "x"})
	x.UpsertService(Service{Namespace: "a", Name: "y"})
	if x.Generation() <= g0 {
		t.Fatal("generation did not advance")
	}

	ws := x.Workloads()
	if len(ws) != 3 || ws[0].IP != ip("10.0.0.5") || ws[1].IP != ip("10.0.0.9") || ws[2].Kind != KindNode {
		t.Fatalf("workloads not sorted by IP: %+v", ws)
	}
	ss := x.Services()
	if len(ss) != 2 || ss[0].Namespace != "a" {
		t.Fatalf("services not sorted: %+v", ss)
	}
}
