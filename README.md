# tyk-sre — SRE automations for Kubernetes

## Running the demo
```bash
# start the kind cluster and install dependencies
# Expected duration (depends on hardware and networking): ~10m
make lab-up

# init the demo deploying the sample workloads. 
# Expected duration (depends on hardware): ~1m
make demo-init

# Run the isolation demo
make demo-isolation

# Run the hardening demo
# Expected duration (depends on hardware): ~5s
make demo-hardening

# In case you want to re run the hardening demo, re init the demo
# make demo-init
# make demo-hardening

# Tier Down the lab
make lab-down
```
---

## Running the Tests
```bash
maket test
```

A Go service, deployed in-cluster, that lets an SRE:

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
  workloads' previous connectivity, not just "allow everything".
- **Undo:** *(core)* it is what "off" means.

### 1.2 Workload hardening

- **Input:** one or more namespaces.
- **Effect:** patch workloads that run without resource requests and limits, or without a hardened
  `securityContext`.
- **Preview:** a way to see what would change before it changes (dryRun).
- **Safety:** patching must not break the workloads.
- **Undo:** *(bonus)* if not built, the approach is described in this README.

### 1.3 Deliverables

- One Go service, covering both features.
- A runnable way to show that isolation blocks traffic.
- Automated tests, including at least one tricky case, with the reasoning behind it.
- This README: set-up, build/deploy/run, decisions, and known limitations.
- Helm chart to deploy the tool into the cluster.

### 1.4 Evaluation focus mentioned in the brief

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
| `POST` | `/hardening?dryRun=true` | Return the per-workload changes without applying them (server-side dry run; a bare `?dryRun` also counts as true) |
| `POST` | `/hardening` | Apply them; returns per-workload results |
| `GET` | `/healthz`, `/readyz` | Probes |

Example request:

```json
{ "namespaces": ["tenant-a", "tenant-b"] }
```

The response lists each namespace and workload as `Patched`, `Unchanged`, `Skipped` or `Failed` (namespaces as `Processed`, `Skipped` or `Failed`). The
status code is `200` once the request has run, even if parts of it failed, because the other
workloads may have been patched. The top-level `failed` field counts the failures, so a script can
check `jq -e '.failed == 0'`. Requests that are rejected before anything is attempted get a `4xx`.
With `dryRun`, the response has `"dryRun": true` and `Patched` means "would be patched": the API
server accepted the change in a dry run, but `changes` shows the tool's plan, not what the server
would store after mutation (see "Preview and apply" in section 4).
Each workload lists the values added under `changes`, and the parts deliberately left alone under
`notes`.

> **Current state:** `POST /hardening` fills in resource requests and limits and the
> `securityContext`. The operator opt-in for higher-risk settings described in section 4 is not
> built yet, and neither is the controlled rollout: every workload is patched immediately (see
> [section 7](#7-what-to-iterate-with-more-time)).

### Access control

- **Isolation:** Kubernetes RBAC on the `isolations` resource.
- **Hardening API:** it restarts workloads, so it must not be openly reachable. It binds to the
  pod's loopback (`127.0.0.1:8080`), so no other pod, Service or node can connect to it; the only
  way in is `kubectl port-forward`, which opens the connection from inside the pod's network
  namespace. Who can call the API is therefore decided by Kubernetes RBAC on `pods/portforward`
  (the `create` verb) in the tool's namespace. In production this would be replaced by real
  authentication.

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

The reference is the **Pod Security Standards `restricted` profile** (https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted):

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
- **Controlled rollout.** *(Not built: every workload is patched immediately. See
  [section 7](#7-what-to-iterate-with-more-time).)* Workloads are patched one at a time and the
  rollout is watched; a rollout that fails to progress is reported (and, with undo, rolled back)
  before moving on.
- **Bookkeeping on the workload, not the pod template**, so the tool's annotations do not trigger
  extra restarts.

### Security context

Init containers get the same settings as the other containers.

| Setting | Level | Set when missing and… |
|---|---|---|
| `seccompProfile: RuntimeDefault` | pod | always |
| `allowPrivilegeEscalation: false` | container | the container does not add `CAP_SYS_ADMIN` (the API server rejects the combination) |
| `runAsNonRoot: true` | container | the container runs as a non-zero `runAsUser`, from the container or the pod |
| `capabilities.drop: [ALL]` | container | same evidence: a non-root process starts with no capabilities and, without privilege escalation, cannot gain any |

- **Why `runAsUser` is the evidence:** the user an image runs as is not visible through the
  Kubernetes API. Without an explicit non-root user, `runAsNonRoot` would stop root images from
  starting, and dropping capabilities would break those that bind ports below 1024 or `chown`.
- **A value set at either level counts as set.** Container settings override pod settings, so
  `runAsUser: 0` on a container wins over a non-root pod, and `runAsNonRoot: false` on the pod is
  respected.
- **An existing `drop` list is left as it is.** The list is replaced as a whole, so `ALL` cannot be
  added to it without changing what is there.
- **Privileged containers are left alone,** and reported in `notes`: they are privileged on
  purpose.
- **Linux only.** Workloads are assumed to run on Linux nodes; these settings do not apply to
  Windows pods.
- **`readOnlyRootFilesystem` is never set:** whether an application writes to its filesystem cannot
  be told from the spec.

### Requests and limits

- Set CPU/memory **requests** and a **memory limit** where missing.
- **No CPU limit by default**: CPU limits cause throttling.
- **Defaults:** CPU request `50m`, memory request `128Mi`, memory limit `512Mi`.
- **A limit without a request is left alone.** Kubernetes already uses the limit as the request, and
  adding a lower one would demote a Guaranteed pod to Burstable.
- **The memory limit is never below the request.** A request above the default limit raises the
  limit to match, since the API server rejects a limit below the request.
- **Pod-level resources:** containers of pods setting `spec.resources` get no resource defaults,
  because they could exceed the pod's budget.
- **Earlier values survive later runs.** Server-Side Apply removes owned fields missing from a
  request, so each request starts from the fields the tool already owns.
- Defaults are configurable. Existing `LimitRange`s in the namespace are reported, since they can
  inject values at admission time. *(Neither is built yet.)*

### Scope

- **In Scope:** Deployments, StatefulSets, DaemonSets.
- **Out of Scope:** bare Pods, Jobs, ReplicaSets owned
  by a Deployment. CronJobs may be added via `jobTemplate`.

### Preview and apply

Changes are applied with **Server-Side Apply** under the tool's own field manager. The preview uses
the same request with **server-side dry-run**. The response lists, per workload and container, each
field the tool would set and its new value.

That list comes from the tool's own plan, not from the dry-run response. The dry run makes the API
server validate the change and run it through admission, so a change that would be rejected
(validation, a conflicting field owner, a webhook or Pod Security Admission) shows up as `Failed`
in the preview. It does not show what defaulting or mutating webhooks would add or change on top of
the tool's values; the object actually stored can differ from the preview in those fields.

### Undo (bonus — approach)

See [Hardener Undo](#hardener-undo).

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

## 6. Layout

```
tyk-sre/
├── cmd/
│   └── tyk-sre/
│       └── main.go             # flags, manager, Isolation controller and hardening API wiring
├── api/
│   └── v1alpha1/               # Isolation CRD Go types (controller-gen markers, generated deepcopy)
├── internal/
│   ├── isolation/
│   │   ├── policy.go           # builds the CiliumClusterwideNetworkPolicy for an Isolation
│   │   ├── safety.go           # checks run before a containment is applied
│   │   └── controller.go       # Isolation reconciler
│   ├── hardening/
│   │   ├── policy.go           # planner and resource defaults
│   │   ├── security.go         # security context rules
│   │   ├── workload.go         # Deployment/StatefulSet/DaemonSet adapters, apply configurations
│   │   └── hardener.go         # per-namespace discovery, Server-Side Apply, retries, results
│   ├── guard/                  # protected namespaces and opt-out label, shared by both features
│   └── httpapi/                # POST /hardening handler and HTTP server
├── charts/
│   └── tyk-sre/                # Helm chart of the tool; no CRD and no namespace (see below)
│       ├── Chart.yaml
│       ├── values.yaml         # image, log level, metrics port, resources, scheduling
│       └── templates/
│           ├── _helpers.tpl    # name, namespace, labels, selector labels, image
│           ├── serviceaccount.yaml
│           ├── clusterrole.yaml        # least-privilege rules: no delete on Cilium policies
│           ├── clusterrolebinding.yaml
│           ├── deployment.yaml         # one replica, Recreate, restricted security context
│           └── service.yaml            # ClusterIP: API on 8080, metrics on 8082
├── deploy/
│   └── crd/                    # Isolation CRD (generated by make generate), installed separately
├── lab/
│   ├── kind.yaml               # kind cluster config
│   ├── isolation-sample.yaml   # sample Isolation
│   ├── workloads/
│   │   ├── isolation.yaml      # tenant-a, tenant-b, shared, and the dashboard's tenant policy
│   │   └── hardening.yaml      # hardening-a, hardening-b: one workload per hardening case
│   └── demo/
│       ├── isolation.sh        # isolation demo: connectivity checks before, during and after
│       ├── long-lived-connection.sh  # run in a pod by isolation.sh: one connection kept open
│       └── hardening.sh        # hardening demo: dry run, apply, rollouts, idempotent re-run
├── docs/                       # the original brief
├── Dockerfile
├── Makefile                # generate, chart-lint, lab-up/down, demo-init, demo-isolation, demo-hardening
├── go.mod
└── README.md
```

The decision logic (isolation policy builder, safety checks, hardening planner and security
rules) is written as pure functions, so most of the tests run without a cluster or fakes; only
the layers that talk to the API server (the Isolation controller and the hardener) use the
`controller-runtime` fake client, and the HTTP handler is tested against a fake hardener.

The CRD is kept out of the chart on purpose: uninstalling the tool must not delete the CRD,
which would delete every Isolation and lift every active containment. `make demo-init` installs
the CRD, creates the `tyk-sre` namespace with the `restricted` Pod Security label, then runs
`helm upgrade --install`. `make chart-lint` lints the chart and validates the rendered manifests
with `kubeconform` against the Kubernetes and Cilium schemas.

Not built: automated end-to-end tests against kind. The demo scripts in `lab/demo/` check the
expected outcomes and print them, but they are run by hand and do not fail a pipeline.

### Libraries

- **`controller-runtime`** — the `Isolation` controller needs watches, caching, work queues and
  owner-based event mapping; this is provided by the controller-runtime, its manager also
  runs the HTTP server and health probes. Its fake client covers unit tests.
- **`controller-gen`** — generates the CRD manifest and deepcopy code from the Go types, so the
  schema cannot drift from the code.
- **Helm** — packages the tool's manifests; `kubeconform` (run with `go run`, nothing to
  install) validates what the chart renders.

---

## 7. What to iterate with more time

### Controlled rollout

The plan was to patch workloads one at a time and watching each rollout. **This is not
built.** Today, `POST /hardening` applies every workload in every requested namespace,
and the response comes back as soon as the API server has accepted the changes.
Because each patch changes the pod template, every patched Deployment, StatefulSet and DaemonSet starts
a rolling update at the same moment.
The workload controllers' rolling-update settings (`maxUnavailable`, `maxSurge`) still apply, so a
workload with several replicas keeps serving while its own new pods fail. A single-replica workload
or a DaemonSet with `maxUnavailable: 100%` does not.

With more time:
1. Configure the the resources using either a configuration mechanism or, even better, relying on the p99
   (plus buffer) of the metrics used on a rolling Xdays window.
2. Patch one workload at a time and wait for its rollout before the next one, every replica is updated and
   ready.
3. Adding a wait for the rollouts changes the behaviour of the requests, which, can take minutes per 
   workload. `POST /hardening` might return a job ID, and a `GET /hardening/{id}` would report progress per workload.

### Expose metrics
The current implementation does not expose metrics. To be production-ready I would expose metrics, alerts, and a Grafana dashboard to visualise the behaviour of the two features.

#### For the controller I would need to monitor:
- The ratio of succeeded changes over valid changes over time. Valid changes are the ones that are not excluded by the [guard](internal/guard/guard.go).
- The reconciliation time. The duration between the moment the event is received and when the Cilium policy is created.
- The isolation correctness, how many policies cannot be applied because are blocked by the [safety tier](internal/isolation/safety.go). 

#### For the the hardener I would need to monitor:
- To know how the hardener behaves I would expose metrics on the result to track the status of the outcome for a namespace or a workload.
- As per the controller, to know the actual ratio of "Patched" resources vs. valid events, I would need to know the count of resources excluded by the [guard](internal/guard/guard.go).

### Hardener Undo
The hardener patches the template of the monitored resources (Deployments, StatefulSets and DaemonSets).
To undo the changes, the hardener would expose **POST** `/hardening/undo`, using the same guard and the same dry-run support.
To revert, the hardener does not require any persisted state. For each workload, the hardener sends an empty server-side apply under its field manager, `tyk-sre-hardening`, and the API server removes every field that manager owns and does not share. Fields that another manager has since changed or co-owns are kept, with their current value.

This is based on three assumptions:
- The hardener only adds missing fields and never changes an existing value.
- The hardener manager name does not change over time or releases.
- Each run applies the fields owned from earlier runs plus the new ones, so a later run does not give up earlier fields. 

In case the object is changed by something/someone else:
- patch, edit, client-side or forced server-side apply moves ownership, undo keeps their value.
- managedFields cleared or the object replaced: the ownership record is lost and undo removes nothing.

Undo changes the pod template, so it triggers a rollout of every workload it touches (see [Controlled rollout](#controlled-rollout)).

Alternatives:
We could store the pre-patch values in an annotation and restore them. This solution is weak as: - Restoring would overwrite anything changed since hardening
- If the annotations are patched by a different manager or person, the undo would not restore the correct state.

### Helm Chart
Moving from a set of manifests to a basic helm chart was a quick win. That's why I implemented the chart. This implementation is not production ready, with more time I would add validators and tests to ensure the values do not break the chart deployment and to avoid regressions.

## 8. Tricky or easy to get wrong scenarios
The code covers unit tests, not integration tests.
I used the lab demo script to cover the integrations scenarios. Even though it does not allow to automatically test in a CD pipeline, I believe it's enough for the purpose of this lab/demo.

### Isolation
I believe that the trickiest scenario to prove is the interruption of an existing open connection.
In case there is a security incident and we want to interrupt the networking between namespaces, the active connections are the one we want to target first, before even thinking of new possible connections.
The isolation script instantiate a long lived connection between two workloads and it proves that once the isolation is created, this connection is interrupted
(`[LLC] - tenant-a/gateway -> tenant-b/dashboard:8080, opened before the isolation: Blocked (request 3: no response)`).

### Hardener
For the hardener I don't see a tricky scenario, but I do see a couple of easy-to-get-wrong ones. 
1. When setting resources, the tool could set the CPU limits, possibly causing a CPU throttling issue. Potentially, making apps slower
2. If the requests are not set and the limits are. Setting the requests would change the QoS class (Quality of Service) transforming a pod from Guaranteed to Burstable. Increasing the chance of eviction.
3. Changing configurations in the SecurityContext could cause a rollout failure.

  
## 9. Feedback on the brief
1. In the brief is not clear what "exercise the behaviour" means. Is it automated integration tests against a real cluster?

2. About time, It's not clear if the statement "if you reach the time limit, ..." refers to the 4-6 hours or to the 5 days.
