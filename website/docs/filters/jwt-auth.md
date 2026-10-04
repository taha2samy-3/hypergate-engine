---
sidebar_position: 3
title: JWT authentication
description: Validate JSON Web Tokens locally with a JWKS or HMAC secret, fall back to token introspection, and map claims to headers.
---

import Icon from '@site/static/img/icons/jwt.svg';

# <Icon className="hg-icon" aria-hidden="true" /> JWT authentication

The JWT filter validates bearer tokens inside the engine: the signature, `exp`/`nbf`, and optionally `iss` and `aud`. It needs no network call per request. Keys come from a JWKS endpoint that is refreshed in the background, or from a shared HMAC secret. Tokens that fail local validation can optionally be checked against an RFC 7662 introspection endpoint, which also covers opaque tokens.

| At a glance | |
| --- | --- |
| CRD kind | `JwtAuthFilter` |
| Engine filter type | `jwt_auth` |
| Runs in phase | request headers |
| Blocks with | `401`, a JSON body `{"error":"unauthorized","message":...}` and `WWW-Authenticate: Bearer` |
| Fail mode | closed; `failOpen: true` only covers an unreachable introspection endpoint |

## Example: JWKS (RS256/ES256)

```yaml
apiVersion: hyper.io/v1alpha1
kind: JwtAuthFilter
metadata:
  name: auth0
spec:
  jwksEndpoint: https://example.eu.auth0.com/.well-known/jwks.json
  jwksRefreshInterval: 10m
  issuer: https://example.eu.auth0.com/
  audience: https://api.example.com
  claimMappings:
    sub: x-user-id
    email: x-user-email
  stripToken: true
```

The engine fetches the key set when the filter loads, with a 10-second timeout. If that first fetch fails, the filter does not compile and the config is rejected, so the previous policy keeps running. After that it refreshes every `jwksRefreshInterval` (default 5 minutes) and keeps the last good key set if a refresh fails. The algorithm is inferred from each key, and keys are matched by the token's `kid`.

**Key rotation:** when a token does not verify with the cached keys (for example it is signed with a new `kid` the identity provider just published), the engine fetches the key set again immediately and retries once. These on-demand refreshes happen at most once every 30 seconds, so invalid tokens cannot be used to hammer the JWKS endpoint.

## Example: shared HMAC secret

Put the secret in a Kubernetes Secret in the engine's target namespace (default `hyper-system`) and reference it. The operator mounts it read-only into the engine pod. The value never appears in the generated ConfigMap.

```bash
kubectl -n hyper-system create secret generic jwt-hmac --from-literal=key='a-long-random-secret'
```

```yaml
apiVersion: hyper.io/v1alpha1
kind: JwtAuthFilter
metadata:
  name: internal-tokens
spec:
  localSecretRef:
    name: jwt-hmac
    key: key
  algorithm: HS256            # default when a secret is used
  claimMappings:
    sub: x-user-id
```

With the engine alone, use `local_secret_file` (preferred) or `local_secret`:

```yaml
chains:
  internal:
    - type: jwt_auth
      options:
        local_secret_file: /etc/hypergate/secrets/jwt-hmac
        algorithm: HS256
        claim_mappings:
          sub: x-user-id
```

## Token sources

| `source` | Where the token is read | Related option |
| --- | --- | --- |
| `header` (default) | `headerName` (default `authorization`); a leading `Bearer ` is stripped (scheme matched case-insensitively) | `headerName` |
| `query` | query parameter (URL-decoded) | `queryParam` |
| `cookie` | cookie in the `Cookie` header | `cookieName` |

## Validation flow

1. No token → `401` `missing authentication token`.
2. Validate locally with the HMAC secret if one is configured, otherwise with the JWKS key set. If both are configured, the secret is used and the JWKS is not consulted for validation; the JWKS is still fetched when the filter loads, and a failed fetch rejects the configuration.
3. If local validation fails:
   - without `introspectionEndpoint` → `401` `invalid token`;
   - with `introspectionEndpoint`: `POST` with the form-encoded body `token=<token>`, plus the optional `Authorization` header from `introspectionAuthSecretRef`. The token is accepted only on `200` with `"active": true`. If `issuer` / `audience` are configured and the response contains `iss` / `aud`, they must match too. The response fields then act as claims. Any other answer → `401` `token validation failed`.
4. `failOpen: true` applies only when the introspection endpoint **cannot give an answer**: a network error, a timeout or a `5xx`. The request then continues **without** claim headers. Invalid, expired, forged or inactive tokens are always rejected.
5. On success each `claimMappings` entry sets its header from the claim.
6. `stripToken` removes the token before forwarding: the header, the query parameter (the upstream `:path` is rewritten) or the single cookie, depending on `source`.

### Claim headers

Mapped headers are identity assertions your services rely on, so the filter **always removes the client's own values of every mapped header** first. A header can only come from a validated claim. A client cannot send `x-user-id: admin` and have it pass through when a claim is missing, or on the `failOpen` path.

Claim names are case-sensitive (`tenantId` is not `tenantid`); header names are not. Values are rendered as follows:

| Claim value | Header value |
| --- | --- |
| string | as-is |
| number, boolean | `42`, `3.5`, `true` |
| `exp`, `iat`, `nbf` | Unix seconds |
| array | comma-separated, e.g. `admin,dev` |
| object | compact JSON |

Control characters (such as CR/LF) are removed, so a claim can never inject extra headers.

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `source` | `source` | enum | `header` | `header`, `query` or `cookie`. |
| `headerName` | `header_name` | string | `authorization` | Header holding the token. |
| `queryParam` | `query_param` | string | — | Query parameter for `source: query`. |
| `cookieName` | `cookie_name` | string | — | Cookie for `source: cookie`. |
| `jwksEndpoint` | `jwks_endpoint` | URL | — | JWKS used for signature validation. |
| `jwksRefreshInterval` | `jwks_refresh_interval` | duration | `5m` | Background refresh period. |
| `localSecretRef` | `local_secret_file` | Secret ref / path | — | Shared HMAC secret. The operator mounts it and passes the file path. |
| — | `local_secret` | string | — | Inline secret (engine only; prefer the file). |
| `algorithm` | `algorithm` | string | `HS256` with a secret, else `RS256` | Expected algorithm for the shared secret. |
| `issuer` | `issuer` | string | not checked | Required `iss`. |
| `audience` | `audience` | string | not checked | Required `aud`. |
| `claimMappings` | `claim_mappings` | map | — | Claim name (case-sensitive) → upstream header. Client values of these headers are always removed. |
| `introspectionEndpoint` | `introspection_endpoint` | URL | — | RFC 7662 endpoint used when local validation fails. |
| `introspectionAuthSecretRef` | `introspection_auth_header_file` | Secret ref / path | — | `Authorization` header value sent to the introspection endpoint. |
| — | `introspection_auth_header` | string | — | Inline variant (engine only). |
| `introspectionTimeout` | `introspection_timeout` | duration | `2s` | Timeout of the introspection call. |
| `failOpen` | `fail_open` | bool | `false` | Allow requests (without claims) when introspection is unavailable. Never accepts invalid tokens. |
| `stripToken` | `strip_token` | bool | `false` | Remove the token (header, query parameter or cookie) before forwarding. |

A filter must have at least one of `jwksEndpoint`, a local secret or `introspectionEndpoint`; otherwise it does not load.

Secrets are read when the filter is built. After rotating a Secret, restart the engine so the new value is loaded:

```bash
kubectl -n hyper-system rollout restart daemonset/hyper-engine
```
