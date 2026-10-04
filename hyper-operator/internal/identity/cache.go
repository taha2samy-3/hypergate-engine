package identity

import (
	"context"
	"fmt"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/taha2samy/hypergate/hyper-operator/internal/leader"
)

// CiliumEndpointsGVR is the CiliumEndpoint resource watched when Cilium is installed.
var CiliumEndpointsGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints"}

// CacheOptions configure a Cache.
type CacheOptions struct {
	Options
	// CiliumRecheck is how often the cache checks whether the CiliumEndpoint CRD
	// has been installed, when it was absent at start-up. Default 5 minutes.
	CiliumRecheck time.Duration
}

// Cache keeps an Index current from informers. It runs on every operator
// replica (warm standby), so a new leader can serve identities immediately.
type Cache struct {
	index   *Index
	client  kubernetes.Interface
	dynamic dynamic.Interface
	opts    CacheOptions
	log     logr.Logger

	synced        atomic.Bool
	ciliumWatched atomic.Bool
	// ciliumCheck reports whether the CiliumEndpoint CRD is installed (replaced in tests).
	ciliumCheck func() bool
}

// NewCache returns a cache that is started with Start or through Runnable.
// dyn may be nil, which disables Cilium support.
func NewCache(client kubernetes.Interface, dyn dynamic.Interface, opts CacheOptions) *Cache {
	if opts.CiliumRecheck <= 0 {
		opts.CiliumRecheck = 5 * time.Minute
	}
	c := &Cache{
		index:   NewIndex(opts.Options),
		client:  client,
		dynamic: dyn,
		opts:    opts,
		log:     ctrl.Log.WithName("identity-cache"),
	}
	c.ciliumCheck = c.ciliumInstalled
	return c
}

// Index returns the index the cache maintains.
func (c *Cache) Index() *Index { return c.index }

// Synced reports whether the initial list of every watched resource is indexed.
func (c *Cache) Synced() bool { return c.synced.Load() }

// CiliumWatched reports whether CiliumEndpoints are being indexed.
func (c *Cache) CiliumWatched() bool { return c.ciliumWatched.Load() }

// Runnable returns the cache as a manager runnable that runs on every replica.
func (c *Cache) Runnable() manager.Runnable {
	return leader.AllReplicas("identity-cache", c.Start)
}

// Start runs the informers until ctx is done.
func (c *Cache) Start(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(c.client, 0)

	pods := factory.Core().V1().Pods().Informer()
	services := factory.Core().V1().Services().Informer()
	slices := factory.Discovery().V1().EndpointSlices().Informer()
	nodes := factory.Core().V1().Nodes().Informer()

	for _, setup := range []struct {
		inf       cache.SharedIndexInformer
		transform cache.TransformFunc
		handler   cache.ResourceEventHandler
	}{
		{pods, c.trimPod, handler(c.onPod, c.onPodDelete)},
		{services, trimService, handler(c.onService, c.onServiceDelete)},
		{slices, trimSlice, handler(c.onSlice, c.onSliceDelete)},
		{nodes, trimNode, handler(c.onNode, c.onNodeDelete)},
	} {
		if err := setup.inf.SetTransform(setup.transform); err != nil {
			return fmt.Errorf("identity cache: set transform: %w", err)
		}
		if _, err := setup.inf.AddEventHandler(setup.handler); err != nil {
			return fmt.Errorf("identity cache: add handler: %w", err)
		}
	}

	factory.Start(ctx.Done())
	defer factory.Shutdown()

	c.log.Info("Waiting for identity informers to sync")
	if !cache.WaitForCacheSync(ctx.Done(), pods.HasSynced, services.HasSynced, slices.HasSynced, nodes.HasSynced) {
		return nil // context cancelled
	}

	var ciliumFactory dynamicinformer.DynamicSharedInformerFactory
	defer func() {
		if ciliumFactory != nil {
			ciliumFactory.Shutdown()
		}
	}()
	startCilium := func() {
		if c.ciliumWatched.Load() || c.dynamic == nil || !c.ciliumCheck() {
			return
		}
		ciliumFactory = dynamicinformer.NewDynamicSharedInformerFactory(c.dynamic, 0)
		inf := ciliumFactory.ForResource(CiliumEndpointsGVR).Informer()
		_ = inf.SetTransform(trimCiliumEndpoint)
		_, _ = inf.AddEventHandler(handler(c.onCiliumEndpoint, c.onCiliumEndpointDelete))
		ciliumFactory.Start(ctx.Done())
		if cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
			c.ciliumWatched.Store(true)
			c.log.Info("Indexing Cilium security identities from CiliumEndpoints")
		}
	}
	startCilium()

	c.synced.Store(true)
	c.updateMetrics()
	podIPs, nodeIPs, svcs := c.index.Counts()
	c.log.Info("Identity cache synced", "podIPs", podIPs, "nodeIPs", nodeIPs, "services", svcs, "cilium", c.ciliumWatched.Load())

	ticker := time.NewTicker(c.opts.CiliumRecheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.synced.Store(false)
			cacheSynced.Set(0)
			return nil
		case <-ticker.C:
			startCilium()
		}
	}
}

// ciliumInstalled checks discovery for the CiliumEndpoint resource.
func (c *Cache) ciliumInstalled() bool {
	list, err := c.client.Discovery().ServerResourcesForGroupVersion(CiliumEndpointsGVR.GroupVersion().String())
	if err != nil || list == nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Name == CiliumEndpointsGVR.Resource {
			return true
		}
	}
	return false
}

// handler builds an event handler; deletes receive the last known object,
// including from tombstones.
func handler(upsert func(any), del func(any)) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    upsert,
		UpdateFunc: func(_, obj any) { upsert(obj) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			del(obj)
		},
	}
}

// --- Pods ---

// trimPod keeps only the fields the index uses, so the informer does not hold
// full pod specs in memory.
func (c *Cache) trimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	labels := map[string]string{}
	for k, v := range p.Labels {
		if c.index.LabelAllowed(k) {
			labels[k] = v
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.Name, Namespace: p.Namespace, UID: p.UID, ResourceVersion: p.ResourceVersion,
			Labels: labels, CreationTimestamp: p.CreationTimestamp, DeletionTimestamp: p.DeletionTimestamp,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: p.Spec.ServiceAccountName,
			NodeName:           p.Spec.NodeName,
			HostNetwork:        p.Spec.HostNetwork,
		},
		Status: corev1.PodStatus{Phase: p.Status.Phase, PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs},
	}, nil
}

func (c *Cache) onPod(obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	addrs := make([]netip.Addr, 0, len(p.Status.PodIPs)+1)
	for _, pip := range p.Status.PodIPs {
		addrs = appendAddr(addrs, pip.IP)
	}
	if len(addrs) == 0 {
		addrs = appendAddr(addrs, p.Status.PodIP)
	}
	c.index.UpsertPod(PodRecord{
		UID:            string(p.UID),
		Namespace:      p.Namespace,
		Name:           p.Name,
		ServiceAccount: p.Spec.ServiceAccountName,
		Node:           p.Spec.NodeName,
		Labels:         p.Labels,
		IPs:            addrs,
		HostNetwork:    p.Spec.HostNetwork,
		Finished:       p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed,
		Created:        p.CreationTimestamp.Time,
	})
	c.updateMetrics()
}

func (c *Cache) onPodDelete(obj any) {
	if p, ok := obj.(*corev1.Pod); ok {
		c.index.DeletePod(string(p.UID))
		c.updateMetrics()
	}
}

// --- Services ---

func trimService(obj any) (any, error) {
	s, ok := obj.(*corev1.Service)
	if !ok {
		return obj, nil
	}
	ports := make([]corev1.ServicePort, len(s.Spec.Ports))
	for i, p := range s.Spec.Ports {
		ports[i] = corev1.ServicePort{Port: p.Port, Protocol: p.Protocol}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace, UID: s.UID, ResourceVersion: s.ResourceVersion},
		Spec:       corev1.ServiceSpec{ClusterIP: s.Spec.ClusterIP, ClusterIPs: s.Spec.ClusterIPs, Ports: ports},
	}, nil
}

func (c *Cache) onService(obj any) {
	s, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	var addrs []netip.Addr
	for _, cip := range s.Spec.ClusterIPs {
		addrs = appendAddr(addrs, cip) // "None" (headless) is not an address and is skipped
	}
	if len(addrs) == 0 {
		addrs = appendAddr(addrs, s.Spec.ClusterIP)
	}
	ports := make([]uint32, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		ports = append(ports, uint32(p.Port))
	}
	c.index.UpsertService(Service{Namespace: s.Namespace, Name: s.Name, ClusterIPs: addrs, Ports: ports})
	c.updateMetrics()
}

func (c *Cache) onServiceDelete(obj any) {
	if s, ok := obj.(*corev1.Service); ok {
		c.index.DeleteService(s.Namespace, s.Name)
		c.updateMetrics()
	}
}

// --- EndpointSlices ---

func trimSlice(obj any) (any, error) {
	s, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return obj, nil
	}
	eps := make([]discoveryv1.Endpoint, len(s.Endpoints))
	for i, e := range s.Endpoints {
		eps[i] = discoveryv1.Endpoint{Addresses: e.Addresses}
		if e.TargetRef != nil && e.TargetRef.Kind == "Pod" {
			eps[i].TargetRef = &corev1.ObjectReference{Kind: "Pod", UID: e.TargetRef.UID}
		}
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name, Namespace: s.Namespace, ResourceVersion: s.ResourceVersion,
			Labels: map[string]string{discoveryv1.LabelServiceName: s.Labels[discoveryv1.LabelServiceName]},
		},
		AddressType: s.AddressType,
		Endpoints:   eps,
	}, nil
}

func (c *Cache) onSlice(obj any) {
	s, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	rec := SliceRecord{Namespace: s.Namespace, Name: s.Name, Service: s.Labels[discoveryv1.LabelServiceName]}
	for _, e := range s.Endpoints {
		if e.TargetRef != nil && e.TargetRef.Kind == "Pod" && e.TargetRef.UID != "" {
			rec.PodUIDs = append(rec.PodUIDs, string(e.TargetRef.UID))
			continue
		}
		for _, a := range e.Addresses {
			rec.IPs = appendAddr(rec.IPs, a)
		}
	}
	c.index.UpsertSlice(rec)
}

func (c *Cache) onSliceDelete(obj any) {
	if s, ok := obj.(*discoveryv1.EndpointSlice); ok {
		c.index.DeleteSlice(s.Namespace, s.Name)
	}
}

// --- Nodes ---

func trimNode(obj any) (any, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.Name, UID: n.UID, ResourceVersion: n.ResourceVersion},
		Status:     corev1.NodeStatus{Addresses: n.Status.Addresses},
	}, nil
}

func (c *Cache) onNode(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	var addrs []netip.Addr
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
			addrs = appendAddr(addrs, a.Address)
		}
	}
	c.index.UpsertNode(NodeRecord{Name: n.Name, IPs: addrs})
	c.updateMetrics()
}

func (c *Cache) onNodeDelete(obj any) {
	if n, ok := obj.(*corev1.Node); ok {
		c.index.DeleteNode(n.Name)
		c.updateMetrics()
	}
}

// --- CiliumEndpoints ---

// trimCiliumEndpoint keeps the name and status.identity.{id,labels}.
func trimCiliumEndpoint(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetNamespace(u.GetNamespace())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	if id, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "identity"); found {
		_ = unstructured.SetNestedField(out.Object, runtime.DeepCopyJSONValue(id), "status", "identity")
	}
	return out, nil
}

func (c *Cache) onCiliumEndpoint(obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	id, _, _ := unstructured.NestedInt64(u.Object, "status", "identity", "id")
	labels, _, _ := unstructured.NestedStringSlice(u.Object, "status", "identity", "labels")
	if id < 0 || id > int64(^uint32(0)) {
		id = 0
	}
	c.index.UpsertCiliumEndpoint(CiliumEndpointRecord{
		Namespace: u.GetNamespace(), Name: u.GetName(), Identity: uint32(id), Labels: labels,
	})
}

func (c *Cache) onCiliumEndpointDelete(obj any) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		c.index.DeleteCiliumEndpoint(u.GetNamespace(), u.GetName())
	}
}

func appendAddr(addrs []netip.Addr, s string) []netip.Addr {
	if a, err := netip.ParseAddr(s); err == nil {
		return append(addrs, a.Unmap())
	}
	return addrs
}
