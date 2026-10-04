package identity

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	cacheSynced = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hypergate_identity_cache_synced",
		Help: "1 once the identity cache has indexed the initial state of every watched resource.",
	})
	cacheEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypergate_identity_cache_entries",
		Help: "Entries in the identity cache by kind (pod_ip, node_ip, service).",
	}, []string{"kind"})
)

func init() {
	metrics.Registry.MustRegister(cacheSynced, cacheEntries)
}

func (c *Cache) updateMetrics() {
	if !c.synced.Load() {
		return
	}
	podIPs, nodeIPs, services := c.index.Counts()
	cacheSynced.Set(1)
	cacheEntries.WithLabelValues("pod_ip").Set(float64(podIPs))
	cacheEntries.WithLabelValues("node_ip").Set(float64(nodeIPs))
	cacheEntries.WithLabelValues("service").Set(float64(services))
}
