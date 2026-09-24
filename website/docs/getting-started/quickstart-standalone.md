---
sidebar_position: 3
title: Standalone quickstart
description: Run the engine with a config file next to Envoy, Redis and an echo backend using Docker Compose.
---

# Standalone quickstart

The engine does not need Kubernetes. This page runs it with a YAML file, using the Docker Compose environment the repository uses for its integration tests (`tests/docker-compose.yaml` and `tests/envoy.yaml`).

The Compose file starts four containers on one bridge network:

| Service | Image | Port on the host | Role |
| --- | --- | --- | --- |
| `hyper-engine` | built from the repository `Dockerfile` | `9001` | The engine, `CONFIG_PROVIDER=FILE`, reading `/etc/hyper-engine/config.yaml`. |
| `envoy-proxy` | `envoyproxy/envoy:v1.28.0` | `8080` (admin `9901` inside the network) | Listener with the `ext_proc` filter pointing at `hyper-engine:9001`. |
| `backend-service` | `ealen/echo-server` | `8082` | Upstream that echoes the request, including headers, as JSON. |
| `redis` | `redis:6-alpine` | `6379` | Backing store for the rate limiter. |

## 1. Write a configuration

The Compose file bind-mounts `tests/config.yaml` into the engine container. Create it:

```yaml title="tests/config.yaml"
version: v1

server:
  address: "0.0.0.0:9001"

telemetry:
  logging:
    level: INFO
    format: console

redis:
  local:
    type: SINGLE
    url: "redis:6379"
    timeout: "200ms"

chains:
  public:
    - type: correlation_id
      options:
        header_name: x-request-id
        algorithm: uuidv7
    - type: embedded_rate_limiter
      options:
        domain: standalone_public
        algorithm: sliding_window_counter
        redis_service: local
        response_headers:
          enabled: true
        descriptors:
          - entries:
              - key: client_ip
            limit: 5
            unit: minute

  internal:
    - type: deny
      options:
        status_code: 403
        body: "internal endpoints are not exposed"

router:
  routes:
    - name: internal
      target_chain: internal
      matches:
        - path_prefix: /internal
    - name: api
      target_chain: public
      matches:
        - path_prefix: /api
  default_chain: public
```

The engine validates the file when it loads it: `version` must be `v1`, every regex must compile and every `target_chain` and `default_chain` must name a chain under `chains`. See [Engine configuration](../reference/engine-configuration.md) for every field.

## 2. Start the stack

```bash
cd tests
docker compose up --build -d
docker compose logs -f hyper-engine
```

The engine logs one `Compiled filter chain` line per chain and then `Starting ext_proc gRPC Server`. Envoy waits for the engine's container health check before it starts.

## 3. Send requests

```bash
curl -si http://localhost:8080/api/hello | grep -i -E '^(HTTP|ratelimit|x-request-id)'
curl -si http://localhost:8080/internal/metrics | head -1   # HTTP/1.1 403 Forbidden
```

The echo backend returns the request it received, so you can see the `x-request-id` header the engine added upstream:

```bash
curl -s http://localhost:8080/api/hello | jq '.request.headers["x-request-id"]'
```

Repeat the first request six times within a minute: the sixth returns `429 Too Many Requests`.

:::note Client IP in this setup
`tests/envoy.yaml` does not set `request_attributes: ["source.address"]` on the ext_proc filter and does not enable `use_remote_address`, so the engine only sees an `X-Forwarded-For` header if the client sends one. Without either, the resolved client IP is empty and the `client_ip` descriptor falls back to the value `default`, so all clients share one bucket. To limit per client, add the attribute to the ext_proc filter:

```yaml
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    request_attributes:
      - source.address
    # ... the rest of the filter unchanged
```

See [Client IP](../concepts/client-ip.md).
:::

## 4. Change the configuration

With the `FILE` provider the engine watches the directory that contains the config file and reloads when the file is written, created, renamed or removed. A new configuration is compiled completely before it replaces the running one; if it is invalid, the engine logs `Config reload rejected, keeping previous policy` and continues with the old policy.

Docker pins a single-file bind mount to the file's inode. Editors that save by writing a new file and renaming it over the old one are then not visible inside the container. To exercise hot reload, mount the directory instead of the file, or write in place (`cat new.yaml > config.yaml`). Otherwise restart the engine:

```bash
docker compose restart hyper-engine
```

## Run the binary directly

```bash
go build -o hyper-engine ./cmd/engine
./hyper-engine -config ./config.yaml
```

The config path is taken from, in order: the `-config` or `-c` flag, the `CONFIG_FILE_PATH` environment variable, and the default `/etc/hyper-engine/config.yaml`. The engine listens for ext_proc on `server.address` (default `:9001`) and serves `/healthz` and `/readyz` on `server.health_address` (default `:9003`):

```bash
curl -s localhost:9003/readyz   # "ready" once the policy is loaded and gRPC is serving
```

Point an Envoy `ext_proc` filter at the gRPC address. The cluster must use HTTP/2, as `tests/envoy.yaml` does with `explicit_http_config.http2_protocol_options`. See [Envoy configuration](../reference/envoy-configuration.md).

## Tear down

```bash
docker compose down
```
