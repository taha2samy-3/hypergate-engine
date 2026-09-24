---
sidebar_position: 2
title: Kubernetes quickstart
description: Deploy a first policy with hyper.io CRDs and wire it into Envoy.
---

# Kubernetes quickstart

This walkthrough deploys a small policy with the operator:

- every request gets a correlation ID,
- requests under `/api` are rate limited to 10 per minute per client IP,
- requests under `/admin` are rejected unless they carry `x-admin-token`,
- everything else passes through without policy.

It assumes the operator is installed ([Installation](./installation.md)) and that you have an Envoy-based gateway. The Envoy part uses the repository's Cilium Gateway API demo; for other Envoy setups use the filter settings in [Envoy configuration](../reference/envoy-configuration.md).

All `hyper.io` resources are **cluster-scoped**: do not set `metadata.namespace` on them.

## 1. Run Redis

Any Redis reachable from the engine pods works. For a test cluster:

```yaml title="redis.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: hyper-demo
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  namespace: hyper-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
        - name: redis
          image: redis:7-alpine
          ports:
            - containerPort: 6379
---
apiVersion: v1
kind: Service
metadata:
  name: redis
  namespace: hyper-demo
spec:
  selector:
    app: redis
  ports:
    - port: 6379
      targetPort: 6379
```

```bash
kubectl apply -f redis.yaml
```

## 2. Declare the Redis service

A `HyperRedis` becomes an entry under `redis:` in the engine configuration. Filters refer to it by its `metadata.name`.

```yaml title="01-redis.yaml"
apiVersion: hyper.io/v1alpha1
kind: HyperRedis
metadata:
  name: shared-redis
spec:
  type: SINGLE
  url: "redis.hyper-demo.svc.cluster.local:6379"
  poolSize: 20
  timeout: "200ms"
```

## 3. Create the filters

```yaml title="02-filters.yaml"
apiVersion: hyper.io/v1alpha1
kind: CorrelationIdFilter
metadata:
  name: request-id
spec:
  headerName: x-request-id
  algorithm: uuidv7
  mode: if_missing
---
apiVersion: hyper.io/v1alpha1
kind: RateLimitFilter
metadata:
  name: api-per-client
spec:
  domain: quickstart_api
  algorithm: fixed_window
  redisService: shared-redis
  responseHeaders:
    enabled: true
  descriptors:
    - entries:
        - key: client_ip
      limit: 10
      unit: minute
---
apiVersion: hyper.io/v1alpha1
kind: DenyFilter
metadata:
  name: admin-requires-token
spec:
  statusCode: 403
  body: "admin access requires a token"
  match:
    notHeaders:
      x-admin-token: "*"
```

The rate limiter key `client_ip` is resolved from Envoy's peer address, not from `X-Forwarded-For`; see [Client IP](../concepts/client-ip.md). The deny filter blocks when `x-admin-token` is absent (`"*"` in `notHeaders` means "header not present").

## 4. Group filters into chains

Filters in a chain run in the listed order.

```yaml title="03-chains.yaml"
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: public-api
spec:
  filters:
    - kind: CorrelationIdFilter
      name: request-id
    - kind: RateLimitFilter
      name: api-per-client
---
apiVersion: hyper.io/v1alpha1
kind: HyperChain
metadata:
  name: admin
spec:
  filters:
    - kind: CorrelationIdFilter
      name: request-id
    - kind: DenyFilter
      name: admin-requires-token
```

With the admission webhook enabled, a HyperChain that references a filter that does not exist is rejected when you apply it.

## 5. Route requests to chains

The operator orders HyperRoutes by `spec.priority`, highest first, and the first matching route wins.

```yaml title="04-routes.yaml"
apiVersion: hyper.io/v1alpha1
kind: HyperRoute
metadata:
  name: admin
spec:
  priority: 200
  targetPolicy: admin
  matches:
    - pathPrefix: /admin
---
apiVersion: hyper.io/v1alpha1
kind: HyperRoute
metadata:
  name: api
spec:
  priority: 100
  targetPolicy: public-api
  matches:
    - pathPrefix: /api
```

## 6. Deploy the engine

A `HyperConfig` tells the operator where to run the engine and which server settings to compile. There is one active HyperConfig per `targetNamespace`.

```yaml title="05-hyperconfig.yaml"
apiVersion: hyper.io/v1alpha1
kind: HyperConfig
metadata:
  name: default-engine
spec:
  targetNamespace: hyper-system
  redisServiceRef: shared-redis
  logLevel: INFO
  trustedProxyHops: 0
```

`spec.defaultChain` is not set, so requests that match no route pass through without policy. Set it to a chain name to apply a policy to all other traffic.

Apply everything:

```bash
kubectl apply -f 01-redis.yaml -f 02-filters.yaml -f 03-chains.yaml -f 04-routes.yaml -f 05-hyperconfig.yaml
```

## 7. Check what the operator did

```bash
kubectl get hyperconfigs
kubectl get hyperchains
kubectl -n hyper-system get daemonset,service,configmap
```

Expected state:

- `HyperConfig default-engine` shows `State: Ready`.
- Both HyperChains show `State: Ready` and `Message: Chain successfully compiled`.
- `hyper-system` contains the `hyper-engine` DaemonSet, the `hyper-engine-svc` Service (gRPC on port 9001) and the `hyper-engine-config` ConfigMap.

Inspect the compiled engine configuration:

```bash
kubectl -n hyper-system get configmap hyper-engine-config -o jsonpath='{.data.config\.yaml}'
```

The engine pods become ready once they have loaded this configuration:

```bash
kubectl -n hyper-system rollout status daemonset/hyper-engine
kubectl -n hyper-system logs daemonset/hyper-engine -c engine | grep -i "compiled filter chain"
```

## 8. Connect Envoy

Envoy must call the engine through `envoy.filters.http.ext_proc`. With Cilium's Gateway API support, the repository demo (`k8s/demo/`) does this with a `CiliumEnvoyConfig` attached to the gateway service and an `HTTPRoute` that references it:

```yaml title="06-extproc-config.yaml (from k8s/demo)"
apiVersion: cilium.io/v2
kind: CiliumEnvoyConfig
metadata:
  name: hyper-engine-ext-proc
  namespace: default
spec:
  services:
    - name: cilium-gateway-demo-gateway
      namespace: default
  backendServices:
    - name: hyper-engine-svc
      namespace: hyper-system
      number:
        - "9001"
  resources:
    - "@type": type.googleapis.com/envoy.config.filter.network.http_connection_manager.v2.HttpFilter
      name: envoy.filters.http.ext_proc
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
        grpc_service:
          envoy_grpc:
            cluster_name: "default/hyper-engine-ext-proc/hyper-system:hyper-engine-svc:9001"
          timeout: 5s
        failure_mode_allow: false
        allow_mode_override: true
        request_attributes:
          - source.address
        processing_mode:
          request_header_mode: SEND
          response_header_mode: SEND
          request_body_mode: NONE
          response_body_mode: NONE
          request_trailer_mode: SKIP
          response_trailer_mode: SKIP
```

The demo also contains the `Gateway` (`02-gateway.yaml`), a `ReferenceGrant` that allows the `CiliumEnvoyConfig` in `default` to reference `hyper-engine-svc` in `hyper-system` (`05-referencegrant.yaml`), and an `HTTPRoute` that adds the config as an `ExtensionRef` filter (`07-httproute.yaml`).

The three settings that matter for Hypergate:

- `failure_mode_allow: false`: if the engine cannot be reached, Envoy fails the request instead of letting it through.
- `allow_mode_override: true`: lets filters that need the request body (a `FirewallFilter` with `inspectBody`) ask Envoy for it.
- `request_attributes: [source.address]`: sends Envoy's downstream peer address, which the engine uses to resolve the client IP.

## 9. Try it

Replace `$GATEWAY` with your gateway address.

```bash
# Rate limited route: headers show the quota.
curl -si http://$GATEWAY/api/items | grep -i -E '^(HTTP|ratelimit|x-request-id)'
# HTTP/1.1 200 OK
# ratelimit-limit: 10
# ratelimit-remaining: 9
# ratelimit-reset: 42
# x-request-id: 0192...

# The 11th request in the same minute is rejected.
for i in $(seq 1 11); do curl -s -o /dev/null -w '%{http_code}\n' http://$GATEWAY/api/items; done
# ... 200 (ten times), then 429

# Admin route without the token.
curl -si http://$GATEWAY/admin/ | head -1
# HTTP/1.1 403 Forbidden

curl -si -H 'x-admin-token: anything' http://$GATEWAY/admin/ | head -1
# HTTP/1.1 200 OK
```

## Change the policy

Edit any resource and apply it again, for example raise the limit to 20. The operator rewrites the ConfigMap and the engine reloads it without a restart. If the new configuration fails to compile, the engine logs the error and keeps serving the previous policy; see [Hot reload](../concepts/hot-reload.md).

## Clean up

```bash
kubectl delete hyperconfig default-engine
kubectl delete hyperroute admin api
kubectl delete hyperchain public-api admin
kubectl delete correlationidfilter request-id
kubectl delete ratelimitfilter api-per-client
kubectl delete denyfilter admin-requires-token
kubectl delete hyperredis shared-redis
kubectl delete -f redis.yaml
```

Deleting the HyperConfig removes the DaemonSet, Service, ServiceAccount and RBAC objects it owns. The compiled ConfigMap is not owned by the HyperConfig and stays in the target namespace until you delete it. Delete HyperChains before the filters they reference; the webhook rejects deleting a filter that is still referenced.
