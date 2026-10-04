package identity

import (
	"net/netip"
	"testing"

	identityv1 "github.com/taha2samy/hypergate/internal/identityapi/v1"
)

func TestIndex_ApplyAndLookup(t *testing.T) {
	x := NewIndex()
	if x.Synced() {
		t.Fatal("new index must not be synced")
	}
	x.applyWorkloads(map[string]versioned[*identityv1.Workload]{
		"10.0.0.5": {&identityv1.Workload{
			Ip: "10.0.0.5", Kind: identityv1.WorkloadKind_WORKLOAD_KIND_POD, Namespace: "shop", Pod: "checkout",
			ServiceAccount: "checkout", SpiffeId: "spiffe://cluster.local/ns/shop/sa/checkout",
			Services: []*identityv1.ServiceRef{{Namespace: "shop", Name: "web"}, {Namespace: "shop", Name: "checkout"}},
		}, "v1"},
		"192.168.1.10": {&identityv1.Workload{Ip: "192.168.1.10", Kind: identityv1.WorkloadKind_WORKLOAD_KIND_NODE, Node: "n1"}, "v2"},
	}, nil)
	if x.Synced() {
		t.Fatal("synced before services arrived")
	}
	x.applyServices(map[string]versioned[*identityv1.Service]{
		"payments/ledger": {&identityv1.Service{Namespace: "payments", Name: "ledger", ClusterIps: []string{"10.96.0.20", "fd00::20"}}, "v3"},
	}, nil)
	if !x.Synced() {
		t.Fatal("not synced after both types")
	}

	w := x.Lookup(netip.MustParseAddr("::ffff:10.0.0.5"))
	if !w.IsPod() || w.Pod != "checkout" || len(w.Services) != 2 || w.Services[0] != "shop/checkout" {
		t.Fatalf("workload = %+v", w)
	}
	if n := x.Lookup(netip.MustParseAddr("192.168.1.10")); n.IsPod() || n.Kind != KindNode {
		t.Fatalf("node = %+v", n)
	}
	if s := x.ServiceByClusterIP(netip.MustParseAddr("fd00::20")); s == nil || s.Key() != "payments/ledger" {
		t.Fatalf("service = %+v", s)
	}
	if v := x.versionsOf(WorkloadTypeURL); v["10.0.0.5"] != "v1" || len(v) != 2 {
		t.Fatalf("versions = %v", v)
	}

	// Removal and replacement.
	x.applyWorkloads(nil, []string{"10.0.0.5"})
	if x.Lookup(netip.MustParseAddr("10.0.0.5")) != nil {
		t.Fatal("removed workload still resolves")
	}
	x.applyServices(map[string]versioned[*identityv1.Service]{
		"payments/ledger": {&identityv1.Service{Namespace: "payments", Name: "ledger", ClusterIps: []string{"10.96.0.21"}}, "v4"},
	}, nil)
	if x.ServiceByClusterIP(netip.MustParseAddr("10.96.0.20")) != nil || x.ServiceByClusterIP(netip.MustParseAddr("10.96.0.21")) == nil {
		t.Fatal("service update did not move the cluster IP")
	}
	x.applyServices(nil, []string{"payments/ledger"})
	if x.Service("payments", "ledger") != nil || x.ServiceByClusterIP(netip.MustParseAddr("10.96.0.21")) != nil {
		t.Fatal("removed service still resolves")
	}
}

func TestIndex_ServiceFromHost(t *testing.T) {
	x := NewIndex()
	x.Replace(nil, []*Service{{Namespace: "payments", Name: "ledger"}})
	for host, want := range map[string]bool{
		"ledger.payments":                   true,
		"ledger.payments.svc":               true,
		"ledger.payments.svc.cluster.local": true,
		"ledger":                            false, // no namespace
		"ledger.billing":                    false, // no such Service
		"api.example.com":                   false, // not a Service
		"":                                  false,
	} {
		if got := x.ServiceFromHost(host) != nil; got != want {
			t.Errorf("%q: got %v, want %v", host, got, want)
		}
	}
}

func TestIndex_NilSafe(t *testing.T) {
	var x *Index
	if x.Lookup(netip.MustParseAddr("10.0.0.1")) != nil || x.ServiceByClusterIP(netip.MustParseAddr("10.0.0.1")) != nil ||
		x.Service("a", "b") != nil || x.ServiceFromHost("a.b") != nil || x.Synced() {
		t.Fatal("nil index must behave as empty")
	}
}
