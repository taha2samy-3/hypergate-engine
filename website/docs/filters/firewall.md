---
sidebar_position: 5
title: Firewall (WAF)
description: Run a web application firewall such as Coraza as a sidecar and let it inspect request headers and bodies.
---

import Icon from '@site/static/img/icons/firewall.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Firewall (WAF)

The firewall filter hands requests to a WAF running as a sidecar in the engine pod, for example Coraza or ModSecurity with the OWASP Core Rule Set. The WAF sees the request headers and, if you enable body inspection, the buffered request body. If it rejects the request, Hypergate ends it with the WAF's response before anything reaches your service.

Keeping the WAF out of the engine process isolates its memory use and crashes, and lets you update rules and engine versions independently.

| At a glance | |
| --- | --- |
| CRD kind | `FirewallFilter` |
| Engine filter type | `firewall` |
| Runs in phase | request headers, plus request body with `inspectBody` |
| Blocks with | the WAF's status and body (default `403`) |
| If the sidecar is down or times out | `500` (fail closed) |

## Example

```yaml
apiVersion: hyper.io/v1alpha1
kind: FirewallFilter
metadata:
  name: coraza
spec:
  protocol: grpc
  container:
    image: registry.example.com/coraza-extproc:1.2.0
    socketEnvKey: WAF_LISTEN_SOCKET        # receives unix:///var/run/hypergate/fw-coraza.sock
    resources:
      requests: {cpu: 200m, memory: 256Mi}
  rulesConfigMap: coraza-rules             # mounted at /etc/firewall/rules/
  engineRules:
    timeout: 1s
    inspectBody: true
    maxBodySizeKB: 512
```

`rulesConfigMap` and `rulesSecretRef` must exist in the engine's target namespace (default `hyper-system`).

## Protocols

| `protocol` | The sidecar implements | How a verdict is expressed |
| --- | --- | --- |
| `grpc` (default) | Envoy's `ext_proc` API (`envoy.service.ext_proc.v3.ExternalProcessor/Process`), like WAFs built as Envoy external processors | an `ImmediateResponse` blocks (its status, body and headers are returned to the client). Otherwise the header mutations it returns are applied to the upstream request. |
| `http` | a plain HTTP server | The request is **replayed** to the sidecar with its original method, path, query, headers and (with `inspectBody`) body. `2xx` allows, anything else blocks with that status and body. |

In `http` mode the sidecar also receives the `X-Forwarded-*` / `X-Original-*` headers described on the [external authorization](./external-auth.md#what-the-sidecar-receives) page.

## Body inspection

![Where the firewall runs in the ext_proc request lifecycle](/img/diagrams/request-lifecycle.svg)

With `inspectBody: true` each request is inspected **once**:

- A request **without a body** (a `GET`, or anything Envoy marks as end-of-stream at the headers) is inspected straight away, during the headers phase.
- A request **with a body** is not forwarded yet. The engine tells Envoy to buffer the body (a processing-mode override), then calls the WAF with headers and body together. The body sent to the WAF is capped at `maxBodySizeKB`.

This needs one of these settings on Envoy's ext_proc filter:

```yaml
allow_mode_override: true        # lets the engine request the body per request
# or statically:
processing_mode:
  request_body_mode: BUFFERED
```

If neither is set, Envoy ignores the engine's request for the body and forwards the request without inspection. Hypergate detects this when the response comes back and replaces the response with `500`. The upstream has already seen the request by then, so treat this as a misconfiguration alarm, not a safety net. See [Envoy configuration](../reference/envoy-configuration.md).

Envoy buffers the body in memory, subject to its buffer limits (`per_connection_buffer_limit_bytes`, 1 MiB by default). Larger requests are rejected by Envoy with `413`.

## How the sidecar is injected

For every `FirewallFilter` the operator adds a container `fw-<name>` to the engine DaemonSet:

| Mount / variable | Value |
| --- | --- |
| socket | `/var/run/hypergate/fw-<name>.sock` (shared `emptyDir` at `/var/run/hypergate/`) |
| `container.socketEnvKey` (default `FIREWALL_SOCKET_PATH`) | `unix:///var/run/hypergate/fw-<name>.sock` |
| `{socket_path}` in `container.args` | the socket path |
| `rulesConfigMap` | mounted at `/etc/firewall/rules/` |
| `rulesSecretRef` | mounted at `/etc/firewall/secrets/` |

## Standalone engine

```yaml
chains:
  api:
    - type: firewall
      options:
        protocol: http
        socket_path: /var/run/hypergate/fw-waf.sock
        timeout: 1s
        inspect_body: true
        max_body_size_kb: 512
        on_failure:
          downstream_pass_through_headers: ["x-waf-rule-id"]
```

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `protocol` | `protocol` | enum | `grpc` | `grpc` (ext_proc) or `http`. |
| — | `socket_path` | path | set by the operator | Unix socket of the sidecar. |
| `engineRules.timeout` | `timeout` | duration | `2s` | Deadline for one inspection (both protocols). |
| `engineRules.inspectBody` | `inspect_body` | bool | `false` | Also inspect the request body (see above). |
| `engineRules.maxBodySizeKB` | `max_body_size_kb` | int | `1024` | Bytes of body sent to the WAF, in KiB. |
| `engineRules.forwardHeaders` | `forward_headers` | list | all headers | Request headers sent to the WAF; `*` or `all` also means all. |
| `engineRules.onSuccess.upstreamHeadersToAdd` | `on_success.upstream_headers_to_add` | list | — | (`http`) WAF response headers copied upstream. With `grpc`, the WAF's header mutations are applied instead. |
| `engineRules.onSuccess.upstreamHeadersToRemove` | `on_success.upstream_headers_to_remove` | list | — | Request headers removed before forwarding. |
| `engineRules.onFailure.downstreamPassThroughHeaders` | `on_failure.downstream_pass_through_headers` | list | — | (`http`) WAF headers returned to the client on a block. With `grpc`, the immediate response's headers are returned. |
| `rulesConfigMap` | — | string | — | ConfigMap with rules, mounted into the sidecar. |
| `rulesSecretRef` | — | string | — | Secret with rules or credentials, mounted into the sidecar. |
| `container.*` | — | object | — | Same fields as for [external authorization](./external-auth.md#option-reference). |
