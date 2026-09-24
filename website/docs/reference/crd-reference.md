---
sidebar_position: 2
title: CRD reference
description: Every hyper.io/v1alpha1 kind with all spec and status fields, defaults, validation and the engine option each field compiles to.
---

# CRD reference

All kinds belong to the API group `hyper.io`, version `v1alpha1`, and are **cluster-scoped**: omit `metadata.namespace`. Names are referenced across kinds by `metadata.name` (a HyperChain lists filters by kind and name, a HyperRoute names a HyperChain, filters name a HyperRedis).

| Kind | Plural | Short names | Compiles to |
| --- | --- | --- | --- |
| [HyperConfig](#hyperconfig) | `hyperconfigs` | `hcfg` | `server`, `telemetry`, `router.default_chain`; the engine DaemonSet |
| [HyperRedis](#hyperredis) | `hyperredis` | `hr` | an entry under `redis` |
| [HyperChain](#hyperchain) | `hyperchains` | `hc` | an entry under `chains` |
| [HyperRoute](#hyperroute) | `hyperroutes` | `hrt` | an entry in `router.routes` |
| [RateLimitFilter](#ratelimitfilter) | `ratelimitfilters` | `rlf` | filter `embedded_rate_limiter` |
| [ApiKeyFilter](#apikeyfilter) | `apikeyfilters` | `apikf` | filter `api_key` |
| [JwtAuthFilter](#jwtauthfilter) | `jwtauthfilters` | `jwtf` | filter `jwt_auth` |
| [ExternalAuthFilter](#externalauthfilter) | `externalauthfilters` | `eaf` | filter `external_auth` and a sidecar |
| [FirewallFilter](#firewallfilter) | `firewallfilters` | `fwf`, `firewall` | filter `firewall` and a sidecar |
| [DenyFilter](#denyfilter) | `denyfilters` | `deny` | filter `deny` |
| [HeaderModifierFilter](#headermodifierfilter) | `headermodifierfilters` | `hmf` | filter `header_modifier` |
| [CorrelationIdFilter](#correlationidfilter) | `correlationidfilters` | `corridf` | filter `correlation_id` |
| [RedisMetadataEnricherFilter](#redismetadataenricherfilter) | `redismetadataenricherfilters` | `rmef` | filter `redis_metadata_enricher` |

"Engine option" columns below name the key the field is compiled to; see the [engine configuration](./engine-configuration.md) and the filter pages for its behaviour. A field left empty is omitted from the compiled options, so the engine default applies.

## HyperConfig

Deploys one engine in `targetNamespace` and holds its server settings. Only one HyperConfig may manage a namespace: the oldest (by creation timestamp, then name) wins.

```yaml
apiVersion: hyper.io/v1alpha1
kind: HyperConfig
metadata:
  name: default-engine
spec:
  targetNamespace: hyper-system
  redisServiceRef: shared-redis
  defaultChain: public
  logLevel: INFO
  trustedProxyHops: 1
  engineResources:
    requests:
      cpu: 250m
      memory: 256Mi
    limits:
      memory: 1Gi
```

### spec

| Field | Type | Default | Engine option | Description |
| --- | --- | --- | --- | --- |
| `serverAddress` | string | `0.0.0.0:9001` | `server.address` | gRPC listen address. The operator's container port and Service are fixed at `9001`, so keep that port. |
| `maxConcurrentStreams` | integer (uint32) | unset (engine: `10000`) | `server.max_concurrent_streams` | Concurrent streams per connection. |
| `logLevel` | enum `DEBUG`, `INFO`, `WARN`, `ERROR` | `INFO` | `telemetry.logging.level` | Engine log level. |
| `targetNamespace` | string | `hyper-system` | | Namespace for the engine DaemonSet, Service, RBAC and ConfigMap. Created if missing. |
| `engineImage` | string | operator's `ENGINE_IMAGE` (the chart sets it to the engine release matching the operator) | | Engine container image. |
| `engineResources` | `ResourceRequirements` (`requests`, `limits`) | requests `cpu: 100m`, `memory: 128Mi`; limits `memory: 512Mi` | | Engine container resources. When set, replaces the defaults entirely. |
| `trustedProxyHops` | integer, minimum `0` | `0` | `server.client_ip.trusted_proxy_hops` | Trusted proxies in front of Envoy. See [Client IP](../concepts/client-ip.md). |
| `redisServiceRef` | string | none, **required** | | Name of a HyperRedis. Required by the schema; the compiler currently does not use it (filters name their Redis service themselves). |
| `defaultChain` | string | empty | `router.default_chain` | HyperChain for unmatched requests. If it names a missing HyperChain, a `503` deny chain is compiled under that name. |
| `poolPrewarmSize` | integer | `5000` | `server.pool_prewarm_size` | Request contexts allocated at start-up. |
| `initialHeaderCapacity` | integer | `64` | `server.initial_header_capacity` | Initial header map capacity per context. |
| `preallocBodyBufferBytes` | integer | `65536` | `server.prealloc_body_buffer_bytes` | Body buffer per context. |

The compiler always sets `server.health_address: ":9003"` so that the probes match.

### status

| Field | Type | Description |
| --- | --- | --- |
| `state` | string | `Ready` after the engine resources were reconciled, `Conflict` when an older HyperConfig manages the same `targetNamespace`. |
| `message` | string | Explanation, for example `Engine resources reconciled in namespace hyper-system` or `targetNamespace "hyper-system" is already managed by HyperConfig "a"`. |

Printer columns: `Server Address`, `Log Level`, `Redis Ref`, `State`.

## HyperRedis

A Redis service. The `metadata.name` becomes the service name that filters use in `redisService`.

```yaml
apiVersion: hyper.io/v1alpha1
kind: HyperRedis
metadata:
  name: shared-redis
spec:
  type: SINGLE
  url: "redis.data.svc.cluster.local:6379"
  poolSize: 20
  timeout: "500ms"
  activeConnHealthCheck: true
```

### spec

| Field | Type | Default | Engine option | Description |
| --- | --- | --- | --- | --- |
| `url` | string | none, **required** | `redis.<name>.url` | Endpoint; the format depends on `type` (see [redis](./engine-configuration.md#redis)). |
| `type` | enum `SINGLE`, `CLUSTER`, `SENTINEL` | none, **required** | `redis.<name>.type` | Topology. |
| `poolSize` | integer, minimum `1` | unset (engine: `10`) | `redis.<name>.pool_size` | Connection pool size. |
| `timeout` | string (duration) | unset (engine: `10s`) | `redis.<name>.timeout` | Dial timeout. |
| `activeConnHealthCheck` | boolean | `false` | `redis.<name>.active_conn_health_check` | Engine: periodic `PING`. Operator: also enables the status check below. |

All other engine Redis settings (authentication, TLS, Sentinel password, pipelining, start-up retry limits) use their engine defaults; HyperRedis does not expose them.

### status

Populated only when `activeConnHealthCheck` is `true`. The operator then connects to the service itself, every 60 seconds after a success and every 10 seconds after a failure, and records a `ConnectionEstablished` or `ConnectionFailed` event.

| Field | Type | Description |
| --- | --- | --- |
| `state` | enum `Connected`, `Error`, `Pending` | Result of the operator's last `PING`. |
| `lastCheck` | timestamp | Time of the last check. |

Printer columns: `State`, `Type`, `Last Check`.

## HyperChain

An ordered list of filter references. Filters run in list order.

```yaml
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: public-api
spec:
  filters:
    - kind: CorrelationIdFilter
      name: request-id
    - kind: RateLimitFilter
      name: api-per-client
```

### spec

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `filters` | list, **required** | | Filter references. |
| `filters[].kind` | enum, **required** | | Filter kind (see below). |
| `filters[].name` | string, **required** | | `metadata.name` of the filter resource. |

The compiler and the validating webhook accept all nine filter kinds: `RateLimitFilter`, `HeaderModifierFilter`, `DenyFilter`, `CorrelationIdFilter`, `RedisMetadataEnricherFilter`, `ApiKeyFilter`, `ExternalAuthFilter`, `FirewallFilter`, `JwtAuthFilter`.

:::caution Check the installed schema
The `kind` enum is enforced by the CRD schema installed in your cluster. The HyperChain CRD in `charts/hyper-operator/crds/hyperchain_crd.yaml` at the time of writing lists only `RateLimitFilter`, `HeaderModifierFilter`, `DenyFilter`, `CorrelationIdFilter` and `RedisMetadataEnricherFilter`; with that schema the API server rejects references to the other four kinds with `Unsupported value`. Check with `kubectl get crd hyperchains.hyper.io -o yaml` and update the CRD if needed.
:::

With webhooks enabled, creating or updating a HyperChain fails if a referenced filter does not exist, and deleting a HyperChain fails while a HyperConfig uses it as `defaultChain`.

### status

| Field | Type | Description |
| --- | --- | --- |
| `state` | string | `Ready` when every filter was resolved and translated; `Degraded` otherwise. Empty until the operator has compiled the chain once. |
| `message` | string | `Chain successfully compiled`, or the reason, for example `Filter api-limit of Kind RateLimitFilter not found`. |

A `Degraded` chain is compiled as a single `deny` filter returning `503 Service Unavailable`, so its routes fail closed. Printer columns: `State`, `Message`.

## HyperRoute

Routes requests to a HyperChain. The compiler orders all HyperRoutes by `priority`, highest first; the first matching route wins.

```yaml
apiVersion: hyper.io/v1alpha1
kind: HyperRoute
metadata:
  name: api-v2
spec:
  priority: 100
  targetPolicy: public-api
  matches:
    - pathPrefix: /api/v2
      headers:
        x-tenant: "*"
    - pathRegexPattern: "^/v2/"
```

### spec

| Field | Type | Default | Engine option | Description |
| --- | --- | --- | --- | --- |
| `priority` | integer, **required** | | route order | Higher values are evaluated first. Equal priorities have no guaranteed order. |
| `targetPolicy` | string, **required** | | `target_chain` | HyperChain name. An empty value skips the route; a missing HyperChain compiles to a `503` deny chain. |
| `matches` | list, **required** | | `matches` | Alternatives (OR). |
| `matches[].pathPrefix` | string | empty | `path_prefix` | Prefix of the raw path (including the query string). |
| `matches[].pathRegexPattern` | string | empty | `path_regex_pattern` | RE2 expression. An invalid expression skips the whole route. |
| `matches[].headers` | map of string to string | empty | `headers.<name>.exact` | Exact values; `"*"` means present. Names are lower-cased by the compiler. |

The route's `metadata.name` becomes the engine route `name`. `status` is an empty object. Printer columns: `Priority`, `Target Policy`.

## RateLimitFilter

See [Rate limiting](/docs/filters/rate-limiting).

```yaml
apiVersion: hyper.io/v1alpha1
kind: RateLimitFilter
metadata:
  name: api-per-client
spec:
  domain: api
  algorithm: fixed_window
  redisService: shared-redis
  responseHeaders:
    enabled: true
  descriptors:
    - entries:
        - key: client_ip
      limit: 100
      unit: minute
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `domain` | string, **required** | | `domain` |
| `algorithm` | enum `fixed_window`, `sliding_window_counter`, `token_bucket`, `leaky_bucket`, `sliding_window_log`, **required** | | `algorithm` |
| `redisService` | string, **required** | | `redis_service` |
| `responseHeaders.enabled` | boolean | `false` | `response_headers.enabled` |
| `responseHeaders.limitHeader` | string | `RateLimit-Limit` | `response_headers.limit_header` |
| `responseHeaders.remainingHeader` | string | `RateLimit-Remaining` | `response_headers.remaining_header` |
| `responseHeaders.resetHeader` | string | `RateLimit-Reset` | `response_headers.reset_header` |
| `dynamicCost.enabled` | boolean | `false` | `dynamic_cost.enabled` |
| `dynamicCost.sourceHeader` | string | empty | `dynamic_cost.source_header` |
| `dynamicCost.defaultFallbackCost` | integer (int64) | `1` | `dynamic_cost.default_fallback_cost` |
| `dynamicCost.maxAllowedCost` | integer (int64) | unset (no cap) | `dynamic_cost.max_allowed_cost` |
| `headerMappings` | map of string to string | empty | `header_mappings` |
| `descriptors` | list | empty | `descriptors` |
| `descriptors[].entries` | list, **required** | | `entries` |
| `descriptors[].entries[].key` | string, **required** | | `key` |
| `descriptors[].entries[].value` | string | empty (any value) | `value` |
| `descriptors[].limit` | integer (uint32) | unset | `limit` |
| `descriptors[].unit` | enum `second`, `minute`, `hour`, `day`, `week`, `month`, `year` | unset (engine: `minute`) | `unit` |
| `descriptors[].maxTokens` | number | unset | `max_tokens` |
| `descriptors[].fillRate` | number | unset | `fill_rate` |
| `descriptors[].bucketCapacity` | integer (uint32) | unset | `bucket_capacity` |
| `descriptors[].leakRate` | number | unset | `leak_rate` |
| `descriptors[].shadowMode` | boolean | `false` | `shadow_mode` |
| `descriptors[].failOpen` | boolean | `false` | `fail_open` |

`status` is an empty object.

## ApiKeyFilter

See [API key](/docs/filters/api-key).

```yaml
apiVersion: hyper.io/v1alpha1
kind: ApiKeyFilter
metadata:
  name: partner-keys
spec:
  redisService: shared-redis
  keyNames: ["x-api-key"]
  keyInQuery: false
  valueFormat: hash
  statusCheck:
    enabled: true
    fieldName: status
    expectedValue: active
  outputMappings:
    - targetHeader: x-consumer-id
      redisField: client_id
```

| Field | Type | CRD default | Engine option | Engine default |
| --- | --- | --- | --- | --- |
| `keyNames` | list of string | `["x-api-key"]` | `key_names` | `["x-api-key"]` |
| `keyInHeader` | boolean | `true` | `key_in_header` | `false` (`true` if both locations are off) |
| `keyInQuery` | boolean | `true` | `key_in_query` | `false` |
| `hideCredentials` | boolean | `true` | `hide_credentials` | `false` |
| `redisService` | string, **required** | | `redis_service` | |
| `redisKeyPrefix` | string | `apikey:` | `redis_key_prefix` | `apikey:` |
| `hashAlgorithm` | enum `sha256`, `md5`, `none` | `sha256` | `hash_algorithm` | `sha256` |
| `valueFormat` | enum `plain`, `hash`, `json` | `hash` | `value_format` | `hash` |
| `delimiter` | string | `\|` | `delimiter` | `\|` for `plain` |
| `statusCheck.enabled` | boolean | `false` | `status_check.enabled` | `false` |
| `statusCheck.fieldName` | string | empty | `status_check.field_name` | |
| `statusCheck.expectedValue` | string | empty | `status_check.expected_value` | |
| `outputMappings[].targetHeader` | string, **required** | | `output_mappings[].target_header` | |
| `outputMappings[].redisField` | string | empty | `output_mappings[].redis_field` | |
| `outputMappings[].jsonPath` | string | empty | `output_mappings[].json_path` | |

The CRD defaults differ from the engine defaults: through the CRD, keys are accepted from both headers and the query string and are stripped before forwarding unless you set the fields to `false`. `status` is an empty object. Printer columns: `Redis Service`, `Value Format`, `Hash Algorithm`.

## JwtAuthFilter

See [JWT auth](/docs/filters/jwt-auth). The schema requires at least one of `jwksEndpoint`, `localSecretRef` or `introspectionEndpoint`.

```yaml
apiVersion: hyper.io/v1alpha1
kind: JwtAuthFilter
metadata:
  name: internal-jwt
spec:
  localSecretRef:
    name: jwt-hmac
    key: secret
  algorithm: HS256
  issuer: https://auth.example.com
  claimMappings:
    sub: x-user-id
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `source` | enum `header`, `query`, `cookie` | unset (engine: `header`) | `source` |
| `headerName` | string | unset (engine: `authorization`) | `header_name` |
| `queryParam` | string | empty | `query_param` |
| `cookieName` | string | empty | `cookie_name` |
| `jwksEndpoint` | string | empty | `jwks_endpoint` |
| `jwksRefreshInterval` | string (duration) | unset (engine: `5m`) | `jwks_refresh_interval` |
| `localSecretRef.name` | string, required in the object | | mounted Secret, compiled to `local_secret_file` |
| `localSecretRef.key` | string, required in the object | | key within the Secret |
| `algorithm` | string | unset (engine: `HS256` with a secret, else `RS256`) | `algorithm` |
| `issuer` | string | empty | `issuer` |
| `audience` | string | empty | `audience` |
| `claimMappings` | map of string to string | empty | `claim_mappings` |
| `introspectionEndpoint` | string | empty | `introspection_endpoint` |
| `introspectionAuthSecretRef.name` | string, required in the object | | mounted Secret, compiled to `introspection_auth_header_file` |
| `introspectionAuthSecretRef.key` | string, required in the object | | key within the Secret |
| `introspectionTimeout` | string (duration) | unset (engine: `2s`) | `introspection_timeout` |
| `failOpen` | boolean | `false` | `fail_open` |
| `stripToken` | boolean | `false` | `strip_token` |

Secrets must exist in the HyperConfig's `targetNamespace` (in every target namespace, if you run several engines). The operator mounts each referenced key read-only into the engine container:

| Reference | Mounted file | Compiled option |
| --- | --- | --- |
| `localSecretRef` | `/etc/hypergate/secrets/jwt-<filter>/<secret>/local-secret` | `local_secret_file` |
| `introspectionAuthSecretRef` | `/etc/hypergate/secrets/jwt-<filter>/<secret>/introspection-auth` | `introspection_auth_header_file` |

The secret values never appear in the ConfigMap. `status` is an empty object.

## ExternalAuthFilter

See [External auth](/docs/filters/external-auth). Every ExternalAuthFilter in the cluster is injected into every engine pod as a sidecar named `ext-auth-<name>`, listening on `/var/run/hypergate/ext-auth-<name>.sock`.

```yaml
apiVersion: hyper.io/v1alpha1
kind: ExternalAuthFilter
metadata:
  name: authz
spec:
  protocol: http
  container:
    image: registry.example.com/authz-sidecar:1.4.0
    args: ["--listen", "unix://{socket_path}"]
  engineRules:
    timeout: 500ms
    forwardHeaders: ["authorization", "cookie"]
    onSuccess:
      upstreamHeadersToAdd: ["x-user-id"]
      upstreamHeadersToRemove: ["cookie"]
    onFailure:
      downstreamPassThroughHeaders: ["www-authenticate"]
```

| Field | Type | Default | Engine option / effect |
| --- | --- | --- | --- |
| `protocol` | enum `http`, `grpc` | `http` | `protocol` |
| `container` | object, **required** | | Sidecar container. See [Sidecar container](#sidecar-container). |
| `engineRules.timeout` | string (duration) | `2s` | `timeout` |
| `engineRules.forwardHeaders` | list of string | empty | `forward_headers` |
| `engineRules.onSuccess.upstreamHeadersToAdd` | list of string | empty | `on_success.upstream_headers_to_add` |
| `engineRules.onSuccess.upstreamHeadersToRemove` | list of string | empty | `on_success.upstream_headers_to_remove` |
| `engineRules.onFailure.downstreamPassThroughHeaders` | list of string | empty | `on_failure.downstream_pass_through_headers` |

The compiler sets `socket_path: /var/run/hypergate/ext-auth-<name>.sock`. If `container.socketEnvKey` is empty and the image name contains `oauth2-proxy`, the sidecar receives `OAUTH2_PROXY_HTTP_ADDRESS=unix://<socket>`. `status` is an empty object. Printer columns: `Protocol`, `Image`.

## FirewallFilter

See [Firewall](/docs/filters/firewall). Every FirewallFilter in the cluster is injected into every engine pod as a sidecar named `fw-<name>`, listening on `/var/run/hypergate/fw-<name>.sock`.

```yaml
apiVersion: hyper.io/v1alpha1
kind: FirewallFilter
metadata:
  name: waf
spec:
  protocol: grpc
  container:
    image: registry.example.com/waf-extproc:2.0.1
  engineRules:
    inspectBody: true
    maxBodySizeKB: 512
  rulesConfigMap: waf-rules
```

| Field | Type | Default | Engine option / effect |
| --- | --- | --- | --- |
| `protocol` | enum `http`, `grpc` | `grpc` | `protocol` |
| `container` | object, **required** | | Sidecar container. See [Sidecar container](#sidecar-container). If `socketEnvKey` is empty, `FIREWALL_SOCKET_PATH` is used. |
| `engineRules.timeout` | string (duration) | `2s` | `timeout` |
| `engineRules.forwardHeaders` | list of string | empty (all headers) | `forward_headers` |
| `engineRules.inspectBody` | boolean | `false` | `inspect_body` |
| `engineRules.maxBodySizeKB` | integer (int32) | `1024` | `max_body_size_kb` |
| `engineRules.onSuccess.upstreamHeadersToAdd` | list of string | empty | `on_success.upstream_headers_to_add` |
| `engineRules.onSuccess.upstreamHeadersToRemove` | list of string | empty | `on_success.upstream_headers_to_remove` |
| `engineRules.onFailure.downstreamPassThroughHeaders` | list of string | empty | `on_failure.downstream_pass_through_headers` |
| `rulesConfigMap` | string | empty | ConfigMap in the target namespace, mounted into the sidecar at `/etc/firewall/rules/`. |
| `rulesSecretRef` | string | empty | Secret in the target namespace, mounted into the sidecar at `/etc/firewall/secrets/`. |

The compiler sets `socket_path: /var/run/hypergate/fw-<name>.sock`. `status` is an empty object. Printer columns: `Image`, `InspectBody`, `MaxBodySizeKB`.

### Sidecar container

`spec.container` of ExternalAuthFilter and FirewallFilter.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `image` | string, **required** | | Sidecar image. |
| `imagePullPolicy` | enum `Always`, `Never`, `IfNotPresent` | Kubernetes default | Pull policy. |
| `imagePullSecrets` | list of `{name}` | empty | Added to the engine pod's pull secrets (deduplicated across sidecars). |
| `args` | list of string | empty | Container arguments. Every `{socket_path}` is replaced with the socket path, for example `/var/run/hypergate/fw-waf.sock`. |
| `env` | list of `EnvVar` (`name`, `value`, `valueFrom`) | empty | Environment. The socket variable is appended. |
| `envFrom` | list of `EnvFromSource` | empty | Environment from ConfigMaps or Secrets in the target namespace. |
| `resources` | `ResourceRequirements` | none | Sidecar resources. |
| `socketEnvKey` | string | empty | Name of the variable that receives `unix://<socket path>`. |

Every sidecar mounts the shared `emptyDir` volume `uds-sockets` at `/var/run/hypergate/`. The operator does not set a security context on sidecars.

## DenyFilter

See [Deny](/docs/filters/deny). The schema requires `match`; use `match: {}` for an unconditional deny.

```yaml
apiVersion: hyper.io/v1alpha1
kind: DenyFilter
metadata:
  name: block-debug
spec:
  statusCode: 404
  body: "not found"
  match:
    pathPrefix: /debug
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `statusCode` | integer (int32) | `403` | `status_code` |
| `body` | string | `Forbidden` | `body` |
| `match` | object, **required** | | `match` |
| `match.pathPrefix` | string | empty | `match.path_prefix` |
| `match.pathRegex` | string | empty | `match.path_regex` |
| `match.headers` | map of string to string | empty | `match.headers` |
| `match.responseHeaders` | map of string to string | empty | `match.response_headers` |
| `match.notHeaders` | map of string to string | empty | `match.not_headers` |
| `match.notResponseHeaders` | map of string to string | empty | `match.not_response_headers` |

`status` is an empty object.

## HeaderModifierFilter

See [Header modifier](/docs/filters/header-modifier).

```yaml
apiVersion: hyper.io/v1alpha1
kind: HeaderModifierFilter
metadata:
  name: gateway-headers
spec:
  upstream:
    add:
      x-gateway: hypergate
    remove: ["x-debug"]
  downstream:
    remove: ["server", "x-powered-by"]
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `upstream.add` | map of string to string | empty | `upstream.add` |
| `upstream.override` | map of string to string | empty | `upstream.override` |
| `upstream.remove` | list of string | empty | `upstream.remove` |
| `downstream.add` | map of string to string | empty | `downstream.add` |
| `downstream.override` | map of string to string | empty | `downstream.override` |
| `downstream.remove` | list of string | empty | `downstream.remove` |
| `add` | map of string to string | empty | `add` (shorthand for `upstream.add`) |
| `override` | map of string to string | empty | `override` (shorthand for `upstream.override`) |
| `remove` | list of string | empty | `remove` (shorthand for `upstream.remove`) |

`status` is an empty object.

## CorrelationIdFilter

See [Correlation ID](/docs/filters/correlation-id).

```yaml
apiVersion: hyper.io/v1alpha1
kind: CorrelationIdFilter
metadata:
  name: request-id
spec:
  headerName: x-request-id
  algorithm: uuidv7
  mode: if_missing
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `headerName` | string | unset (engine: `x-request-id`) | `header_name` |
| `algorithm` | string: `uuidv4`, `uuidv7`, `xid`, `ulid` | unset (engine: `uuidv4`) | `algorithm` |
| `mode` | string: `if_missing`, `overwrite` | unset (engine: `if_missing`) | `mode` |
| `prefix` | string | empty | `prefix` |
| `propagateToUpstream` | boolean | unset (engine: `true`) | `propagate_to_upstream` |
| `propagateToDownstream` | boolean | unset (engine: `true`) | `propagate_to_downstream` |
| `inputHeaderName` | string | unset (engine: `headerName`) | `input_header_name` |
| `responseHeaderName` | string | unset (engine: `headerName`) | `response_header_name` |
| `validationRegex` | string | empty | `validation_regex` |

`propagateToUpstream` and `propagateToDownstream` are compiled only when `true`; a `false` value is dropped and the engine default `true` applies, so propagation cannot be turned off through the CRD. `status` is an empty object.

## RedisMetadataEnricherFilter

See [Redis metadata enricher](/docs/filters/redis-metadata-enricher).

```yaml
apiVersion: hyper.io/v1alpha1
kind: RedisMetadataEnricherFilter
metadata:
  name: tenant-plan
spec:
  redisService: shared-redis
  keyPattern: "tenant:{tenant}"
  variables:
    tenant:
      source: "{header:x-tenant-id}"
      default: unknown
  outputMappings:
    - jsonPath: plan
      targetHeader: x-tenant-plan
```

| Field | Type | Default | Engine option |
| --- | --- | --- | --- |
| `redisService` | string, **required** | | `redis_service` |
| `cacheSizeMB` | integer | unset (engine: `10`) | `cache_size_mb` |
| `cacheTimeout` | string (duration) | unset (engine: `10s`) | `cache_timeout` |
| `variables` | map of name to variable | empty | `variables` |
| `variables.<name>.source` | string, **required** | | `source` |
| `variables.<name>.default` | string | empty | `default` |
| `variables.<name>.regexPattern` | string | empty | `regex_pattern` |
| `variables.<name>.jsonPath` | string | empty | `json_path` |
| `keyPattern` | string, **required** | | `key_pattern` |
| `outputMappings[].jsonPath` | string | empty (whole value) | `output_mappings[].json_path` |
| `outputMappings[].targetHeader` | string, **required** | | `output_mappings[].target_header` |

`status` is an empty object.
