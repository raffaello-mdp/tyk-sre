#!/bin/bash

probe() {
  if kubectl exec -n "$1" "deploy/$2" -c app -- /agnhost connect --timeout=3s "$3" >/dev/null 2>&1
  then echo "$1/$2 -> $3: ok"; else echo "$1/$2 -> $3: blocked"; fi
}

check() {
  probe tenant-a gateway dashboard.tenant-b:8080
  probe tenant-b dashboard gateway.tenant-a:8080
  probe tenant-a not-isolated dashboard.tenant-b:8080
  probe tenant-a gateway echo.shared:8080
  probe tenant-b not-isolated dashboard.tenant-b:8080
}


# Baseline
echo -e "In the cluster we are running:"
echo -e "\t - tenant-a/gateway"
echo -e "\t - tenant-a/not-isolated"
echo -e "\t - tenant-a/gateway"
echo -e "\t - tenant-b/dashboard"
echo -e "\t - tenant-b/not-isolated"

echo ""
echo "🧐 Pre-existing tenant policy:"
echo "tenant-b/dashboard only accepts traffic from tenant-a and shared"
echo "tenant-b/not-isolated -> tenant-b/dashboard is blocked before, during and after an isolation."

echo "Running checks:"
echo "- expecting only tenant-b/not-isolated -> tenant-b/dashboard to be blocked"
check

echo ""
echo "Create an Isolation netween tenant-a/gateway -> tenant-b/dashboard"

# Isolation on
kubectl apply -f lab/isolation-sample.yaml
kubectl get isolations
kubectl get ciliumclusterwidenetworkpolicies

echo "Running check:"
echo "- expecting tenant-b/not-isolated -> tenant-b/dashboard to be blocked"
echo "- expecting tenant-a/gateway <-> tenant-b/dashboard to be blocked"
check

echo "Self-healing: Remove the cilium policy. It comes back within seconds"
kubectl delete ciliumclusterwidenetworkpolicy isolation-inc-1234

echo "Running check:"
echo "- expecting cilium policy to be listed"
kubectl get ciliumclusterwidenetworkpolicies

echo "Unsafe edit: overlap side b and side a"
kubectl patch isolation inc-1234 --type=json -p \
  '[{"op":"replace","path":"/spec/b","value":{"namespaces":["tenant-a"],"selector":{}}}]'

echo "Running check:"
echo "- expecting Isolation status to be LastValidPolicyKept. The controller should keep the previous version as this is not valid"
echo "- expecting tenant-a/gateway <-> tenant-b/dashboard to be blocked"

kubectl get isolations
probe tenant-a gateway dashboard.tenant-b:8080
probe tenant-b dashboard gateway.tenant-a:8080

echo "Fix the spec"
echo "Running check:"
echo "- expecting Isolation status to be PolicyApplied. The controller should apply the valid version"

kubectl apply -f lab/isolation-sample.yaml
kubectl get isolations

echo "Stop the tool and delete the Isolation"
echo "- expecting the Cilium policy to not be recreated"
echo "- expecting only tenant-b/not-isolated -> tenant-b/dashboard to be blocked"

kubectl -n tyk-sre scale deployment/tyk-sre --replicas=0
kubectl delete isolation inc-1234
kubectl get ciliumclusterwidenetworkpolicies
check

# Bring the tool back
kubectl -n tyk-sre scale deployment/tyk-sre --replicas=1
