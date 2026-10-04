package identity

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"

	engineidentity "github.com/taha2samy/hypergate/internal/identity"
)

// leaderSwitch serves identities from one operator server at a time and lets
// the test replace it, like a leader change behind the identity Service.
type leaderSwitch struct {
	current atomic.Pointer[bufconn.Listener]
}

func (l *leaderSwitch) dial(ctx context.Context, _ string) (net.Conn, error) {
	return l.current.Load().DialContext(ctx)
}

func (l *leaderSwitch) start(t *testing.T, x *Index) context.CancelFunc {
	t.Helper()
	srv, err := NewServer(x, ServerOptions{Insecure: true, Authenticator: testAuth})
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, lis); close(done) }()
	l.current.Store(lis)
	return func() { cancel(); <-done }
}

func engineClient(t *testing.T, sw *leaderSwitch, token string) (*engineidentity.Client, context.CancelFunc) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := engineidentity.NewClient(engineidentity.Options{
		Address: "hyper-operator-identity.hyper-system.svc:9444", Insecure: true, TokenFile: tokenFile,
		NodeID: "node-1", Dialer: sw.dial,
	}, engineidentity.NewIndex())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	return c, func() { cancel(); <-done }
}

func TestEndToEnd_EngineClientFollowsTheLeader(t *testing.T) {
	a, b := pod("u1", "shop", "checkout", t0, "10.0.0.1"), pod("u2", "shop", "cart", t0, "10.0.0.2")
	sw := &leaderSwitch{}

	leader1Index := indexWith(a, b)
	leader1Index.UpsertSlice(SliceRecord{Namespace: "shop", Name: "s", Service: "checkout", PodUIDs: []string{"u1"}})
	stop1 := sw.start(t, leader1Index)

	client, stopClient := engineClient(t, sw, "good")
	defer stopClient()
	idx := client.Index()

	eventually(t, "engine sync", idx.Synced)
	w := idx.Lookup(netip.MustParseAddr("10.0.0.1"))
	if !w.IsPod() || w.Namespace != "shop" || w.ServiceAccount != "checkout" || len(w.Services) != 1 || w.Services[0] != "shop/checkout" {
		t.Fatalf("workload = %+v", w)
	}
	if s := idx.ServiceByClusterIP(netip.MustParseAddr("10.96.0.20")); s == nil || s.Key() != "payments/ledger" {
		t.Fatalf("service = %+v", s)
	}

	// Live update from the leader.
	leader1Index.UpsertPod(pod("u3", "shop", "web", t0, "10.0.0.3"))
	eventually(t, "live update", func() bool { return idx.Lookup(netip.MustParseAddr("10.0.0.3")) != nil })

	// Leader change: the old leader stops; the engine keeps its last state.
	stop1()
	time.Sleep(100 * time.Millisecond)
	if idx.Lookup(netip.MustParseAddr("10.0.0.2")) == nil {
		t.Fatal("engine dropped its state while disconnected")
	}

	// The new leader's state differs: cart is gone, a new pod exists.
	sw.start(t, indexWith(a, pod("u3", "shop", "web", t0, "10.0.0.3"), pod("u4", "shop", "api", t0, "10.0.0.4")))
	eventually(t, "resync with the new leader", func() bool {
		return idx.Lookup(netip.MustParseAddr("10.0.0.4")) != nil && idx.Lookup(netip.MustParseAddr("10.0.0.2")) == nil
	})
	if idx.Lookup(netip.MustParseAddr("10.0.0.1")) == nil || idx.Lookup(netip.MustParseAddr("10.0.0.3")) == nil {
		t.Fatal("unchanged workloads lost across the leader change")
	}
	if !client.Connected() {
		t.Fatal("client not connected to the new leader")
	}
}

func TestEndToEnd_RejectedTokenNeverSyncs(t *testing.T) {
	sw := &leaderSwitch{}
	stop := sw.start(t, indexWith(pod("u1", "shop", "a", t0, "10.0.0.1")))
	defer stop()
	client, stopClient := engineClient(t, sw, "wrong")
	defer stopClient()
	time.Sleep(300 * time.Millisecond)
	if client.Index().Synced() || client.Connected() {
		t.Fatal("an engine with a rejected token received identities")
	}
}
