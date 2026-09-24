---
sidebar_position: 3
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
- Header names must be written in **lower case** in the engine configuration: the engine lower-cases incoming header names but compares route header names as written. (The operator lower-cases HyperRoute header names for you.)
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

## Default chain

When no route matches, the engine runs `router.default_chain` (`HyperConfig.spec.defaultChain` with the operator). The legacy key `router.other` is still accepted and used when `default_chain` is empty.

When no route matches **and** no default chain is configured, the request passes through without policy. Set a default chain if every request must be subject to some policy, even an empty chain `[]` combined with explicit routes for everything else.

## Validation

The engine rejects a configuration, at start-up or on reload, when:

- a route has no `target_chain`,
- a route's `target_chain`, `default_chain` or `other` names a chain that is not defined under `chains`,
- a `path_regex_pattern` or header `regex_pattern` does not compile.

A rejected reload leaves the previous policy in place. See [Hot reload](./hot-reload.md).

## Fail-closed behaviour

| Situation | Result |
| --- | --- |
| A route matches and its chain is loaded | The chain runs. |
| A route matches but its chain is not in the loaded snapshot | `503 Service Unavailable` |
| No route matches, `default_chain` is set and loaded | The default chain runs. |
| No route matches, `default_chain` is set but not loaded | `503 Service Unavailable` |
| No route matches and no default chain is set | The request passes through without policy. |
| No policy has been loaded yet | `503 Service Unavailable` |
| A filter in the chain fails internally | `500 Internal Server Error` |
| The chain is empty (`[]`) | The request passes through; the route still counts as matched. |

Because the engine validates chain references and publishes routes and chains together, "route to a chain that is not loaded" should not occur with a configuration the engine accepted; the `503` is a safety net.

### With the operator

The operator adds its own fail-closed rules when it compiles CRDs:

| Situation | What the operator compiles |
| --- | --- |
| A HyperChain references a filter that does not exist, or a filter cannot be translated | The chain keeps its name but becomes a single `deny` filter returning `503 Service Unavailable`. The HyperChain's `status.state` is `Degraded` and `status.message` says why. |
| A HyperRoute's `targetPolicy` names a HyperChain that does not exist | A `503` deny chain under that name is added, so matching requests are rejected. |
| `HyperConfig.spec.defaultChain` names a HyperChain that does not exist | A `503` deny chain under that name is added for that namespace. |
| A HyperRoute has an empty `targetPolicy` or an invalid `pathRegexPattern` | The route is skipped and an error is logged by the operator. Its requests fall through to later routes or the default chain. |

Skipped routes are the one case where a mistake widens access rather than narrowing it, because the traffic falls through to other routes. Watch the operator logs for `HyperRoute has an invalid regex, skipping` and `HyperRoute has no targetPolicy, skipping`.
