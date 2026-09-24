---
sidebar_position: 2
title: Request lifecycle
description: The ext_proc phases, where each filter runs, and where a request can be ended.
---

# Request lifecycle

Envoy opens one ext_proc gRPC stream per HTTP request and sends one message per processing phase. The engine handles each message against the policy snapshot the stream started with, so a reload in the middle of a request never changes the chain applied to it.

![The six ext_proc phases, the filters that run in each, and where an ImmediateResponse can end the request](/img/diagrams/request-lifecycle.svg)

## Phases

| # | Phase | Sent by Envoy when | What the engine does |
| --- | --- | --- | --- |
| 1 | Request headers | Always (with the default `processing_mode`). | Resolves the client IP, selects the chain, runs the filters that handle this phase. May ask Envoy for the request body. |
| 2 | Request body | `request_body_mode` is not `NONE`, or a filter requested it through a mode override. | Runs filters that inspect the body (the firewall with `inspect_body`). |
| 3 | Request trailers | `request_trailer_mode: SEND`. | No built-in filter uses this phase; it is passed through. |
| 4 | Response headers | `response_header_mode: SEND` (the Envoy default). | Applies response headers queued by earlier filters, runs filters with response conditions, enforces fail-closed checks. |
| 5 | Response body | `response_body_mode` is not `NONE`. | No built-in filter uses this phase; it is passed through. |
| 6 | Response trailers | `response_trailer_mode: SEND`. | Applies trailer mutations only. |

The chain is selected once, on the first message of the stream, and reused for every later phase.

## Where filters run

| Filter | Phases |
| --- | --- |
| `embedded_rate_limiter`, `api_key`, `jwt_auth`, `external_auth`, `redis_metadata_enricher`, `correlation_id`, `header_modifier` | Request headers |
| `deny` without response conditions | Request headers |
| `deny` with `response_headers` or `not_response_headers` | Response headers |
| `firewall` | Request headers; with `inspect_body`, the request body instead when the request has one |

Filters run in the order they are listed in the chain. A filter sees the effect of earlier filters in the same chain: a header set by `header_modifier` or injected by `api_key`, `jwt_auth` or `redis_metadata_enricher` is visible to later filters, and a header removed earlier reads as absent. This is how an enricher can feed a rate limiter (`header_mappings`), or a header modifier can supply a rate-limit cost.

## Mutations

Header changes are collected while the chain runs and sent to Envoy in the response to the current message:

- **Upstream request headers** (set or remove) are sent with the request-headers response. Setting a header replaces any existing value (`OVERWRITE_IF_EXISTS_OR_ADD`).
- **`:path`**: the API key filter rewrites `:path` when it strips a key from the query string. Changing only the query does not change Envoy's route selection, which has already happened.
- **Client-facing response headers** (rate-limit headers, a correlation ID sent downstream, `header_modifier` `downstream` rules) are recorded during the request phase and applied when the response headers arrive. `downstream.remove` also removes headers that the upstream set.

Client-facing response headers therefore require Envoy to send response headers to the engine (`response_header_mode: SEND`, the default).

## Ending a request

Any filter can *block* a request by setting a status and body. The chain stops at that filter, and the engine answers the current message with an `ImmediateResponse`:

- In phases 1 to 3 the upstream is never called.
- In phases 4 and 5 the upstream has already answered; the immediate response replaces its response. This is how a `deny` filter with response header conditions works.
- In phase 6 the response headers are already on their way to the client, so a block cannot replace the response; only trailer mutations are applied.

The immediate response carries the client-facing headers queued so far, so a `429` from the rate limiter still includes `RateLimit-*` headers and a JWT rejection includes `content-type: application/json`. A block without an explicit status is sent as `403`.

If a filter returns an internal error (for example Redis is unreachable and the rate-limit descriptor is not `fail_open`), the chain stops and the request is answered with `500 Internal Server Error`. See [Failure modes](./failure-modes.md) for every case.

## Requesting the body

Envoy's default `processing_mode` does not send bodies. A firewall filter with `inspect_body: true` sets a flag during the request-headers phase when the request has a body (the headers message did not carry `end_of_stream`). The engine then attaches a **mode override** to its response asking Envoy to send the request body `BUFFERED`, and the firewall calls its sidecar once, in the body phase, with headers and body together.

Envoy only honours the override when the ext_proc filter has `allow_mode_override: true`; alternatively configure `request_body_mode: BUFFERED` statically. If a body was required and Envoy never sent it, the engine detects this when the response headers arrive and replaces the response with `500`. At that point the upstream has already processed the request, so treat this as a configuration error to fix, not as a protection. See [Envoy configuration](../reference/envoy-configuration.md).

Requests without a body (headers message with `end_of_stream`) are inspected on the headers phase and never trigger a body request.

## Stream end

When the stream closes, the request context returns to the pool and the stream releases its snapshot reference. If a reload retired that snapshot and this was its last stream, the filters and Redis clients that only the old snapshot used are closed at that moment. See [Hot reload](./hot-reload.md).
