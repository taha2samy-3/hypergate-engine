package identity

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func k8sPod(uid, ns, name, podIP string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: labels,
			CreationTimestamp: metav1.NewTime(t0)},
		Spec:   corev1.PodSpec{ServiceAccountName: name + "-sa", NodeName: "node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: podIP, PodIPs: []corev1.PodIP{{IP: podIP}}},
	}
}

func ciliumEndpoint(ns, name string, id int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumEndpoint",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"status": map[string]any{
			"identity":   map[string]any{"id": id, "labels": []any{"k8s:app=" + name}},
			"networking": map[string]any{"node": "10.0.0.1"},
		},
	}}
	return u
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startCache(t *testing.T, c *Cache) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start returned %v", err)
		}
	})
	eventually(t, "initial sync", c.Synced)
}

func TestCache_IndexesClusterState(t *testing.T) {
	client := fake.NewClientset(
		k8sPod("u1", "shop", "checkout", "10.244.1.5", map[string]string{"app": "checkout", "secret-label": "x"}),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "kube-system", UID: "u2"},
			Spec:       corev1.PodSpec{HostNetwork: true, NodeName: "node-1"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "192.168.1.10", PodIPs: []corev1.PodIP{{IP: "192.168.1.10"}}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				{Type: corev1.NodeHostName, Address: "node-1"},
			}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.30", ClusterIPs: []string{"10.96.0.30"},
				Ports: []corev1.ServicePort{{Port: 8080}}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: "shop"},
			Spec:       corev1.ServiceSpec{ClusterIP: "None", ClusterIPs: []string{"None"}},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: "checkout-x1", Namespace: "shop", Labels: map[string]string{discoveryv1.LabelServiceName: "checkout"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses: []string{"10.244.1.5"},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "checkout", Namespace: "shop", UID: "u1"},
			}},
		},
	)
	c := NewCache(client, nil, CacheOptions{Options: Options{TrustDomain: "td"}})
	startCache(t, c)
	x := c.Index()

	w, ok := x.Lookup(netip.MustParseAddr("10.244.1.5"))
	if !ok || w.Pod != "checkout" || w.SPIFFEID != "spiffe://td/ns/shop/sa/checkout-sa" {
		t.Fatalf("pod not indexed: %+v %v", w, ok)
	}
	if len(w.Services) != 1 || w.Services[0] != (ServiceRef{"shop", "checkout"}) {
		t.Fatalf("service membership missing: %v", w.Services)
	}
	if _, leaked := w.Labels["secret-label"]; leaked || w.Labels["app"] != "checkout" {
		t.Fatalf("labels not filtered: %v", w.Labels)
	}
	if n, _ := x.Lookup(netip.MustParseAddr("192.168.1.10")); n.Kind != KindNode || n.Node != "node-1" {
		t.Fatalf("hostNetwork pod must resolve to the node: %+v", n)
	}
	if s, ok := x.ServiceByClusterIP(netip.MustParseAddr("10.96.0.30")); !ok || s.Name != "checkout" || s.Ports[0] != 8080 {
		t.Fatalf("service by cluster IP: %+v %v", s, ok)
	}
	if s, ok := x.Service("shop", "headless"); !ok || len(s.ClusterIPs) != 0 {
		t.Fatalf("headless service: %+v %v", s, ok)
	}
	if c.CiliumWatched() {
		t.Fatal("cilium watched without a dynamic client")
	}

	// Live events: a new pod reusing the IP, then the old pod's delete.
	ctx := context.Background()
	newer := k8sPod("u9", "shop", "checkout-2", "10.244.1.5", nil)
	newer.CreationTimestamp = metav1.NewTime(t0.Add(time.Hour))
	if _, err := client.CoreV1().Pods("shop").Create(ctx, newer, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new pod to own the IP", func() bool {
		w, _ := x.Lookup(netip.MustParseAddr("10.244.1.5"))
		return w.PodUID == "u9"
	})
	if err := client.CoreV1().Pods("shop").Delete(ctx, "checkout", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "old pod removal", func() bool {
		w, ok := x.Lookup(netip.MustParseAddr("10.244.1.5"))
		return ok && w.PodUID == "u9" && x.Generation() > 0
	})

	if err := client.CoreV1().Services("shop").Delete(ctx, "checkout", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "service removal", func() bool {
		_, ok := x.ServiceByClusterIP(netip.MustParseAddr("10.96.0.30"))
		return !ok
	})
}

func TestCache_CiliumIdentities(t *testing.T) {
	client := fake.NewClientset(k8sPod("u1", "shop", "checkout", "10.244.1.5", nil))
	client.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "cilium.io/v2",
		APIResources: []metav1.APIResource{{Name: "ciliumendpoints", Namespaced: true, Kind: "CiliumEndpoint"}},
	}}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{CiliumEndpointsGVR: "CiliumEndpointList"},
		ciliumEndpoint("shop", "checkout", 51234))

	c := NewCache(client, dyn, CacheOptions{})
	startCache(t, c)
	if !c.CiliumWatched() {
		t.Fatal("cilium CRD present but not watched")
	}
	w, _ := c.Index().Lookup(netip.MustParseAddr("10.244.1.5"))
	if w.SecurityIdentity != 51234 || len(w.CiliumLabels) != 1 || w.CiliumLabels[0] != "k8s:app=checkout" {
		t.Fatalf("cilium identity not indexed: %+v", w)
	}

	if _, err := dyn.Resource(CiliumEndpointsGVR).Namespace("shop").Update(context.Background(),
		ciliumEndpoint("shop", "checkout", 777), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "identity update", func() bool {
		w, _ := c.Index().Lookup(netip.MustParseAddr("10.244.1.5"))
		return w.SecurityIdentity == 777
	})
}

func TestCache_CiliumInstalledLater(t *testing.T) {
	client := fake.NewClientset(k8sPod("u1", "shop", "checkout", "10.244.1.5", nil))
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{CiliumEndpointsGVR: "CiliumEndpointList"},
		ciliumEndpoint("shop", "checkout", 42))

	c := NewCache(client, dyn, CacheOptions{CiliumRecheck: 20 * time.Millisecond})
	// The fake discovery client is not safe to mutate while in use, so the
	// installation is simulated through the check itself.
	var installed atomic.Bool
	c.ciliumCheck = installed.Load
	startCache(t, c)
	if c.CiliumWatched() {
		t.Fatal("watched before the CRD exists")
	}
	installed.Store(true)
	eventually(t, "late CRD detection", func() bool {
		w, _ := c.Index().Lookup(netip.MustParseAddr("10.244.1.5"))
		return c.CiliumWatched() && w.SecurityIdentity == 42
	})
}

func TestCache_TrimsPods(t *testing.T) {
	c := NewCache(fake.NewClientset(), nil, CacheOptions{})
	full := k8sPod("u1", "shop", "checkout", "10.0.0.1", map[string]string{"app": "x", "other": "y"})
	full.Spec.Containers = []corev1.Container{{Name: "big", Image: "img", Env: []corev1.EnvVar{{Name: "A", Value: "B"}}}}
	full.Annotations = map[string]string{"huge": "annotation"}
	out, _ := c.trimPod(full)
	p := out.(*corev1.Pod)
	if len(p.Spec.Containers) != 0 || p.Annotations != nil || p.Labels["other"] != "" || p.Labels["app"] != "x" {
		t.Fatalf("pod not trimmed: %+v", p)
	}
	if p.Status.PodIPs[0].IP != "10.0.0.1" || p.Spec.ServiceAccountName != "checkout-sa" {
		t.Fatalf("needed fields dropped: %+v", p)
	}
}
