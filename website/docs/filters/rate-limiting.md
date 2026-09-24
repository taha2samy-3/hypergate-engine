---
sidebar_position: 1
title: Rate limiting
description: Redis-backed rate limiting with five algorithms, composite descriptors, dynamic cost and RateLimit response headers.
---

import Icon from '@site/static/img/icons/rate-limit.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Rate limiting

The rate limiter counts requests in Redis and rejects requests over the limit with `429 Too Many Requests`. Counters are shared by every engine replica that uses the same Redis service, so a limit is global across the cluster rather than per node.

| At a glance | |
| --- | --- |
| CRD kind | `RateLimitFilter` |
| Engine filter type | `embedded_rate_limiter` |
| Runs in phase | request headers |
| Blocks with | `429 Too Many Requests` |
| On Redis errors | `500` (fail closed), or skip the descriptor when `failOpen: true` |

## Example

```yaml
apiVersion: hyper.io/v1alpha1
kind: RateLimitFilter
metadata:
  name: per-user-limits
spec:
  domain: public-api            # namespaces the Redis keys
  algorithm: sliding_window_counter
  redisService: main            # name of a HyperRedis
  headerMappings:
    user: x-user-id             # descriptor key "user" is read from header x-user-id
  responseHeaders:
    enabled: true
  descriptors:
    # premium users: 600 requests per minute
    - entries:
        - key: user
        - key: plan
          value: premium
      limit: 600
      unit: minute
    # every other plan: 60 per minute
    - entries:
        - key: user
        - key: plan
      limit: 60
      unit: minute
    # and at most 1000 per minute per client IP, whoever they are
    - entries:
        - key: client_ip
      limit: 1000
      unit: minute
```

The same filter in an engine config file:

```yaml
chains:
  api:
    - type: embedded_rate_limiter
      options:
        domain: public-api
        algorithm: sliding_window_counter
        redis_service: main
        header_mappings:
          user: x-user-id
        response_headers:
          enabled: true
        descriptors:
          - entries: [{key: user}, {key: plan, value: premium}]
            limit: 600
            unit: minute
          - entries: [{key: user}, {key: plan}]
            limit: 60
            unit: minute
          - entries: [{key: client_ip}]
            limit: 1000
            unit: minute
```

## How descriptors are evaluated

A **descriptor** is one limit. Its `entries` are conditions that must all hold (AND). Each entry names a *key* and, optionally, a *value*:

- An entry **with** a `value` matches only requests whose runtime value equals it.
- An entry **without** a `value` is a wildcard: it matches any value, and the runtime value becomes part of the counter key, so every distinct value gets its own counter (one counter per user, per IP, …).

The runtime value of a key is resolved like this, in order:

1. `ip`, `client_ip` and `remote_ip` always resolve to the request's [client IP](../concepts/client-ip.md). The raw `X-Forwarded-For` header is never used, so a client cannot get a fresh counter by rotating that header.
2. If `headerMappings` maps the key to a header, that header's value is used.
3. Otherwise the header with the same name as the key is used.
4. If the value is empty, the literal `default` is used.

Headers are read *after* the effect of earlier filters in the chain. For example, a [Redis metadata enricher](./redis-metadata-enricher.md) can inject `x-user-tier` and a later rate limiter can limit on it.

:::note Case sensitivity
Keys and configured values are lower-cased when the filter loads. Runtime values are compared as they arrive, so a descriptor with `value: premium` does not match a header value `Premium`. Normalise such values upstream (for example with the enricher) or configure them in lower case.
:::

Descriptors that use the **same set of keys** form one *dimension* (above, `user`+`plan` is one dimension and `client_ip` another). Within a dimension, the most specific descriptor that matches wins and the rest are skipped. Specific means more entries first, then more entries with explicit values. Every dimension is evaluated, and the request is rejected if **any** matching descriptor is over its limit. In the example, a premium user is counted against the 600/minute descriptor and the 1000/minute IP descriptor, but not the 60/minute one.

## Algorithms

![The five rate limiting algorithms side by side](/img/diagrams/rate-limit-algorithms.svg)

| `algorithm` | Model | Descriptor fields used | Notes |
| --- | --- | --- | --- |
| `fixed_window` | Counter per calendar window (`INCRBY` + `EXPIRE`) | `limit`, `unit` | Cheapest. Allows bursts of up to 2× the limit around a window boundary. Key TTLs get up to 5 s of random jitter. |
| `sliding_window_counter` | Current window + weighted previous window (Lua) | `limit`, `unit` | Smooths the boundary burst with a close approximation. A good default. |
| `sliding_window_log` | Sorted set of request timestamps (Lua) | `limit`, `unit` | Exact, but uses memory per request. **Does not support `dynamicCost`**: the filter refuses to load if it is enabled. |
| `token_bucket` | Tokens refill continuously (Lua) | `maxTokens`, `fillRate` | `fillRate` is tokens **per second**. `limit` and `unit` are ignored. Allows bursts up to `maxTokens`. |
| `leaky_bucket` | Bucket drains at a constant rate (Lua) | `bucketCapacity`, `leakRate`, `unit` | `leakRate` is requests per `unit`. Enforces a steady output rate. |

`unit` is one of `second`, `minute`, `hour`, `day`, `week`, `month` (30 days) or `year` (365 days). An unknown unit falls back to `minute`.

Lua-based algorithms load their scripts with `EVALSHA` and reload them automatically after a Redis restart or failover (`NOSCRIPT`).

## Dynamic cost

By default each request costs 1 unit. With `dynamicCost.enabled`, the cost is read from a request header, so expensive calls consume more of the quota:

```yaml
dynamicCost:
  enabled: true
  sourceHeader: x-cost
  defaultFallbackCost: 1
  maxAllowedCost: 50
```

| Header value | Cost used |
| --- | --- |
| a positive integer | that integer, capped at `maxAllowedCost` when `maxAllowedCost > 0` |
| missing, not an integer, `0` or negative | `defaultFallbackCost` (at least 1) |

Zero and negative costs are never accepted. Otherwise a caller could consume nothing, or even hand quota back.

:::warning Who sets the cost header
If clients can send the cost header themselves, they choose their own cost within `[1, maxAllowedCost]`. Normally a filter earlier in the chain sets it, for example a [header modifier](./header-modifier.md) with `upstream.override`, which replaces any client value.
:::

## Response headers

With `responseHeaders.enabled`, three headers are added to the response. Hypergate adds them to allowed responses and to the `429`:

| Header (default name) | Value |
| --- | --- |
| `ratelimit-limit` | the limit of the most restrictive matching descriptor |
| `ratelimit-remaining` | the remaining quota of that descriptor |
| `ratelimit-reset` | seconds until that descriptor's window resets or its bucket refills |

Rename them with `limitHeader`, `remainingHeader` and `resetHeader`.

## Shadow mode and failure handling

- `shadowMode: true` counts and reports but never blocks. Use it to roll out a new limit: watch `ratelimit-remaining` and the logs, then switch it off.
- `failOpen: true` on a descriptor skips that descriptor when Redis is unreachable. Without it (the default) a Redis error fails the filter and the request gets `500`.
- Once a counter is over its limit, the engine caches the "blocked" state locally until the window resets, so a client being throttled does not keep hitting Redis.

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `domain` | `domain` | string | required | Prefix of every Redis key; keeps unrelated limiters apart. |
| `algorithm` | `algorithm` | enum | required | `fixed_window`, `sliding_window_counter`, `sliding_window_log`, `token_bucket`, `leaky_bucket`. |
| `redisService` | `redis_service` | string | required | Name of the `HyperRedis` / `redis:` entry to use. |
| `headerMappings` | `header_mappings` | map | — | Descriptor key → request header name. |
| `descriptors[]` | `descriptors[]` | list | — | The limits; see below. |
| `responseHeaders.enabled` | `response_headers.enabled` | bool | `false` | Add RateLimit headers to responses. |
| `responseHeaders.limitHeader` | `response_headers.limit_header` | string | `RateLimit-Limit` | |
| `responseHeaders.remainingHeader` | `response_headers.remaining_header` | string | `RateLimit-Remaining` | |
| `responseHeaders.resetHeader` | `response_headers.reset_header` | string | `RateLimit-Reset` | |
| `dynamicCost.enabled` | `dynamic_cost.enabled` | bool | `false` | Read the cost from a header. |
| `dynamicCost.sourceHeader` | `dynamic_cost.source_header` | string | — | Header holding the cost. |
| `dynamicCost.defaultFallbackCost` | `dynamic_cost.default_fallback_cost` | int | `1` | Cost when the header is missing or invalid (values below 1 become 1). |
| `dynamicCost.maxAllowedCost` | `dynamic_cost.max_allowed_cost` | int | no cap | Upper bound for the cost; `0` means no cap. |

Descriptor fields:

| CRD field | Engine option | Type | Used by | Description |
| --- | --- | --- | --- | --- |
| `entries[].key` | `entries[].key` | string | all | Descriptor key (see resolution order above). |
| `entries[].value` | `entries[].value` | string | all | Required value; empty means "any value, counted separately". |
| `limit` | `limit` | int | window algorithms | Requests (cost units) allowed per `unit`. |
| `unit` | `unit` | string | window algorithms, `leaky_bucket` | Window length / leak-rate unit. |
| `maxTokens` | `max_tokens` | number | `token_bucket` | Bucket size (maximum burst). |
| `fillRate` | `fill_rate` | number | `token_bucket` | Tokens added per second. |
| `bucketCapacity` | `bucket_capacity` | int | `leaky_bucket` | Maximum queued requests. |
| `leakRate` | `leak_rate` | number | `leaky_bucket` | Requests drained per `unit`. |
| `shadowMode` | `shadow_mode` | bool | all | Count and report, never block. |
| `failOpen` | `fail_open` | bool | all | Skip this descriptor on Redis errors instead of failing the request. |
