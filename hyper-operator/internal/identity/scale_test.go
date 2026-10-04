package identity

import (
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

// TestScale_SimulatedPods measures the identity pipeline with many simulated
// pods (identity Phase 5): index memory, the engine's initial sync, and the
// latency of single changes. It runs only with HYPERGATE_SCALE_TEST=1
// (the kind end-to-end workflow sets it); HYPERGATE_SCALE_PODS sets the size.
func TestScale_SimulatedPods(t *testing.T) {
	if os.Getenv("HYPERGATE_SCALE_TEST") != "1" {
		t.Skip("set HYPERGATE_SCALE_TEST=1 to run")
	}
	pods := 10000
	if v := os.Getenv("HYPERGATE_SCALE_PODS"); v != "" {
		if _, err := fmt.Sscan(v, &pods); err != nil {
			t.Fatal(err)
		}
	}
	const services = 200

	podIP := func(i int) netip.Addr {
		return netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	x := NewIndex(Options{})
	for i := range pods {
		ns := fmt.Sprintf("ns-%d", i%50)
		x.UpsertPod(PodRecord{
			UID: fmt.Sprintf("uid-%d", i), Namespace: ns, Name: fmt.Sprintf("pod-%d", i),
			ServiceAccount: fmt.Sprintf("sa-%d", i%services), Node: fmt.Sprintf("node-%d", i%100),
			Labels: map[string]string{"app": fmt.Sprintf("app-%d", i%services), "version": "v1"},
			IPs:    []netip.Addr{podIP(i)}, Created: t0,
		})
	}
	for s := range services {
		var uids []string
		for i := s; i < pods; i += services {
			uids = append(uids, fmt.Sprintf("uid-%d", i))
		}
		x.UpsertSlice(SliceRecord{Namespace: fmt.Sprintf("ns-%d", s%50), Name: fmt.Sprintf("svc-%d-x", s), Service: fmt.Sprintf("svc-%d", s), PodUIDs: uids})
		x.UpsertService(Service{Namespace: fmt.Sprintf("ns-%d", s%50), Name: fmt.Sprintf("svc-%d", s),
			ClusterIPs: []netip.Addr{netip.AddrFrom4([4]byte{10, 96, byte(s >> 8), byte(s)})}, Ports: []uint32{80}})
	}
	buildTime := time.Since(start)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	indexMB := float64(after.HeapAlloc-before.HeapAlloc) / (1 << 20)

	sw := &leaderSwitch{}
	stop := sw.start(t, x)
	defer stop()
	start = time.Now()
	client, stopClient := engineClient(t, sw, "good")
	defer stopClient()
	deadline := time.Now().Add(60 * time.Second)
	for !client.Index().Synced() || client.Index().Status().Workloads < pods {
		if time.Now().After(deadline) {
			t.Fatalf("engine did not receive %d workloads (has %d)", pods, client.Index().Status().Workloads)
		}
		time.Sleep(10 * time.Millisecond)
	}
	syncTime := time.Since(start)

	// Single changes: a new pod appears; measure until the engine sees it.
	var deltas []time.Duration
	for i := range 20 {
		n := pods + i
		start := time.Now()
		x.UpsertPod(PodRecord{UID: fmt.Sprintf("new-%d", i), Namespace: "ns-0", Name: fmt.Sprintf("new-%d", i),
			IPs: []netip.Addr{podIP(n)}, Created: t0})
		for client.Index().Lookup(podIP(n)) == nil {
			if time.Since(start) > 10*time.Second {
				t.Fatalf("change %d never reached the engine", i)
			}
			time.Sleep(time.Millisecond)
		}
		deltas = append(deltas, time.Since(start))
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	p50, p95 := deltas[len(deltas)/2], deltas[len(deltas)*95/100]

	report := fmt.Sprintf(`## Identity scale (%d simulated pods, %d Services)

| Measure | Value |
| --- | --- |
| Index build | %s |
| Index heap | %.1f MiB (%.0f bytes per pod) |
| Engine initial sync (stream + decode) | %s |
| Single change to engine, p50 | %s |
| Single change to engine, p95 | %s |
`, pods, services, buildTime.Round(time.Millisecond), indexMB, indexMB*(1<<20)/float64(pods),
		syncTime.Round(time.Millisecond), p50.Round(time.Microsecond), p95.Round(time.Microsecond))
	t.Log("\n" + report)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			_, _ = f.WriteString(report + "\n")
			_ = f.Close()
		}
	}

	if syncTime > 30*time.Second {
		t.Errorf("initial sync took %s", syncTime)
	}
	if p95 > 2*time.Second {
		t.Errorf("p95 change latency %s", p95)
	}
}
