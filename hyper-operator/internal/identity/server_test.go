package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	identityv1 "github.com/taha2samy/hypergate/internal/identityapi/v1"
)

// fakeAuth accepts tokens mapped to an identity; "forbidden" maps to ErrForbidden.
type fakeAuth map[string]string

func (f fakeAuth) Authenticate(_ context.Context, token string) (string, error) {
	switch id, ok := f[token]; {
	case !ok:
		return "", ErrUnauthenticated
	case id == "forbidden":
		return "", ErrForbidden
	default:
		return id, nil
	}
}

var testAuth = fakeAuth{"good": "system:serviceaccount:hyper-system:hyper-engine-sa", "other": "forbidden"}

func indexWith(pods ...PodRecord) *Index {
	x := NewIndex(Options{})
	for _, p := range pods {
		x.UpsertPod(p)
	}
	x.UpsertService(Service{Namespace: "payments", Name: "ledger", ClusterIPs: ips("10.96.0.20"), Ports: []uint32{8080}})
	return x
}

// startServer serves the index over an in-memory connection and returns a client.
func startServer(t *testing.T, x *Index, opts ServerOptions, dialCreds credentials.TransportCredentials) discoverygrpc.AggregatedDiscoveryServiceClient {
	t.Helper()
	if opts.Authenticator == nil {
		opts.Authenticator = testAuth
	}
	if opts.CertFile == "" {
		opts.Insecure = true
	}
	srv, err := NewServer(x, opts)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, lis) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	if dialCreds == nil {
		dialCreds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient("passthrough:///identity",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return discoverygrpc.NewAggregatedDiscoveryServiceClient(conn)
}

type deltaClient struct {
	t      *testing.T
	stream discoverygrpc.AggregatedDiscoveryService_DeltaAggregatedResourcesClient
}

func subscribe(t *testing.T, c discoverygrpc.AggregatedDiscoveryServiceClient, token, typeURL string, known map[string]string) (*deltaClient, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	stream, err := c.DeltaAggregatedResources(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&discoverygrpc.DeltaDiscoveryRequest{
		Node:                    &corev3.Node{Id: "engine-node-1"},
		TypeUrl:                 typeURL,
		ResourceNamesSubscribe:  []string{"*"},
		InitialResourceVersions: known,
	})
	return &deltaClient{t: t, stream: stream}, err
}

// recv reads one response, ACKs it and returns resource versions and removals.
func (d *deltaClient) recv() (map[string]string, []string, error) {
	resp, err := d.stream.Recv()
	if err != nil {
		return nil, nil, err
	}
	got := map[string]string{}
	for _, r := range resp.Resources {
		got[r.Name] = r.Version
	}
	err = d.stream.Send(&discoverygrpc.DeltaDiscoveryRequest{TypeUrl: resp.TypeUrl, ResponseNonce: resp.Nonce})
	sort.Strings(resp.RemovedResources)
	return got, resp.RemovedResources, err
}

func TestServer_SnapshotThenDeltas(t *testing.T) {
	x := indexWith(pod("u1", "shop", "a", t0, "10.0.0.1"), pod("u2", "shop", "b", t0, "10.0.0.2"))
	client := startServer(t, x, ServerOptions{}, nil)

	d, err := subscribe(t, client, "good", WorkloadTypeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := d.recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["10.0.0.1"] == "" || got["10.0.0.2"] == "" {
		t.Fatalf("initial snapshot = %v", got)
	}

	// Change the index: one pod gone, one pod new.
	x.DeletePod("u1")
	x.UpsertPod(pod("u3", "shop", "c", t0, "10.0.0.3"))
	updated, removed := map[string]string{}, []string{}
	deadline := time.Now().Add(5 * time.Second)
	for (len(updated) == 0 || len(removed) == 0) && time.Now().Before(deadline) {
		u, r, err := d.recv()
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range u {
			updated[k] = v
		}
		removed = append(removed, r...)
	}
	if len(updated) != 1 || updated["10.0.0.3"] == "" {
		t.Fatalf("delta must carry only the new pod, got %v", updated)
	}
	if len(removed) != 1 || removed[0] != "10.0.0.1" {
		t.Fatalf("removed = %v", removed)
	}
}

func TestServer_WorkloadContentAndServices(t *testing.T) {
	p := pod("u1", "shop", "checkout", t0, "10.0.0.5")
	x := indexWith(p)
	x.UpsertSlice(SliceRecord{Namespace: "shop", Name: "s1", Service: "checkout", PodUIDs: []string{"u1"}})
	client := startServer(t, x, ServerOptions{}, nil)

	d, err := subscribe(t, client, "good", WorkloadTypeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := d.stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var w identityv1.Workload
	if err := resp.Resources[0].Resource.UnmarshalTo(&w); err != nil {
		t.Fatal(err)
	}
	if w.Ip != "10.0.0.5" || w.Pod != "checkout" || w.Kind != identityv1.WorkloadKind_WORKLOAD_KIND_POD ||
		w.SpiffeId != "spiffe://cluster.local/ns/shop/sa/checkout" || len(w.Services) != 1 || w.Services[0].Name != "checkout" {
		t.Fatalf("workload = %v", &w)
	}

	ds, err := subscribe(t, client, "good", ServiceTypeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	sresp, err := ds.stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var s identityv1.Service
	if len(sresp.Resources) != 1 || sresp.Resources[0].Name != "payments/ledger" {
		t.Fatalf("services = %v", sresp.Resources)
	}
	if err := sresp.Resources[0].Resource.UnmarshalTo(&s); err != nil || s.ClusterIps[0] != "10.96.0.20" || s.Ports[0] != 8080 {
		t.Fatalf("service = %v %v", &s, err)
	}
}

// A new leader built from the same cluster state produces the same versions, so
// an engine that reconnects with its known versions receives only differences.
func TestServer_ResumeAgainstNewLeader(t *testing.T) {
	a, b := pod("u1", "shop", "a", t0, "10.0.0.1"), pod("u2", "shop", "b", t0, "10.0.0.2")

	leader1 := startServer(t, indexWith(a, b), ServerOptions{}, nil)
	d, err := subscribe(t, leader1, "good", WorkloadTypeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	known, _, err := d.recv()
	if err != nil {
		t.Fatal(err)
	}

	// The new leader's index has one more pod.
	leader2 := startServer(t, indexWith(a, b, pod("u3", "shop", "c", t0, "10.0.0.3")), ServerOptions{}, nil)
	d2, err := subscribe(t, leader2, "good", WorkloadTypeURL, known)
	if err != nil {
		t.Fatal(err)
	}
	got, removed, err := d2.recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["10.0.0.3"] == "" || len(removed) != 0 {
		t.Fatalf("resume must send only the difference, got %v removed %v", got, removed)
	}
}

func TestServer_RejectsUnauthenticatedClients(t *testing.T) {
	client := startServer(t, indexWith(), ServerOptions{}, nil)
	for token, want := range map[string]codes.Code{"": codes.Unauthenticated, "wrong": codes.Unauthenticated, "other": codes.PermissionDenied} {
		d, err := subscribe(t, client, token, WorkloadTypeURL, nil)
		if err == nil {
			_, _, err = d.recv()
		}
		if status.Code(err) != want {
			t.Errorf("token %q: got %v, want %v", token, err, want)
		}
	}
}

func TestNewServer_RequiresTLSAndAuthenticator(t *testing.T) {
	if _, err := NewServer(NewIndex(Options{}), ServerOptions{Authenticator: testAuth}); err == nil {
		t.Error("a server without TLS and without Insecure must be refused")
	}
	if _, err := NewServer(NewIndex(Options{}), ServerOptions{Insecure: true}); err == nil {
		t.Error("a server without an authenticator must be refused")
	}
}

func TestServer_TLS(t *testing.T) {
	certFile, keyFile, pool := selfSignedCert(t, "hyper-operator-identity.hyper-system.svc")
	client := startServer(t, indexWith(pod("u1", "shop", "a", t0, "10.0.0.1")),
		ServerOptions{CertFile: certFile, KeyFile: keyFile},
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "hyper-operator-identity.hyper-system.svc", MinVersion: tls.VersionTLS13}))
	d, err := subscribe(t, client, "good", WorkloadTypeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := d.recv(); err != nil || len(got) != 1 {
		t.Fatalf("TLS stream: %v %v", got, err)
	}
}

func selfSignedCert(t *testing.T, dnsName string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}
