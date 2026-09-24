---
sidebar_position: 4
title: Hot reload
description: How a new configuration is detected, compiled, published atomically and how old resources are retired.
---

# Hot reload

The engine applies configuration changes without restarting and without interrupting requests. A reload is transactional: either the whole new policy is published, or nothing changes.

![Hot reload: parse and validate, compile everything, publish atomically; streams drain on the old snapshot before its resources are closed](/img/diagrams/hot-reload.svg)

## Where changes come from

The provider is chosen with `CONFIG_PROVIDER` (default `FILE`).

| Provider | Initial load | Change detection |
| --- | --- | --- |
| `FILE` | The `-config` / `-c` flag, else `CONFIG_FILE_PATH`, else `/etc/hyper-engine/config.yaml`. | `fsnotify` on the file's directory. A write, create, rename or remove of the config file (or of `..data`, which is how Kubernetes swaps mounted ConfigMap volumes) triggers a reload after a 100 ms pause. |
| `K8S` | ConfigMap `CONFIG_K8S_NAME` (default `hyper-engine-config`) in `CONFIG_K8S_NAMESPACE` (default `hyper-system`), key `config.yaml`. | A watch on that ConfigMap. The engine re-reads the ConfigMap whenever the watch reconnects, so updates made while it was disconnected are not lost. Each `resourceVersion` is applied at most once; a rejected version is not retried until the ConfigMap changes again. |
| `URL` | HTTP `GET` of `CONFIG_URL` (5 s timeout, must return `200`). | No polling. A `POST /v1/reload` to the reload endpoint fetches `CONFIG_URL` again (body limit 16 MiB) and applies it. |

The operator uses the `K8S` provider.

### Reload endpoint (URL provider)

| Setting | Behaviour |
| --- | --- |
| Neither `CONFIG_RELOAD_TOKEN` nor `CONFIG_RELOAD_ADDRESS` set | Listens on `127.0.0.1:9002`, reachable only from inside the pod or host. No authentication. |
| `CONFIG_RELOAD_TOKEN` set | Listens on `:9002` and requires `Authorization: Bearer <token>` (constant-time comparison). |
| `CONFIG_RELOAD_ADDRESS` set | Listens on that address instead, with or without a token as above. |

| Response | Meaning |
| --- | --- |
| `200 Config successfully reloaded` | The new policy is live. |
| `401 Unauthorized` | Missing or wrong bearer token. |
| `405 Method not allowed` | Anything other than `POST`. |
| `422` | The configuration was fetched but rejected; the message says why. The previous policy is still active. |
| `502` | Fetching `CONFIG_URL` failed or returned a non-`200` status. |

```bash
curl -X POST -H "Authorization: Bearer $CONFIG_RELOAD_TOKEN" http://engine-host:9002/v1/reload
```

## What happens on a reload

1. **Parse and validate.** The YAML is decoded and checked: `version: v1`, `server.client_ip.trusted_proxy_hops >= 0`, Redis settings (topology, socket type, durations), every regex, and every route and default chain reference. Any error rejects the reload.
2. **Compile everything.** The engine builds the complete new policy before touching the running one:
   - Redis clients: a service whose settings are unchanged keeps its existing client; new or changed services are dialled and must answer `PING`.
   - Filters: every filter of every chain is built. A filter whose type, options and Redis client are identical to one in the running policy is reused, so its in-process caches (rate-limit L1 cache, API key cache, enricher cache, JWKS key set, sidecar connections) stay warm. Identical filter definitions in different chains share one instance.

   If any step fails, everything created during this attempt is closed, the error is logged as `Config reload rejected, keeping previous policy`, and the running policy is untouched.
3. **Publish atomically.** Routes and chains are swapped together as one snapshot. A request can therefore never be routed by the new routes to a chain that only exists in the old config, or the other way round.
4. **Retire the old snapshot.** Every ext_proc stream holds the snapshot it started with until it ends. Streams that were already running finish on the old routes and chains; new streams use the new snapshot. When the last stream holding the old snapshot ends, the filters and Redis clients that the new policy no longer uses are closed. Redis health checks follow the client lifetimes.

Readiness is unaffected by reloads: `/readyz` reports ready after the first successful load and stays ready when a later reload is rejected.

## What a reload changes

Everything under `chains`, `router` and `redis` takes effect on reload, as does `server.client_ip.trusted_proxy_hops`, which is read from the snapshot for each new stream.

The following are read once at start-up and need a restart to change: `server.address`, `server.max_concurrent_streams`, `server.tls`, `server.health_address`, `server.pprof_address`, `server.pool_prewarm_size`, `server.initial_header_capacity`, `server.prealloc_body_buffer_bytes` and `telemetry.logging`.

## Things that can make a reload slow or fail

- **New or changed Redis services.** The engine dials them during step 2 and retries with the `startup_initial_interval` / `startup_max_interval` back-off until `PING` succeeds or `startup_max_elapsed_time` has passed. The default `startup_max_elapsed_time` is `0s`. At start-up that means "retry until Redis answers". On a reload, the wait is capped at 30 seconds, after which the reload is rejected and the previous policy keeps serving, so one unreachable service cannot block later reloads. Set `startup_max_elapsed_time` explicitly to choose a different limit.
- **JWKS endpoints.** A `jwt_auth` filter with a new `jwks_endpoint` fetches the key set while it is compiled. If the fetch fails, the reload is rejected.
- **Secret files.** `jwt_auth` reads `local_secret_file` and `introspection_auth_header_file` while it is compiled; a missing file rejects the reload.
- **Sidecar sockets.** `external_auth` and `firewall` filters create their Unix socket clients lazily, so a sidecar that is not listening yet does not fail the reload; requests fail with `500` until it is.

## Failure at start-up

The first load is not a reload: if the initial configuration cannot be read, parsed or compiled, the engine exits with an error. In Kubernetes the pod restarts until the configuration is fixed. With the operator, a HyperChain that cannot be resolved does not cause this, because the operator compiles it into a `503` chain instead of producing invalid configuration.
