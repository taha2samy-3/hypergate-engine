---
sidebar_position: 1
title: Architecture
description: The engine, the operator, Envoy, Redis and sidecars, and how they connect.
---

# Architecture

![Hypergate architecture: client, Envoy, engine pod with sidecars, Redis, and the operator control plane](/img/diagrams/architecture.svg)

Hypergate splits into a data plane that sits on the request path and a control plane that produces the policy.

## Data plane

### Envoy

Envoy is the proxy clients connect to. Its `envoy.filters.http.ext_proc` HTTP filter opens one bidirectional gRPC stream to the engine for each HTTP request and exchanges one message per processing phase: request headers, optionally the request body and trailers, response headers, and optionally the response body and trailers. What Envoy sends is controlled by the filter's `processing_mode`; see [Envoy configuration](../reference/envoy-configuration.md).

Envoy keeps doing everything a proxy does: listener and TLS handling, route and cluster selection, retries and load balancing. The engine never proxies traffic itself.

### Engine

The engine is a single Go binary (`cmd/engine`) serving the `envoy.service.ext_proc.v3.ExternalProcessor` API on `server.address` (default `:9001`). For each stream it:

1. takes a reference to the current **policy snapshot** (the compiled routes and chains),
2. resolves the client IP from Envoy's peer address and trusted proxy hops ([Client IP](./client-ip.md)),
3. matches the request against the router to choose one **chain** ([Routing](./routing.md)),
4. runs the chain's filters in order in every phase they declare ([Request lifecycle](./request-lifecycle.md)),
5. answers Envoy with header or body mutations, or with an `ImmediateResponse` that ends the request.

Per-request state lives in pooled request contexts (`server.pool_prewarm_size`, `server.initial_header_capacity`, `server.prealloc_body_buffer_bytes`) so steady-state traffic does not allocate a new context per request. Filters that talk to Redis keep small in-process caches: the rate limiter remembers keys that are over their limit until the window resets, the API key filter caches lookups for 30 seconds (10 seconds for rejections), and the metadata enricher caches Redis values for `cache_timeout`. These caches are per engine process.

The engine also serves `/healthz` and `/readyz` on `server.health_address` (default `:9003`) and, only when `server.pprof_address` is set, the Go `pprof` handlers. See [Operations](../reference/operations.md).

### Redis

Rate limiting, API keys and the metadata enricher read from or write to Redis. Each Redis deployment is declared once under `redis:` (or as a `HyperRedis`) and referenced by name from filters. `SINGLE`, `CLUSTER` and `SENTINEL` topologies, TCP or Unix sockets, authentication and TLS are supported; see [Engine configuration](../reference/engine-configuration.md#redis).

### Sidecars

`external_auth` and `firewall` filters delegate the decision to a separate process: an authorization service or a web application firewall. With the operator, each `ExternalAuthFilter` and `FirewallFilter` becomes a sidecar container in the engine pod, and the engine reaches it through a Unix domain socket on a shared `emptyDir` volume.

![Engine pod with the engine container and sidecars sharing /var/run/hypergate](/img/diagrams/sidecar-uds.svg)

The operator derives socket paths from the resource name:

| Resource | Sidecar container | Socket |
| --- | --- | --- |
| `ExternalAuthFilter` named `oidc` | `ext-auth-oidc` | `/var/run/hypergate/ext-auth-oidc.sock` |
| `FirewallFilter` named `waf` | `fw-waf` | `/var/run/hypergate/fw-waf.sock` |

The sidecar learns its socket path from an environment variable (`socketEnvKey`; for firewalls the default is `FIREWALL_SOCKET_PATH`; for images containing `oauth2-proxy` the operator sets `OAUTH2_PROXY_HTTP_ADDRESS`) or from `{socket_path}` placeholders in its `args`, which the operator replaces with the socket path. Environment variables carry the path with a `unix://` prefix; `{socket_path}` is replaced with the bare path.

Compared with running the authorization service or WAF behind its own Kubernetes Service, the socket removes a network hop, a DNS lookup and a TLS or Service-mesh leg from every checked request, and it cannot be reached from outside the pod. The trade-off is that each engine pod carries its own copy of every sidecar, and every `ExternalAuthFilter` and `FirewallFilter` in the cluster is injected whether or not a chain uses it.

## Control plane

The operator watches the cluster-scoped `hyper.io/v1alpha1` resources and turns them into engine configuration and workloads.

![Operator flow: CRDs are compiled into one ConfigMap per target namespace; the HyperConfig controller manages the engine DaemonSet](/img/diagrams/operator-flow.svg)

### Master compiler

Any change to a HyperConfig, HyperRedis, HyperChain, HyperRoute or filter resource triggers a full compilation:

- every `HyperRedis` becomes a `redis:` entry keyed by its name,
- every `HyperChain` becomes a chain; each referenced filter's `spec` is translated into the engine's snake_case options,
- every `HyperRoute` becomes a route; routes are ordered by `spec.priority`, highest first,
- one ConfigMap `hyper-engine-config` (key `config.yaml`) is written per active HyperConfig, in its `targetNamespace`.

Chains, routes and Redis services are shared by all HyperConfigs; each target namespace receives the same policy with its own `server` block, log level and default chain. The ConfigMap is only updated when the compiled YAML changes, so unrelated reconciles do not cause reloads.

The compiler fails closed. A HyperChain whose filters cannot be resolved is marked `Degraded` and compiled into a chain that returns `503`; a HyperRoute or `defaultChain` that names a missing chain gets the same `503` chain. HyperRoutes with an empty `targetPolicy` or an invalid `pathRegexPattern` are skipped and logged, so one broken route cannot block every later update. See [Failure modes](./failure-modes.md).

### HyperConfig controller

For each HyperConfig the controller manages, in `spec.targetNamespace` (default `hyper-system`, created if missing):

| Object | Name | Notes |
| --- | --- | --- |
| ServiceAccount | `hyper-engine-sa` | Used by the engine pods. |
| Role and RoleBinding | `hyper-engine-config-reader`, `hyper-engine-config-reader-binding` | `get` and `watch` on the `hyper-engine-config` ConfigMap only. |
| DaemonSet | `hyper-engine` | One engine pod per node, plus the injected sidecars. |
| Service | `hyper-engine-svc` | Port `9001` named `grpc`, `appProtocol: kubernetes.io/h2c`, `trafficDistribution: PreferSameNode`. |

Only one HyperConfig can manage a namespace. The oldest one (by creation time, then name) wins; others get `status.state: Conflict` and are not reconciled.

The engine container runs with `CONFIG_PROVIDER=K8S`, readiness (`/readyz`) and liveness (`/healthz`) probes on port 9003, resource requests of `100m` CPU and `128Mi` memory and a `512Mi` memory limit (override with `spec.engineResources`), as UID/GID 10001 with a read-only root filesystem, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile. Secrets referenced by `JwtAuthFilter` resources are mounted read-only into the engine container; the compiled ConfigMap only contains their file paths.

## Deployment topology

The engine runs as a DaemonSet so that every node that hosts Envoy has an engine instance. The Service's `PreferSameNode` traffic distribution makes Envoy prefer the engine on its own node, which keeps the ext_proc round trip node-local when possible and falls back to other nodes otherwise.

Every engine instance is independent: it loads the full policy and keeps its own caches. Shared state (rate-limit counters, API key records, enrichment data) lives in Redis, so limits are enforced across instances.
