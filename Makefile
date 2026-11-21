CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
KUBECONFORM ?= go run github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0
# Cilium CRD schemas, from the community catalog kubeconform documents.
CILIUM_SCHEMAS = https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json

CLUSTER ?= sre-lab
# The image is tagged with the chart appVersion, the tag the chart uses by default,
# so a plain helm install finds the image built and loaded here.
VERSION := $(shell awk -F'"' '/^appVersion:/ { print $$2 }' charts/tyk-sre/Chart.yaml)
IMAGE := tyk-sre:$(VERSION)
CILIUM_VERSION ?= 1.17.6 # enableDefaultDeny needs at least 1.15

.PHONY: generate
generate: ## Generate the deepcopy code and the CRD manifest from the API types.
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=deploy/crd

.PHONY: chart-lint
chart-lint: ## Lint the chart and validate its rendered manifests against the Kubernetes and Cilium schemas.
	helm lint --strict charts/tyk-sre
	helm template tyk-sre charts/tyk-sre --namespace tyk-sre \
		| $(KUBECONFORM) -strict -summary -schema-location default -schema-location '$(CILIUM_SCHEMAS)'

.PHONY: lab-up
lab-up:
	## Create the kind cluster
	@for b in docker kind kubectl helm; do command -v $$b >/dev/null || { echo "missing: $$b"; exit 1; }; done

	kind get clusters 2>/dev/null | grep -qx "$(CLUSTER)" || kind create cluster --name "$(CLUSTER)" --config lab/kind.yaml
	kubectl config use-context "kind-$(CLUSTER)"

	## Install Cilium
	helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
	helm repo update cilium >/dev/null
	helm upgrade --install cilium cilium/cilium --version "$(strip $(CILIUM_VERSION))" -n kube-system \
		--set kubeProxyReplacement=true \
		--set k8sServiceHost="$(CLUSTER)-control-plane" \
		--set k8sServicePort=6443 \
		--set ipam.mode=kubernetes \
		--set image.pullPolicy=IfNotPresent \
		--set operator.replicas=1 \
		--wait

	kubectl -n kube-system rollout status ds/cilium --timeout=300s
	kubectl wait --for=condition=Ready nodes --all --timeout=300s

.PHONY: lab-down
lab-down: ## Delete the kind cluster.
	kind delete cluster --name "$(CLUSTER)"

.PHONY: demo-init
demo-init:
	kubectl config use-context kind-sre-lab
	kubectl apply --server-side -f deploy/crd/sre.tyk.io_isolations.yaml
	kubectl wait --for=condition=Established crd/isolations.sre.tyk.io
	docker build -t $(IMAGE) .
	kind load docker-image $(IMAGE) --name sre-lab

	kubectl create namespace tyk-sre --dry-run=client -o yaml | kubectl apply -f -
	kubectl label namespace tyk-sre pod-security.kubernetes.io/enforce=restricted --overwrite
	helm upgrade --install tyk-sre charts/tyk-sre --namespace tyk-sre --take-ownership
	kubectl -n tyk-sre rollout status deployment/tyk-sre

	## Deploy the sample workloads.
	kubectl apply -f lab/workloads/
	for ns in tenant-a tenant-b shared hardening-a; do \
		kubectl -n $$ns wait --for=condition=Available deploy --all --timeout=300s || exit 1; \
	done
	kubectl -n hardening-b rollout status statefulset/redis --timeout=300s
	kubectl -n hardening-b rollout status daemonset/node-agent --timeout=300s

	@# Force to Create/Recreate workloads
	kubectl delete -f lab/workloads/hardening.yaml -f lab/workloads/isolation.yaml \
		--ignore-not-found --force --grace-period=0 --wait
	kubectl apply -f ./lab/workloads/
	kubectl -n hardening-a wait --for=condition=Available deploy --all --timeout=300s
	kubectl -n hardening-b rollout status statefulset/redis --timeout=300s
	kubectl -n hardening-b rollout status daemonset/node-agent --timeout=300s

.PHONY: demo-isolation
demo-isolation:
	@./lab/demo/isolation.sh

.PHONY: demo-hardening
demo-hardening:
	@./lab/demo/hardening.sh

.PHONY: test
test:
	go test -cover ./...
