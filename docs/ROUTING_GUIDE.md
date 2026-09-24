# Routing guide

The routing documentation lives on the documentation site:

- Published: https://taha2samy-3.github.io/hypergate-engine/docs/concepts/routing
- Source: [`website/docs/concepts/routing.md`](../website/docs/concepts/routing.md)

## Summary

- The engine picks one filter chain per request, on the request headers, from `router.routes`. Routes are evaluated in order and the first match wins. With the operator, HyperRoutes are ordered by `spec.priority`, highest first.
- A route's `matches` entries are alternatives (OR). Inside one entry every set field must hold (AND): `path_prefix` and `path_regex_pattern` test the raw `:path` including the query string; `headers` require an exact value, `"*"` for presence, or a `regex_pattern`. Write header names in lower case in engine YAML.
- No match: `router.default_chain` runs. No match and no default chain: the request passes without policy.
- Fail closed: a route or default chain that points at a chain that is not loaded returns `503`, and so does a request arriving before any policy is loaded. A filter's internal error returns `500`.
- The engine rejects configurations whose routes or default chain reference undefined chains, or whose regexes do not compile; on reload the previous policy keeps serving.
- The operator compiles a HyperChain that fails to resolve (`Degraded`), and any missing chain named by a HyperRoute or `defaultChain`, into a deny chain that returns `503`. HyperRoutes with an empty `targetPolicy` or an invalid regex are skipped and logged, so their traffic falls through to other routes.

See also the engine configuration reference: [`website/docs/reference/engine-configuration.md`](../website/docs/reference/engine-configuration.md#router).
