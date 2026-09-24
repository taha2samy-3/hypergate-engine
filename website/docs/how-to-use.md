---
sidebar_position: 1
---

# How to Use Hypergate Engine

Hypergate Engine is a highly optimized API Gateway built in Go. It acts as a gRPC External Processing (`ext_proc`) server for Envoy Proxy, enforcing advanced routing, authentication, and traffic modification policies.

## Getting Started

Hypergate is driven entirely by Kubernetes Custom Resource Definitions (CRDs). You configure everything using Kubernetes manifests!

### 1. Define your Redis Connections
Define your backend Redis servers used for ratelimiting and metadata enrichment:
```yaml
apiVersion: hypergate.com/v1alpha1
kind: HyperRedis
metadata:
  name: primary-redis
spec:
  address: "redis.default.svc.cluster.local:6379"
  poolSize: 100
```

### 2. Create Filters
You can create powerful, reusable filters. For example, a Header Modifier:
```yaml
apiVersion: hypergate.com/v1alpha1
kind: FilterHeaderModifier
metadata:
  name: inject-headers
spec:
  addHeaders:
    - key: "X-Gateway"
      value: "Hypergate"
```

### 3. Build a Chain
Link your filters together into an execution chain:
```yaml
apiVersion: hypergate.com/v1alpha1
kind: HyperChain
metadata:
  name: secure-chain
spec:
  filters:
    - name: inject-headers
```

### 4. Apply to Routes
Finally, map your chain to a route in the master config:
```yaml
apiVersion: hypergate.com/v1alpha1
kind: HyperConfig
metadata:
  name: main-gateway
spec:
  routes:
    - match: "/api/v1/secure"
      chain: "secure-chain"
```

The **Hypergate Operator** will instantly compile these CRDs into a unified `ConfigMap`. The Hypergate Engine watches this `ConfigMap` and seamlessly hot-reloads the new rules without dropping a single connection!
