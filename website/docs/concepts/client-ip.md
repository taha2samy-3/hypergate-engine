---
sidebar_position: 5
title: Client IP
description: How the engine resolves the client address from Envoy's peer and trusted X-Forwarded-For hops.
---

# Client IP

Per-client rate limits and client-keyed metadata need the real client address. `X-Forwarded-For` alone cannot provide it: any client can send the header with any value. The engine therefore resolves the client IP from addresses that trusted parties added, and ignores the rest.

![Client IP resolution: the address chain is X-Forwarded-For entries plus Envoy's peer; the client is trusted_proxy_hops positions from the right](/img/diagrams/client-ip.svg)

## The rule

1. Build an address chain: the entries of `X-Forwarded-For`, left to right, followed by the **peer address Envoy observed** on the downstream connection. Envoy sends the peer as the ext_proc attribute `source.address`. If the last `X-Forwarded-For` entry already equals the peer (Envoy with `use_remote_address: true` appends it), it is not counted twice.
2. The client is the entry `trusted_proxy_hops` positions from the right.
   - `0` (default): the client is Envoy's direct peer.
   - `1`: the client is the address appended by the one trusted proxy or load balancer in front of Envoy.
   - `n`: the address appended by the outermost of `n` trusted proxies.
3. If `trusted_proxy_hops` is larger than the chain, the leftmost entry is used.

Ports and IPv6 brackets are stripped, so `[2001:db8::1]:443` becomes `2001:db8::1`.

Configure the hop count in the engine configuration:

```yaml
server:
  client_ip:
    trusted_proxy_hops: 1
```

or on the HyperConfig:

```yaml
apiVersion: hyper.io/v1alpha1
kind: HyperConfig
metadata:
  name: default-engine
spec:
  redisServiceRef: shared-redis
  trustedProxyHops: 1
```

The value takes effect on reload.

## Sending the peer address from Envoy

Add `source.address` to the ext_proc filter's request attributes:

```yaml
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    grpc_service:
      envoy_grpc:
        cluster_name: hyper_engine
    request_attributes:
      - source.address
```

If Envoy does not send `source.address`, the engine treats the **rightmost** `X-Forwarded-For` entry as the peer. That entry is only trustworthy when Envoy itself appended it, which it does with `use_remote_address: true` on the HTTP connection manager. Without the attribute and without `use_remote_address`, a client can choose its own address; if no `X-Forwarded-For` header is present at all, the resolved client IP is empty.

## Choosing `trusted_proxy_hops`

| Topology in front of Envoy | Value |
| --- | --- |
| Clients connect to Envoy directly (for example Envoy behind a pass-through L4 load balancer that preserves source IPs) | `0` |
| One L7 load balancer or CDN that appends to `X-Forwarded-For` | `1` |
| A CDN, then a cloud load balancer, both appending | `2` |

Count only proxies that append the address they saw. A proxy that forwards `X-Forwarded-For` unchanged does not add an entry and must not be counted.

If the value is too low, the client IP is one of your own proxies and all clients behind it share a rate-limit bucket. If it is too high, the engine reads an entry the client wrote, and clients can spoof their address.

## Where the client IP is used

- **Rate limiting.** Descriptor entries with the key `ip`, `client_ip` or `remote_ip` always use the resolved client IP. A `header_mappings` entry for these keys is ignored, so a rate limit cannot be keyed on raw `X-Forwarded-For` by mistake. If the client IP is empty, the descriptor value is `default`, which puts every such request into one bucket.
- **Redis metadata enricher.** A variable with `source: "{client_ip}"` receives the resolved address.

The raw `X-Forwarded-For` header is still forwarded upstream by Envoy unchanged; the engine does not rewrite it.
