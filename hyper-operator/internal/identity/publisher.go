package identity

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/taha2samy/hypergate/hyper-operator/internal/leader"
)

// EndpointSliceManagedBy marks the slice as written by the operator, so the
// Kubernetes EndpointSlice controller leaves it alone.
const EndpointSliceManagedBy = "hyper-operator.hyper.io"

// EndpointPublisher points the selector-less identity Service at the leader by
// writing its single EndpointSlice with the leader's pod IP. It runs on the
// leader only; a new leader overwrites the slice with its own address.
type EndpointPublisher struct {
	Client      kubernetes.Interface
	Namespace   string
	ServiceName string
	PodName     string
	PodIP       string
	Port        int32
	// Interval re-asserts the slice in case it was changed. Default 30s.
	Interval time.Duration

	log logr.Logger
}

// Runnable returns the publisher as a leader-only manager runnable.
func (p *EndpointPublisher) Runnable() manager.Runnable {
	return leader.LeaderOnly("identity-endpoint-publisher", p.Start)
}

// Start publishes the leader's endpoint until ctx is done, then marks it not
// ready if the slice still points at this pod.
func (p *EndpointPublisher) Start(ctx context.Context) error {
	p.log = ctrl.Log.WithName("identity-endpoint-publisher")
	if _, err := netip.ParseAddr(p.PodIP); err != nil {
		return fmt.Errorf("identity endpoint publisher: invalid pod IP %q (set POD_IP from status.podIP)", p.PodIP)
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	backoff := time.Second
	for {
		err := p.publish(ctx, true)
		if err == nil {
			p.log.Info("Identity Service now points at this replica", "service", p.ServiceName, "ip", p.PodIP)
			break
		}
		p.log.Error(err, "Publishing the identity endpoint failed, retrying", "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best effort: tell engines to stop using this replica right away. The
			// update is conditional on the slice still listing this pod, so it never
			// clobbers a slice a new leader has already written.
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := p.markNotReady(stopCtx); err != nil {
				p.log.V(1).Info("Could not mark the identity endpoint not ready", "error", err.Error())
			}
			return nil
		case <-ticker.C:
			if err := p.publish(ctx, true); err != nil {
				p.log.Error(err, "Re-publishing the identity endpoint failed")
			}
		}
	}
}

func (p *EndpointPublisher) desired(ready bool) *discoveryv1.EndpointSlice {
	addrType := discoveryv1.AddressTypeIPv4
	if a, _ := netip.ParseAddr(p.PodIP); a.Is6() && !a.Is4In6() {
		addrType = discoveryv1.AddressTypeIPv6
	}
	terminating := !ready
	port := p.Port
	name := "grpc"
	proto := corev1.ProtocolTCP
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.ServiceName,
			Namespace: p.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: p.ServiceName,
				discoveryv1.LabelManagedBy:   EndpointSliceManagedBy,
			},
		},
		AddressType: addrType,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{p.PodIP},
			Conditions: discoveryv1.EndpointConditions{
				Ready: &ready, Serving: &ready, Terminating: &terminating,
			},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.PodName},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: &name, Port: &port, Protocol: &proto}},
	}
}

func (p *EndpointPublisher) publish(ctx context.Context, ready bool) error {
	slices := p.Client.DiscoveryV1().EndpointSlices(p.Namespace)
	want := p.desired(ready)
	got, err := slices.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = slices.Create(ctx, want, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}
	if equality.Semantic.DeepEqual(got.Endpoints, want.Endpoints) &&
		equality.Semantic.DeepEqual(got.Ports, want.Ports) &&
		got.Labels[discoveryv1.LabelManagedBy] == EndpointSliceManagedBy &&
		got.AddressType == want.AddressType {
		return nil
	}
	if got.AddressType != want.AddressType {
		// AddressType is immutable.
		if err := slices.Delete(ctx, want.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		_, err = slices.Create(ctx, want, metav1.CreateOptions{})
		return err
	}
	got.Labels = want.Labels
	got.Endpoints = want.Endpoints
	got.Ports = want.Ports
	_, err = slices.Update(ctx, got, metav1.UpdateOptions{})
	return err
}

func (p *EndpointPublisher) markNotReady(ctx context.Context) error {
	slices := p.Client.DiscoveryV1().EndpointSlices(p.Namespace)
	got, err := slices.Get(ctx, p.ServiceName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(got.Endpoints) != 1 || len(got.Endpoints[0].Addresses) != 1 || got.Endpoints[0].Addresses[0] != p.PodIP {
		return nil // a new leader already owns the slice
	}
	got.Endpoints = p.desired(false).Endpoints
	_, err = slices.Update(ctx, got, metav1.UpdateOptions{}) // resourceVersion guards against races
	return err
}
