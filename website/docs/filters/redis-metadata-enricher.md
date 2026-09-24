---
sidebar_position: 9
title: Redis metadata enricher
description: Look up JSON metadata in Redis using values from the request and inject selected fields as upstream headers.
---

import Icon from '@site/static/img/icons/enricher.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Redis metadata enricher

The enricher builds a Redis key from parts of the request (a header, the path, the client IP), reads a JSON document stored under that key, and copies fields from it into upstream headers. Services receive the user's tier, tenant or feature flags without looking them up. Later filters can use them too, for example to [rate limit](./rate-limiting.md) by tier.

| At a glance | |
| --- | --- |
| CRD kind | `RedisMetadataEnricherFilter` |
| Engine filter type | `redis_metadata_enricher` |
| Runs in phase | request headers |
| Blocks | never |
| On Redis errors | continues without enrichment (fail open, logged) |

## Example

```bash
redis-cli SET user_meta:alice '{"profile":{"tier":"gold","tenant":"acme"}}'
```

```yaml
apiVersion: hyper.io/v1alpha1
kind: RedisMetadataEnricherFilter
metadata:
  name: user-metadata
spec:
  redisService: main
  keyPattern: "user_meta:{user_id}"
  variables:
    user_id:
      source: "{header:X-User-ID}"
      default: anonymous
  outputMappings:
    - jsonPath: profile.tier
      targetHeader: x-user-tier
    - jsonPath: profile.tenant
      targetHeader: x-tenant
  cacheSizeMB: 32
  cacheTimeout: 30s
```

Engine configuration:

```yaml
chains:
  api:
    - type: redis_metadata_enricher
      options:
        redis_service: main
        key_pattern: "user_meta:{user_id}"
        variables:
          user_id:
            source: "{header:X-User-ID}"
            default: anonymous
        output_mappings:
          - {json_path: profile.tier, target_header: x-user-tier}
          - {json_path: profile.tenant, target_header: x-tenant}
        cache_size_mb: 32
        cache_timeout: 30s
```

Combined with a rate limiter that reads the injected header:

```yaml
    - type: embedded_rate_limiter
      options:
        domain: tiers
        algorithm: fixed_window
        redis_service: main
        header_mappings: {tier: x-user-tier}
        descriptors:
          - entries: [{key: tier, value: gold}]
            limit: 1000
            unit: minute
          - entries: [{key: tier}]
            limit: 100
            unit: minute
```

## How a request is enriched

1. **Resolve variables.** For each entry in `variables`:
   1. read `source`:

      | `source` | Value |
      | --- | --- |
      | `{header:Name}` | request header `Name` (as changed by earlier filters) |
      | `{path}` | path including query |
      | `{method}` | request method |
      | `{client_ip}` | the resolved [client IP](../concepts/client-ip.md) |

   2. if `jsonPath` is set, treat the value as JSON and extract that [gjson path](https://github.com/tidwall/gjson/blob/master/SYNTAX.md);
   3. if `regexPattern` is set, keep the **first capture group** (no match → empty);
   4. if the result is empty, use `default`;
   5. lower-case the result.
2. **Build the key.** Each `{name}` in `keyPattern` is replaced by the variable's value. Unknown placeholders are left as written.
3. **Look it up**, first in the local cache, then in Redis with `GET`. The value must be a JSON document (or a plain string when only the whole value is mapped).
4. **Inject headers.** For each output mapping, the value at `jsonPath` is set as `targetHeader` on the upstream request. An empty `jsonPath` injects the whole document. Missing fields are skipped.

Found values are cached for `cacheTimeout` in a per-filter in-memory cache of `cacheSizeMB`. Keys that don't exist are not cached. The cache is kept across config reloads as long as the filter definition is unchanged.

:::warning Trust
The injected headers are only as trustworthy as the variables used to build the key. Build keys from values a client cannot forge, such as a header set by the [JWT filter](./jwt-auth.md) (`claimMappings`) or the [API key filter](./api-key.md), rather than from a header the client sends directly.
:::

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `redisService` | `redis_service` | string | required | `HyperRedis` / `redis:` entry to read from. |
| `keyPattern` | `key_pattern` | string | required | Redis key with `{variable}` placeholders. |
| `variables.<name>.source` | `variables.<name>.source` | string | required | `{header:Name}`, `{path}`, `{method}` or `{client_ip}`. |
| `variables.<name>.default` | `variables.<name>.default` | string | — | Used when the value resolves to empty. |
| `variables.<name>.jsonPath` | `variables.<name>.json_path` | string | — | Extract from a JSON-valued source. |
| `variables.<name>.regexPattern` | `variables.<name>.regex_pattern` | string | — | Keep the first capture group. An invalid regex stops the filter from loading. |
| `outputMappings[].jsonPath` | `output_mappings[].json_path` | string | — | Field of the stored document; empty = whole document. |
| `outputMappings[].targetHeader` | `output_mappings[].target_header` | string | required | Upstream header to set. |
| `cacheSizeMB` | `cache_size_mb` | int | `10` | Size of the local cache. |
| `cacheTimeout` | `cache_timeout` | duration | `10s` | How long found values are cached. An invalid duration stops the filter from loading. |
