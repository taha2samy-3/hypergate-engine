---
sidebar_position: 2
---

# Architecture (The Real Deal)

Hypergate Engine operates using a highly advanced **DaemonSet and Unix Domain Socket (UDS)** architecture. This makes it incredibly powerful and extensible, without incurring network latency!

![Architecture Map](/img/architecture.svg)

## How It Works Under The Hood

1. **Envoy Proxy (`ext_proc`)**
   When Envoy receives an HTTP request, it pauses and forwards the headers/body to Hypergate via the `ext_proc` gRPC protocol.

2. **The Hypergate DaemonSet Pod**
   The Hypergate Operator deploys a Kubernetes `DaemonSet`. Inside this Pod, there are multiple containers running side-by-side:
   - **The Engine Container:** Written in Go, this is the master coordinator. It handles lightweight filters natively (like Redis Ratelimits and API Keys).
   - **Sidecar Containers:** Heavy, custom filters (like Web Application Firewalls or External OAuth proxies) are injected into this exact same Pod by the Operator!

3. **Unix Domain Sockets (UDS) for Microsecond Latency**
   Instead of sending traffic over the cluster network to reach an external authentication server or WAF, the Engine communicates with its injected sidecars over a **shared Unix Domain Socket** volume (`/var/run/hypergate/`). This results in almost zero latency between the Engine and the Sidecars!
