package identity

import (
	"encoding/json"
	"net/http"
	"net/netip"
)

// DebugHandler serves GET /debug/identity (status) and /debug/identity?ip=<addr>
// (the workload behind an address) for troubleshooting.
func DebugHandler(c *Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ipParam := r.URL.Query().Get("ip")
		if ipParam == "" {
			_ = json.NewEncoder(w).Encode(struct {
				Status
				Connected bool `json:"connected"`
			}{c.Index().Status(), c.Connected()})
			return
		}
		ip, err := netip.ParseAddr(ipParam)
		if err != nil {
			http.Error(w, `{"error":"invalid ip"}`, http.StatusBadRequest)
			return
		}
		wl := c.Index().Lookup(ip)
		if wl == nil {
			if svc := c.Index().ServiceByClusterIP(ip); svc != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"service": svc.Key(), "clusterIPs": svc.ClusterIPs, "ports": svc.Ports})
				return
			}
			http.Error(w, `{"error":"unknown"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ip": wl.IP, "kind": wl.Kind.String(), "namespace": wl.Namespace, "pod": wl.Pod,
			"serviceAccount": wl.ServiceAccount, "spiffeID": wl.SPIFFEID, "node": wl.Node,
			"labels": wl.Labels, "services": wl.Services, "securityIdentity": wl.SecurityIdentity,
		})
	})
}
