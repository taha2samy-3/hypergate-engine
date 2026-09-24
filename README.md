<p align="center">
  <img src="website/static/img/logo-wordmark.svg" alt="Hypergate" width="320">
</p>

<p align="center">
  A policy engine for <a href="https://www.envoyproxy.io/">Envoy</a>: authentication, rate limiting and traffic rules,
  declared as Kubernetes resources and enforced over the <code>ext_proc</code> gRPC API.
</p>

<p align="center">
  <a href="https://taha2samy-3.github.io/hypergate-engine/">Documentation</a> ·
  <a href="https://taha2samy-3.github.io/hypergate-engine/docs/getting-started/quickstart-kubernetes">Quickstart</a> ·
  <a href="https://taha2samy-3.github.io/hypergate-engine/docs/concepts/architecture">Architecture</a>
</p>

---

Envoy streams every request to the Hypergate engine before forwarding it. The engine matches the request to a
route, runs that route's filter chain and answers with header mutations or an immediate response.

| | |
| --- | --- |
| **Filters** | rate limiting (5 algorithms, Redis-backed), API keys, JWT, external auth and WAF sidecars over Unix sockets, deny rules, header modification, correlation IDs, Redis metadata enrichment |
| **Hot reload** | a new policy is compiled completely before it is published; in-flight requests keep the policy they started with |
| **Fails closed** | a route pointing at a missing or degraded chain is rejected instead of silently skipping its policy |
| **Kubernetes** | an operator compiles filter/chain/route CRDs into the engine config and runs the engine as a DaemonSet with its sidecars |

## Repository layout

| Path | What it is |
| --- | --- |
| `cmd/engine`, `internal/` | The ext_proc engine (Go) |
| `hyper-operator/` | The Kubernetes operator: CRD types, controllers, admission webhooks |
| `charts/` | Helm charts for the operator and for a standalone engine |
| `tests/` | Integration tests (pytest) against Envoy, the engine and Redis in Docker Compose |
| `k8s/` | kind/Cilium setup and demo manifests |
| `website/` | Documentation site (Docusaurus) |

## Development

```bash
go test ./...          # unit tests (engine and operator)
task engine:build      # build the engine
task test:all          # integration tests (Docker Compose + pytest)
task docs:dev          # documentation site on http://localhost:3000/hypergate-engine/
```
