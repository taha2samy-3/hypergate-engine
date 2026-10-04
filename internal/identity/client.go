package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"go.uber.org/zap"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	identityv1 "github.com/taha2samy/hypergate/internal/identityapi/v1"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// DefaultTokenFile is where the engine mounts its projected ServiceAccount token.
const DefaultTokenFile = "/var/run/secrets/hypergate/identity/token"

// Options configure the client.
type Options struct {
	// Address of the operator's identity Service, host:port.
	Address string
	// CAFile verifies the server certificate. Re-read on every connection.
	CAFile string
	// ServerName overrides the name checked in the certificate (default: the host of Address).
	ServerName string
	// TokenFile holds the projected ServiceAccount token (audience
	// hypergate-identity). Re-read on every connection, so rotation needs no restart.
	TokenFile string
	// Insecure dials without TLS (development only).
	Insecure bool
	// NodeID identifies this engine in the xDS node field.
	NodeID string
	// Dialer overrides how connections are made (tests).
	Dialer func(ctx context.Context, addr string) (net.Conn, error)
}

// Client keeps an Index current from the operator's identity stream. It
// reconnects with jittered backoff and keeps serving the last state while
// disconnected.
type Client struct {
	opts      Options
	index     *Index
	connected atomic.Bool
}

// NewClient returns a client that fills index.
func NewClient(opts Options, index *Index) *Client {
	if opts.TokenFile == "" {
		opts.TokenFile = DefaultTokenFile
	}
	if opts.NodeID == "" {
		opts.NodeID, _ = os.Hostname()
	}
	return &Client{opts: opts, index: index}
}

// Index returns the index the client fills.
func (c *Client) Index() *Index { return c.index }

// Connected reports whether a stream is currently open.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run streams identities until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := c.stream(ctx)
		c.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond // the stream was healthy for a while
		}
		wait := backoff/2 + rand.N(backoff)
		mylogger.Warn("Identity stream ended, reconnecting",
			zap.String("address", c.opts.Address), zap.Duration("in", wait), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) stream(ctx context.Context) error {
	creds, err := c.transportCredentials()
	if err != nil {
		return err
	}
	token, err := os.ReadFile(c.opts.TokenFile)
	if err != nil {
		return fmt.Errorf("read identity token: %w", err)
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	}
	if c.opts.Dialer != nil {
		dialOpts = append(dialOpts, grpc.WithContextDialer(c.opts.Dialer))
	}
	target := c.opts.Address
	if c.opts.Dialer != nil {
		target = "passthrough:///" + target
	}
	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	streamCtx = metadata.AppendToOutgoingContext(streamCtx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
	ads, err := discoverygrpc.NewAggregatedDiscoveryServiceClient(conn).DeltaAggregatedResources(streamCtx)
	if err != nil {
		return err
	}

	node := &corev3.Node{Id: c.opts.NodeID, Cluster: "hypergate-engine"}
	for _, typeURL := range []string{WorkloadTypeURL, ServiceTypeURL} {
		if err := ads.Send(&discoverygrpc.DeltaDiscoveryRequest{
			Node:                    node,
			TypeUrl:                 typeURL,
			ResourceNamesSubscribe:  []string{"*"},
			InitialResourceVersions: c.index.versionsOf(typeURL),
		}); err != nil {
			return err
		}
	}

	for {
		resp, err := ads.Recv()
		if err != nil {
			return err
		}
		if !c.connected.Swap(true) {
			mylogger.Info("Identity stream connected", zap.String("address", c.opts.Address))
		}
		ack := &discoverygrpc.DeltaDiscoveryRequest{TypeUrl: resp.GetTypeUrl(), ResponseNonce: resp.GetNonce()}
		if err := c.apply(resp); err != nil {
			mylogger.Error("Rejecting identity update", zap.String("type", resp.GetTypeUrl()), zap.Error(err))
			ack.ErrorDetail = &statuspb.Status{Code: int32(codes.InvalidArgument), Message: err.Error()}
		}
		if err := ads.Send(ack); err != nil {
			return err
		}
	}
}

// apply decodes a response and updates the index; a response with any
// undecodable resource is rejected as a whole (NACK).
func (c *Client) apply(resp *discoverygrpc.DeltaDiscoveryResponse) error {
	switch resp.GetTypeUrl() {
	case WorkloadTypeURL:
		updated := make(map[string]versioned[*identityv1.Workload], len(resp.GetResources()))
		for _, r := range resp.GetResources() {
			var w identityv1.Workload
			if err := r.GetResource().UnmarshalTo(&w); err != nil {
				return fmt.Errorf("resource %s: %w", r.GetName(), err)
			}
			updated[r.GetName()] = versioned[*identityv1.Workload]{&w, r.GetVersion()}
		}
		c.index.applyWorkloads(updated, resp.GetRemovedResources())
	case ServiceTypeURL:
		updated := make(map[string]versioned[*identityv1.Service], len(resp.GetResources()))
		for _, r := range resp.GetResources() {
			var s identityv1.Service
			if err := r.GetResource().UnmarshalTo(&s); err != nil {
				return fmt.Errorf("resource %s: %w", r.GetName(), err)
			}
			updated[r.GetName()] = versioned[*identityv1.Service]{&s, r.GetVersion()}
		}
		c.index.applyServices(updated, resp.GetRemovedResources())
	default:
		return fmt.Errorf("unexpected type %q", resp.GetTypeUrl())
	}
	return nil
}

func (c *Client) transportCredentials() (credentials.TransportCredentials, error) {
	if c.opts.Insecure {
		return insecure.NewCredentials(), nil
	}
	if c.opts.CAFile == "" {
		return nil, errors.New("identity: ca_file is required unless insecure")
	}
	pem, err := os.ReadFile(c.opts.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read identity CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("identity CA file %s contains no certificate", c.opts.CAFile)
	}
	serverName := c.opts.ServerName
	if serverName == "" {
		serverName, _, _ = net.SplitHostPort(c.opts.Address)
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}), nil
}
