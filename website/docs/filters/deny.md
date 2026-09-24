---
sidebar_position: 6
title: Deny
description: Reject requests, or replace upstream responses, that match path and header conditions.
---

import Icon from '@site/static/img/icons/deny.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Deny

The deny filter returns a fixed status and body when its conditions match. With no conditions, it rejects everything that reaches it, which is useful as the last filter of a chain that should never allow traffic, or to take a route offline. With response-header conditions it inspects the **upstream response** and replaces it, for example to stop debug output from leaking.

| At a glance | |
| --- | --- |
| CRD kind | `DenyFilter` |
| Engine filter type | `deny` |
| Runs in phase | request headers; response headers when response conditions are set |
| Blocks with | `statusCode` (default `403`) and `body` (default `Forbidden`) |

## Examples

Block the admin area unless the request comes through the internal network marker:

```yaml
apiVersion: hyper.io/v1alpha1
kind: DenyFilter
metadata:
  name: admin-internal-only
spec:
  statusCode: 404          # don't reveal that the path exists
  body: Not Found
  match:
    pathPrefix: /admin
    notHeaders:
      x-internal: "true"
```

Replace any upstream response that carries a debug header:

```yaml
apiVersion: hyper.io/v1alpha1
kind: DenyFilter
metadata:
  name: no-debug-leaks
spec:
  statusCode: 502
  body: Bad Gateway
  match:
    responseHeaders:
      x-debug-trace: "*"
```

Engine configuration equivalent of the first example:

```yaml
chains:
  admin:
    - type: deny
      options:
        status_code: 404
        body: Not Found
        match:
          path_prefix: /admin
          not_headers:
            x-internal: "true"
```

## Matching rules

All configured conditions must hold (AND). A filter without any condition always denies.

| Condition | Holds when |
| --- | --- |
| `pathPrefix` | the path (including query) starts with the value |
| `pathRegex` | the Go regular expression matches the path (including query) |
| `headers` | every listed request header equals its value; `"*"` means "present with any non-empty value" |
| `notHeaders` | no listed request header equals its value; `"*"` means "absent or empty" |
| `responseHeaders` | every listed upstream response header equals its value (`"*"`: present) |
| `notResponseHeaders` | no listed upstream response header equals its value (`"*"`: absent) |

- Header names are case-insensitive, values are compared exactly.
- Request headers are read as changed by earlier filters in the chain, so a deny placed after an authentication filter can test headers that filter set.
- If either response condition is set, the whole filter is evaluated when the upstream response headers arrive (path and request-header conditions still apply). A match replaces the upstream response with the deny status and body.

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `statusCode` | `status_code` | int | `403` | Status returned on a match. |
| `body` | `body` | string | `Forbidden` | Response body returned on a match. |
| `match.pathPrefix` | `match.path_prefix` | string | — | Path prefix condition. |
| `match.pathRegex` | `match.path_regex` | string | — | Path regex condition (RE2 syntax); an invalid regex stops the filter from loading. |
| `match.headers` | `match.headers` | map | — | Required request header values. |
| `match.notHeaders` | `match.not_headers` | map | — | Forbidden request header values. |
| `match.responseHeaders` | `match.response_headers` | map | — | Required upstream response header values. |
| `match.notResponseHeaders` | `match.not_response_headers` | map | — | Forbidden upstream response header values. |

The operator also uses a deny filter internally: a route whose chain is missing or failed to compile gets a generated `503 Service Unavailable` deny chain. See [failure modes](../concepts/failure-modes.md).
