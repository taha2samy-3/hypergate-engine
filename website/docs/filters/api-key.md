---
sidebar_position: 2
title: API keys
description: Validate API keys against Redis, check account status and inject key metadata as upstream headers.
---

import Icon from '@site/static/img/icons/api-key.svg';

# <Icon className="hg-icon" aria-hidden="true" /> API keys

The API key filter reads a key from a header or query parameter, hashes it, and looks the hash up in Redis. It rejects missing, unknown and suspended keys. For valid keys, it copies fields of the stored record into upstream headers, so your services get `x-tenant-id` or `x-plan` without calling Redis themselves.

| At a glance | |
| --- | --- |
| CRD kind | `ApiKeyFilter` |
| Engine filter type | `api_key` |
| Runs in phase | request headers |
| Blocks with | `401` (missing or unknown key), `403` (status check failed) |
| On Redis errors | `500` (fail closed) |

## Example

Store a key as a Redis hash. Keys are stored under `redisKeyPrefix` + the SHA-256 hex digest of the raw key:

```bash
KEY=sk_live_123
HASH=$(printf '%s' "$KEY" | sha256sum | cut -d' ' -f1)
redis-cli HSET "apikey:$HASH" tenant acme plan gold status active
```

```yaml
apiVersion: hyper.io/v1alpha1
kind: ApiKeyFilter
metadata:
  name: partner-keys
spec:
  redisService: main
  keyNames: ["x-api-key", "api_key"]
  keyInHeader: true
  keyInQuery: true
  hideCredentials: true        # strip the key before forwarding
  hashAlgorithm: sha256
  valueFormat: hash
  statusCheck:
    enabled: true
    fieldName: status
    expectedValue: active
  outputMappings:
    - targetHeader: x-tenant-id
      redisField: tenant
    - targetHeader: x-plan
      redisField: plan
```

Equivalent engine configuration:

```yaml
chains:
  partners:
    - type: api_key
      options:
        redis_service: main
        key_names: ["x-api-key", "api_key"]
        key_in_header: true
        key_in_query: true
        hide_credentials: true
        hash_algorithm: sha256
        value_format: hash
        status_check:
          enabled: true
          field_name: status
          expected_value: active
        output_mappings:
          - {target_header: x-tenant-id, redis_field: tenant}
          - {target_header: x-plan, redis_field: plan}
```

## Request flow

1. **Find the key.** For each name in `keyNames`, in order, the filter checks the request header of that name (case-insensitive) if `keyInHeader` is on, then the query parameter of that name (case-sensitive) if `keyInQuery` is on. The first non-empty value wins. No key → `401 Unauthorized: Missing API Key`.
2. **Hash it** with `hashAlgorithm` (`sha256`, `md5` or `none`) and prepend `redisKeyPrefix`.
3. **Look it up** according to `valueFormat` (below). Not found → `401 Unauthorized: Invalid API Key`.
4. **Check status** if `statusCheck.enabled`. A value other than `expectedValue` → `403 Forbidden: Account is <status>`.
5. **Inject headers** listed in `outputMappings` into the upstream request. They overwrite headers of the same name sent by the client.
6. **Hide credentials** if `hideCredentials`: the key header is removed, and a key in the query string is removed from the `:path` sent upstream.

### Value formats

| `valueFormat` | Redis type | How `outputMappings` read it | Status field |
| --- | --- | --- | --- |
| `hash` (default) | Hash, read with `HMGET` | `redisField` names a hash field | `statusCheck.fieldName` is a hash field |
| `json` | String containing JSON, read with `GET` | `jsonPath` is a [gjson path](https://github.com/tidwall/gjson/blob/master/SYNTAX.md) such as `owner.tenant` | `statusCheck.fieldName` is a gjson path |
| `plain` | String split by `delimiter` (default `|`), read with `GET` | mappings are filled **by position**: the first mapping gets the first part, and so on | not supported |

### Local cache

Results are cached in memory on each engine instance, so a hot key does not hit Redis on every request:

| Result | Cached for |
| --- | --- |
| valid key (with its injected headers) | 30 seconds |
| unknown key, or failed status check | 10 seconds |

Revoking or suspending a key in Redis therefore takes effect within 30 seconds. Each filter definition has its own 10 MB cache. The cache survives config reloads as long as the filter's definition does not change (see [hot reload](../concepts/hot-reload.md)).

:::tip Headers the upstream trusts
A mapping only overwrites the client's header when the stored record has a value for it. If a field can be missing, remove the header first with a [header modifier](./header-modifier.md) placed before the API key filter, so a client cannot supply its own `x-tenant-id`.
:::

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `redisService` | `redis_service` | string | required | `HyperRedis` / `redis:` entry holding the keys. |
| `keyNames` | `key_names` | list | `["x-api-key"]` | Header and/or query parameter names to look for, in priority order. |
| `keyInHeader` | `key_in_header` | bool | `true` | Look in request headers. |
| `keyInQuery` | `key_in_query` | bool | CRD: `true`, engine: `false` | Look in the query string. If both are false, headers are used. |
| `hideCredentials` | `hide_credentials` | bool | CRD: `true`, engine: `false` | Remove the key before forwarding upstream. |
| `redisKeyPrefix` | `redis_key_prefix` | string | `apikey:` | Prefix of the Redis key. |
| `hashAlgorithm` | `hash_algorithm` | enum | `sha256` | `sha256`, `md5` or `none` (store raw keys; not recommended). |
| `valueFormat` | `value_format` | enum | `hash` | `hash`, `json` or `plain`. |
| `delimiter` | `delimiter` | string | `|` | Separator for `plain`. |
| `statusCheck.enabled` | `status_check.enabled` | bool | `false` | Reject keys whose status field differs from `expectedValue`. |
| `statusCheck.fieldName` | `status_check.field_name` | string | — | Hash field or JSON path holding the status. |
| `statusCheck.expectedValue` | `status_check.expected_value` | string | — | The only accepted status. |
| `outputMappings[].targetHeader` | `output_mappings[].target_header` | string | required | Upstream header to set. |
| `outputMappings[].redisField` | `output_mappings[].redis_field` | string | — | Source field for `hash`. |
| `outputMappings[].jsonPath` | `output_mappings[].json_path` | string | — | Source path for `json`. |
