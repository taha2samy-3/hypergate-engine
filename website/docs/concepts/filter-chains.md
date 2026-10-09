---
sidebar_position: 3
title: Filter chains
description: What a chain is, how its filters run and pass data to each other, how to order them, and a complete example for the engine and the operator.
---

# Filter chains

A **chain** is a named, ordered list of filters. [Routing](./routing.md) picks exactly one chain per request (or none), and the engine runs that chain's filters in order on every [phase](./request-lifecycle.md) of the request. Chains are where the policy lives: authentication, rate limits, CORS, header rules and so on.

![A request runs through the web-api chain: cors, correlation_id, jwt_auth, redis_metadata_enricher, embedded_rate_limiter and header_modifier. Headers written by earlier filters are read by later ones. A block ends the chain with an immediate response; response-phase work is applied when the upstream answers.](/img/diagrams/chain-execution.svg)

## How a chain runs

- **One chain per request.** The chain is chosen on the first message of the ext_proc stream and kept for the whole request, against the policy snapshot the stream started with.
- **In order.** Filters run in the order they are listed. Each filter runs only in the phases it handles (most run on the request headers; see [where filters run](./request-lifecycle.md#where-filters-run)).
- **Shared request context.** A filter sees what earlier filters did: a header set by `jwt_auth` (`claim_mappings`), `api_key` (`output_mappings`), `redis_metadata_enricher` or `header_modifier` is visible to later filters, and a header removed earlier reads as absent. This is how filters feed each other.
- **First block wins.** When a filter blocks (a `401` from `jwt_auth`, a `429` from the rate limiter, a `403` from `deny`), the chain stops there and the engine answers with an immediate response. Later filters do not run, and the upstream is not called. Headers queued so far, such as `RateLimit-*` or CORS headers, are included.
- **Errors fail closed.** If a filter fails internally (for example Redis is down and the rate-limit descriptor is not `fail_open`), the chain stops and the request gets `500`. See [Failure modes](./failure-modes.md).
- **Empty chains are allowed.** `[]` lets requests through and still counts as a match, which is useful as an explicit "no policy" choice.
- **Shared instances.** Identical filter definitions (same type, options and Redis service) share one instance across chains and across reloads, so listing the same rate limiter in two chains shares its counters.

## Architecture

![Loading: config.yaml is parsed and validated, the policy manager builds Redis clients and shared filter instances all or nothing, and the routes and chains become one snapshot the registry swaps in. Serving: each stream acquires the snapshot, the router picks a chain, the executor runs it per phase, and the stream releases the snapshot.](/img/diagrams/chain-architecture.svg)

Chains are compiled once per configuration, not per request:

1. **Parse and validate.** The engine rejects a configuration whose routes or default chains name a chain that does not exist, or whose regexes or selectors do not compile.
2. **Build.** The policy manager creates the Redis clients and one filter instance per distinct definition (type, options and Redis client). Chains that list the same definition share the instance. Unchanged definitions reuse the instance from the previous configuration, so L1 caches, JWKS key sets and sidecar connections stay warm across reloads. If any filter fails to build, nothing is published and the previous policy keeps serving.
3. **Publish.** The routes and the compiled chains become one immutable, reference-counted **snapshot**, swapped in as a unit, so a request can never be routed to a chain that is not compiled.
4. **Serve.** Each ext_proc stream acquires the current snapshot and keeps it until the request ends, even if a reload happens meanwhile. The router picks one chain, and the executor runs it phase by phase, skipping filters that do not handle the phase and stopping at the first block.
5. **Retire.** When the last stream holding an old snapshot ends, the filters and Redis clients that only the old snapshot used are closed.

See [Hot reload](./hot-reload.md) for how new configurations arrive.

## Ordering filters

Put cheap, broad decisions first and expensive or identity-dependent ones later:

| Position | Filter | Why there |
| --- | --- | --- |
| 1 | `cors` | Browser preflights are answered before authentication, which they cannot pass. |
| 2 | `correlation_id` | Every later log line and every error response carries the ID. |
| 3 | `deny`, `firewall` | Reject known-bad traffic before spending work on it. |
| 4 | `api_key`, `jwt_auth`, `external_auth` | Establish who is calling and write it into headers. |
| 5 | `redis_metadata_enricher` | Look up data about that caller (plan, tenant). |
| 6 | `embedded_rate_limiter` | Limit per user, plan or tenant using the headers above. |
| 7 | `header_modifier` | Final header clean-up for the upstream and the client. |

A rate limiter placed **before** authentication can only limit by IP or by headers the client sends, which the client controls.

## Example

The example below protects an API with three chains and three routes.

![The example: three routes select three chains. /admin uses the admin chain (deny unless internal, then JWT), /api/ uses web-api (cors, correlation, JWT, enricher, rate limit, header modifier), everything else uses the empty public chain. With the operator, HyperRoutes point to HyperChains, which list filter resources by kind and name.](/img/diagrams/chain-example.svg)

| Route | Matches | Chain | What happens |
| --- | --- | --- | --- |
| `admin` | `/admin…` | `admin` | `404` unless the request carries `x-internal: "true"`, then a valid JWT is required. |
| `api` | `/api/…` | `web-api` | CORS, a correlation ID, JWT validation (the `sub` claim becomes `x-user-id`), a Redis lookup of the user's tier, a per-user limit by tier, and security headers. |
| (none) | everything else | `public` | Passes without policy. |

Data flows through the `web-api` chain like this: `jwt_auth` writes `x-user-id` → `redis_metadata_enricher` reads it to build the Redis key `user_meta:<id>` and writes `x-user-tier` → `embedded_rate_limiter` limits per `user` and per `tier` (600/minute for `premium`, 60/minute otherwise).

### Engine configuration

```yaml title="config.yaml"
version: v1

redis:
  main:
    type: SINGLE
    url: "redis.data.svc:6379"

chains:
  web-api:
    - type: cors
      options:
        allow_origins: ["https://app.example.com"]
        allow_methods: [GET, POST, PUT, DELETE]
        allow_headers: [Authorization, Content-Type]
        expose_headers: [X-Request-Id, RateLimit-Remaining]
        max_age: 600
    - type: correlation_id
      options:
        header_name: x-request-id
        algorithm: uuidv7
        mode: if_missing
    - type: jwt_auth
      options:
        jwks_endpoint: "https://example.eu.auth0.com/.well-known/jwks.json"
        issuer: "https://example.eu.auth0.com/"
        audience: "https://api.example.com"
        claim_mappings:
          sub: x-user-id
        strip_token: true
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
    - type: embedded_rate_limiter
      options:
        domain: web-api
        algorithm: sliding_window_counter
        redis_service: main
        header_mappings:
          user: x-user-id
          tier: x-user-tier
        response_headers:
          enabled: true
        descriptors:
          - entries: [{key: user}, {key: tier, value: premium}]
            limit: 600
            unit: minute
          - entries: [{key: user}, {key: tier}]
            limit: 60
            unit: minute
    - type: header_modifier
      options:
        upstream:
          remove: [x-internal-debug]
        downstream:
          add:
            x-content-type-options: nosniff
          remove: [server]

  admin:
    - type: deny
      options:
        status_code: 404
        body: Not Found
        match:
          not_headers:
            x-internal: "true"
    - type: jwt_auth
      options:
        jwks_endpoint: "https://example.eu.auth0.com/.well-known/jwks.json"
        issuer: "https://example.eu.auth0.com/"
        audience: "https://admin.example.com"

  public: []

router:
  routes:
    - name: admin
      target_chain: admin
      matches:
        - path_prefix: /admin
    - name: api
      target_chain: web-api
      matches:
        - path_prefix: /api/
  default_chain: public
```

The two `jwt_auth` entries differ (different `audience`), so they are two instances. Had they been identical, both chains would share one.

### With the operator

The same policy as Kubernetes resources. Each filter is its own resource, a `HyperChain` lists filters by kind and name in order, a `HyperRoute` points at a chain, and the `HyperConfig` names the default chain. The operator compiles them into the configuration above.

```yaml title="crds.yaml"
apiVersion: hyper.io/v1alpha1
kind: HyperRedis
metadata:
  name: main
spec:
  type: SINGLE
  url: "redis.data.svc:6379"
---
apiVersion: hyper.io/v1alpha1
kind: CorsFilter
metadata:
  name: web-app
spec:
  allowOrigins: ["https://app.example.com"]
  allowMethods: [GET, POST, PUT, DELETE]
  allowHeaders: [Authorization, Content-Type]
  exposeHeaders: [X-Request-Id, RateLimit-Remaining]
  maxAge: 600
---
apiVersion: hyper.io/v1alpha1
kind: CorrelationIdFilter
metadata:
  name: request-id
spec:
  headerName: x-request-id
  algorithm: uuidv7
  mode: if_missing
  propagateToUpstream: true
  propagateToDownstream: true
---
apiVersion: hyper.io/v1alpha1
kind: JwtAuthFilter
metadata:
  name: users
spec:
  jwksEndpoint: "https://example.eu.auth0.com/.well-known/jwks.json"
  issuer: "https://example.eu.auth0.com/"
  audience: "https://api.example.com"
  claimMappings:
    sub: x-user-id
  stripToken: true
---
apiVersion: hyper.io/v1alpha1
kind: RedisMetadataEnricherFilter
metadata:
  name: user-tier
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
---
apiVersion: hyper.io/v1alpha1
kind: RateLimitFilter
metadata:
  name: per-user
spec:
  domain: web-api
  algorithm: sliding_window_counter
  redisService: main
  headerMappings:
    user: x-user-id
    tier: x-user-tier
  responseHeaders:
    enabled: true
  descriptors:
    - entries: [{key: user}, {key: tier, value: premium}]
      limit: 600
      unit: minute
    - entries: [{key: user}, {key: tier}]
      limit: 60
      unit: minute
---
apiVersion: hyper.io/v1alpha1
kind: HeaderModifierFilter
metadata:
  name: security-headers
spec:
  upstream:
    remove: [x-internal-debug]
  downstream:
    add:
      x-content-type-options: nosniff
    remove: [server]
---
apiVersion: hyper.io/v1alpha1
kind: DenyFilter
metadata:
  name: internal-only
spec:
  statusCode: 404
  body: Not Found
  match:
    notHeaders:
      x-internal: "true"
---
apiVersion: hyper.io/v1alpha1
kind: JwtAuthFilter
metadata:
  name: admins
spec:
  jwksEndpoint: "https://example.eu.auth0.com/.well-known/jwks.json"
  issuer: "https://example.eu.auth0.com/"
  audience: "https://admin.example.com"
---
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: web-api
spec:
  filters:
    - {kind: CorsFilter, name: web-app}
    - {kind: CorrelationIdFilter, name: request-id}
    - {kind: JwtAuthFilter, name: users}
    - {kind: RedisMetadataEnricherFilter, name: user-tier}
    - {kind: RateLimitFilter, name: per-user}
    - {kind: HeaderModifierFilter, name: security-headers}
---
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: admin
spec:
  filters:
    - {kind: DenyFilter, name: internal-only}
    - {kind: JwtAuthFilter, name: admins}
---
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: public
spec:
  filters: []
---
apiVersion: hyper.io/v1alpha1
kind: HyperRoute
metadata:
  name: admin
spec:
  priority: 200
  targetPolicy: admin
  matches:
    - pathPrefix: /admin
---
apiVersion: hyper.io/v1alpha1
kind: HyperRoute
metadata:
  name: api
spec:
  priority: 100
  targetPolicy: web-api
  matches:
    - pathPrefix: /api/
---
apiVersion: hyper.io/v1alpha1
kind: HyperConfig
metadata:
  name: main
spec:
  targetNamespace: hyper-system
  defaultChain: public
```

With webhooks enabled, creating a `HyperChain` fails while a filter it lists does not exist, so apply the filters before the chains (the file above is in that order, and `kubectl apply -f` applies it top to bottom). A filter cannot be deleted while a chain uses it.

Check the result:

```bash
kubectl get hyperchains          # State: Ready, or Degraded with the reason
kubectl get hyperroutes          # State: Ready, or Invalid with the reason
kubectl -n hyper-system get configmap hyper-engine-config -o jsonpath='{.data.config\.yaml}'
```

A chain whose filter cannot be compiled becomes `Degraded` and is replaced by a `503` deny chain, so its routes fail closed instead of losing their policy. See [Routing: with the operator](./routing.md#with-the-operator).

## Audit mode

Audit mode tries a policy on live traffic without enforcing it, like Envoy RBAC's shadow rules or Istio's `AUDIT` action. An audited filter runs normally, but when it would deny, or fails internally:

- the decision is counted in `hypergate_audit_denies_total{chain, filter, status}` and logged;
- the request continues to the next filter, and any header changes the filter made for its denial (such as JWT's `content-type: application/json`) are rolled back;
- answers that are not denials, such as a CORS preflight `204`, are still sent.

Side effects remain: an audited rate limiter still counts requests (that is how you see what would be limited), and an audited external auth filter still calls its sidecar.

```yaml title="engine configuration"
chains:
  web-api:
    - {type: cors, options: {allow_origins: ["https://app.example.com"]}}
    - {name: new-jwt, type: jwt_auth, audit: true, options: {jwks_endpoint: "https://example.eu.auth0.com/.well-known/jwks.json"}}
```

With the operator, set `mode: Audit` on a `HyperChain` to audit all its filters, and use `audit: true` or `audit: false` on a filter reference to override that for one filter:

```yaml title="HyperChain in audit mode"
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: web-api
spec:
  mode: Audit
  filters:
    - {kind: CorsFilter, name: web-app, audit: false}   # keep enforcing CORS
    - {kind: JwtAuthFilter, name: users}                 # audited
```

When `hypergate_audit_denies_total` stays at zero for the traffic you expect to pass, switch the chain to `Enforce` (the default).

## Chain limits

All workloads on a node share one engine, so a chain with slow filters (a WAF inspecting bodies, a remote auth call) could slow down the others. Two limits keep each chain in its lane:

| Setting | Effect | When it is hit |
| --- | --- | --- |
| `timeout` | A deadline on the chain's work for each ext_proc message. Filters that call Redis or a sidecar stop at the deadline. | `503`, or with `on_timeout: allow` the request continues without the remaining filters |
| `max_concurrency` | How many requests may run this chain's filters at the same time on one engine. Held only while the filters run, not while the upstream answers. | `503` with `Retry-After: 1`, or with `on_overload: allow` the chain is skipped for that request |

Both are counted in `hypergate_chain_rejections_total{chain, reason}`. Keep `timeout` below Envoy's ext_proc `message_timeout` (2.5 s in the recommended configuration), otherwise Envoy gives up first. A filter that never checks its context (pure CPU work) is not interrupted by the deadline. `allow` trades protection for availability: the request then passes without the rest of the chain, so prefer the default `deny` for security filters.

```yaml title="engine configuration"
chain_settings:
  web-api:
    timeout: 300ms
    max_concurrency: 200
```

```yaml title="HyperChain with limits"
spec:
  timeout: 300ms
  maxConcurrency: 200
  onTimeout: Deny      # default
  onOverload: Deny     # default
```

## See also

- [Request lifecycle](./request-lifecycle.md): phases, mutations and immediate responses.
- [Engine configuration: chains](../reference/engine-configuration.md#chains) and [CRD reference: HyperChain](../reference/crd-reference.md#hyperchain).
- Each filter's page under **Filters** for its options.
