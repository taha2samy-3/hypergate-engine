---
sidebar_position: 1
title: Installation
description: Install the Hypergate operator and its CRDs with Helm, or deploy the engine on its own.
---

# Installation

Hypergate ships two Helm charts:

| Chart | Path | Use it when |
| --- | --- | --- |
| `hyper-operator` | `charts/hyper-operator` | You want to manage policy with `hyper.io` CRDs. The operator installs the CRDs, compiles them, and creates the engine DaemonSet for you. **Recommended.** |
| `hypergate-engine` | `charts/hypergate-engine` | You want to run the engine alone and manage its YAML configuration yourself (for example from GitOps). |

Do not install both into the same namespace: the operator creates and owns its own engine DaemonSet, Service and ConfigMap.

## Prerequisites

- A Kubernetes cluster. The engine Service created by the operator sets `spec.trafficDistribution: PreferSameNode`, which is honoured from Kubernetes 1.31.
- Helm 3.8 or later (OCI registry support).
- [cert-manager](https://cert-manager.io/) if you keep the default webhook settings. The chart creates a self-signed `Issuer` and a `Certificate` for the admission webhook.
- An Envoy-based gateway where you can add the `envoy.filters.http.ext_proc` HTTP filter. See [Envoy configuration](../reference/envoy-configuration.md).
- A Redis deployment if you use rate limiting, API keys or the metadata enricher.

## Install the operator

The release pipeline publishes the charts as OCI artifacts under `oci://ghcr.io/taha2samy-3/charts`. Pick the version you want to run:

```bash
helm install hyper-operator oci://ghcr.io/taha2samy-3/charts/hyper-operator \
  --version <version> \
  --namespace hyper-operator-system --create-namespace
```

From a checkout of the repository:

```bash
helm install hyper-operator ./charts/hyper-operator \
  --namespace hyper-operator-system --create-namespace
```

The operator runs two replicas with leader election. Check that it is up and that the CRDs are registered:

```bash
kubectl -n hyper-operator-system get pods
kubectl get crds | grep hyper.io
```

You should see 13 CRDs: `hyperconfigs`, `hyperredis`, `hyperchains`, `hyperroutes` and the nine filter kinds, all in the `hyper.io` group and all cluster-scoped.

Nothing is deployed for the engine yet. The engine DaemonSet appears in the target namespace when you create a `HyperConfig`; continue with the [Kubernetes quickstart](./quickstart-kubernetes.md).

### Engine image

When a `HyperConfig` does not set `spec.engineImage`, the operator uses the `ENGINE_IMAGE` environment variable, which the chart sets to `engine.image.repository:engine.image.tag`. The tag defaults to the chart's `appVersion`, so operator and engine versions stay in step. The operator never falls back to `:latest`.

### Chart values

| Value | Default | Description |
| --- | --- | --- |
| `replicaCount` | `2` | Operator replicas. Only the leader reconciles. |
| `image.repository` | `ghcr.io/taha2samy-3/hyper-operator` | Operator image. |
| `image.tag` | chart `appVersion` | Operator image tag. |
| `image.pullPolicy` | `IfNotPresent` | |
| `engine.image.repository` | `ghcr.io/taha2samy-3/hyper-engine` | Default engine image passed to the operator as `ENGINE_IMAGE`. |
| `engine.image.tag` | chart `appVersion` | Default engine image tag. |
| `imagePullSecrets` | `[]` | Pull secrets for the operator pod. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Operator ServiceAccount. |
| `operator.leaderElect` | `true` | Passes `--leader-elect`. Keep it on when `replicaCount > 1`. |
| `operator.enableWebhooks` | `true` | Registers the validating webhooks (see below). Sets `ENABLE_WEBHOOKS`. |
| `operator.webhookPort` | `9443` | Webhook server port in the operator container. |
| `operator.webhookCertSecret` | `webhook-server-cert` | Secret with the webhook serving certificate. |
| `certManager.enabled` | `true` | Create a self-signed `Issuer` and `Certificate`, and inject the CA into the webhook configuration. |
| `certManager.issuerName` / `.certificateName` | `hyper-selfsigned-issuer` / `hyper-operator-cert` | Names of the cert-manager objects. |
| `certManager.duration` / `.renewBefore` | `8760h` / `360h` | Certificate lifetime and renewal window. |
| `service.port` / `.targetPort` | `443` / `9443` | Webhook Service. |
| `resources` | requests `100m`/`64Mi`, limits `500m`/`512Mi` | Operator container resources. |
| `podSecurityContext`, `securityContext` | non-root `65532`, read-only root FS, all capabilities dropped | Operator pod hardening. |
| `podAntiAffinity.enabled` | `true` | Prefer spreading operator replicas across nodes. |
| `nodeSelector`, `tolerations`, `podAnnotations` | empty | Scheduling and metadata. |

### Admission webhooks

With `operator.enableWebhooks: true` the operator validates:

- **HyperChain create and update**: every filter in `spec.filters` must exist.
- **Filter delete**: a filter that a HyperChain references cannot be deleted.
- **HyperChain delete**: a chain that a HyperConfig uses as `spec.defaultChain` cannot be deleted.

The webhooks use `failurePolicy: Fail`, so the operator must be running for these operations to succeed.

Without cert-manager, either set `certManager.enabled=false` and provide the `webhook-server-cert` Secret (keys `tls.crt` and `tls.key`, valid for `<release>.<namespace>.svc`) plus the CA bundle in the `ValidatingWebhookConfiguration` yourself, or turn the webhooks off with `operator.enableWebhooks=false`. Without webhooks, a HyperChain that references a missing filter is still accepted by the API server; the operator then marks it `Degraded` and compiles it to a chain that rejects requests with `503` (see [Failure modes](../concepts/failure-modes.md)).

## Upgrading the CRDs

The CRDs live in the chart's `crds/` directory. Helm installs them on the first `helm install` but never upgrades or deletes them. After upgrading the chart, apply the CRDs from the same version:

```bash
kubectl apply --server-side -f charts/hyper-operator/crds/
```

See [Operations](../reference/operations.md#upgrading) for the full upgrade order.

## Running the engine without the operator

The `hypergate-engine` chart deploys the engine as a DaemonSet (or a Deployment with `workload.kind: Deployment`), a Service and, by default, a bootstrap ConfigMap. The engine reads its configuration with `CONFIG_PROVIDER=K8S` from that ConfigMap and hot-reloads it when it changes.

```bash
helm install hypergate ./charts/hypergate-engine \
  --namespace hypergate --create-namespace \
  --set-file initialConfig.content=./config.yaml
```

The file you pass is the engine configuration described in [Engine configuration](../reference/engine-configuration.md). Relevant values:

| Value | Default | Description |
| --- | --- | --- |
| `workload.kind` | `DaemonSet` | `DaemonSet` or `Deployment`. |
| `replicaCount` | `1` | Replicas when `workload.kind` is `Deployment`. |
| `image.repository` / `image.tag` | `ghcr.io/taha2samy-3/hyper-engine` / chart `appVersion` | Engine image. |
| `engine.configProvider` | `K8S` | `K8S`, `FILE` or `URL`. With `K8S` the chart also creates a Role that can `get` and `watch` the ConfigMap. |
| `engine.configMapName` | `hyper-engine-config` | ConfigMap read with the `K8S` provider (key `config.yaml`). |
| `initialConfig.create` | `true` | Create the ConfigMap from `initialConfig.content`. It carries `helm.sh/resource-policy: keep`. |
| `initialConfig.content` | a minimal config with an empty `public` chain | Engine YAML. |
| `engine.tls.enabled`, `.secretName`, `.mutualTLS`, `.caSecretName` | off | Mount a TLS Secret at `/etc/hypergate/tls` and a CA Secret at `/etc/hypergate/ca`. |
| `service.port`, `service.appProtocol`, `service.trafficDistribution` | `9001`, `kubernetes.io/h2c`, `PreferSameNode` | gRPC Service. |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | empty | For example `CONFIG_RELOAD_TOKEN`, or secret files referenced from the config. |
| `resources`, probes, security contexts, scheduling | see `values.yaml` | Probes target `/healthz` and `/readyz` on the `health` port (9003). |

:::note
The engine reads TLS settings only from `server.tls` in its configuration. With `engine.tls.enabled` the chart mounts the certificates, and you point the configuration at them, for example `cert_file: /etc/hypergate/tls/tls.crt` and `key_file: /etc/hypergate/tls/tls.key`. The `engine.address`, `engine.maxConcurrentStreams` and pool values in `values.yaml` are not injected into the configuration either; set the corresponding `server.*` fields in `initialConfig.content`.
:::
