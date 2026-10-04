---
sidebar_position: 4
title: Routing
description: How a request is matched to a filter chain, the default chain, and what fails closed.
---

# Routing

Envoy decides which upstream a request goes to. The engine separately decides which **filter chain** applies to it. Engine routing happens once per request, on the request headers, against the snapshot the stream started with.

![Routing decision flow: policy loaded, route matched, chain loaded, default chain, pass-through](/img/diagrams/routing.svg)

## Matching order

Routes are evaluated in the order they appear in `router.routes`. The first route with a matching entry wins, and its `target_chain` runs. Later routes are not evaluated.

With the operator, the order comes from `HyperRoute.spec.priority`: the compiler sorts routes by priority, highest first. Routes with equal priority have no guaranteed relative order, so give overlapping routes distinct priorities.

```yaml
router:
  routes:
    - name: admin              # evaluated first
      target_chain: admin
      matches:
        - path_prefix: /admin
    - name: api
      target_chain: public-api
      matches:
        - path_prefix: /api/v2
          headers:
            x-tenant: "*"
        - path_regex_pattern: "^/api/v1/(orders|invoices)"
  default_chain: public
```

## Match semantics

- A route's `matches` list holds alternatives: **any** entry that matches selects the route.
- Inside one entry, **every** field that is set must match.
- `path_prefix` is a plain string prefix test on the raw `:path` pseudo-header. The raw path includes the query string, so `/api?x=1` matches the prefix `/api`.
- `path_regex_pattern` is a Go (RE2) regular expression tested with `MatchString` on the same raw path. It is not anchored: add `^` and `$` yourself.
- `headers` maps a header name to a condition:
  - a plain string, or `exact: <value>`, requires that exact value;
  - `"*"` (plain or as `exact`) only requires that the header is present;
  - `regex_pattern: <re>` requires the value to match the expression;
  - `exact` and `regex_pattern` may both be set, and both must hold.
- Header names are case-insensitive: the engine lower-cases both incoming header names and route header names (the operator does the same for HyperRoutes). Header values are compared exactly.
- Repeated request headers are joined with `, ` before matching (cookies with `; `).
- An entry with no fields matches every request.

```yaml
matches:
  - headers:
      x-api-version: "2"                 # shorthand for exact
      x-canary: "*"                      # present, any value
      user-agent:
        regex_pattern: "^curl/"
```

HyperRoute `matches[].headers` only supports exact values (and `"*"`). Use the engine configuration directly if you need header regexes.

## Traffic class

Every request is either **north-south** (it entered the cluster through a gateway) or **east-west** (one workload calling another). The engine reads the class from the `x-hypergate-traffic` gRPC metadata that Envoy sends on the ext_proc stream (`north-south` or `east-west`). The metadata is part of the Envoy filter configuration, not of the request, so clients cannot set it. See [Envoy configuration](../reference/envoy-configuration.md#traffic-class-metadata).

Without the metadata, or with an invalid value, the engine infers the class: with [workload identity](#workload-selectors) enabled, a caller that is a pod in the identity map is east-west and anything else north-south; without identity every such request counts as north-south.

A match entry with `traffic: north_south` or `traffic: east_west` applies only to that class; `any` (the default) applies to both.

## Sources and destinations

`sources` and `destinations` restrict a match entry by who calls and what is called. Each is a list of alternatives in the form `prefix:value`:

```yaml
router:
  routes:
    - name: partner-api
      target_chain: partner
      matches:
        - traffic: north_south
          sources: ["cidr:203.0.113.0/24", "ip:192.0.2.7"]
          destinations: ["host:api.example.com"]
    - name: internal-ledger
      target_chain: internal-strict
      matches:
        - traffic: east_west
          sources: ["cidr:10.244.0.0/16"]
          destinations: ["ip:10.96.0.20"]
          path_prefix: /v1/charges
  default_chains:
    north_south: public
    east_west: internal
```

- `ip:` and `cidr:` on the source side match the resolved [client IP](./client-ip.md).
- `ip:` and `cidr:` on the destination side match Envoy's `destination.address` attribute: the address the connection was made to. For a call Envoy intercepts transparently, that is the original destination, such as a Service ClusterIP. On a gateway it is the gateway's own listener address. Add `destination.address` to the filter's `request_attributes`; without it, destination `ip:`/`cidr:` selectors never match.
- `host:` matches the `:authority` header without port. The client chooses this value, so use it to select a virtual host, not as an identity.
- `any` always matches. An omitted list puts no constraint on that side.

The full selector list is in the [engine configuration reference](../reference/engine-configuration.md#selectors).

## Workload selectors

With [workload identity](../reference/engine-configuration.md#identity) enabled, routes can name callers and targets by what they are rather than by address:

![The engine looks up the caller's IP in the identity map to get its namespace, ServiceAccount, SPIFFE ID, labels and Services, resolves the destination Service from the destination address or a cluster-local authority, settles the traffic class, applies unknown_source, and then matches routes.](/img/diagrams/identity-routing.svg)

```yaml
identity:
  enabled: true
  address: hyper-operator-identity.hyper-system.svc:9444
  ca_file: /etc/hypergate/identity/ca.crt
router:
  routes:
    - name: checkout-to-ledger
      target_chain: internal-strict
      matches:
        - traffic: east_west
          sources: ["service:shop/checkout", "sa:shop/checkout"]
          destinations: ["service:payments/ledger"]
          path_prefix: /v1/charges
    - name: partners
      target_chain: partner
      matches:
        - sources: [external]
          destinations: ["host:api.example.com"]
  default_chains:
    north_south: public
    east_west: deny-unlisted
  unknown_source: deny
```

- The caller is looked up by its [client IP](./client-ip.md). Selectors by workload therefore trust the source address, which holds where the CNI prevents spoofing and nothing SNATs traffic inside the cluster.
- `labels:` sees only the labels on the operator's allow-list (`operator.identity.labelKeys`).
- Node addresses (hostNetwork traffic) are known, but are not pods: they match no workload selector and are not `external`.

### Destination Service

`service:` and `namespace:` on the destination side use the Service the request goes to, found in this order:

1. `destination.address` is a Service cluster IP → that Service.
2. `destination.address` is a pod in the identity map (the CNI already picked a backend, as Cilium does) → that pod's Services.
3. The `:authority` is a cluster-local name, `<svc>.<ns>`, `<svc>.<ns>.svc` or `<svc>.<ns>.svc.cluster.local`, of a Service that exists → that Service.

### Unknown callers

An east-west request whose caller is not in the identity map, for example a pod created a moment ago or traffic during an operator leader change, follows `router.unknown_source` (`HyperConfig.spec.unknownSource`):

| Value | Behaviour |
| --- | --- |
| `deny` (default) | `503 Service Unavailable`, logged with the client IP. |
| `default` | Routing continues; only `ip:`, `cidr:` and `any` can match the caller, and `external` does not. |

## Default chain

When no route matches, the engine runs the chain for the request's [traffic class](#traffic-class) from `router.default_chains` (`north_south` or `east_west`). If that is not set it runs `router.default_chain` (`HyperConfig.spec.defaultChain` with the operator). The legacy key `router.other` is still accepted and used when `default_chain` is empty.

When no route matches **and** no default chain is configured, the request passes through without policy. Set a default chain if every request must be subject to some policy, even an empty chain `[]` combined with explicit routes for everything else.

## Validation

The engine rejects a configuration, at start-up or on reload, when:

- a route has no `target_chain`,
- a route's `target_chain`, `default_chain`, a `default_chains` entry or `other` names a chain that is not defined under `chains`,
- a `path_regex_pattern` or header `regex_pattern` does not compile,
- a `traffic` value is invalid, or a selector has an unknown prefix, a malformed value, or a prefix that is not allowed on its side (for example `host:` in `sources`),
- a workload selector is used without workload identity.

A rejected reload leaves the previous policy in place. See [Hot reload](./hot-reload.md).

## Fail-closed behaviour

| Situation | Result |
| --- | --- |
| A route matches and its chain is loaded | The chain runs. |
| A route matches but its chain is not in the loaded snapshot | `503 Service Unavailable` |
| No route matches, a `default_chains` entry for the class is set | That chain runs. |
| No route matches, `default_chain` is set and loaded | The default chain runs. |
| No route matches, `default_chain` is set but not loaded | `503 Service Unavailable` |
| No route matches and no default chain is set | The request passes through without policy. |
| No policy has been loaded yet | `503 Service Unavailable` |
| A filter in the chain fails internally | `500 Internal Server Error` |
| The chain is empty (`[]`) | The request passes through; the route still counts as matched. |

Because the engine validates chain references and publishes routes and chains together, "route to a chain that is not loaded" should not occur with a configuration the engine accepted; the `503` is a safety net.

### With the operator

![Operator routing: the developer writes HTTPRoutes, HyperRoutes, HyperChains and HyperConfigs; the operator validates and compiles them into the engine ConfigMap and status, and never writes HTTPRoutes](/img/diagrams/operator-routing.svg)

HyperRoutes carry the same fields as engine routes (`traffic`, `sources`, `destinations`, `pathPrefix`, `pathRegexPattern`, `headers`), and `HyperConfig.spec.defaultChains` maps to `router.default_chains`. Gateway API `HTTPRoute`s stay under the developer's control: the operator never creates or edits them.

The operator adds its own fail-closed rules when it compiles CRDs:

| Situation | What the operator compiles |
| --- | --- |
| A HyperChain references a filter that does not exist, or a filter cannot be translated | The chain keeps its name but becomes a single `deny` filter returning `503 Service Unavailable`. The HyperChain's `status.state` is `Degraded` and `status.message` says why. |
| A HyperRoute's `targetPolicy` names a HyperChain that does not exist | A `503` deny chain under that name is added, so matching requests are rejected. |
| `HyperConfig.spec.defaultChain` names a HyperChain that does not exist | A `503` deny chain under that name is added for that namespace. |
| A HyperRoute has an empty `targetPolicy`, an invalid `pathRegexPattern`, `traffic` or selector | The route is skipped, its `status.state` is `Invalid` with the reason in `status.message`, and the operator logs an error. Its requests fall through to later routes or the default chain. With webhooks enabled such a HyperRoute is rejected at admission. |

Skipped routes are the one case where a mistake widens access rather than narrowing it, because the traffic falls through to other routes. Check `kubectl get hyperroutes` (the `State` column) or watch the operator logs for `HyperRoute is invalid, skipping`.
