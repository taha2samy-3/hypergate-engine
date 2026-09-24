---
sidebar_position: 4
title: Operations
description: Health endpoints, profiling, logging, securing the reload endpoint, upgrades and troubleshooting.
---

# Operations

## Health endpoints

The engine serves two HTTP endpoints on `server.health_address` (default `:9003`):

| Path | `200` when | Otherwise |
| --- | --- | --- |
| `/healthz` | The process is running. | |
| `/readyz` | A policy has been loaded successfully **and** the gRPC listener is serving. | `503 not ready` |

`/readyz` stays `200` when a later reload is rejected, because the previous policy is still serving. On `SIGTERM` or `SIGINT` the engine marks itself not ready, stops accepting new streams, waits for in-flight streams to finish (`GracefulStop`), closes filters and Redis clients, and exits.

The operator configures the engine container with:

| Probe | Path | Timing |
| --- | --- | --- |
| Readiness | `/readyz` on port 9003 | every 5 s, 3 failures |
| Liveness | `/healthz` on port 9003 | after 10 s, every 10 s, 3 failures |

Redis health checks (`active_conn_health_check`) only log state changes; they do not affect `/readyz`. At start-up, however, the engine does not become ready until every configured Redis service has answered `PING` (see `startup_max_elapsed_time`).

## Profiling

`pprof` is disabled unless `server.pprof_address` is set. When enabled, the engine serves `/debug/pprof/`, `/debug/pprof/cmdline`, `/debug/pprof/profile`, `/debug/pprof/symbol` and `/debug/pprof/trace` on that address. Bind it to loopback and use port forwarding:

```yaml
server:
  pprof_address: "127.0.0.1:6060"
```

```bash
kubectl -n hyper-system port-forward pod/<engine-pod> 6060:6060
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

`HyperConfig` does not expose `pprof_address`. It is read at start-up only.

## Logging

Logs go to `telemetry.logging.output_path` (default `stdout`) in `console` or `json` format; see [Engine configuration](./engine-configuration.md#telemetrylogging). Use `json` in production so that fields such as `chain`, `path`, `status_code` and `phase` are searchable. Useful messages:

| Message | Level | Meaning |
| --- | --- | --- |
| `Compiled filter chain` | INFO | A chain was built (at start-up and on every reload). |
| `Policy reloaded successfully` | INFO | A reload was published. |
| `Config reload rejected, keeping previous policy` | ERROR | A reload failed; the error field says why. |
| `Request blocked by filter chain, sending ImmediateResponse` | INFO | A filter ended a request; includes `status_code` and `phase`. |
| `Filter execution failed with internal error` | ERROR | A filter errored; the request received `500`. |
| `Route targets a chain that is not loaded, rejecting request` | ERROR | Fail-closed `503`. |
| `Request body inspection was required but Envoy never sent the body` | ERROR | Envoy is missing `allow_mode_override: true`; the response was replaced with `500`. |
| `Redis connection attempt failed; will retry` | WARN | A Redis service is unreachable while connecting. |
| `Redis health check: service marked UNHEALTHY` | ERROR | An active health check failed. |
| `jwt_auth: background JWKS refresh failed` | ERROR | The last good key set stays in use. |

`DEBUG` logs every phase and routing decision and is too verbose for sustained production traffic.

The engine does not expose Prometheus metrics. For request-level numbers, use Envoy's ext_proc filter statistics and access logs, which also show the status codes produced by the engine.

## Securing the reload endpoint

The `POST /v1/reload` endpoint exists only with `CONFIG_PROVIDER=URL`.

- By default it binds to `127.0.0.1:9002`, so only processes in the same network namespace (the pod) can trigger a reload.
- To call it from elsewhere, set `CONFIG_RELOAD_TOKEN`. The endpoint then binds to `:9002` and every call must send `Authorization: Bearer <token>`. Store the token in a Secret and inject it with `extraEnv` (`hypergate-engine` chart) or your own manifest.
- `CONFIG_RELOAD_ADDRESS` overrides the address in either case. Setting it to a non-loopback address without a token exposes an unauthenticated endpoint; avoid that.
- The endpoint only triggers a fetch of `CONFIG_URL`; it never accepts configuration in the request body. Protect the configuration source itself as carefully as the engine.

## Upgrading

1. **CRDs.** Helm does not upgrade CRDs. Apply the CRDs of the target version first:

   ```bash
   kubectl apply --server-side -f charts/hyper-operator/crds/
   ```

2. **Operator.**

   ```bash
   helm upgrade hyper-operator oci://ghcr.io/taha2samy-3/charts/hyper-operator \
     --version <version> --namespace hyper-operator-system
   ```

3. **Engine.** HyperConfigs without `spec.engineImage` follow the operator's `ENGINE_IMAGE`, which the chart sets to the matching engine release, so the operator rolls the DaemonSet after its own upgrade. HyperConfigs that pin `spec.engineImage` must be updated explicitly.

The DaemonSet uses a rolling update. While the engine pod on a node restarts, its readiness probe removes it from the Service, and Envoy on that node is served by engine pods on other nodes (`PreferSameNode` falls back when there is no ready local endpoint). Keep `failure_mode_allow: false` during upgrades; with enough nodes no request is left without an engine.

Configuration changes do not need a rollout; they are hot-reloaded. Changing `serverAddress`, `logLevel`, `maxConcurrentStreams` or the pool sizes on a HyperConfig updates the ConfigMap, but these settings are only read at start-up; restart the DaemonSet (`kubectl -n hyper-system rollout restart daemonset/hyper-engine`) to apply them.

## Troubleshooting

### HyperConfig shows `Conflict`

```bash
kubectl get hyperconfigs
# NAME        SERVER ADDRESS   LOG LEVEL   REDIS REF      STATE
# edge        0.0.0.0:9001     INFO        shared-redis   Ready
# edge-copy   0.0.0.0:9001     DEBUG       shared-redis   Conflict
```

Two HyperConfigs have the same `targetNamespace`. The oldest one owns the namespace; the other is not reconciled and does not contribute a ConfigMap. `status.message` names the owner. Delete the duplicate or give it a different `targetNamespace`.

### HyperChain shows `Degraded`

```bash
kubectl get hyperchains
# NAME         STATE      MESSAGE
# public-api   Degraded   Filter api-limit of Kind RateLimitFilter not found
```

The chain references a filter that does not exist (or has an unknown kind). The operator compiles it into a chain that answers every request with `503 Service Unavailable`, so its routes fail closed instead of losing their policy. Create the missing filter or fix the reference; the chain becomes `Ready` on the next reconcile.

### A HyperChain is rejected with `Unsupported value`

The HyperChain CRD installed in the cluster is older than the operator and does not list the filter kind in `spec.filters[].kind`. Helm installs CRDs only on first install and never upgrades them. Re-apply them: `kubectl apply -f charts/hyper-operator/crds/`.

### Every request on a route returns 503

- The route's HyperChain is `Degraded`, or the HyperRoute's `targetPolicy` or the HyperConfig's `defaultChain` names a HyperChain that does not exist. Check `kubectl get hyperchains` and the operator logs (`HyperRoute targets a missing HyperChain`, `HyperConfig defaultChain does not exist`).
- Standalone: the engine log shows `Route targets a chain that is not loaded` or `No policy loaded`.

### Requests return 500

Look for `Filter execution failed with internal error` in the engine log. Typical causes are Redis errors for `embedded_rate_limiter` or `api_key` (set `fail_open` on rate-limit descriptors if availability matters more than the limit), an unreachable sidecar for `external_auth` or `firewall`, or `Request body inspection was required but Envoy never sent the body` (set `allow_mode_override: true` on the ext_proc filter).

### A reload did not take effect

- Search the engine log for `Config reload rejected, keeping previous policy`; the error explains which route, chain or filter failed.
- `K8S` provider: make sure the ConfigMap key is `config.yaml` and the engine's ServiceAccount can `get` and `watch` it. A rejected ConfigMap version is not retried until the ConfigMap changes.
- A reload that adds or changes a Redis service waits until that service answers `PING`. With the default `startup_max_elapsed_time: 0s` it can wait indefinitely and hold later reloads behind it.
- Settings under `server` (except `client_ip`) and `telemetry` need a restart.

### Engine pods are never ready

The engine connects to every configured Redis service before it starts serving. Check the log for `Redis connection attempt failed; will retry` and verify the HyperRedis `url`. A `jwt_auth` filter whose JWKS endpoint is unreachable at start-up makes the engine exit instead; the pod then restarts.

### Engine pods stay in `ContainerCreating`

A JwtAuthFilter references a Secret (`localSecretRef`, `introspectionAuthSecretRef`), or a FirewallFilter references a `rulesConfigMap` or `rulesSecretRef`, that does not exist in the target namespace. `kubectl -n hyper-system describe pod <engine-pod>` shows the missing volume source.

### All clients share one rate-limit bucket

The resolved client IP is empty or is the address of your own proxy. Add `request_attributes: ["source.address"]` to the ext_proc filter and set `trusted_proxy_hops` for the proxies in front of Envoy. See [Client IP](../concepts/client-ip.md).

### Response headers from filters are missing

Client-facing headers (rate-limit headers, a downstream correlation ID, `header_modifier` `downstream` rules) are applied when Envoy sends the response headers. Check that `processing_mode.response_header_mode` is `SEND`.

### Webhook errors when applying resources

Errors such as `failed calling webhook "vhyperchain.kb.io"` mean the API server cannot reach the operator's webhook: the operator is not running, or its serving certificate is missing or untrusted. Check the operator pods, the `webhook-server-cert` Secret and, with cert-manager, the `hyper-operator-cert` Certificate in the operator namespace.
