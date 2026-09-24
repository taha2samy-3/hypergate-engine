---
sidebar_position: 1
---

# External Authentication (The Sidecar Way)

Have you ever wanted to write a complex authentication flow in NodeJS or Rust, but were afraid it would slow down your Go API Gateway? Welcome to **Hypergate External Auth**.

![Sidecar Concept](/img/sidecar.svg)

## The Problem
Usually, if you want a gateway to authenticate a request against an external service (like OAuth2 Proxy or an Identity Provider), you have to make a network call over the cluster. That adds a minimum of 2-5ms of latency to *every single request*. 

## The Hypergate Solution: UDS Sidecars!
With Hypergate, you define a `FilterExternalAuth` CRD. When the Hypergate Operator sees this, it takes your custom Docker image and injects it **directly into the Hypergate Engine DaemonSet Pod as a sidecar container**.

Instead of talking over the network, the Engine communicates with your sidecar over a **Unix Domain Socket (UDS)** mounted at `/var/run/hypergate/`. This drops the network overhead to absolutely zero.

## Example Configuration

```yaml
apiVersion: hypergate.com/v1alpha1
kind: FilterExternalAuth
metadata:
  name: oauth2-proxy-auth
spec:
  container:
    image: "quay.io/oauth2-proxy/oauth2-proxy:latest"
    args:
      - "--upstream=static://200"
      - "--http-address={socket_path}" # Hypergate auto-replaces this with the UDS!
    env:
      - name: OAUTH2_PROXY_CLIENT_ID
        value: "my-id"
```

### What happens when you apply this?
1. The Operator restarts the Engine Pod (or creates a new one).
2. The Pod spins up with an `oauth2-proxy` container running right alongside the `hyper-engine`.
3. The Engine starts routing incoming requests through the socket at microseconds speed.

You get the performance of native Go code, with the flexibility of writing your Auth filter in any language you want!
