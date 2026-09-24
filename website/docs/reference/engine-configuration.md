---
sidebar_position: 1
title: Engine configuration
description: Every key of the engine's YAML configuration, with types, defaults and validation rules, plus the environment variables the engine reads.
---

# Engine configuration

The engine reads one YAML document (`config.yaml`). With the operator, this document is generated from the CRDs and stored in the `hyper-engine-config` ConfigMap; you can read it there to see exactly what the engine runs.

## Complete example

```yaml
version: v1                              # required, must be "v1"

server:
  address: "0.0.0.0:9001"                # ext_proc gRPC listener
  max_concurrent_streams: 10000
  pool_prewarm_size: 5000
  initial_header_capacity: 64
  prealloc_body_buffer_bytes: 65536
  health_address: ":9003"                # /healthz and /readyz
  pprof_address: ""                      # empty = pprof disabled
  client_ip:
    trusted_proxy_hops: 0
  tls:
    enabled: false
    cert_file: /etc/hypergate/tls/tls.crt
    key_file: /etc/hypergate/tls/tls.key
    ca_file: /etc/hypergate/ca/ca.crt
    mutual_tls: false

telemetry:
  logging:
    level: INFO                          # DEBUG, INFO, WARN, ERROR
    format: json                         # json or console
    output_path: stdout                  # stdout, stderr or a file path
    development: false

redis:
  main:                                  # service name referenced by filters
    type: SINGLE                         # SINGLE, CLUSTER or SENTINEL
    socket_type: tcp                     # tcp or unix
    url: "redis.data.svc:6379"
    pool_size: 20
    auth: "app-user:s3cret"              # "password" or "username:password"
    tls: false
    timeout: "10s"
    active_conn_health_check: true
    startup_max_elapsed_time: "30s"

chains:
  public:
    - type: correlation_id
      options:
        header_name: x-request-id
    - type: embedded_rate_limiter
      options:
        domain: public
        algorithm: fixed_window
        redis_service: main
        descriptors:
          - entries:
              - key: client_ip
            limit: 100
            unit: minute
  blocked:
    - type: deny
      options:
        status_code: 403

router:
  routes:
    - name: internal
      target_chain: blocked
      matches:
        - path_prefix: /internal
    - name: api
      target_chain: public
      matches:
        - path_prefix: /api
          headers:
            x-tenant: "*"
        - path_regex_pattern: "^/v[0-9]+/public/"
  default_chain: public
```

## Top level

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `version` | string | none, **required** | Must be `v1`. Any other value rejects the configuration. |
| `server` | object | see [server](#server) | Listener, pools, health, pprof, client IP and TLS settings. |
| `telemetry` | object | see [telemetry](#telemetry) | Logging. |
| `redis` | map of name to object | empty | Named Redis services. See [redis](#redis). |
| `chains` | map of name to list | empty | Named filter chains. See [chains](#chains). |
| `router` | object | empty | Routes and the default chain. See [router](#router). |

Unknown keys are ignored.

## server

| Key | Type | Default | Reloadable | Description |
| --- | --- | --- | --- | --- |
| `address` | string | `:9001` | No | Listen address of the ext_proc gRPC server. The operator compiles `0.0.0.0:9001` by default. |
| `max_concurrent_streams` | uint32 | `10000` (when `0`) | No | Maximum concurrent gRPC streams per connection from Envoy. Each HTTP request is one stream. |
| `pool_prewarm_size` | int | `5000` (when `<= 0`) | No | Number of request contexts allocated at start-up. |
| `initial_header_capacity` | int | `64` (when `<= 0`) | No | Initial capacity of each context's header maps. |
| `prealloc_body_buffer_bytes` | int | `0` | No | Size of the body buffer pre-allocated per request context. Bodies larger than the buffer are used without copying. The operator compiles `65536` by default. |
| `health_address` | string | `:9003` | No | Address of the HTTP server for `/healthz` and `/readyz`. |
| `pprof_address` | string | empty (disabled) | No | When set, serves `/debug/pprof/` on this address. Bind it to `127.0.0.1` and never expose it publicly. |
| `client_ip` | object | | | See [server.client_ip](#serverclient_ip). |
| `tls` | object | | No | See [server.tls](#servertls). |

The gRPC server uses fixed keepalive settings: connections idle for 15 minutes are closed, connections are recycled after 30 minutes (with a 5 minute grace period), the server pings idle connections every 5 minutes, and it rejects client keepalive pings more frequent than every 5 minutes. If you configure gRPC keepalive on Envoy's cluster, keep its interval at 5 minutes or more.

### server.client_ip

| Key | Type | Default | Reloadable | Description |
| --- | --- | --- | --- | --- |
| `trusted_proxy_hops` | int | `0` | Yes | Number of proxies in front of Envoy whose `X-Forwarded-For` entries are trusted. `0` means the client is Envoy's direct peer. Must be `>= 0`. See [Client IP](../concepts/client-ip.md). |

### server.tls

TLS for the ext_proc listener. Without it the listener accepts plaintext HTTP/2 (h2c), which is the usual setup when Envoy and the engine run in the same cluster.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | Serve TLS. The minimum version is TLS 1.3. |
| `cert_file` | string | empty | PEM server certificate. Required when `enabled`. |
| `key_file` | string | empty | PEM private key. Required when `enabled`. |
| `ca_file` | string | empty | PEM CA bundle used to verify client certificates. Required when `mutual_tls` is `true`. |
| `mutual_tls` | bool | `false` | Require and verify a client certificate signed by `ca_file`. |

A TLS error (unreadable files, `mutual_tls` without `ca_file`) stops the engine at start-up.

## telemetry

### telemetry.logging

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `level` | string | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR`, case-insensitive. An unrecognised value falls back to `INFO`. `DEBUG` logs every phase and routing decision. |
| `format` | string | `console` | `json` for structured logs, anything else for the console encoder. |
| `output_path` | string | `stdout` | `stdout`, `stderr` or a file path (opened in append mode). |
| `development` | bool | `false` | Use zap's development encoder and development mode (coloured levels in console format). |

Logging settings are applied at start-up only. With the operator, `HyperConfig.spec.logLevel` sets `level`.

## redis

A map from service name to connection settings. Filters reference a service by name in `redis_service`. A filter that names a service that is not defined fails to compile, which rejects the configuration.

Omitted fields get the defaults below; every duration is a Go duration string such as `250ms`, `10s` or `1m`.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `type` | string | `SINGLE` | Topology: `SINGLE`, `CLUSTER` or `SENTINEL`. |
| `socket_type` | string | `tcp` | `tcp` or `unix`. With `unix`, `url` is the socket path. |
| `url` | string | `localhost:6379` | `SINGLE`: `host:port` (or a socket path). `CLUSTER`: comma-separated seed nodes, `host1:port1,host2:port2`. `SENTINEL`: `<master_name>,<sentinel1>:26379[,<sentinel2>:26379...]`. |
| `pool_size` | int | `10` | Connections per pool (per node for clusters). |
| `auth` | string | empty | Data-node credentials: `password` or `username:password` (ACL). |
| `tls` | bool | `false` | Connect with TLS (minimum TLS 1.2). |
| `tls_client_cert` | string | empty | PEM client certificate for mutual TLS. Used only together with `tls_client_key`. |
| `tls_client_key` | string | empty | PEM client key. |
| `tls_cacert` | string | empty | PEM CA bundle used to verify the server. Without it the system roots are used. |
| `tls_skip_hostname_verification` | bool | `false` | Verify the server's certificate chain but not its hostname. |
| `pipeline_window` | duration | `0s` | How long the client waits to batch commands before flushing. `0s` disables implicit pipelining. |
| `cluster_pipeline_parallelism` | int | `1` | Number of shards flushed concurrently when a pipeline spans several cluster nodes. |
| `sentinel_auth` | string | empty | Password for the Sentinel nodes (separate from `auth`). |
| `active_conn_health_check` | bool | `false` | Send `PING` every 5 seconds and log when the service becomes unhealthy or recovers. It does not affect readiness or routing. |
| `timeout` | duration | `10s` | Dial timeout for new connections. Requests are additionally bounded by the ext_proc stream's context. |
| `on_empty_behavior` | string | `WAIT` | Behaviour when the pool is exhausted. Only `WAIT` is accepted. |
| `wait_timeout` | duration | `1s` | Validated, but not currently applied by the client. |
| `startup_initial_interval` | duration | `1s` | First back-off interval when connecting, and minimum reconnect interval of the pool. |
| `startup_max_interval` | duration | `30s` | Maximum back-off interval when connecting, and maximum reconnect interval of the pool. |
| `startup_max_elapsed_time` | duration | `0s` | Total time to keep retrying the initial connection. `0s` retries forever. See the note below. |

When a service is added or its settings change, the engine connects to it and waits for a successful `PING` before the configuration is accepted. At start-up this delays readiness; on reload it delays (and, with `0s`, can indefinitely hold) the reload. Set `startup_max_elapsed_time` for services that may be down. Services whose settings are unchanged keep their existing connections across reloads.

## chains

A map from chain name to an ordered list of filters. Each filter is:

| Key | Type | Description |
| --- | --- | --- |
| `type` | string | Filter type, one of the values below. An unknown type rejects the configuration. |
| `options` | map | Filter-specific options, documented on the filter's page. |

| `type` | Filter page |
| --- | --- |
| `embedded_rate_limiter` | [Rate limiting](/docs/filters/rate-limiting) |
| `api_key` | [API key](/docs/filters/api-key) |
| `jwt_auth` | [JWT auth](/docs/filters/jwt-auth) |
| `external_auth` | [External auth](/docs/filters/external-auth) |
| `firewall` | [Firewall](/docs/filters/firewall) |
| `deny` | [Deny](/docs/filters/deny) |
| `header_modifier` | [Header modifier](/docs/filters/header-modifier) |
| `correlation_id` | [Correlation ID](/docs/filters/correlation-id) |
| `redis_metadata_enricher` | [Redis metadata enricher](/docs/filters/redis-metadata-enricher) |

Filters run in list order. An empty chain (`[]`) is valid and lets requests through. Identical filter definitions (same type, options and Redis service) share one instance, within a configuration and across reloads.

## router

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `routes` | list of [route](#routerroutes) | empty | Evaluated in order; the first match wins. |
| `default_chain` | string | empty | Chain for requests that match no route. Empty means such requests pass without policy. Must name a defined chain. |
| `other` | string | empty | Legacy alias of `default_chain`, used only when `default_chain` is empty. Must name a defined chain. |

### router.routes[]

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `name` | string | empty | Label used in logs and validation errors. |
| `target_chain` | string | none, **required** | Chain to run. Must name a defined chain. |
| `matches` | list of [match](#routerroutesmatches) | empty | Alternatives: the route matches when any entry matches. A route with no entries never matches. |

### router.routes[].matches[]

All fields that are set must match (AND).

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `path_prefix` | string | empty | Prefix of the raw `:path`, including the query string. |
| `path_regex_pattern` | string | empty | RE2 expression matched against the raw `:path` (unanchored). Must compile. |
| `headers` | map of header name to [header match](#header-match) | empty | Every listed header must be present and satisfy its condition. Write header names in lower case. |

### Header match

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `exact` | string | empty | Required value. `"*"` only requires the header to be present. |
| `regex_pattern` | string | empty | RE2 expression the value must match. Must compile. |

A scalar is shorthand for `exact`: `x-tenant: acme` equals `x-tenant: {exact: acme}`. When both `exact` and `regex_pattern` are set, both must hold. When neither is set, the header only has to be present.

See [Routing](../concepts/routing.md) for the full semantics.

## Validation rules

A configuration is rejected, at start-up (the engine exits) or on reload (the previous policy stays), when:

- `version` is not `v1`,
- `server.client_ip.trusted_proxy_hops` is negative,
- a Redis service has an invalid `type`, `socket_type`, `on_empty_behavior` or duration,
- a route has no `target_chain`, or a `target_chain`, `default_chain` or `other` names an undefined chain,
- a route `path_regex_pattern` or header `regex_pattern` does not compile,
- any filter fails to compile: unknown `type`, invalid options, an undefined `redis_service`, a Redis service that cannot be reached within `startup_max_elapsed_time`, a failed JWKS fetch, an unreadable secret file.

## Environment variables and flags

| Variable or flag | Default | Description |
| --- | --- | --- |
| `CONFIG_PROVIDER` | `FILE` | Where to load the configuration from: `FILE`, `K8S` or `URL`. |
| `-config`, `-c` | none | `FILE` provider: path of the config file. Takes precedence over `CONFIG_FILE_PATH`. |
| `CONFIG_FILE_PATH` | `/etc/hyper-engine/config.yaml` | `FILE` provider: path of the config file. |
| `CONFIG_K8S_NAME` | `hyper-engine-config` | `K8S` provider: ConfigMap name. The configuration is read from the key `config.yaml`. |
| `CONFIG_K8S_NAMESPACE` | `hyper-system` | `K8S` provider: ConfigMap namespace. Uses the in-cluster service account, which needs `get` and `watch` on the ConfigMap. |
| `CONFIG_URL` | none | `URL` provider: URL fetched with `GET` at start-up and on every reload. Required with `URL`. |
| `CONFIG_RELOAD_ADDRESS` | `127.0.0.1:9002`, or `:9002` when a token is set | `URL` provider: listen address of `POST /v1/reload`. |
| `CONFIG_RELOAD_TOKEN` | none | `URL` provider: when set, reload calls must send `Authorization: Bearer <token>`. |

See [Hot reload](../concepts/hot-reload.md) for how each provider detects changes.
