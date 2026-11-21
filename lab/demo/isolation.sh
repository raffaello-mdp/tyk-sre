#!/bin/bash

probe() {
  local service=${3%%.*} rest=${3#*.}
  local destination="${rest%%:*}/$service:${rest##*:}"
  if kubectl exec -n "$1" "deploy/$2" -c app -- /agnhost connect --timeout=3s "$3" >/dev/null 2>&1
  then effect=$'\e[1;32mAllowed\e[0m'; else effect=$'\e[1;31mBlocked\e[0m'; fi
  echo "- $1/$2 -> $destination: $effect"
}

check() {
  probe tenant-a gateway dashboard.tenant-b:8080
  probe tenant-b dashboard gateway.tenant-a:8080
  probe tenant-a not-isolated dashboard.tenant-b:8080
  probe tenant-a gateway echo.shared:8080
  probe tenant-b not-isolated dashboard.tenant-b:8080
}

# Validate open connections before isolation is tunred on
# Open an long lived connection in the background inside tenant-a/gateway.
# The target of the connection is tenant-b/dashboard
CONNECTION_LOG=$(mktemp)
CONNECTION_PID=""
trap '[ -n "$CONNECTION_PID" ] && kill "$CONNECTION_PID" 2>/dev/null; rm -f "$CONNECTION_LOG"' EXIT

open_connection() {
  kubectl -n tenant-a exec -i deploy/gateway -c app -- bash -s dashboard.tenant-b 8080 90 \
    < lab/demo/long-lived-connection.sh > "$CONNECTION_LOG" 2>&1 &
  CONNECTION_PID=$!
  for _ in $(seq 15); do grep -q " 200 " "$CONNECTION_LOG" && return 0; sleep 1; done
  kill "$CONNECTION_PID" 2>/dev/null
  if [ -s "$CONNECTION_LOG" ]; then
    echo "[LLC] the connection could not be opened:"; cat "$CONNECTION_LOG"
  else
    echo "[LLC] the connection could not be opened: no answer from tenant-b/dashboard:8080 (is an Isolation already active? kubectl get isolations)"
  fi
  return 1
}

check_open_connection() {
  local after failure answered
  after=$(tail -n +"$(($1 + 1))" "$CONNECTION_LOG")
  failure=$(grep -E "no response|connection closed" <<<"$after")
  answered=$(grep -c " 200 " <<<"$after")
  if [ -n "$failure" ]; then
    effect=$'\e[1;31mBlocked\e[0m'" ($failure)"
  elif [ "$answered" -gt 0 ]; then
    effect=$'\e[1;32mAllowed\e[0m'" ($answered requests answered after the isolation)"
  else
    effect="Unknown (no request sent since the isolation)"
  fi
  echo "[LLC] - tenant-a/gateway -> tenant-b/dashboard:8080, opened before the isolation: $effect"
}

# Start without an isolation: a previous run ends with inc-1234 applied again.
kubectl delete isolation inc-1234 --ignore-not-found --wait
kubectl wait --for=delete ciliumclusterwidenetworkpolicy/isolation-inc-1234 --timeout=60s >/dev/null 2>&1

# Baseline
echo "🗺️  Sample workloads considered by this demo:"
for ns in tenant-a tenant-b; do
  kubectl -n "$ns" get deployments,statefulsets,daemonsets \
    -o jsonpath='{range .items[*]}- {.metadata.namespace}/{.metadata.name}{"\n"}{end}'
done

echo ""
echo "🧐 Pre-existing tenant policy:"
echo "tenant-b/dashboard only accepts traffic from tenant-a and shared"
echo "tenant-b/not-isolated -> tenant-b/dashboard is blocked before, during and after an isolation."

echo ""
echo "✨ Running checks:"
echo "- expecting only tenant-b/not-isolated -> tenant-b/dashboard to be blocked"
check

echo ""
echo "🔌 Open a connection tenant-a/gateway -> tenant-b/dashboard and keep it open"
echo "- one request per second over the same connection, through the isolation"
echo "Note that logs for Long Lived Connection are prefixed with \"[LLC]\" to be easily searchable"
open_connection && echo "[LLC] $(tail -n 1 $CONNECTION_LOG)"

echo ""
echo "🏝️  Create an Isolation between tenant-a/gateway -> tenant-b/dashboard"

requests_before_isolation=$(wc -l < "$CONNECTION_LOG")
kubectl apply -f lab/isolation-sample.yaml
kubectl wait isolation/inc-1234 --for=condition=Enforced --timeout=30s
kubectl get isolations
kubectl get ciliumclusterwidenetworkpolicies

echo "✨ Running checks:"
echo "- expecting tenant-b/not-isolated -> tenant-b/dashboard to be blocked"
echo "- expecting tenant-a/gateway <-> tenant-b/dashboard to be blocked"
echo "- reporting whether the connection opened before the isolation still works"
check
check_open_connection "$requests_before_isolation"
kill "$CONNECTION_PID" 2>/dev/null

echo ""
echo "✨ Self-healing: after removing the cilium policy, it comes back within seconds"
kubectl delete ciliumclusterwidenetworkpolicy isolation-inc-1234
kubectl wait --for=create ciliumclusterwidenetworkpolicy/isolation-inc-1234 --timeout=30s

echo "✨ Running checks:"
echo "- expecting cilium policy to be listed"
kubectl get ciliumclusterwidenetworkpolicies

echo ""
echo "‼️  Unsafe edit: overlap side b and side a"
kubectl patch isolation inc-1234 --type=json -p \
  '[{"op":"replace","path":"/spec/b","value":{"namespaces":["tenant-a"],"selector":{}}}]'
kubectl wait isolation/inc-1234 --for=jsonpath='{.status.conditions[?(@.type=="Enforced")].reason}'=LastValidPolicyKept --timeout=30s

echo "✨ Running checks:"
echo "- expecting Isolation status to be LastValidPolicyKept. The controller should keep the previous version as this is not valid"
echo "- expecting tenant-a/gateway <-> tenant-b/dashboard to be blocked"
echo ""

kubectl get isolations
probe tenant-a gateway dashboard.tenant-b:8080
probe tenant-b dashboard gateway.tenant-a:8080

echo "🛠️ Fix the spec reverting the sample isolation"
echo "✨ Running checks:"
echo "- expecting Isolation status to be PolicyApplied. The controller should apply the valid version"
echo ""

kubectl apply -f lab/isolation-sample.yaml
kubectl wait isolation/inc-1234 --for=jsonpath='{.status.conditions[?(@.type=="Enforced")].reason}'=PolicyApplied --timeout=30s
kubectl get isolations

echo ""
echo "🛑 Stop the tool and delete the Isolation"
echo "- expecting the Cilium policy to not be recreated"
echo "- expecting only tenant-b/not-isolated -> tenant-b/dashboard to be blocked"
echo ""

kubectl -n tyk-sre scale deployment/tyk-sre --replicas=0

for _ in $(seq 60); do
  [ -z "$(kubectl -n tyk-sre get pods -l app.kubernetes.io/name=tyk-sre -o name)" ] && break
  sleep 1
done
if [ -n "$(kubectl -n tyk-sre get pods -l app.kubernetes.io/name=tyk-sre -o name)" ]; then
  echo "the tool pod is still running after 60s: the checks below would not prove anything"
  exit 1
fi

kubectl delete isolation inc-1234
kubectl wait --for=delete ciliumclusterwidenetworkpolicy/isolation-inc-1234 --timeout=60s
kubectl get ciliumclusterwidenetworkpolicies
echo "✨ Running checks:"
check

echo ""
echo "💫 Bring the Isolation"
echo "- expecting the Cilium policy to not be recreated"
kubectl apply -f lab/isolation-sample.yaml
kubectl get isolations


echo ""
echo "💫 Bring the tool back"
echo "- expecting the Cilium policy to be recreated"

kubectl -n tyk-sre scale deployment/tyk-sre --replicas=1
kubectl -n tyk-sre rollout status deployment/tyk-sre --timeout=120s
kubectl wait --for=create ciliumclusterwidenetworkpolicy/isolation-inc-1234 --timeout=60s
kubectl get ciliumclusterwidenetworkpolicies
