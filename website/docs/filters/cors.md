---
sidebar_position: 7.5
title: CORS (cross-origin)
description: Answer CORS preflights at the gateway and add Access-Control headers so browser apps on other origins can call your API.
---

import Icon from '@site/static/img/icons/cors.svg';

# <Icon className="hg-icon" aria-hidden="true" /> CORS (cross-origin requests)

Browsers only let a web page read responses from another origin when the response carries the right `Access-Control-*` headers. Before "non-simple" requests (JSON bodies, `Authorization` headers, `PUT`/`DELETE`, ...) they also send a **preflight** `OPTIONS` request. The CORS filter handles both at the gateway, so your services don't have to implement CORS and every route behaves the same way.

| At a glance | |
| --- | --- |
| CRD kind | `CorsFilter` |
| Engine filter type | `cors` |
| Runs in phase | request headers (decision), response headers (`Vary` merge) |
| Preflight | answered by the engine: `204` for an allowed origin, `403` otherwise |
| Disallowed origin, actual request | forwarded without CORS headers, or `403` with `blockDisallowedOrigins` |

## Example

```yaml
apiVersion: hyper.io/v1alpha1
kind: CorsFilter
metadata:
  name: web-app
spec:
  allowOrigins:
    - https://app.example.com
  allowOriginRegex:
    - 'https://[a-z0-9-]+\.preview\.example\.com'
  allowMethods: [GET, POST, PUT, DELETE]
  allowHeaders: [Authorization, Content-Type]
  exposeHeaders: [X-Request-Id, RateLimit-Remaining]
  allowCredentials: true
  maxAge: 600
---
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: public-api
spec:
  filters:
    - kind: CorsFilter        # first, so preflights never reach authentication
      name: web-app
    - kind: JwtAuthFilter
      name: auth0
    - kind: RateLimitFilter
      name: per-user-limits
```

Equivalent engine configuration:

```yaml
chains:
  public-api:
    - type: cors
      options:
        allow_origins: ["https://app.example.com"]
        allow_origin_regex: ['https://[a-z0-9-]+\.preview\.example\.com']
        allow_methods: [GET, POST, PUT, DELETE]
        allow_headers: [Authorization, Content-Type]
        expose_headers: [X-Request-Id, RateLimit-Remaining]
        allow_credentials: true
        max_age: 600
    - type: jwt_auth
      options: { jwks_endpoint: "https://example.eu.auth0.com/.well-known/jwks.json" }
```

:::tip Put CORS first
Browsers send preflights **without** cookies or `Authorization`. If an authentication filter ran first it would answer the preflight with `401`, and the browser would never send the real request. With CORS first, the preflight is answered before authentication runs, and the CORS headers are also attached to any `401`, `403` or `429` that later filters produce, so the page can read those errors.
:::

## Behaviour

Requests without an `Origin` header (server-to-server calls, curl) are not affected.

**Preflight**: an `OPTIONS` request with `Origin` and `Access-Control-Request-Method`:

| Origin | Response (from the engine; nothing is forwarded upstream) |
| --- | --- |
| allowed | `204` with `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers` (if configured), `Access-Control-Max-Age` (if `maxAge > 0`), `Access-Control-Allow-Credentials` (if enabled) and `Vary: Origin` (unless `allowOrigins` is `"*"`) |
| not allowed | `403 CORS origin not allowed`, without CORS headers |

The requested method and headers are not validated by the engine: the browser compares them with the returned lists and refuses to send the request if they are not covered. Use `"*"` in `allowMethods` / `allowHeaders` to echo whatever the browser asked for.

**Actual request**: any other request with an `Origin` header:

- **Allowed origin**: the request continues through the chain. The response (from the upstream or from a later filter) gets `Access-Control-Allow-Origin`, plus `Access-Control-Expose-Headers` (if configured), `Access-Control-Allow-Credentials` (if enabled) and `Vary: Origin` (unless `"*"`). An upstream `Vary` value is kept and `Origin` is added to it.
- **Disallowed origin**: by default the request is forwarded without CORS headers, so the browser hides the response from the page. With `blockDisallowedOrigins: true` the engine answers `403` instead, so the upstream never handles requests from foreign origins.

**Which origin is returned**:

| Configuration | `Access-Control-Allow-Origin` | `Vary: Origin` |
| --- | --- | --- |
| `allowOrigins: ["*"]` (no credentials) | `*` | no |
| explicit origins or regex | the request's own origin | yes |

Origins are compared case-insensitively, and a trailing `/` in the configuration is ignored. `allowOriginRegex` patterns use RE2 syntax and must match the **whole** origin, so `https://.*\.example\.com` does not match `https://x.example.com.evil.io`.

:::warning Credentials
`allowCredentials: true` lets the browser send cookies and `Authorization` headers to your API from the listed origins. It cannot be combined with `allowOrigins: ["*"]`: the CORS specification forbids it, and reflecting every origin with credentials would let any website make authenticated requests on your users' behalf. Hypergate rejects that combination (the CRD and the engine both refuse it).
:::

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `allowOrigins` | `allow_origins` | list | — | Exact origins (`scheme://host[:port]`) or `"*"`. |
| `allowOriginRegex` | `allow_origin_regex` | list | — | RE2 patterns matched against the whole origin. At least one of these two fields is required. |
| `allowMethods` | `allow_methods` | list | `GET, HEAD, POST` | Methods returned to preflights; `"*"` echoes the requested method. |
| `allowHeaders` | `allow_headers` | list | — | Request headers returned to preflights; `"*"` echoes the requested headers. |
| `exposeHeaders` | `expose_headers` | list | — | Response headers that page scripts may read. |
| `allowCredentials` | `allow_credentials` | bool | `false` | Allow cookies and `Authorization`; not allowed with `"*"`. |
| `maxAge` | `max_age` | int (seconds) | `0` (not sent) | How long browsers may cache a preflight result. Browsers cap this (Chromium: 2 hours). |
| `blockDisallowedOrigins` | `block_disallowed_origins` | bool | `false` | Answer `403` to actual requests from origins that are not allowed. |

An invalid regex, a missing origin list, a negative `maxAge` or credentials combined with `"*"` stop the filter from loading, and the previous policy stays active (see [hot reload](../concepts/hot-reload.md)).
