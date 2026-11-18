# tyk-sre — SRE automations for Kubernetes

> **Status: design phase.** This README currently holds the initial analysis and the decisions taken
> so far. Build/deploy/run instructions, test notes and limitations will be filled in as while working on the implementation.

A small Go service, deployed in-cluster, that lets an SRE:

1. **Isolate** two workloads from each other on demand (and lift the isolation), without
   affecting any of their other traffic.
2. **Harden** workloads in one or more namespaces that run without resource requests/limits or a
   hardened `securityContext`, with a preview of what would change before anything changes.

The original brief is in [`docs/`](docs/).

---

## 1. Requirements (as understood)

### 1.1 Network isolation

- **Input:** two sides, A and B, each defined by one or more namespaces plus a label selector.
- **Effect:** block all network traffic between A and B, in both directions.
- **Unaffected:** all other traffic of both workloads keeps working.
- **On demand:** isolation can be turned on and off again; turning it off must restore the
  workloads' *previous* connectivity, not just "allow everything".
- **Undo:** *(core)* it is what "off" means.

### 1.2 Workload hardening

- **Input:** one or more namespaces.
- **Effect:** patch workloads that run without resource requests and limits, or without a hardened
  `securityContext`.
- **Preview:** a way to see what would change before it changes (dry-run).
- **Safety:** patching must not break the workloads.
- **Undo:** *(bonus)* if not built, the approach is described in this README.

### 1.3 Deliverables

- One Go service, covering both features.
- Kubernetes manifests to deploy it into the cluster.
- A runnable way to show that isolation blocks traffic.
- Automated tests, including at least one tricky case, with the reasoning behind it.
- This README: set-up, build/deploy/run, decisions, and known limitations.

### 1.4 Evaluation focus

- Correct behaviour at the edges (especially turning isolation **off**
and patching **without breaking** workloads);
- Judgement on open decisions;
- Idiomatic use of the
Kubernetes API;
- Readable Go;
- Tests that genuinely exercise behaviour;
- Version control.

---

## 2. Architecture

One Go binary running in the cluster, built on `controller-runtime`. A single manager runs:

- an **`Isolation` controller**: isolation is declared as a custom resource and reconciled
  continuously;
- an **HTTP API** for hardening: preview and apply are synchronous requests;
- health probes *(required)*, metrics *(bonus)*.

### CRD for isolation and an API for hardening

- **Isolation is desired state** ("A and B stay apart until I say otherwise"). A custom resource
  gives, for free:
  - **Kubernetes RBAC and audit logging** on the dangerous operation: who may create or delete an
    `Isolation` is a plain RBAC rule, and every change is in the API server audit log. No custom
    authentication is needed for this critical operation.
  - **Visibility:** `kubectl get isolations` shows what is active, whether it is enforced, how many
    pods each side matches, and any warnings.
  - **An abstraction over the CNI:** operators deal with `Isolation`; the Cilium policy behind it is
    an implementation detail that could be swapped (e.g. for `AdminNetworkPolicy`) without changing
    the interface.
  - **Declarative operation:** it can be applied from Git, reviewed, and reverted like any other
    manifest.
- **Hardening is a one-shot action** ("patch these namespaces now, and show me first"). There is no
  desired state to keep converging, and a synchronous request can return the diff. 

  > With a CRD, dry-run would become a hand-rolled plan/apply protocol: create it with
  `mode: Plan`, poll `.status` for the computed changes, flip to `Apply`, and guard against the plan
  going stale in between. `kubectl apply --dry-run=server` on such a resource would only dry-run the
  resource itself, never the hardening.

The trade-off is two interaction models: operators use kubectl for isolation and HTTP for
hardening. A change that must persist is declared as a manifest, while a
one-off corrective operation is an imperative command.

### Isolation resource (draft)

`Isolation` is **cluster-scoped** (`sre.tyk.io/v1alpha1`), since a single isolation spans
namespaces.

```yaml
apiVersion: sre.tyk.io/v1alpha1
kind: Isolation
metadata:
  name: inc-1234
spec:
  reason: "INC-1234 suspected cross-tenant compromise"
  a:
    namespaces: [tenant-a]
    selector:
      matchLabels: { app: gateway }
  b:
    namespaces: [tenant-b]
    selector:
      matchLabels: { app: dashboard }
status:
  observedGeneration: 1
  conditions:
    - type: Enforced  # True once the network policy is in place
      status: "True"
      reason: PolicyApplied
  matchedPods: { a: 2, b: 2 }
  warnings: []
```

#### Operations:
- **On:** `kubectl apply -f isolation.yaml`
- **Status:** `kubectl get isolations`
- **Off:** `kubectl delete isolation inc-1234`

Structural validation (required fields, namespace and label syntax, A ≠ B) lives in the CRD's
OpenAPI schema and CEL rules, so malformed requests are rejected by the API server immediately.
Checks that need to look at the cluster (e.g. a pod matching both sides) run in the controller and
are reported through status conditions.

### Hardening API (draft)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/hardening?dryRun=true` | Return the per-workload changes without applying them |
| `POST` | `/hardening` | Apply them; returns per-workload results |
| `GET` | `/healthz`, `/readyz` | Probes |

Example request:

```json
{ "namespaces": ["tenant-a", "tenant-b"] }
```

### Access control

- **Isolation:** Kubernetes RBAC on the `isolations` resource.
- **Hardening API:** it restarts workloads, so it must not be openly reachable. It is exposed only
  through a `ClusterIP` Service and reached via `kubectl port-forward`; whether someone may
  port-forward is therefore decided by Kubernetes RBAC. In production this would be replaced by
  real authentication.

### About state and "not just on a good day"

The cluster is the source of truth; the service keeps no state of its own, so a restart or crash
loses nothing.

- Each `Isolation` owns exactly one network policy, linked by an **ownerReference**.
- **An active isolation stays enforced while the tool is down**.
- **Turning isolation off also works while the tool is down:** deleting the `Isolation` lets the
  Kubernetes GC delete the policy it owns, without the controller.

---

## 3. Network isolation — decisions

### Mechanism: explicit deny rules (Cilium), not standard `NetworkPolicy`

Standard `NetworkPolicy` is allow-only and additive:

- once a policy selects a pod, all traffic not explicitly allowed is dropped, so "deny only A↔B"
  has to be expressed as "allow everything except B". Doable in a lab but hard to maintain in Production;
- Also, if a tenant already has a policy that **allows** B→A, nothing additive can take
  that away.

So the lab replaces kind's default CNI (kindnet) with **Cilium**, and for each `Isolation` the
controller creates one `CiliumClusterwideNetworkPolicy` containing two rule sets (A's and B's), each with
`ingressDeny`/`egressDeny` against the other side:

- **Enforced on both ends**, so neither side can bypass it from its own end.
- **`enableDefaultDeny: {ingress: false, egress: false}`** — without it, a deny-only policy would
  still switch the selected pods into default-deny and cut all their other traffic.
- **Deny beats allow** in Cilium, so a tenant's own `NetworkPolicy` cannot re-open A↔B.
- **Nothing existing is modified.** Turning isolation off means deleting the one policy the
  `Isolation` owns, which restores the previous connectivity *exactly*.

### Pods coming and going

Rules match pods by label, so new replicas and restarted pods are covered automatically, without the
controller having to act. On top of that, the controller:

- **watches the policies it owns** and recreates or corrects one if someone deletes or edits it;
- **watches pods** in the selected namespaces, re-runs the safety checks and refreshes
  `status.matchedPods` and `status.warnings`;
- rebuilds its view from the cluster on startup.

### Changing an active isolation

The spec is **mutable**: editing an `Isolation` updates its policy in place, in a single write.
Making it immutable would force delete-and-recreate to change a side, which opens a window during an
incident in which the two workloads can talk again.

If an edit fails the safety checks (e.g. it would make a pod match both sides), the controller
**keeps enforcing the last valid policy** and reports the edit as rejected in status. Failing open
(removing the policy because the new spec is invalid) would be the wrong default for containment.

### Edge cases to handle (and test)

- **A pod matching both sides** → refuse to create the policy and report it in status: the pod would
  be cut off from its own peers. On an edit, the previous policy stays (see above).
- **A pod starting to match both sides later** (labels change) → the policy stays, a warning is
  reported.
- **A side matching no pods** → allow (it covers future pods) but warn: often a typo.
- **Empty selector** → the whole namespace(s); visible in `status.matchedPods`.
- **Overlapping isolations** → one policy per `Isolation`, so deleting one never lifts another.
- **Deleting twice / an unknown name** → standard Kubernetes semantics (`NotFound`); nothing to undo.
- **Ownership** → the controller only manages policies it owns (ownerReference + label); it never
  adopts or deletes anything else.
- **Cilium not installed** → the `Isolation` is reported as not enforced, with the reason, instead of
  the controller crash-looping.
- **`hostNetwork` pods** → not Cilium endpoints, therefore not covered; warn.
- **Connections already open before "on"** → may survive depending on CNI connection tracking;
  verified by the test script and documented.

### Uninstalling lifts every isolation

Deleting the `Isolation` CRD deletes every `Isolation`, and the GC then deletes every
policy they own: uninstalling the tool would silently end every active containment. Mitigations:

- the CRD is shipped separately from the tool's deployment manifests, so removing the tool does not
  remove the CRD;
- the risk is documented in the uninstall instructions (check `kubectl get isolations` first).

---

## 4. Workload hardening — decisions

### What "hardened" means

The reference is the **Pod Security Standards `restricted` profile**, an external and widely
understood baseline rather than a home-grown list:

- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `runAsNonRoot: true`
- `seccompProfile.type: RuntimeDefault`

`readOnlyRootFilesystem: true` is treated as an additional, opt-in setting.

### Not breaking workloads

These settings have a known way of breaking a running application:

| Setting | Typical breakage |
|---|---|
| `runAsNonRoot` | Image runs as root → `CreateContainerConfigError` (e.g. official redis/nginx) |
| `readOnlyRootFilesystem` | App writes to its filesystem (temp files, caches, pid files) |
| `capabilities.drop: [ALL]` | App binds a port < 1024 or `chown`s at start-up |
| Memory limit | Set too low → OOMKilled |

Approach:

- Evaluation is based on the **pod template**.
- **Fill only what is missing.** Existing values are never overridden.
- **Tiered settings.** Low-risk fields (`allowPrivilegeEscalation`, `seccompProfile`) are applied
  by default. Higher-risk fields are applied only when there is evidence they are safe (e.g. a
  non-zero `runAsUser` is already set) or when the operator opts in explicitly.
- **Controlled rollout.** Workloads are patched one at a time and the rollout is watched; a rollout
  that fails to progress is reported (and, with undo, rolled back) before moving on.
- **Bookkeeping on the workload, not the pod template**, so the tool's annotations do not trigger
  extra restarts.

### Requests and limits

- Set CPU/memory **requests** and a **memory limit** where missing.
- **No CPU limit by default**: CPU limits cause throttling.
- Defaults are configurable. Existing `LimitRange`s in the namespace are reported, since they can
  inject values at admission time.

### Scope

- **In Scope:** Deployments, StatefulSets, DaemonSets.
- **Out of Scope:** bare Pods, Jobs, ReplicaSets owned
  by a Deployment. CronJobs may be added via `jobTemplate`.

### Preview and apply

Changes are applied with **Server-Side Apply** under the tool's own field manager. The preview uses
the same request with **server-side dry-run**, so the diff reflects what the API server would really
persist, including defaulting and admission. The response lists, per workload and container, each
field that would be set and its new value.

### Undo (bonus — approach)

TBD

### GitOps caveat

If workloads are managed by Argo CD / Flux, the tool's patches will be reverted. The durable fix lives in the source manifests or in an
admission policy (Kyverno, Gatekeeper, Pod Security Admission).

---

## 5. Shared decisions

### Never touched

Both features do not act on: `kube-system`, `kube-public`, `kube-node-lease`,
`local-path-storage`, the CNI's namespace, the tool's own namespace, and any namespace or workload
carrying an opt-out label. The list may be configurable in a future iteration, with these as defaults, and enforced in a
single place so the two features cannot drift apart.

### Behaviour over time and when unhealthy

- Liveness/readiness probes and graceful shutdown.
- Retries with backoff when the API server is unavailable; `RetryOnConflict` on update conflicts.
- All operations idempotent; partial failures reported per workload instead of aborting silently.
- Narrow `ClusterRole` (only the verbs and resources each feature needs).
- The tool's own Deployment is hardened by the same standard it enforces.

---

## 6. Planned layout

```
tyk-sre/
├── cmd/
│   └── tyk-sre/           # entrypoint: config, manager, controller + API server wiring, signals
├── api/
│   └── v1alpha1/          # Isolation CRD Go types (controller-gen markers, generated deepcopy)
├── internal/
│   ├── isolation/         # policy builder (pure), safety checks, Isolation controller
│   ├── hardening/         # hardening policy, planner (pure), workload discovery, apply/dry-run
│   ├── guard/             # protected namespaces and opt-out label, shared by both features
│   └── httpapi/           # hardening HTTP handlers and server
├── deploy/
│   ├── crd/               # Isolation CRD (generated), shipped separately from the tool
│   └── tool/              # manifests to deploy the tool: RBAC, Deployment, Service (kustomize)
├── lab/                   # kind config and sample workloads for the demo
├── scripts/               # lab set-up and isolation verification scripts
├── test/
│   └── e2e/               # integration tests against kind (bonus)
├── docs/                  # the original brief
├── Dockerfile
├── Makefile
├── go.mod
└── README.md
```

The decision logic (policy builder, safety checks, hardening planner) is written as pure functions,
so most of the tests run without a cluster or fakes; only the layer that talks to the API
server uses the `controller-runtime` fake client.

### Libraries

- **`controller-runtime`** — the `Isolation` controller needs watches, caching, work queues and
  owner-based event mapping; this is provided by the controller-runtime, its manager also
  runs the HTTP server and health probes. Its fake client covers unit tests.
- **`controller-gen`** — generates the CRD manifest and deepcopy code from the Go types, so the
  schema cannot drift from the code.

---

## 7. Feedback on the brief

1. **GitOps-managed workloads** are not mentioned. In most real clusters they are the norm
   and would revert the tool's patches.
