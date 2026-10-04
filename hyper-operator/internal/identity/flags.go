package identity

import (
	"flag"
	"fmt"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Config holds the operator flags of the identity cache.
type Config struct {
	Enabled     bool
	TrustDomain string
	LabelKeys   string
}

// BindFlags registers the identity cache flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.Enabled, "identity-cache", false,
		"Build the IP to workload identity map on every replica (needs read access to pods, nodes, services and endpointslices).")
	fs.StringVar(&c.TrustDomain, "identity-trust-domain", "cluster.local", "Trust domain of the SPIFFE IDs in the identity map.")
	fs.StringVar(&c.LabelKeys, "identity-label-keys", strings.Join(DefaultLabelKeys, ","),
		"Comma-separated pod label keys kept in the identity map.")
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
