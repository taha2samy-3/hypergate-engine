---
sidebar_position: 2
---

# Firewall (WAF)

Security is paramount. But running a full Web Application Firewall (WAF) directly inside the API Gateway's Go process can be risky and incredibly memory-intensive.

Hypergate solves this elegantly using the same **Unix Domain Socket (UDS) Sidecar pattern** it uses for External Auth.

## Injecting a WAF

When you define a `FilterFirewall`, Hypergate injects a custom Firewall image into the Gateway Pod. The traffic is paused by Envoy, routed to Hypergate, and then streamed over the UDS directly to your WAF sidecar. If the WAF blocks it, the Engine immediately instructs Envoy to drop the connection.

## Example

```yaml
apiVersion: hypergate.com/v1alpha1
kind: FilterFirewall
metadata:
  name: modsecurity-waf
spec:
  rulesConfigMap: "my-waf-rules" # The operator automatically mounts this ConfigMap!
  container:
    image: "my-org/coraza-waf-sidecar:latest"
    socketEnvKey: "WAF_LISTEN_SOCKET"
```

### Automatic ConfigMap Mounting!
Notice the `rulesConfigMap` field? The Hypergate Operator is smart. If you provide a ConfigMap containing your ModSecurity or Coraza rules, the Operator will automatically mount that ConfigMap inside the firewall sidecar container. 

You don't have to touch Kubernetes Deployments or manually mess with VolumeMounts. The Operator handles everything seamlessly.
