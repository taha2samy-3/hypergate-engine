package identity

import (
	"flag"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Config holds the operator flags of the identity cache and server.
type Config struct {
	Enabled     bool
	TrustDomain string
	LabelKeys   string

	ServerEnabled  bool
	ServerAddress  string
	ServerCertDir  string
	ServerInsecure bool
	ServiceName    string
}

// BindFlags registers the identity cache flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.Enabled, "identity-cache", false,
		"Build the IP to workload identity map on every replica (needs read access to pods, nodes, services and endpointslices).")
	fs.StringVar(&c.TrustDomain, "identity-trust-domain", "cluster.local", "Trust domain of the SPIFFE IDs in the identity map.")
	fs.StringVar(&c.LabelKeys, "identity-label-keys", strings.Join(DefaultLabelKeys, ","),
		"Comma-separated pod label keys kept in the identity map.")
	fs.BoolVar(&c.ServerEnabled, "identity-server", false,
		"On the leader, stream the identity map to engines (needs --identity-cache) and point the identity Service at this replica.")
	fs.StringVar(&c.ServerAddress, "identity-server-address", ":9444", "Listen address of the identity stream server.")
	fs.StringVar(&c.ServerCertDir, "identity-server-cert-dir", "",
		"Directory with tls.crt and tls.key for the identity server (reloaded when they change).")
	fs.BoolVar(&c.ServerInsecure, "identity-server-insecure", false,
		"Serve identities without TLS. Development only: engine tokens would be sent in clear text.")
	fs.StringVar(&c.ServiceName, "identity-service-name", "hyper-operator-identity",
		"Selector-less Service whose EndpointSlice the leader writes.")
}

// Validate checks flag combinations.
func (c *Config) Validate() error {
	if !c.ServerEnabled {
		return nil
	}
	if !c.Enabled {
		return fmt.Errorf("--identity-server needs --identity-cache")
	}
	if c.ServerCertDir == "" && !c.ServerInsecure {
		return fmt.Errorf("--identity-server needs --identity-server-cert-dir (or --identity-server-insecure for development)")
	}
	return nil
}

// ServerOptions converts the flags into server options.
func (c *Config) ServerOptions(auth Authenticator) ServerOptions {
	opts := ServerOptions{Address: c.ServerAddress, Insecure: c.ServerInsecure, Authenticator: auth}
	if c.ServerCertDir != "" {
		opts.CertFile = filepath.Join(c.ServerCertDir, "tls.crt")
		opts.KeyFile = filepath.Join(c.ServerCertDir, "tls.key")
	}
	return opts
}

// ServerPort returns the port of ServerAddress.
func (c *Config) ServerPort() (int32, error) {
	_, port, err := net.SplitHostPort(c.ServerAddress)
	if err != nil {
		return 0, err
	}
	p, err := strconv.ParseInt(port, 10, 32)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid identity server port %q", port)
	}
	return int32(p), nil
}

// Options converts the flags into cache options.
func (c *Config) Options() CacheOptions {
	var keys []string
	for _, k := range strings.Split(c.LabelKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return CacheOptions{Options: Options{TrustDomain: c.TrustDomain, LabelKeys: keys}}
}

// NewCacheForConfig builds a Cache with clients for the given REST config.
func NewCacheForConfig(cfg *rest.Config, opts CacheOptions) (*Cache, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("identity cache: kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("identity cache: dynamic client: %w", err)
	}
	return NewCache(client, dyn, opts), nil
}
