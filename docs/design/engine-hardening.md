# Design: engine hardening before the ext_proc release

| | |
| --- | --- |
| Status | **Proposed** (research done, no code yet) |
| Scope | Engine (`cmd/engine`, `internal/memory`, `internal/grpc`, `internal/engine`), operator (engine DaemonSet), CI |
| Out of scope | `CiliumEnvoyConfig`: it does not support ext_proc, so nothing here builds on it |

Five items, in the order proposed for implementation: CI (1), graceful shutdown (3), memory (2), metrics (4), audit mode and chain limits (5). Every number below was measured on the current code unless marked otherwise. Revised after a second research round (2026-10-09): adaptive shutdown, how `PreferSameNode` applies on Envoy Gateway, and the memory-limit library.

## 1. kind e2e as a required check

**Findings**

- The pending commit is pushed and the kind run **passed** (run 37872949825, 2026-10-09): all routing, identity and failover checks green.
- The "crash" check is not a real crash: a force-deleted pod still shut down cleanly and released its Lease (takeover 2.1 s). It must kill the process without SIGTERM (e.g. `crictl stop --timeout 0` on the kind node), with a budget of about the lease duration.
- `main` has no branch protection at all today.
- The repository uses a merge queue (`02-merge-queue.yaml`, `on: merge_group`). `e2e-kind.yaml` triggers only on `pull_request` and `push`, with a `paths` filter. Made required as it is, it would block merges in two ways:
  - a PR that does not touch the filtered paths never runs the workflow, and the required check waits forever;
  - merge-queue runs never run it, so the queue stalls.

**Proposal**

- Add `merge_group` to the workflow triggers.
- Replace the `paths` filter with a first job that detects changes (`dorny/paths-filter`, already used in `01-pr-validation.yaml`) and a final job named e.g. `kind-e2e-result` that always runs and succeeds when the e2e job succeeded **or was skipped because nothing relevant changed**. Only that final job is marked required.
- Confirmed by GitHub's docs ([troubleshooting required status checks](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/collaborating-on-repositories-with-code-quality-features/troubleshooting-required-status-checks)): a workflow skipped by a path filter leaves its required check pending, while a *job* skipped by a condition reports success; and required checks must also run on `merge_group`.
- Enable branch protection on `main` with `kind-e2e-result` as a required status check. This is a repository setting change; it needs your go-ahead.
- Cost: about 8–10 minutes per PR that touches Go, charts or tests.
- Fix the crash check as described above.

**Done (CI part):** `e2e-kind.yaml` now runs on `pull_request`, `merge_group`, `push` and manual runs, with a `changes` job deciding whether the expensive jobs run and an always-reporting `kind-e2e-result` job. The crash check kills the leader's container with SIGKILL from the node (`crictl stop --timeout 0`) and fails if the takeover is faster than 5 s (that would mean the Lease was released, so not a crash).

**Not enabled automatically:** `main` receives direct pushes from you and from the staging workflow (`03-deploy-test-staging.yaml` runs `git push origin main`). Classic branch protection with a required check would reject those pushes. Use a repository ruleset that applies to pull requests and lets admins and GitHub Actions bypass:

```bash
gh api -X POST repos/taha2samy-3/hypergate-engine/rulesets --input - <<'JSON'
{
  "name": "main: kind e2e required",
  "target": "branch",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/heads/main"], "exclude": [] } },
  "bypass_actors": [
    { "actor_type": "RepositoryRole", "actor_id": 5, "bypass_mode": "always" },
    { "actor_type": "Integration", "actor_id": 15368, "bypass_mode": "always" }
  ],
  "rules": [
    { "type": "required_status_checks",
      "parameters": { "strict_required_status_checks_policy": false,
        "required_status_checks": [ { "context": "kind-e2e-result" } ] } }
  ]
}
JSON
```

(`RepositoryRole` 5 is the admin role; integration 15368 is GitHub Actions.)

## 3. Graceful shutdown (no 5xx during engine rollouts)

**Findings** (second research round, from source code and upstream issues)

- On SIGTERM the engine sets `/readyz` to false and calls `GracefulStop()` **immediately** (`cmd/engine/main.go`). Streams Envoy opens before it has dropped the endpoint are refused; with `failure_mode_allow: false` those requests get a 5xx.
- **How Envoy Gateway stops using an endpoint:** in [`internal/gatewayapi/route.go`](https://github.com/envoyproxy/gateway/blob/main/internal/gatewayapi/route.go) an endpoint becomes *draining* as soon as its EndpointSlice condition is `terminating: true` or `serving: false` (feature added in [v1.3.0](https://gateway.envoyproxy.io/news/releases/notes/v1.3.0/)). Kubernetes sets `terminating` the moment the pod is deleted, so our readiness probe timing does **not** matter on this path. What matters is how long Envoy Gateway takes to push the change, and that can be slow: [envoyproxy/gateway#7366](https://github.com/envoyproxy/gateway/issues/7366) reports significant delays during rollouts, and [#4997](https://github.com/envoyproxy/gateway/issues/4997) recommends a pre-stop wait for exactly this race. A fixed 5 s delay is therefore a guess.
- **How long an ext_proc stream lives:** Envoy keeps the stream for the whole HTTP request (it closes it when the filter is destroyed at the end of the request; see [envoyproxy/envoy#47458](https://github.com/envoyproxy/envoy/issues/47458)). A long request (streaming download, SSE, WebSocket) keeps its ext_proc stream open, so `GracefulStop()` alone can wait indefinitely.
- The operator sets no `updateStrategy`, `terminationGracePeriodSeconds` or `lifecycle` on the engine DaemonSet (defaults: `maxUnavailable: 1`, no surge, 30 s grace period). The pod uses no `hostPort`, so it can surge (GA since Kubernetes 1.25).

**Proposal**

1. **Adaptive shutdown in the engine** (in-process; no preStop hook, which would only work in Kubernetes):
   SIGTERM → `/readyz` false → keep accepting streams until **no new stream has arrived for `server.shutdown_quiet_period`** (default 2 s), at least `server.shutdown_min_delay` (default 3 s) and at most `server.shutdown_max_delay` (default 15 s) → `GracefulStop()` bounded by `server.drain_timeout` (default 20 s) → `Stop()` for streams still open → close the policy.
   Waiting for quiet instead of a fixed time adapts to however long Envoy Gateway (or any other Envoy) takes to stop sending.
2. **DaemonSet update strategy set by the operator:** `maxSurge: 1, maxUnavailable: 0`, so the replacement engine is ready before the old one stops and total capacity never drops.
3. **`terminationGracePeriodSeconds: 40`** (max delay 15 s + drain 20 s + margin).
4. Streams cut by `Stop()` after the drain timeout are requests that already passed their request-phase decision; what Envoy does with them (fail-closed reset or completion) is measured in the test below, not assumed.

**Tests** (kind e2e): constant traffic through the gateway while `kubectl rollout restart ds/hyper-engine` → **zero** non-2xx; plus one long-lived request (slow streaming response) across the restart, recording what happens to it.

### `PreferSameNode`: when it applies, and how to make it apply on Envoy Gateway

Verified against the Kubernetes docs source, KEP-3015 and the Envoy Gateway source (2026-10-09):

- **How Kubernetes implements it** ([Virtual IPs and Service proxies, "Traffic distribution"](https://kubernetes.io/docs/reference/networking/virtual-ips/#traffic-distribution)): the EndpointSlice controller writes `hints` (`forNodes`) into the EndpointSlices, and **the service proxy** (kube-proxy, or a CNI that replaces it) reads them when it load-balances a connection to the Service's **ClusterIP**. If the client's node has no ready endpoint it falls back to the same zone, then cluster-wide. It is a preference, applied by whoever does the load balancing.
- **Version** ([KEP-3015](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/3015-prefer-same-node)): alpha 1.33 (gate `PreferSameTrafficDistribution`), beta and on by default **1.34**, GA 1.35. Our docs say "honoured from Kubernetes 1.31", which is wrong (1.31 had only `PreferClose`).
- **What Envoy Gateway does with the Service we name in `EnvoyExtensionPolicy.extProc.backendRefs`**: `ext_service.go` → `processServiceDestinationSetting` in [`route.go`](https://github.com/envoyproxy/gateway/blob/main/internal/gatewayapi/route.go). By default (**Endpoint routing**) it reads the EndpointSlices itself and programs Envoy with the **pod IPs**; Envoy balances across them and connects to the pod directly, so kube-proxy never sees the connection and the hint is not used. With **`EnvoyProxy.spec.routingType: Service`** it programs the **ClusterIP** instead, so the service proxy on Envoy's node balances the connection and applies `PreferSameNode`.
- `BackendTrafficPolicy.routingType` (Envoy Gateway ≥ 1.8) can override this per Gateway or route **for routes only**: the ext_proc path passes no override (`processServiceDestinationSetting(..., gtwCtx.envoyProxy, nil)`), so it always follows the `EnvoyProxy` setting.

**Proposal**

1. Make the engine node-local on Envoy Gateway with the combination:
   - `EnvoyProxy.spec.routingType: Service` → ext_proc calls go to `hyper-engine-svc`'s ClusterIP, and the node's service proxy picks the engine on the same node;
   - `BackendTrafficPolicy` with `routingType: Endpoint` targeting the Gateway → application routes keep Envoy's own per-endpoint load balancing, health checks and draining.
   The operator can generate the `BackendTrafficPolicy` next to the `EnvoyExtensionPolicy`; the `EnvoyProxy` is the platform team's (it is referenced from the GatewayClass or Gateway), so we document it rather than write it.
2. Trade-offs to document: with Service routing Envoy balances **per connection** (HTTP/2 to the engine is long-lived, so each Envoy connection stays on one engine, the local one; that is the goal here); the shutdown design still holds, because the service proxy stops picking a terminating engine endpoint.
3. Requirements: Kubernetes **1.34+** (or 1.33 with the feature gate), a service proxy that implements `PreferSameNode` (kube-proxy does; for Cilium kube-proxy replacement, verify before claiming it).
4. Correct `concepts/architecture.md`, `getting-started/installation.md` (version) and the Cilium gateway diagram.
5. **Test** in the kind e2e (kind node image ≥ 1.34, kube-proxy): with the combination above, the ext_proc calls from each Envoy pod land on the engine of the same node (engine per-node request counters from item 4), and without it they are spread across nodes. The benchmark of item 4 measures the latency difference.

## 2. Memory and context pooling

**Findings**

| Measurement (current code) | Result |
| --- | --- |
| Heap after `Prewarm(5000)` (the default) | **+429 MiB** |
| After the first GC / the second GC | +429 MiB / **0** |
| Size of one context with the 64 KiB body buffer / without it | **87.9 KiB / 23.9 KiB** |
| Allocations per `Acquire`/`Release` once warm | 0 |
| `sync.Pool` Acquire+Release, 8 goroutines in parallel | **10.9 ns** |
| Buffered-channel pool, same benchmark | **310.7 ns** (≈ 30× slower, contention on the channel lock) |

- The engine's default memory limit is **512 MiB**: the pre-warm alone takes 84 % of it before Redis clients, JWKS keys and the identity map. That is the OOMKill risk.
- `sync.Pool` drops its objects across two GC cycles (Go's victim cache), so the 5,000 pre-warmed contexts are gone shortly after start. Under traffic the pool stays populated by `Release`, so the steady state is already allocation-free; the pre-warm only helps the first burst.
- The 64 KiB body buffer saves nothing: gRPC's protobuf codec has already allocated `msg.Body` when the message arrives, and the handler then **copies** it into `RawBodyBuffer`. No filter modifies the body (checked), so using `msg.Body` directly is safe.

**Proposal (differs from the original idea, based on the measurements)**

- **Keep `sync.Pool` as the pool.** Replacing it with a buffered channel costs ~300 ns per request under concurrency and would only keep contexts warm across idle periods, where a 24 KiB allocation per new context is cheap.
- **Remove the per-context body buffer** and use `msg.Body` directly. Context size drops from 87.9 KiB to 23.9 KiB (−73 %).
- **Pre-warm becomes small and optional:** default `pool_prewarm_size` 256 (≈ 6 MiB) instead of 5,000 (429 MiB); `prealloc_body_buffer_bytes` is accepted but ignored, and documented as deprecated.
- **Set the Go memory limit from the container limit.** Go 1.25 made `GOMAXPROCS` container-aware but not the memory limit, and Go 1.26 did not change that ([golang/go#75164](https://github.com/golang/go/issues/75164) is still open), so without help the GC ignores the 512 MiB limit until the kernel OOM-kills. Use [`KimMachineGun/automemlimit`](https://github.com/KimMachineGun/automemlimit) (cgroup v1, v2 and hybrid; also used by GitLab Runner) with a 0.9 ratio, unless `GOMEMLIMIT` is already set. Setting `GOMEMLIMIT` from the Downward API was rejected: it uses the full limit with no headroom.
- If a bounded warm pool is still wanted later: a **sharded** free list (one small channel per P) avoids most of the contention; not proposed now.

**Done.** `sync.Pool` kept; per-context body buffer removed (bodies are referenced, and released contexts drop them); `pool_prewarm_size` default 256 in the engine and the CRD; `prealloc_body_buffer_bytes` / `preallocBodyBufferBytes` accepted but ignored (warning logged); `automemlimit` sets Go's memory limit to 90 % of the cgroup limit at start-up.

| Measured after the change | Before | After |
| --- | --- | --- |
| Memory for the default pre-warm | 429 MiB (5,000 × 88 KiB) | **5.9 MiB** (256 × 24 KiB) |
| `BenchmarkProcess_Headers` (parallel, 8 CPUs) | ≈1.05 µs, 979 B, 13 allocs | ≈1.05 µs, 974 B, 13 allocs |
| `BenchmarkProcess_Body16K` | ≈1.1–1.5 µs, 1225 B, 16 allocs | ≈1.0–1.6 µs, 1219 B, 16 allocs |

The request path is not measurably faster (the 16 KiB copy was cheap); the gain is memory, and the start-up OOM risk is gone. Existing HyperConfigs keep the value they were created with (5,000 → about 120 MiB now); lower `poolPrewarmSize` on them.

## 4. Prometheus metrics

**Findings**

- The engine exposes no metrics today (only `/healthz`, `/readyz`, `/debug/identity`).
- `client_golang` is already a dependency (the operator uses it).
- `CounterVec.WithLabelValues` costs a hash and a map lookup per call; resolving the label children once per policy snapshot leaves an atomic add (~10 ns) per counter and ~25 ns per histogram observation in the hot path.

**Proposal**

- Serve `/metrics` on the existing health port (9003). The operator adds `prometheus.io/scrape|port|path` annotations to engine pods, and the chart gains an optional `PodMonitor` (Prometheus Operator).
- Metrics (labels limited to names that come from the configuration, never paths, IPs or users):

| Metric | Type | Labels |
| --- | --- | --- |
| `hypergate_requests_total` | counter | `route`, `chain`, `outcome` (`allowed`, `denied`, `error`) |
| `hypergate_denies_total` | counter | `chain`, `filter`, `status` |
| `hypergate_phase_duration_seconds` | histogram (25 µs … 2.5 s) | `phase`, `chain` |
| `hypergate_streams_active` | gauge | — |
| `hypergate_unknown_source_total` | counter | — |
| `hypergate_policy_reloads_total` | counter | `result` |
| `hypergate_policy_last_reload_timestamp_seconds` | gauge | — |
| `hypergate_identity_connected` | gauge | — |
| `hypergate_identity_last_update_timestamp_seconds` | gauge | — |
| `hypergate_identity_entries` | gauge | `kind` (`workload`, `service`) |
| `hypergate_identity_reconnects_total` | counter | — |

- To name filters in `filter`, the engine `FilterConfig` gains an optional `name`; the operator fills it with the CRD name (`JwtAuthFilter/users`). Without a name the label is `<position>:<type>` (`2:jwt_auth`).

**Test:** unit tests for each counter on the ext_proc test harness; the kind e2e scrapes `/metrics` and checks the deny counter of the denied caller.

**Done.** Metrics as listed (the identity entries are exposed as `hypergate_identity_workloads` and `hypergate_identity_services`, plus `hypergate_identity_synced`; durations as `hypergate_message_duration_seconds`; reloads timestamp as `hypergate_policy_last_reload_success_timestamp_seconds`). Measured cost: `BenchmarkProcess_Headers` ≈1.05–1.18 µs without metrics, ≈1.25–1.32 µs with metrics (8 parallel goroutines, no extra allocations); recording alone ≈128 ns. Filter labels come from the optional engine `FilterConfig.name` (the operator sets `Kind/name`), else `<position>:<type>`. Engine pods are annotated for scraping and the chart has an optional `PodMonitor`.

## 5. Audit (dry-run) mode and chain limits

### Audit mode

**Prior art:** Envoy RBAC `shadow_rules` (evaluate and emit stats, never enforce), Istio `AUDIT` action and `istio.io/dry-run`, ModSecurity/Coraza `DetectionOnly`, Gatekeeper `enforcementAction: dryrun`, Kyverno `Audit`.

**Proposal**

- Per filter: `audit: true` in the engine `FilterConfig`. In the CRDs, `HyperChain.spec.mode: Enforce | Audit` (all filters) with a per-reference override, so one new filter can be audited inside an existing chain.
- When an audited filter would block or fails internally: count it (`hypergate_audit_denies_total{chain,filter,status}`), log it (rate-limited), clear the block and **continue the chain**. Header changes the filter makes still apply, because later filters may depend on them.
- **Answers are not denials:** a CORS preflight answered with `204` is still sent in audit mode; only denials are suppressed. This needs a small API split in `RequestContext`: `Block()` (a denial) versus a new `Answer()` (an immediate response that is not a denial), used by the CORS preflight path.
- Side effects stay: an audited rate limiter still counts requests (that is what shows what would be limited), and an audited external auth still calls its sidecar. Documented.
- Not in this step: a "shadow chain" evaluated alongside the enforced chain. Stateful filters shared between the two (rate limiter counters) would be double-counted; it needs read-only evaluation first.

### Chain limits (noisy neighbours on a shared per-node engine)

**Proposal**

- `timeout` per chain: a deadline on the engine's work for each ext_proc message. Implemented by giving filters a context with that deadline; every filter that calls Redis or a sidecar already derives its own timeout from that context (checked), so the bound reaches them without changes. On expiry: `503` (or continue, with `on_timeout: allow`). Must stay below Envoy's `message_timeout` (2.5 s by default), otherwise Envoy fails the request first; validated with a warning.
- `max_concurrency` per chain: an atomic counter per chain and engine, taken only **while the chain's filters run** (not while the upstream answers), so it limits work, not open requests. On overload: `503` with `Retry-After` (or continue, with `on_overload: allow`). Counted in `hypergate_chain_rejections_total{chain,reason}`.
- Sidecar CPU and memory stay bounded by the sidecar container's own resources, which the CRDs already expose.

**Test:** a slow test filter proves the deadline and the overload path; the kind e2e runs a slow chain next to a fast one and checks the fast chain's latency stays flat.

**Done.** Audit: engine `FilterConfig.audit`; the executor saves the header and path state before an audited filter and restores it when the filter denies or fails, records an `AuditHit`, and continues; `RequestContext.Answer()` marks non-denial immediate responses (the CORS preflight uses it) and is never suppressed. Operator: `HyperChain.spec.mode` and `filters[].audit`. Metric `hypergate_audit_denies_total`. Limits: engine `chain_settings` (`timeout`, `max_concurrency`, `on_timeout`, `on_overload`), enforced per ext_proc message in the server; operator `HyperChain.spec.timeout|maxConcurrency|onTimeout|onOverload`, with an invalid timeout degrading the chain instead of reaching the engine. Metric `hypergate_chain_rejections_total`. Tests: executor audit semantics (denial, failure, answer, shared instance, phases), timeout and overload through the ext_proc server with both actions, config validation, compiler output. The kind e2e slow-chain-next-to-fast-chain test is not added yet.

## Decisions needed

1. **Item 2:** keep `sync.Pool` and drop the body buffer (measured: channel pool ≈ 30× slower) instead of a buffered-channel pool. Agree?
2. **Item 1:** may I enable branch protection on `main` with the `kind-e2e-result` check?
3. **Item 5:** audit granularity per filter with a chain-level default, as above?
