---
sidebar_position: 8
title: Correlation ID
description: Generate, validate and propagate request IDs so a request can be traced across the gateway and your services.
---

import Icon from '@site/static/img/icons/correlation-id.svg';

# <Icon className="hg-icon" aria-hidden="true" /> Correlation ID

The correlation ID filter makes sure every request carries an ID. It keeps a valid incoming ID or generates a new one, then passes it to the upstream service and back to the client. Put it first in a chain so that logs from later filters and from your services share the same ID.

| At a glance | |
| --- | --- |
| CRD kind | `CorrelationIdFilter` |
| Engine filter type | `correlation_id` |
| Runs in phase | request headers |
| Blocks | never |

## Example

```yaml
apiVersion: hyper.io/v1alpha1
kind: CorrelationIdFilter
metadata:
  name: request-id
spec:
  headerName: x-correlation-id
  algorithm: uuidv7              # time-ordered, sorts well in logs
  mode: if_missing
  validationRegex: '^[0-9a-f-]{36}$'
  propagateToUpstream: true
  propagateToDownstream: true
```

Engine configuration:

```yaml
chains:
  api:
    - type: correlation_id
      options:
        header_name: x-correlation-id
        algorithm: uuidv7
        mode: if_missing
        validation_regex: '^[0-9a-f-]{36}$'
```

## Behaviour

A new ID is generated when:

- the incoming header (`inputHeaderName`) is missing or empty, or
- `mode` is `overwrite`, or
- `validationRegex` is set and the incoming value does not match it. This stops clients from injecting arbitrary strings into your logs.

Otherwise the incoming value is kept as-is. `prefix` is only added to generated IDs.

| `algorithm` | Example | Notes |
| --- | --- | --- |
| `uuidv4` (default) | `3f2b8c1e-7d4a-4f6e-9b0c-2a1d5e8f7c3b` | random |
| `uuidv7` | `01923f4e-8a7b-7c2d-9e1f-3a4b5c6d7e8f` | time-ordered UUID |
| `ulid` | `01J9Z3K8QW4XR7T2M5N6P8V0YB` | time-ordered, 26 characters |
| `xid` | `cs2k4t0u5vbs73c8r3m0` | time-ordered, 20 characters |

:::note Envoy's own request ID
Envoy sets `x-request-id` on every request by default (`generate_request_id` on the HTTP connection manager). With `headerName: x-request-id` and `mode: if_missing`, Hypergate keeps Envoy's value. Use `mode: overwrite` to always use Hypergate's format, or a different header name.
:::

## Option reference

| CRD field (`spec.`) | Engine option | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `headerName` | `header_name` | string | `x-request-id` | Header written upstream. |
| `inputHeaderName` | `input_header_name` | string | `headerName` | Header an existing ID is read from. |
| `responseHeaderName` | `response_header_name` | string | `headerName` | Header written on the response. |
| `algorithm` | `algorithm` | enum | `uuidv4` | `uuidv4`, `uuidv7`, `ulid`, `xid`. |
| `mode` | `mode` | enum | `if_missing` | `if_missing` keeps valid incoming IDs; `overwrite` always generates. |
| `prefix` | `prefix` | string | — | Prepended to generated IDs, e.g. `gw-`. |
| `validationRegex` | `validation_regex` | string | — | Incoming IDs that don't match are replaced. |
| `propagateToUpstream` | `propagate_to_upstream` | bool | `true` | Send the ID to the upstream service. |
| `propagateToDownstream` | `propagate_to_downstream` | bool | `true` | Return the ID to the client. |
