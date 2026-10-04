package identity

import (
	"context"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func runPublisher(t *testing.T, p *EndpointPublisher) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Start(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func slice(t *testing.T, c *fake.Clientset) *discoveryv1.EndpointSlice {
	t.Helper()
	s, err := c.DiscoveryV1().EndpointSlices("hyper-system").Get(context.Background(), "hyper-operator-identity", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return s
}

func TestEndpointPublisher_Failover(t *testing.T) {
	c := fake.NewClientset()
	pub := func(name, ip string) *EndpointPublisher {
		return &EndpointPublisher{Client: c, Namespace: "hyper-system", ServiceName: "hyper-operator-identity",
			PodName: name, PodIP: ip, Port: 9444, Interval: 20 * time.Millisecond}
	}

	stopA := runPublisher(t, pub("operator-a", "10.0.0.10"))
	eventually(t, "leader A endpoint", func() bool {
		s := slice(t, c)
		return s != nil && s.Endpoints[0].Addresses[0] == "10.0.0.10" && *s.Endpoints[0].Conditions.Ready
	})
	s := slice(t, c)
	if s.Labels[discoveryv1.LabelServiceName] != "hyper-operator-identity" || s.Labels[discoveryv1.LabelManagedBy] != EndpointSliceManagedBy {
		t.Fatalf("labels = %v", s.Labels)
	}
	if *s.Ports[0].Port != 9444 || s.AddressType != discoveryv1.AddressTypeIPv4 || s.Endpoints[0].TargetRef.Name != "operator-a" {
		t.Fatalf("slice = %+v", s)
	}

	// Leader B takes over and writes its own address.
	stopB := runPublisher(t, pub("operator-b", "10.0.0.11"))
	stopA() // A stops after B published or concurrently; it must not touch B's slice
	eventually(t, "leader B endpoint", func() bool {
		s := slice(t, c)
		return s != nil && s.Endpoints[0].Addresses[0] == "10.0.0.11" && *s.Endpoints[0].Conditions.Ready
	})
	time.Sleep(50 * time.Millisecond)
	if s := slice(t, c); s.Endpoints[0].Addresses[0] != "10.0.0.11" || !*s.Endpoints[0].Conditions.Ready {
		t.Fatalf("old leader clobbered the new leader's endpoint: %+v", s.Endpoints)
	}

	// B stopping marks its own endpoint not ready.
	stopB()
	if s := slice(t, c); *s.Endpoints[0].Conditions.Ready || !*s.Endpoints[0].Conditions.Terminating {
		t.Fatalf("stopped leader still ready: %+v", s.Endpoints[0].Conditions)
	}
}

func TestEndpointPublisher_IPv6AndRepair(t *testing.T) {
	c := fake.NewClientset()
	stop := runPublisher(t, &EndpointPublisher{Client: c, Namespace: "hyper-system", ServiceName: "hyper-operator-identity",
		PodName: "operator-a", PodIP: "fd00::10", Port: 9444, Interval: 20 * time.Millisecond})
	defer stop()
	eventually(t, "IPv6 slice", func() bool { s := slice(t, c); return s != nil && s.AddressType == discoveryv1.AddressTypeIPv6 })

	// Someone edits the slice: the publisher puts it back.
	s := slice(t, c)
	s.Endpoints[0].Addresses = []string{"fd00::99"}
	if _, err := c.DiscoveryV1().EndpointSlices("hyper-system").Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "repair", func() bool { s := slice(t, c); return s.Endpoints[0].Addresses[0] == "fd00::10" })
}

func TestEndpointPublisher_RequiresPodIP(t *testing.T) {
	p := &EndpointPublisher{Client: fake.NewClientset(), Namespace: "x", ServiceName: "s", PodIP: ""}
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected an error without POD_IP")
	}
}
