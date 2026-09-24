---
sidebar_position: 6
title: Failure modes
description: What happens when the engine, Redis, a sidecar, an identity provider or the configuration fails, and which failures can be configured to fail open.
---

# Failure modes

Hypergate fails closed by default: when it cannot make a decision, the request is rejected. A few behaviours are fail-open by design or by option; they are marked below.

## By situation

| Situation | Result | Configurable |
| --- | --- | --- |
| Envoy cannot reach the engine, the stream errors, or a message times out | Decided by Envoy. With `failure_mode_allow: false` (recommended) Envoy fails the request with a 5xx; with `true` the request continues **without any policy**. | Envoy `failure_mode_allow`, `message_timeout` |
| No policy has been loaded | `503 Service Unavailable` | No |
| A route or the default chain names a chain that is not loaded | `503 Service Unavailable` | No |
| No route matches and no default chain is set | Request passes **without policy** | Set `default_chain` |
| A filter returns an internal error | `500 Internal Server Error`; the chain stops | Per filter, see below |
| A filter needed the request body but Envoy never sent it | Response replaced with `500 Internal Server Error` (the upstream has already been called) | Fix Envoy: `allow_mode_override: true` or `request_body_mode: BUFFERED` |
| Invalid configuration at start-up | Engine exits; the pod restarts | No |
| Invalid configuration on reload | Rejected and logged; the previous policy keeps serving | No |
| Reload adds a Redis service that is unreachable | Reload waits for it (forever with the default `startup_max_elapsed_time: 0s`), then is rejected if the deadline passes; the previous policy keeps serving | `startup_max_elapsed_time` |

### Operator

| Situation | Result |
| --- | --- |
| HyperChain references a missing filter or cannot be translated | Chain compiled as a `deny` returning `503`; HyperChain `status.state: Degraded` |
| HyperRoute or `defaultChain` names a missing HyperChain | A `503` deny chain is compiled under that name |
| HyperRoute with an empty `targetPolicy` or invalid `pathRegexPattern` | Route skipped and logged. **Its traffic falls through** to later routes or the default chain |
| Two HyperConfigs with the same `targetNamespace` | The oldest owns the namespace; the other gets `status.state: Conflict` and is ignored |
| Operator not running | The engine keeps serving the last ConfigMap. With webhooks enabled (`failurePolicy: Fail`), creating or updating HyperChains and deleting filters or HyperChains is rejected until it is back |

## By filter

| Filter | Dependency | Dependency failure | Invalid or missing credentials | Fail-open option |
| --- | --- | --- | --- | --- |
| `embedded_rate_limiter` | Redis | `500` | Not applicable. Over limit: `429 Too Many Requests` | `fail_open: true` on a descriptor skips that descriptor when Redis fails. `shadow_mode: true` never blocks for being over the limit (a Redis error still returns `500` unless `fail_open` is set). |
| `api_key` | Redis | `500` | Missing or unknown key: `401`; status check mismatch: `403` | None |
| `jwt_auth` | JWKS endpoint, introspection endpoint | JWKS unavailable when the filter is compiled: configuration rejected. Background refresh failure: the last good key set is kept. Introspection failure: `401` | Missing token: `401`. Invalid token: `401` | `fail_open: true` lets the request through whenever validation fails, including invalid and expired tokens. A missing token is still rejected. |
| `external_auth` | Sidecar over UDS | `500` | The sidecar's status (gRPC deny without a status: `403`) | None |
| `firewall` | Sidecar over UDS | `500` | The sidecar's status, or `403` for a gRPC immediate response without a status | None |
| `redis_metadata_enricher` | Redis | **Fails open**: the request continues without the enrichment headers | Key not found: no headers are added | Always fail-open |
| `deny` | None | Not applicable | Configured status, default `403` | Not applicable |
| `header_modifier` | None | Not applicable | Not applicable | Not applicable |
| `correlation_id` | None | Not applicable | Invalid incoming ID (with `validation_regex`): replaced by a new one | Not applicable |

Notes:

- The rate limiter and API key filter keep per-instance caches. A key that was just revoked in Redis can still be accepted for up to 30 seconds by an engine instance that cached it; a key that is over its rate limit is rejected locally until its window resets, without asking Redis.
- `jwt_auth` with `fail_open: true` removes authentication for malformed or forged tokens as well. Use it only where the upstream enforces authentication itself.
- The enricher being fail-open matters when a later filter depends on its headers. For example a rate limiter keyed on an enriched tier header sees the header missing and uses the value `default` (or the descriptor's fallback entry) during a Redis outage.

## Choosing Envoy's failure mode

`failure_mode_allow` applies when the engine itself is unavailable or too slow, not to decisions the engine makes. With `false`, an engine outage becomes an outage of the routes it protects; with `true`, it silently removes rate limits and authentication. Run the engine as a DaemonSet with readiness probes (the operator does this), keep `failure_mode_allow: false`, and set `message_timeout` above the slowest filter's worst case (sidecar timeouts default to `2s`). See [Envoy configuration](../reference/envoy-configuration.md).
