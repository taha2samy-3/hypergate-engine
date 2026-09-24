---
sidebar_position: 1
title: Introduction
description: What Hypergate is, how it fits next to Envoy, and what it can enforce.
---

# Introduction

Hypergate is a policy engine for API gateways built on [Envoy](https://www.envoyproxy.io/). It runs as an Envoy **external processor** (`ext_proc`): for every HTTP request Envoy opens a gRPC stream to the Hypergate engine, sends it the request (and, when asked, the response), and the engine answers with header mutations or an immediate response that ends the request.

Hypergate has two parts:

- **The engine** (`cmd/engine`, Go). It loads a YAML policy, matches each request to a *filter chain* and runs the chain: rate limiting, API keys, JWT validation, external authorization, a firewall hook, conditional denies, header changes, correlation IDs and Redis-backed metadata enrichment.
- **The operator** (`hyper-operator`). It adds cluster-scoped `hyper.io/v1alpha1` custom resources, compiles them into the engine's YAML, writes that YAML to a ConfigMap and runs the engine as a DaemonSet, injecting sidecars for external authorization and firewall filters.

You can use the engine without the operator: it reads the same configuration from a file, a ConfigMap or a URL.

![Hypergate architecture: client, Envoy, engine pod with sidecars, Redis, and the operator control plane](/img/diagrams/architecture.svg)

## How it fits with Envoy

Envoy remains the proxy. It terminates TLS, selects routes and clusters, load-balances and talks to your services. Hypergate only decides what happens to a request on its way through:

1. Envoy's `envoy.filters.http.ext_proc` HTTP filter sends the request headers to the engine.
2. The engine picks a chain with its own router (path prefix, path regex and header matches) and runs the chain's filters.
3. The engine replies with header mutations (added, replaced or removed headers, a rewritten `:path`) or with an `ImmediateResponse` such as `401`, `403`, `429` or `503`.
4. When the upstream answers, Envoy sends the response headers to the engine, which applies queued response headers and can still replace the response.

Because the integration point is standard `ext_proc`, Hypergate works with any Envoy deployment where you can add that filter. The repository includes a Cilium Gateway API example (`k8s/demo/`) and a standalone Envoy example (`tests/envoy.yaml`). See [Envoy configuration](./reference/envoy-configuration.md) for the settings the engine relies on.

## Features

| Filter | Engine type | CRD kind | What it does |
| --- | --- | --- | --- |
| [Rate limiting](/docs/filters/rate-limiting) | `embedded_rate_limiter` | `RateLimitFilter` | Five Redis-backed algorithms, composite descriptors, dynamic cost, shadow mode, `RateLimit-*` headers. |
| [API key](/docs/filters/api-key) | `api_key` | `ApiKeyFilter` | Looks up hashed keys in Redis, checks account status, injects consumer metadata, can strip the key. |
| [JWT auth](/docs/filters/jwt-auth) | `jwt_auth` | `JwtAuthFilter` | Local validation with JWKS or an HMAC secret, optional RFC 7662 introspection, claim-to-header mapping. |
| [External auth](/docs/filters/external-auth) | `external_auth` | `ExternalAuthFilter` | Calls an authorization sidecar over a Unix domain socket (HTTP or Envoy `ext_authz` gRPC). |
| [Firewall](/docs/filters/firewall) | `firewall` | `FirewallFilter` | Sends headers, and optionally the buffered body, to a WAF sidecar over a Unix domain socket. |
| [Deny](/docs/filters/deny) | `deny` | `DenyFilter` | Blocks on path, request header or upstream response header conditions. |
| [Header modifier](/docs/filters/header-modifier) | `header_modifier` | `HeaderModifierFilter` | Sets and removes request headers and client-facing response headers. |
| [Correlation ID](/docs/filters/correlation-id) | `correlation_id` | `CorrelationIdFilter` | Generates or propagates a request ID (UUIDv4, UUIDv7, ULID, XID). |
| [Redis metadata enricher](/docs/filters/redis-metadata-enricher) | `redis_metadata_enricher` | `RedisMetadataEnricherFilter` | Builds a Redis key from request data and maps fields of the stored JSON to headers. |

Platform behaviour that applies to every chain:

- **Routing with a fail-closed default.** Routes are matched in order. A route or default chain that points at a chain that is not loaded returns `503`; an internal filter error returns `500`. See [Routing](./concepts/routing.md) and [Failure modes](./concepts/failure-modes.md).
- **Transactional hot reload.** A new configuration is compiled completely before it is published. If anything fails, the previous policy keeps serving. In-flight requests finish on the policy they started with. See [Hot reload](./concepts/hot-reload.md).
- **Trustworthy client IP.** The client address comes from Envoy's peer address and a configured number of trusted proxy hops, never from a raw `X-Forwarded-For` value. See [Client IP](./concepts/client-ip.md).
- **Sidecars over Unix domain sockets.** Authorization and firewall sidecars run inside the engine pod and are reached through a shared socket directory, without a Service or network hop. See [Architecture](./concepts/architecture.md).
- **Operational endpoints.** `/healthz` and `/readyz` for probes, optional `pprof`, structured logs. See [Operations](./reference/operations.md).

## Where to go next

- New to Hypergate on Kubernetes: [Installation](./getting-started/installation.md), then the [Kubernetes quickstart](./getting-started/quickstart-kubernetes.md).
- Trying the engine locally with Docker Compose: [Standalone quickstart](./getting-started/quickstart-standalone.md).
- Understanding the request path: [Request lifecycle](./concepts/request-lifecycle.md).
- Looking up a field: [Engine configuration](./reference/engine-configuration.md) and [CRD reference](./reference/crd-reference.md).
