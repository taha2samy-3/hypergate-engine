---
sidebar_position: 3
title: Envoy configuration
description: The ext_proc filter settings Hypergate relies on, with a complete standalone Envoy example.
---

# Envoy configuration

Hypergate is called by Envoy's external processing filter, `envoy.filters.http.ext_proc` (`envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor`). This page lists the settings that matter and why.

## Recommended filter

```yaml
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    grpc_service:
      envoy_grpc:
        cluster_name: hyper_engine
    failure_mode_allow: false
    allow_mode_override: true
    message_timeout: 2.5s
    request_attributes:
      - source.address
    processing_mode:
      request_header_mode: SEND
      response_header_mode: SEND
      request_body_mode: NONE
      response_body_mode: NONE
      request_trailer_mode: SKIP
      response_trailer_mode: SKIP
```

Place it before `envoy.filters.http.router` and after any filter whose effect Hypergate should see.

## Settings

| Setting | Recommended | Why |
| --- | --- | --- |
| `grpc_service` | `envoy_grpc` pointing at an HTTP/2 cluster | The engine speaks gRPC over HTTP/2, plaintext (h2c) unless `server.tls` is enabled. |
| `failure_mode_allow` | `false` | When the engine is unreachable, errors or times out, Envoy fails the request instead of forwarding it without policy. See [Failure modes](../concepts/failure-modes.md). |
| `processing_mode.request_header_mode` | `SEND` | Routing and almost every filter run on request headers. |
| `processing_mode.response_header_mode` | `SEND` | Needed to apply client-facing headers (rate-limit headers, correlation ID, `header_modifier` `downstream`) and for `deny` filters with response header conditions. |
| `processing_mode.request_body_mode` | `NONE` | The engine asks for the body per request when a filter needs it (see `allow_mode_override`). Use `BUFFERED` if you want every body sent without relying on overrides. `STREAMED` is not supported by the built-in filters. |
| `processing_mode.response_body_mode` | `NONE` | No built-in filter inspects response bodies. |
| `processing_mode.request_trailer_mode`, `response_trailer_mode` | `SKIP` | No built-in filter uses trailers. |
| `allow_mode_override` | `true` | Lets the engine switch `request_body_mode` to `BUFFERED` for a single request, which a `firewall` filter with `inspect_body` needs. Without it (and without a static `BUFFERED`), such requests reach the upstream uninspected and the engine replaces the response with `500`. |
| `allowed_override_modes` | unset | If you restrict overrides, allow the mode the engine sends: headers `SEND`, request body `BUFFERED`, response body `NONE`, trailers `SKIP`. |
| `request_attributes` | `["source.address"]` | Sends Envoy's downstream peer address. The engine uses it to resolve the client IP instead of trusting `X-Forwarded-For`. See [Client IP](../concepts/client-ip.md). |
| `message_timeout` | above the slowest filter | Deadline for each ext_proc message. Envoy's default is 200 ms. The sidecar filters default to a `2s` timeout and JWT introspection to `2s`, and a new Redis connection can take up to its dial `timeout`. Either lower those timeouts or raise `message_timeout`. |
| `mutation_rules` | unset | By default Envoy lets an external processor change any header except `host`, `:authority`, `:scheme`, `:method` and `x-envoy-*`. The API key filter rewrites `:path` to strip a key from the query string, so do not set `disallow_system: true` if you rely on that. |

A buffered request body is subject to Envoy's buffer limits; bodies above them are rejected by Envoy before the engine sees them.

Header mutations from the engine do not make Envoy re-select the route, because route selection has already happened when ext_proc runs.

## Complete standalone example

This is `tests/envoy.yaml` from the repository with the client IP attribute, `use_remote_address` and a message timeout added.

```yaml title="envoy.yaml"
static_resources:
  listeners:
    - name: main_listener
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 8080
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress_http
                use_remote_address: true
                http_filters:
                  - name: envoy.filters.http.ext_proc
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
                      grpc_service:
                        envoy_grpc:
                          cluster_name: hyper_engine_cluster
                        timeout: 5s
                      failure_mode_allow: false
                      allow_mode_override: true
                      message_timeout: 2.5s
                      request_attributes:
                        - source.address
                      processing_mode:
                        request_header_mode: SEND
                        response_header_mode: SEND
                        request_body_mode: NONE
                        response_body_mode: NONE
                        request_trailer_mode: SKIP
                        response_trailer_mode: SKIP
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: local_service
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/"
                          route:
                            cluster: upstream_cluster
                            timeout: 10s
  clusters:
    - name: hyper_engine_cluster
      type: STRICT_DNS
      connect_timeout: 5s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: hyper_engine_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: hyper-engine
                      port_value: 9001
    - name: upstream_cluster
      type: STRICT_DNS
      connect_timeout: 5s
      load_assignment:
        cluster_name: upstream_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: backend-service
                      port_value: 80
```

`use_remote_address: true` makes Envoy append the peer it observed to `X-Forwarded-For`. With `source.address` also sent, the engine recognises that entry and does not count it twice.

## Engine cluster details

- **HTTP/2 is required.** In static configuration use `explicit_http_config.http2_protocol_options`. In Kubernetes, the operator's `hyper-engine-svc` sets `appProtocol: kubernetes.io/h2c`, which gateway implementations use to pick HTTP/2 for the backend.
- **TLS.** When the engine has `server.tls.enabled: true`, add an `UpstreamTlsContext` transport socket to the cluster. The engine requires TLS 1.3, and with `mutual_tls: true` Envoy must present a client certificate signed by `server.tls.ca_file`.
- **Keepalive.** The engine rejects client keepalive pings more frequent than every 5 minutes. If you set `http2_protocol_options.connection_keepalive` on the cluster, keep `interval` at 5 minutes or more. The engine itself recycles connections after 30 minutes.
- **Locality.** Run one engine per node (the operator's DaemonSet) and prefer the local endpoint; the operator's Service uses `trafficDistribution: PreferSameNode` for this.

## Skipping ext_proc on some routes

Health checks and static assets often do not need policy. Disable the filter per route with `ExtProcPerRoute`:

```yaml
routes:
  - match:
      prefix: /healthz
    route:
      cluster: upstream_cluster
    typed_per_filter_config:
      envoy.filters.http.ext_proc:
        "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute
        disabled: true
```

Requests on such routes never reach the engine, so no chain, not even the default chain, applies to them.

## Cilium Gateway API

With Cilium, the filter is added through a `CiliumEnvoyConfig` attached to the gateway's service, as shown in the [Kubernetes quickstart](../getting-started/quickstart-kubernetes.md#8-connect-envoy) and in `k8s/demo/06-extproc-config.yaml`. The same settings apply.
