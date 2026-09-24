---
sidebar_position: 7
title: Header modifier
description: Add, replace and remove headers on the request sent upstream and on the response sent to the client.
---

import Icon from '@site/static/img/icons/header-modifier.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Header modifier

The header modifier changes headers in two directions:

- **upstream**: the request forwarded to your service,
- **downstream**: the response returned to the client, including responses produced by Hypergate itself (for example a `429` from a later rate limiter).

| At a glance | |
| --- | --- |
| CRD kind | `HeaderModifierFilter` |
| Engine filter type | `header_modifier` |
| Runs in phase | request headers (downstream changes are applied when the response arrives) |
| Blocks | never |

## Example

```yaml
apiVersion: hyper.io/v1alpha1
kind: HeaderModifierFilter
metadata:
  name: edge-headers
spec:
  upstream:
    override:
      x-gateway: hypergate
    remove:
      - x-internal-debug        # clients must not be able to send this
  downstream:
    add:
      strict-transport-security: max-age=31536000; includeSubDomains
      x-content-type-options: nosniff
    remove:
      - server                  # hide the upstream's server header
      - x-powered-by
```

Engine configuration:

```yaml
chains:
  web:
    - type: header_modifier
      options:
        upstream:
          override: {x-gateway: hypergate}
          remove: [x-internal-debug]
        downstream:
          add:
            strict-transport-security: max-age=31536000; includeSubDomains
            x-content-type-options: nosniff
          remove: [server, x-powered-by]
```

## Behaviour

- `add` and `override` both **set** the header, replacing any existing value, including one sent by the client or the upstream. They exist as two names for readability; neither appends a second value.
- Within one section the order is `add`, then `override`, then `remove`, so a header listed in both `add` and `remove` ends up removed.
- `downstream.remove` removes headers the upstream service returned, and also headers queued by earlier filters.
- Header names are case-insensitive (stored lower-case). Values are used as written.
- Filters later in the chain see the modified request headers. That is how you set a value, such as a rate-limit cost, that clients cannot override.
- The top-level `add`, `override` and `remove` fields are shorthands for the `upstream` section and are applied first.

## Option reference

| CRD field (`spec.`) | Engine option | Type | Description |
| --- | --- | --- | --- |
| `upstream.add` | `upstream.add` | map | Headers set on the upstream request. |
| `upstream.override` | `upstream.override` | map | Same effect as `add`. |
| `upstream.remove` | `upstream.remove` | list | Headers removed from the upstream request. |
| `downstream.add` | `downstream.add` | map | Headers set on the response to the client. |
| `downstream.override` | `downstream.override` | map | Same effect as `add`. |
| `downstream.remove` | `downstream.remove` | list | Headers removed from the response to the client. |
| `add`, `override`, `remove` | `add`, `override`, `remove` | map / list | Shorthand for the `upstream` section. |
