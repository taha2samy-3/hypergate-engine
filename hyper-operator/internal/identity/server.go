package identity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/taha2samy/hypergate/hyper-operator/internal/leader"
	identityv1 "github.com/taha2samy/hypergate/internal/identityapi/v1"
)

// Type URLs of the identity resources (docs/design/identity-distribution.md §5).
const (
	WorkloadTypeURL = "type.googleapis.com/hypergate.identity.v1.Workload"
	ServiceTypeURL  = "type.googleapis.com/hypergate.identity.v1.Service"
)

// ServerOptions configure the identity stream server.
type ServerOptions struct {
	// Address to listen on. Default ":9444".
	Address string
	// CertFile and KeyFile hold the server certificate (reloaded when they change).
	CertFile string
	KeyFile  string
	// Insecure serves plaintext. Only for tests and local development: the
	// engines' ServiceAccount tokens would cross the network unencrypted.
	Insecure bool
	// Authenticator validates the bearer token of every stream. Required.
	Authenticator Authenticator
}

// Server streams the index to engines over delta xDS. It runs on the leader only.
type Server struct {
	index *Index
	opts  ServerOptions
	log   logr.Logger

	workloads *cachev3.LinearCache
	services  *cachev3.LinearCache

	// Last resources pushed into the caches, to send only real changes.
	mu            sync.Mutex
	lastWorkloads map[string]*identityv1.Workload
	lastServices  map[string]*identityv1.Service
}

// NewServer returns a server for the index.
func NewServer(index *Index, opts ServerOptions) (*Server, error) {
	if opts.Authenticator == nil {
		return nil, errors.New("identity server: an authenticator is required")
	}
	if !opts.Insecure && (opts.CertFile == "" || opts.KeyFile == "") {
		return nil, errors.New("identity server: a TLS certificate and key are required (or Insecure for development)")
	}
	if opts.Address == "" {
		opts.Address = ":9444"
	}
	return &Server{
		index:         index,
		opts:          opts,
		log:           ctrl.Log.WithName("identity-server"),
		workloads:     cachev3.NewLinearCache(WorkloadTypeURL),
		services:      cachev3.NewLinearCache(ServiceTypeURL),
		lastWorkloads: map[string]*identityv1.Workload{},
		lastServices:  map[string]*identityv1.Service{},
	}, nil
}

// Runnable returns the server as a leader-only manager runnable: standbys never
// serve, so an engine can never receive a stale map from a former leader.
func (s *Server) Runnable() manager.Runnable {
	return leader.LeaderOnly("identity-server", s.Start)
}

// Start listens on the configured address and serves until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.opts.Address)
	if err != nil {
		return fmt.Errorf("identity server: listen on %s: %w", s.opts.Address, err)
	}
	return s.Serve(ctx, lis)
}

// Serve serves on lis until ctx is done.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	creds, err := s.credentials()
	if err != nil {
		_ = lis.Close()
		return err
	}
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(s.authStream),
		grpc.UnaryInterceptor(s.authUnary),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	mux := &cachev3.MuxCache{
		Classify:      func(r *cachev3.Request) string { return r.GetTypeUrl() },
		ClassifyDelta: func(r *cachev3.DeltaRequest) string { return r.GetTypeUrl() },
		Caches: map[string]cachev3.Cache{
			WorkloadTypeURL: s.workloads,
			ServiceTypeURL:  s.services,
		},
	}
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, serverv3.NewServer(ctx, mux, serverv3.CallbackFuncs{}))

	// The first sync completes before engines are accepted, so nobody receives
	// a partial map.
	s.Sync()
	go s.syncLoop(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(lis) }()
	s.log.Info("Serving workload identities", "address", lis.Addr().String(), "tls", !s.opts.Insecure)

	select {
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() { grpcServer.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
		s.log.Info("Stopped serving workload identities")
		return nil
	case err := <-errCh:
		return fmt.Errorf("identity server: %w", err)
	}
}

func (s *Server) syncLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.index.Changed():
			s.Sync()
		}
	}
}

// Sync pushes the differences between the index and the caches.
func (s *Server) Sync() {
	s.mu.Lock()
	defer s.mu.Unlock()

	nextW := map[string]*identityv1.Workload{}
	for _, w := range s.index.Workloads() {
		nextW[w.IP.String()] = workloadProto(w)
	}
	nextS := map[string]*identityv1.Service{}
	for _, svc := range s.index.Services() {
		nextS[svc.Namespace+"/"+svc.Name] = serviceProto(svc)
	}

	if err := applyDiff(s.workloads, s.lastWorkloads, nextW); err != nil {
		s.log.Error(err, "Updating workload resources failed")
	}
	if err := applyDiff(s.services, s.lastServices, nextS); err != nil {
		s.log.Error(err, "Updating service resources failed")
	}
	s.lastWorkloads, s.lastServices = nextW, nextS
}

func applyDiff[M proto.Message](c *cachev3.LinearCache, last, next map[string]M) error {
	update := map[string]types.Resource{}
	var remove []string
	for name, res := range next {
		if old, ok := last[name]; !ok || !proto.Equal(old, res) {
			update[name] = res
		}
	}
	for name := range last {
		if _, ok := next[name]; !ok {
			remove = append(remove, name)
		}
	}
	if len(update) == 0 && len(remove) == 0 {
		return nil
	}
	return c.UpdateResources(update, remove)
}

func workloadProto(w Workload) *identityv1.Workload {
	out := &identityv1.Workload{
		Ip:               w.IP.String(),
		PodUid:           w.PodUID,
		Namespace:        w.Namespace,
		Pod:              w.Pod,
		ServiceAccount:   w.ServiceAccount,
		SpiffeId:         w.SPIFFEID,
		SecurityIdentity: w.SecurityIdentity,
		Labels:           w.Labels,
		Node:             w.Node,
		CiliumLabels:     w.CiliumLabels,
	}
	switch w.Kind {
	case KindPod:
		out.Kind = identityv1.WorkloadKind_WORKLOAD_KIND_POD
	case KindNode:
		out.Kind = identityv1.WorkloadKind_WORKLOAD_KIND_NODE
	}
	for _, ref := range w.Services {
		out.Services = append(out.Services, &identityv1.ServiceRef{Namespace: ref.Namespace, Name: ref.Name})
	}
	return out
}

func serviceProto(s Service) *identityv1.Service {
	out := &identityv1.Service{Name: s.Name, Namespace: s.Namespace, Ports: s.Ports}
	for _, ip := range s.ClusterIPs {
		out.ClusterIps = append(out.ClusterIps, ip.String())
	}
	return out
}

// --- authentication ---

// Authentication errors returned by an Authenticator.
var (
	ErrUnauthenticated = errors.New("token not accepted")
	ErrForbidden       = errors.New("identity not allowed")
)

// Authenticator validates a bearer token and returns the caller's identity.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

func (s *Server) authStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	id, err := s.authenticate(ss.Context())
	if err != nil {
		return err
	}
	s.log.V(1).Info("Engine stream opened", "identity", id, "method", info.FullMethod)
	return handler(srv, ss)
}

func (s *Server) authUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if _, err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) authenticate(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	var token string
	for _, v := range md.Get("authorization") {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok && t != "" {
			token = t
			break
		}
	}
	if token == "" {
		return "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	id, err := s.opts.Authenticator.Authenticate(ctx, token)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, ErrUnauthenticated):
		return "", status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, ErrForbidden):
		return "", status.Error(codes.PermissionDenied, err.Error())
	}
	s.log.Error(err, "Token validation failed")
	return "", status.Error(codes.Unavailable, "token validation unavailable")
}

// --- TLS ---

func (s *Server) credentials() (credentials.TransportCredentials, error) {
	if s.opts.Insecure {
		s.log.Info("WARNING: identity server runs without TLS; engine tokens are sent in clear text")
		return insecure.NewCredentials(), nil
	}
	reloader := &certReloader{certFile: s.opts.CertFile, keyFile: s.opts.KeyFile}
	if _, err := reloader.get(); err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return reloader.get()
		},
	}), nil
}

// certReloader serves the certificate files and reloads them when they change,
// so a renewal by cert-manager needs no restart.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
}

func (r *certReloader) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.certFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil // keep serving the last good certificate
		}
		return nil, fmt.Errorf("identity server: certificate: %w", err)
	}
	if r.cert != nil && info.ModTime().Equal(r.modTime) {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, fmt.Errorf("identity server: load certificate: %w", err)
	}
	r.cert, r.modTime = &cert, info.ModTime()
	return r.cert, nil
}
