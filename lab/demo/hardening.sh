#!/bin/bash

# Hardening
echo ""
echo "Harden the workloads of hardening-a and hardening-b through the API"

API_PORT=18080
kubectl -n tyk-sre port-forward svc/tyk-sre "$API_PORT:8080" >/dev/null 2>&1 &
PORT_FORWARD_PID=$!
RESPONSES=$(mktemp -d)
trap 'kill $PORT_FORWARD_PID 2>/dev/null; rm -rf "$RESPONSES"' EXIT

for _ in $(seq 30); do curl -s -o /dev/null "localhost:$API_PORT/hardening" && break; sleep 1; done
curl -s -o /dev/null "localhost:$API_PORT/hardening" || { echo "the hardening API is not reachable"; exit 1; }

harden() {
  curl -s -X POST "localhost:$API_PORT/hardening$1" \
    -d '{"namespaces":["hardening-a","hardening-b"]}'
}

# Statuses in bold color: Patched green, Unchanged cyan, Skipped yellow, Failed red.
summary() {
  jq -r 'def highlight: {"Patched": "32", "Unchanged": "36", "Skipped": "33", "Failed": "31"}[.] as $color
      | if $color then "\u001b[1;\($color)m\(.)\u001b[0m" else . end;
    .namespaces[] | .namespace as $ns
    | if .workloads then .workloads[]
    | "- \($ns)/\(.kind)/\(.name): \(.status | highlight) \(.reason // "") \((.notes // []) | join("; "))"
      else "- \($ns): \(.status | highlight) \(.reason // "")" end'
}

GREEN=$'\e[1;32m' RED=$'\e[1;31m' RESET=$'\e[0m'

qos_classes() {
  for ns in hardening-a hardening-b; do
    kubectl -n "$ns" get pods -o json | jq -r '.items[] | select(.metadata.deletionTimestamp == null)
      | "\(.metadata.namespace)/\(.metadata.labels.app) \(.status.qosClass)"'
  done | sort -u
}

no_cpu_limit_added() {
  local offenders
  offenders=$(jq -r '.namespaces[] | .namespace as $ns | .workloads[]?
    | select(any(.changes.containers[]?; .limits.cpu != null)) | "\($ns)/\(.kind)/\(.name)"' "$1")
  if [ -z "$offenders" ]; then
    echo "- no CPU limit added: ${GREEN}OK${RESET}"
  else
    echo "- CPU limit added to: ${RED}${offenders//$'\n'/, }${RESET}"
  fi
}

rollout_failures=0
rollout_result() {
  if kubectl -n "$1" rollout status "$2" --timeout=180s >/dev/null 2>&1; then
    echo "- $1/$2: ${GREEN}Rolled out${RESET}"
  else
    echo "- $1/$2: ${RED}Not ready after 180s${RESET} (kubectl -n $1 get pods)"
    rollout_failures=$((rollout_failures + 1))
  fi
}

echo ""
echo "🌵 Showcase Harden dry run"
sleep 2
echo "🌵 Dry run:"
echo "- expecting every workload, excluding hardened and opted-out, to be Patched (would be patched)"
echo "- expecting hardened to be Unchanged and opted-out to be Skipped"
echo "- expecting privileged to keep its securityContext (see its note) and sys-admin to get no allowPrivilegeEscalation"
echo "- expecting guaranteed and partial to get no requests, since their limits already act as requests"
echo "- expecting no CPU limit anywhere"
echo "- expecting nothing to change in the cluster"
generation_before=$(kubectl -n hardening-a get deploy bare -o jsonpath='{.metadata.generation}')
qos_before=$(qos_classes)
echo ""
echo "🧐 List of workloads with status and reason (if any):"
harden "?dryRun" | tee "$RESPONSES/dry-run.json" | summary
echo ""
echo "🧐 Values the tool would add to each workload to be patched:"

jq -r '.namespaces[] | .namespace as $ns | .workloads[]? | select(.status == "Patched")
  | "\n\($ns)/\(.kind)/\(.name):", .changes' "$RESPONSES/dry-run.json"

echo ""
echo "🧐 Checks on the planned changes:"
no_cpu_limit_added "$RESPONSES/dry-run.json"
echo "- hardening-a/bare generation: $generation_before -> $(kubectl -n hardening-a get deploy bare -o jsonpath='{.metadata.generation}') (unchanged by the dry run)"

echo ""
echo "✨ Showcase Harden application"
sleep 2
echo "✨ Apply:"
echo "- expecting the same statuses as the dry run, and failed to be 0"
harden | tee "$RESPONSES/apply.json" | summary
jq -e '.failed == 0' "$RESPONSES/apply.json" >/dev/null && echo "failed: 0" || echo "failed: $(jq .failed "$RESPONSES/apply.json")"
no_cpu_limit_added "$RESPONSES/apply.json"

echo ""
echo "⏳ Waiting for the patched workloads to roll out..."
echo "- expecting every workload to roll out: the new pods pass their readiness probe"
echo "- including root-nginx and redis, which start as root, and privileged and sys-admin"
for ns in hardening-a hardening-b; do
  for workload in $(kubectl -n "$ns" get deployments,statefulsets,daemonsets -o name); do
    rollout_result "$ns" "$workload"
  done
done
if [ "$rollout_failures" -eq 0 ]; then
  echo "rollouts failed: 0"
else
  echo "rollouts failed: ${RED}$rollout_failures${RESET}"
fi

echo ""
echo "🧐 QoS class of the pods, before -> after:"
echo "- expecting guaranteed to stay Guaranteed: no request was added below its limits"
echo "- expecting the workloads without resources to go from BestEffort to Burstable,"
echo "  except opted-out and the standalone Pod, which the tool does not touch"
join <(echo "$qos_before") <(qos_classes) | while read -r app before after; do
  if [ "$before" = "$after" ]; then
    echo "- $app: $before"
  else
    echo "- $app: $before -> $after"
  fi
done
# A demotion from Guaranteed is the one change the tool must never cause.
if join <(echo "$qos_before") <(qos_classes) | awk '$2 == "Guaranteed" && $3 != "Guaranteed" { found = 1 } END { exit !found }'; then
  echo "${RED}a Guaranteed pod was demoted${RESET}"
else
  echo "- no Guaranteed pod demoted: ${GREEN}OK${RESET}"
fi

echo ""
echo "🧐 Values now on hardening-a/bare:"
kubectl -n hardening-a get deploy bare -o jsonpath='{.spec.template.spec.containers[0].resources}{"\n"}{.spec.template.spec.containers[0].securityContext}{"\n"}' | jq

echo ""
echo "✨ Showcase idempotency"
sleep 2
echo "✨ Apply again:"
echo "- expecting every workload to be Unchanged (opted-out still Skipped) because hardening is idempotent"
harden | summary
