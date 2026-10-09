#!/usr/bin/env bash
# End-to-end test of workload identity and identity-aware routing on kind.
# Expects a cluster with cert-manager, Envoy Gateway, the hyper-operator chart
# (test/e2e/kind/operator-values.yaml) and the manifests in this directory.
# Run by .github/workflows/e2e-kind.yaml; writes a summary to $GITHUB_STEP_SUMMARY.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
OP_NS=${OP_NS:-hyper-operator-system}
ENGINE_NS=hyper-system
LEASE=hyper-operator.hyper.io
SLICE=hyper-operator-identity
SCALE_PODS=${SCALE_PODS:-150}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
declare -a RESULTS=()

log() { echo -e "\n==> $*"; }
now_ms() { date +%s%3N; }
record() { RESULTS+=("| $1 | $2 | $3 |"); echo "    $1: $2 ($3)"; }

dump() {
  echo "::group::Diagnostics"
  kubectl get pods -A -o wide || true
  kubectl get hyperconfig main -o yaml || true
  kubectl get hyperroutes,hyperchains || true
  kubectl -n edge get envoyextensionpolicy -o yaml || true
  kubectl -n "$OP_NS" get lease "$LEASE" -o yaml || true
  kubectl -n "$OP_NS" get endpointslice "$SLICE" -o yaml || true
  kubectl -n "$OP_NS" logs -l app.kubernetes.io/name=hyper-operator --tail=200 --prefix || true
  kubectl -n "$ENGINE_NS" logs ds/hyper-engine --tail=200 || true
  echo "::endgroup::"
}
fail() { echo "::error::$*"; dump; write_summary "failed: $*"; exit 1; }

write_summary() {
  {
    echo "## Identity end-to-end (kind)"
    echo
    echo "Result: ${1:-passed}"
    echo
    echo "| Check | Result | Detail |"
    echo "| --- | --- | --- |"
    printf '%s\n' "${RESULTS[@]}"
  } >> "$SUMMARY"
}

# wait_until <description> <timeout seconds> <command...>
# On timeout the output of the last attempt is printed.
wait_until() {
  local desc=$1 timeout=$2; shift 2
  local deadline=$(( $(date +%s) + timeout )) out
  until out=$("$@" 2>&1); do
    if (( $(date +%s) >= deadline )); then
      echo "Last attempt output:"; echo "$out" | tail -20
      fail "timed out after ${timeout}s waiting for: $desc"
    fi
    sleep 1
  done
}

leader_pod() { kubectl -n "$OP_NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1; }
pod_ip() { kubectl -n "$1" get pod "$2" -o jsonpath='{.status.podIP}'; }
slice_ip() { kubectl -n "$OP_NS" get endpointslice "$SLICE" -o jsonpath='{.endpoints[0].addresses[0]}' 2>/dev/null; }
slice_ready() { [[ $(kubectl -n "$OP_NS" get endpointslice "$SLICE" -o jsonpath='{.endpoints[0].conditions.ready}' 2>/dev/null) == true ]]; }

# slice_points_at_leader succeeds when the EndpointSlice lists the current leader and is ready.
slice_points_at_leader() {
  local leader ip
  leader=$(leader_pod) && [[ -n $leader ]] || return 1
  ip=$(pod_ip "$OP_NS" "$leader") && [[ -n $ip ]] || return 1
  [[ $(slice_ip) == "$ip" ]] && slice_ready
}

pod_of() { kubectl -n "$1" get pod -l "app=$2" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}'; }

# engine_get <path>: GET on an engine's health port, from the checkout pod.
engine_get() {
  local ip
  ip=$(kubectl -n "$ENGINE_NS" get pod -l app=hyper-engine -o jsonpath='{.items[0].status.podIP}')
  kubectl -n shop exec "deploy/checkout" -- curl -s --max-time 5 "http://$ip:9003$1"
}
engine_synced() { [[ $(engine_get /debug/identity | jq -r '.synced and .connected') == true ]]; }
engine_knows() { engine_get "/debug/identity?ip=$1" | jq -e --arg ns "$2" '.namespace == $ns' >/dev/null; }

gateway_url() {
  local svc
  svc=$(kubectl -n envoy-gateway-system get svc \
    -l gateway.envoyproxy.io/owning-gateway-name=public,gateway.envoyproxy.io/owning-gateway-namespace=edge \
    -o jsonpath='{.items[0].metadata.name}')
  echo "http://$svc.envoy-gateway-system.svc.cluster.local/"
}

# call <namespace> <pod> -> "<status> <x-hypergate-chain header>"
call() {
  local out status chain
  out=$(kubectl -n "$1" exec "$2" -- curl -s -i --max-time 5 "$GATEWAY_URL" 2>/dev/null) || { echo "000 -"; return; }
  status=$(head -1 <<<"$out" | awk '{print $2}')
  chain=$(grep -i '^x-hypergate-chain:' <<<"$out" | awk '{print $2}' | tr -d '\r')
  echo "${status:-000} ${chain:--}"
}
expect_call() { [[ $(call "$1" "$2") == "$3" ]]; }

check_routing() {
  local label=$1 checkout intruder outside
  checkout=$(pod_of shop checkout); intruder=$(pod_of shop intruder); outside=$(pod_of default outside)
  wait_until "checkout routed to its chain ($label)" 90 expect_call shop "$checkout" "200 checkout-to-ledger"
  record "Routing $label: checkout (service:/sa:)" "200, checkout-to-ledger chain" "east-west inferred from the identity map"
  wait_until "intruder denied ($label)" 60 expect_call shop "$intruder" "403 -"
  record "Routing $label: other pod" "403" "east-west default chain denies"
  wait_until "node-address client gets public ($label)" 60 expect_call default "$outside" "200 public"
  record "Routing $label: hostNetwork client" "200, public chain" "node address is north-south"
}

# measure_failover <label> <budget seconds> <kubectl delete args...>
measure_failover() {
  local label=$1 budget=$2; shift 2
  local old old_ip start elapsed
  old=$(leader_pod); old_ip=$(pod_ip "$OP_NS" "$old")
  start=$(now_ms)
  kubectl -n "$OP_NS" delete pod "$old" --wait=false "$@" >/dev/null
  local deadline=$(( $(date +%s) + budget + 60 ))
  while true; do
    local ip; ip=$(slice_ip || true)
    if [[ -n $ip && $ip != "$old_ip" ]] && slice_ready && slice_points_at_leader; then break; fi
    (( $(date +%s) < deadline )) || fail "$label: the identity Service never moved to a new leader"
    sleep 0.5
  done
  elapsed=$(( $(now_ms) - start ))
  local secs; secs=$(awk "BEGIN{printf \"%.1f\", $elapsed/1000}")
  (( elapsed <= budget * 1000 )) || fail "$label: takeover took ${secs}s (budget ${budget}s)"
  record "Failover: $label" "${secs}s" "new leader $(leader_pod), budget ${budget}s"
  wait_until "engines reconnected after $label" 60 engine_synced
}

# measure_crash <budget seconds>: kills the leader's container with SIGKILL from
# the node (no SIGTERM, so the Lease is not released) and measures the takeover.
measure_crash() {
  local budget=$1 holder pod node cid start elapsed secs
  holder=$(kubectl -n "$OP_NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}')
  pod=${holder%%_*}
  node=$(kubectl -n "$OP_NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')
  cid=$(docker exec "$node" crictl ps -q --label "io.kubernetes.pod.name=$pod" --label io.kubernetes.container.name=manager)
  [[ -n $cid ]] || fail "crash: leader container of $pod not found on $node"
  start=$(now_ms)
  docker exec "$node" crictl stop --timeout 0 "$cid" >/dev/null
  local deadline=$(( $(date +%s) + budget + 60 ))
  until [[ $(kubectl -n "$OP_NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}') != "$holder" ]] && slice_points_at_leader; do
    (( $(date +%s) < deadline )) || fail "crash: no new leader took over"
    sleep 0.5
  done
  elapsed=$(( $(now_ms) - start ))
  secs=$(awk "BEGIN{printf \"%.1f\", $elapsed/1000}")
  (( elapsed >= 5000 )) || fail "crash: takeover in ${secs}s is too fast for an expired Lease; the kill was not a crash"
  (( elapsed <= budget * 1000 )) || fail "crash: takeover took ${secs}s (budget ${budget}s)"
  record "Failover: crash (SIGKILL)" "${secs}s" "new leader $(leader_pod), Lease expiry, budget ${budget}s"
  wait_until "engines reconnected after the crash" 90 engine_synced
}

# ---------------------------------------------------------------------------

log "Operator leadership and the identity Service"
kubectl -n "$OP_NS" rollout status deploy -l app.kubernetes.io/name=hyper-operator --timeout=180s >/dev/null ||
  kubectl -n "$OP_NS" wait --for=condition=Available deploy --all --timeout=180s
wait_until "a leader holds the Lease" 120 leader_pod
wait_until "the identity EndpointSlice points at the leader" 120 slice_points_at_leader
record "Leader EndpointSlice" "ok" "$(slice_ip) = $(leader_pod)"

log "Workloads and policy"
kubectl apply -f "$HERE/manifests/workloads.yaml" >/dev/null
kubectl apply -f "$HERE/manifests/gateway.yaml" >/dev/null
for d in payments/ledger shop/checkout shop/intruder default/outside; do
  kubectl -n "${d%/*}" rollout status "deploy/${d#*/}" --timeout=180s >/dev/null
done
# The webhook must be serving before HyperChains are created.
wait_until "policy accepted" 120 kubectl apply -f "$HERE/manifests/hypergate.yaml"

log "Engine and Envoy Gateway attachment"
wait_until "engine DaemonSet created" 120 kubectl -n "$ENGINE_NS" get ds hyper-engine
kubectl -n "$ENGINE_NS" rollout status ds/hyper-engine --timeout=240s >/dev/null ||
  fail "engines never became ready (readiness waits for the identity map)"
record "Engine readiness" "ok" "ready only after the first identity map"
wait_until "EnvoyExtensionPolicy generated" 120 kubectl -n edge get envoyextensionpolicy hypergate-public
wait_until "EnvoyExtensionPolicy accepted by Envoy Gateway" 180 bash -c \
  "[[ \$(kubectl -n edge get envoyextensionpolicy hypergate-public -o jsonpath='{.status.ancestors[0].conditions[?(@.type==\"Accepted\")].status}') == True ]]"
record "EnvoyExtensionPolicy" "Accepted" "generated from extProc.gateways"
kubectl -n edge wait --for=condition=Programmed gateway/public --timeout=180s >/dev/null || fail "Gateway not programmed"
GATEWAY_URL=$(gateway_url)

log "Engine identity map"
wait_until "engine synced and connected" 120 engine_synced
checkout_ip=$(pod_ip shop "$(pod_of shop checkout)")
wait_until "engine knows the checkout pod" 60 engine_knows "$checkout_ip" shop
detail=$(engine_get "/debug/identity?ip=$checkout_ip" | jq -c '{serviceAccount, services, spiffeID}')
engine_get "/debug/identity?ip=$checkout_ip" | jq -e '.serviceAccount == "checkout" and (.services | index("shop/checkout"))' >/dev/null ||
  fail "unexpected checkout identity: $detail"
record "Engine identity lookup" "ok" "$detail"

log "Identity-aware routing through Envoy Gateway"
check_routing "before failover"

log "Graceful leader change (rolling restart, Lease released)"
measure_failover "graceful stop" 15

log "New workloads reach engines through the new leader"
kubectl -n shop scale deploy/checkout --replicas=2 >/dev/null
kubectl -n shop rollout status deploy/checkout --timeout=120s >/dev/null
newest=$(kubectl -n shop get pod -l app=checkout --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
start=$(now_ms)
wait_until "engine learns the new checkout pod" 60 engine_knows "$(pod_ip shop "$newest")" shop
record "New pod propagation" "$(( $(now_ms) - start )) ms" "after it was Ready, via the new leader"
wait_until "new checkout pod routed to its chain" 60 expect_call shop "$newest" "200 checkout-to-ledger"

log "Leader crash (process killed without SIGTERM, Lease must expire)"
measure_crash 45
check_routing "after failovers"

log "Scale: $SCALE_PODS pods"
before=$(engine_get /debug/identity | jq -r .workloads)
kubectl create deployment scale --image=registry.k8s.io/pause:3.10 --replicas="$SCALE_PODS" -n default >/dev/null
start=$(now_ms)
kubectl -n default rollout status deploy/scale --timeout=300s >/dev/null || fail "scale pods did not start"
ready_at=$(now_ms)
wait_until "engine sees $SCALE_PODS more workloads" 120 bash -c \
  "(( \$(kubectl -n shop exec deploy/checkout -- curl -s http://$(kubectl -n "$ENGINE_NS" get pod -l app=hyper-engine -o jsonpath='{.items[0].status.podIP}'):9003/debug/identity | jq -r .workloads) >= $before + $SCALE_PODS ))"
record "Scale propagation" "$(( $(now_ms) - ready_at )) ms after Ready" "$SCALE_PODS pods, $(( ready_at - start )) ms to schedule; engine now has $(engine_get /debug/identity | jq -r .workloads) workloads"
mem=$(kubectl -n "$OP_NS" top pod -l app.kubernetes.io/name=hyper-operator --no-headers 2>/dev/null | awk '{print $1": "$3}' | paste -sd ' ' || true)
record "Operator memory" "${mem:-n/a}" "kubectl top (metrics-server may be absent)"

write_summary passed
log "All identity end-to-end checks passed"
printf '%s\n' "${RESULTS[@]}"
