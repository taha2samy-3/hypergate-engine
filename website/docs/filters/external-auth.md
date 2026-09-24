---
sidebar_position: 4
title: External authorization
description: Delegate the allow/deny decision to oauth2-proxy, OPA or your own service running as a sidecar on a Unix domain socket.
---

import Icon from '@site/static/img/icons/external-auth.svg';

# <Icon className="hg-icon" aria-hidden="true" /> External authorization

The external authorization filter asks a separate service whether a request may continue. The service can be oauth2-proxy, an OPA/Envoy authz server, or your own code in any language. Hypergate runs it as a **sidecar in the engine pod** and talks to it over a **Unix domain socket**, so the check never leaves the node, needs no Service or TLS setup, and scales with the engine DaemonSet.

| At a glance | |
| --- | --- |
| CRD kind | `ExternalAuthFilter` |
| Engine filter type | `external_auth` |
| Runs in phase | request headers |
| Blocks with | the sidecar's status and body (HTTP), or its denied response (gRPC, default `403`) |
| If the sidecar is down, times out or misbehaves | `500` (fail closed) |

![Engine pod with sidecars sharing the /var/run/hypergate socket directory](/img/diagrams/sidecar-uds.svg)

## Two protocols

| `protocol` | The sidecar implements | Allowed when | Typical services |
| --- | --- | --- | --- |
| `http` (CRD default) | a plain HTTP endpoint (forward-auth style) | it answers `2xx` | oauth2-proxy (`/oauth2/auth`), Authelia, Pomerium, any small HTTP service |
| `grpc` | Envoy's `envoy.service.auth.v3.Authorization/Check` | it returns status `OK` | OPA-Envoy plugin, existing Envoy `ext_authz` servers |

### What the sidecar receives

**HTTP:** a `GET` to `path` (default `/`) on the socket, with:

- the selected request headers (all of them unless `forwardHeaders` is set; hop-by-hop headers and pseudo-headers are never sent),
- the original request, described by the conventional forward-auth headers:

| Header | Value |
| --- | --- |
| `X-Forwarded-Method`, `X-Original-Method` | original method |
| `X-Forwarded-Uri`, `X-Original-Uri` | original path and query |
| `X-Forwarded-Host` | original `:authority` |
| `X-Forwarded-Proto` | original scheme |
| `X-Original-Url` | `scheme://host/path?query` |
| `X-Forwarded-For`, `X-Real-Ip` | the resolved [client IP](../concepts/client-ip.md) (replaces any client-supplied value) |

Redirects from the sidecar are not followed. They are the answer, and you can relay them to the client (see below).

**gRPC:** a `CheckRequest` whose `attributes.request.http` has `method`, `path`, `host`, `scheme`, `id` (from `x-request-id`) and the selected `headers`, and whose `attributes.source.address` is the client IP.

Headers set by earlier filters in the chain (for example a correlation ID) are included.

## Example: oauth2-proxy

```yaml
apiVersion: hyper.io/v1alpha1
kind: ExternalAuthFilter
metadata:
  name: sso
spec:
  protocol: http
  container:
    image: quay.io/oauth2-proxy/oauth2-proxy:v7.6.0
    # OAUTH2_PROXY_HTTP_ADDRESS=unix:///var/run/hypergate/ext-auth-sso.sock is set
    # automatically for oauth2-proxy images, so no listen address is needed here.
    args:
      - --upstream=static://202
      - --provider=oidc
      - --oidc-issuer-url=https://login.example.com
      - --email-domain=*
      - --set-xauthrequest=true
      - --reverse-proxy=true
    envFrom:
      - secretRef:
          name: oauth2-proxy-credentials   # client id/secret, cookie secret (in the engine namespace)
  engineRules:
    path: /oauth2/auth
    timeout: 2s
    forwardHeaders: ["cookie", "authorization"]
    onSuccess:
      upstreamHeadersToAdd: ["x-auth-request-user", "x-auth-request-email"]
      upstreamHeadersToRemove: ["cookie"]
    onFailure:
      downstreamPassThroughHeaders: ["location", "set-cookie", "www-authenticate"]
```

- Authenticated requests continue with `x-auth-request-user` and `x-auth-request-email` set from oauth2-proxy's answer, and the session cookie is not forwarded to your service.
- Unauthenticated requests get oauth2-proxy's `401`, together with its `www-authenticate` or `location` header. Point browsers at oauth2-proxy's `/oauth2/start` from your login page, or route `/oauth2/*` to the sidecar with an Envoy route.

## Example: an Envoy ext_authz server over gRPC

Any server implementing `envoy.service.auth.v3.Authorization` works. Tell it where to listen with `socketEnvKey` (or `{socket_path}` in its arguments):

```yaml
apiVersion: hyper.io/v1alpha1
kind: ExternalAuthFilter
metadata:
  name: policy
spec:
  protocol: grpc
  container:
    image: registry.example.com/authz-server:1.4.0
    socketEnvKey: AUTHZ_LISTEN            # receives unix:///var/run/hypergate/ext-auth-policy.sock
  engineRules:
    timeout: 500ms
    onSuccess:
      upstreamHeadersToAdd: ["x-authz-subject"]
    onFailure:
      downstreamPassThroughHeaders: ["www-authenticate"]
```

## How the sidecar is injected

For every `ExternalAuthFilter` the operator adds a container named `ext-auth-<name>` to the engine DaemonSet:

- A shared `emptyDir` is mounted at `/var/run/hypergate/` in the engine and in every sidecar. The socket for this filter is `/var/run/hypergate/ext-auth-<name>.sock`.
- `{socket_path}` in `container.args` is replaced with that path (a plain path; write `unix://{socket_path}` if your server expects a URL).
- If `container.socketEnvKey` is set, that environment variable is set to `unix://<socket path>`. For images whose name contains `oauth2-proxy`, `OAUTH2_PROXY_HTTP_ADDRESS` is set automatically.
- `imagePullSecrets` of all sidecars are merged into the pod.

Adding, changing or deleting an `ExternalAuthFilter` changes the pod template, so the DaemonSet rolls out new engine pods.

The engine config written for the chain only contains the socket path and the rules. With the standalone engine, run your auth service next to the engine and point `socket_path` at its socket:

```yaml
chains:
  web:
    - type: external_auth
      options:
        protocol: http
        socket_path: /var/run/hypergate/ext-auth-sso.sock
        path: /oauth2/auth
        timeout: 2s
        forward_headers: ["cookie", "authorization"]
        on_success:
          upstream_headers_to_add: ["x-auth-request-user"]
          upstream_headers_to_remove: ["cookie"]
        on_failure:
          downstream_pass_through_headers: ["location", "www-authenticate"]
```

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `protocol` | `protocol` | enum | `http` | `http` or `grpc`. |
| — | `socket_path` | path | set by the operator | Unix socket of the sidecar. |
| `engineRules.path` | `path` | string | `/` | Request path of HTTP checks. |
| `engineRules.timeout` | `timeout` | duration | `2s` | Deadline for one check (both protocols). |
| `engineRules.forwardHeaders` | `forward_headers` | list | all headers | Request headers sent to the sidecar; `*` or `all` also means all. |
| `engineRules.onSuccess.upstreamHeadersToAdd` | `on_success.upstream_headers_to_add` | list | — | Sidecar response headers copied into the upstream request. |
| `engineRules.onSuccess.upstreamHeadersToRemove` | `on_success.upstream_headers_to_remove` | list | — | Request headers removed before forwarding (e.g. `cookie`). |
| `engineRules.onFailure.downstreamPassThroughHeaders` | `on_failure.downstream_pass_through_headers` | list | — | Sidecar headers returned to the client on a deny (e.g. `location`, `www-authenticate`). |
| `container.image` | — | string | required | Sidecar image. |
| `container.imagePullPolicy` | — | string | cluster default | |
| `container.imagePullSecrets` | — | list | — | Merged into the engine pod. |
| `container.args` | — | list | — | `{socket_path}` is substituted. |
| `container.env`, `container.envFrom` | — | list | — | Standard container environment (Secrets must be in the engine namespace). |
| `container.resources` | — | object | — | Sidecar requests and limits. |
| `container.socketEnvKey` | — | string | — | Environment variable that receives `unix://<socket path>`. |

The deny body relayed to the client is capped at 16 KB.
