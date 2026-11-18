package isolation

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
	"github.com/raffaello-mdp/tyk-sre/internal/guard"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(PolicyGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(PolicyGVK.GroupVersion().WithKind(PolicyGVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

func newMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{v1alpha1.GroupVersion, PolicyGVK.GroupVersion(), corev1.SchemeGroupVersion})
	mapper.Add(v1alpha1.GroupVersion.WithKind("Isolation"), meta.RESTScopeRoot)
	mapper.Add(PolicyGVK, meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	return mapper
}

// harness runs the reconciler against a fake API server.
type harness struct {
	t *testing.T
	r *Reconciler
	c client.Client
}

func newHarness(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) *harness {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithRESTMapper(newMapper()).
		WithStatusSubresource(&v1alpha1.Isolation{}).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		Build()
	return &harness{t: t, c: c, r: &Reconciler{Client: c, Guard: guard.New("tyk-sre")}}
}

func (h *harness) reconcile(name string) reconcile.Result {
	h.t.Helper()
	result, err := h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return result
}

func (h *harness) isolation(name string) *v1alpha1.Isolation {
	h.t.Helper()
	iso := &v1alpha1.Isolation{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name}, iso); err != nil {
		h.t.Fatalf("get isolation: %v", err)
	}
	return iso
}

// policy returns the policy of the named Isolation, or nil if there is none.
func (h *harness) policy(name string) *unstructured.Unstructured {
	h.t.Helper()
	policy := newPolicy()
	err := h.c.Get(context.Background(), types.NamespacedName{Name: PolicyName(name)}, policy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("get policy: %v", err)
	}
	return policy
}

// edit applies a spec change the way the API server would, bumping the generation.
func (h *harness) edit(name string, change func(*v1alpha1.IsolationSpec)) {
	h.t.Helper()
	iso := h.isolation(name)
	change(&iso.Spec)
	iso.Generation++
	if err := h.c.Update(context.Background(), iso); err != nil {
		h.t.Fatalf("update isolation: %v", err)
	}
}

func (h *harness) assertEnforced(name string, status metav1.ConditionStatus, reason string) {
	h.t.Helper()
	iso := h.isolation(name)
	condition := meta.FindStatusCondition(iso.Status.Conditions, v1alpha1.ConditionEnforced)
	if condition == nil {
		h.t.Fatal("Enforced condition is missing")
	}
	if condition.Status != status || condition.Reason != reason {
		h.t.Fatalf("Enforced = %s/%s (%s), want %s/%s", condition.Status, condition.Reason, condition.Message, status, reason)
	}
	if condition.ObservedGeneration != iso.Generation || iso.Status.ObservedGeneration != iso.Generation {
		h.t.Fatalf("observed generation %d/%d, want %d", condition.ObservedGeneration, iso.Status.ObservedGeneration, iso.Generation)
	}
}

func side(namespace, app string) v1alpha1.WorkloadSelector {
	return v1alpha1.WorkloadSelector{
		Namespaces: []v1alpha1.NamespaceName{v1alpha1.NamespaceName(namespace)},
		Selector:   v1alpha1.PodSelector{MatchLabels: map[string]v1alpha1.LabelValue{"app": v1alpha1.LabelValue(app)}},
	}
}

func isolation(name string) *v1alpha1.Isolation {
	return &v1alpha1.Isolation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: v1alpha1.IsolationSpec{
			Reason: "test",
			A:      side("tenant-a", "gateway"),
			B:      side("tenant-b", "dashboard"),
		},
	}
}

func pod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func workloadPods() []client.Object {
	return []client.Object{
		pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
		pod("tenant-a", "bystander", map[string]string{"app": "not-isolated"}),
		pod("tenant-b", "dashboard-1", map[string]string{"app": "dashboard"}),
	}
}

func TestReconcileCreatesOwnedPolicy(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"))...)
	h.reconcile("inc")

	policy := h.policy("inc")
	if policy == nil {
		t.Fatal("policy was not created")
	}
	if !metav1.IsControlledBy(policy, h.isolation("inc")) {
		t.Fatal("policy is not owned by the Isolation, so deleting it would not lift the block")
	}
	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonPolicyApplied)
	if got := h.isolation("inc").Status.MatchedPods; got != (v1alpha1.MatchedPods{A: 1, B: 1}) {
		t.Fatalf("matchedPods = %+v, the bystander must not be counted", got)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"))...)
	h.reconcile("inc")
	policyVersion := h.policy("inc").GetResourceVersion()
	statusVersion := h.isolation("inc").ResourceVersion

	h.reconcile("inc")

	if got := h.policy("inc").GetResourceVersion(); got != policyVersion {
		t.Fatalf("policy was rewritten: %s -> %s", policyVersion, got)
	}
	if got := h.isolation("inc").ResourceVersion; got != statusVersion {
		t.Fatalf("status was rewritten: %s -> %s", statusVersion, got)
	}
}

func TestReconcileRestoresTamperedPolicy(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"))...)
	h.reconcile("inc")

	tampered := h.policy("inc")
	want := tampered.DeepCopy().Object["specs"]
	tampered.Object["specs"] = []any{}
	if err := h.c.Update(context.Background(), tampered); err != nil {
		t.Fatal(err)
	}

	h.reconcile("inc")

	if got := h.policy("inc").Object["specs"]; !equality.Semantic.DeepEqual(got, want) {
		t.Fatalf("specs were not restored: %v", got)
	}
}

func TestReconcileRecreatesDeletedPolicy(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"))...)
	h.reconcile("inc")
	if err := h.c.Delete(context.Background(), h.policy("inc")); err != nil {
		t.Fatal(err)
	}

	h.reconcile("inc")

	if h.policy("inc") == nil {
		t.Fatal("deleted policy was not recreated")
	}
}

func TestValidEditUpdatesPolicyInPlace(t *testing.T) {
	deletes := 0
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		deletes++
		return c.Delete(ctx, obj, opts...)
	}}
	h := newHarness(t, funcs, append(workloadPods(), isolation("inc"))...)
	h.reconcile("inc")

	h.edit("inc", func(spec *v1alpha1.IsolationSpec) { spec.B = side("tenant-b", "api") })
	h.reconcile("inc")

	if !strings.Contains(specsOf(t, h.policy("inc")), "api") {
		t.Fatal("policy does not reflect the edit")
	}
	if deletes != 0 {
		t.Fatal("the policy was deleted during the edit, which lets the two sides talk")
	}
	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonPolicyApplied)
}

// A new spec selecting the same pods on both sides would cut those pods off from
// their own peers, so nothing may be created.
func TestNewIsolationWithPodOnBothSidesIsRefused(t *testing.T) {
	iso := isolation("inc")
	iso.Spec.B = v1alpha1.WorkloadSelector{
		Namespaces: []v1alpha1.NamespaceName{"tenant-a"},
		Selector:   v1alpha1.PodSelector{MatchLabels: map[string]v1alpha1.LabelValue{"tier": "front"}},
	}
	h := newHarness(t, interceptor.Funcs{}, iso,
		pod("tenant-a", "shared-1", map[string]string{"app": "gateway", "tier": "front"}))

	h.reconcile("inc")

	if h.policy("inc") != nil {
		t.Fatal("a policy was created for a spec with a pod on both sides")
	}
	h.assertEnforced("inc", metav1.ConditionFalse, v1alpha1.ReasonPodsMatchBothSides)
}

// The tricky case of the controller: during an incident, a bad edit must not lift
// the containment that is already in place.
func TestUnsafeEditKeepsLastValidPolicy(t *testing.T) {
	objects := append(workloadPods(), isolation("inc"),
		pod("tenant-a", "shared-1", map[string]string{"app": "gateway", "tier": "front"}))
	h := newHarness(t, interceptor.Funcs{}, objects...)
	h.reconcile("inc")
	before := specsOf(t, h.policy("inc"))

	h.edit("inc", func(spec *v1alpha1.IsolationSpec) {
		spec.B = v1alpha1.WorkloadSelector{
			Namespaces: []v1alpha1.NamespaceName{"tenant-a"},
			Selector:   v1alpha1.PodSelector{MatchLabels: map[string]v1alpha1.LabelValue{"tier": "front"}},
		}
	})
	h.reconcile("inc")

	policy := h.policy("inc")
	if policy == nil {
		t.Fatal("the policy was removed by an unsafe edit")
	}
	if got := specsOf(t, policy); got != before {
		t.Fatalf("the policy changed after an unsafe edit:\n%s", got)
	}
	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonLastValidPolicyKept)
}

func TestUnsafeEditRecoversOnceFixed(t *testing.T) {
	objects := append(workloadPods(), isolation("inc"),
		pod("tenant-a", "shared-1", map[string]string{"app": "gateway", "tier": "front"}))
	h := newHarness(t, interceptor.Funcs{}, objects...)
	h.reconcile("inc")
	h.edit("inc", func(spec *v1alpha1.IsolationSpec) {
		spec.B = v1alpha1.WorkloadSelector{
			Namespaces: []v1alpha1.NamespaceName{"tenant-a"},
			Selector:   v1alpha1.PodSelector{MatchLabels: map[string]v1alpha1.LabelValue{"tier": "front"}},
		}
	})
	h.reconcile("inc")

	h.edit("inc", func(spec *v1alpha1.IsolationSpec) { spec.B = side("tenant-b", "api") })
	h.reconcile("inc")

	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonPolicyApplied)
	if !strings.Contains(specsOf(t, h.policy("inc")), "api") {
		t.Fatal("the fixed spec was not applied")
	}
}

func TestPodStartingToMatchBothSidesKeepsPolicyAndWarns(t *testing.T) {
	iso := isolation("inc")
	iso.Spec.B = v1alpha1.WorkloadSelector{
		Namespaces: []v1alpha1.NamespaceName{"tenant-a"},
		Selector:   v1alpha1.PodSelector{MatchLabels: map[string]v1alpha1.LabelValue{"tier": "front"}},
	}
	target := pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"})
	h := newHarness(t, interceptor.Funcs{}, iso, target,
		pod("tenant-a", "front-1", map[string]string{"tier": "front"}))
	h.reconcile("inc")

	target.Labels["tier"] = "front"
	if err := h.c.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	h.reconcile("inc")

	if h.policy("inc") == nil {
		t.Fatal("the policy was removed after a label change")
	}
	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonPolicyApplied)
	warnings := strings.Join(h.isolation("inc").Status.Warnings, "\n")
	if !strings.Contains(warnings, "tenant-a/gateway-1") {
		t.Fatalf("warnings do not name the pod: %q", warnings)
	}
}

func TestSideWithoutPodsIsEnforcedWithWarning(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, isolation("inc"),
		pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}))

	h.reconcile("inc")

	if h.policy("inc") == nil {
		t.Fatal("the policy must also cover future pods")
	}
	h.assertEnforced("inc", metav1.ConditionTrue, v1alpha1.ReasonPolicyApplied)
	if !slices.Contains(h.isolation("inc").Status.Warnings, "side b matches no pods") {
		t.Fatalf("warnings = %v", h.isolation("inc").Status.Warnings)
	}
}

func TestProtectedNamespacesAreRefused(t *testing.T) {
	tests := map[string]struct {
		namespace string
		objects   []client.Object
	}{
		"system namespace": {namespace: "kube-system"},
		"tool namespace":   {namespace: "tyk-sre"},
		"opted-out namespace": {
			namespace: "tenant-c",
			objects: []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "tenant-c", Labels: map[string]string{guard.OptOutLabel: "true"},
			}}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			iso := isolation("inc")
			iso.Spec.B = side(test.namespace, "dashboard")
			h := newHarness(t, interceptor.Funcs{}, append(test.objects, iso)...)

			h.reconcile("inc")

			if h.policy("inc") != nil {
				t.Fatal("a policy was created for a protected namespace")
			}
			h.assertEnforced("inc", metav1.ConditionFalse, v1alpha1.ReasonNamespaceProtected)
		})
	}
}

func TestForeignPolicyIsNeverAdopted(t *testing.T) {
	foreign := newPolicy()
	foreign.SetName(PolicyName("inc"))
	foreign.Object["specs"] = []any{map[string]any{"description": "someone else's"}}
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"), foreign)...)

	result := h.reconcile("inc")

	policy := h.policy("inc")
	if len(policy.GetOwnerReferences()) != 0 || !strings.Contains(specsOf(t, policy), "someone else's") {
		t.Fatalf("the foreign policy was modified: %v", policy.Object)
	}
	h.assertEnforced("inc", metav1.ConditionFalse, v1alpha1.ReasonPolicyConflict)
	if result.RequeueAfter == 0 {
		t.Fatal("no watch reports the foreign policy leaving, so a recheck is needed")
	}
}

func TestOverlappingIsolationsOwnSeparatePolicies(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("one"), isolation("two"))...)
	h.reconcile("one")
	h.reconcile("two")

	if err := h.c.Delete(context.Background(), h.policy("one")); err != nil {
		t.Fatal(err)
	}
	if h.policy("two") == nil {
		t.Fatal("removing one isolation lifted the other")
	}
	if !metav1.IsControlledBy(h.policy("two"), h.isolation("two")) {
		t.Fatal("the second policy has the wrong owner")
	}
}

func TestCiliumNotInstalledIsReportedAndRetried(t *testing.T) {
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == PolicyGVK {
			return &meta.NoKindMatchError{GroupKind: PolicyGVK.GroupKind(), SearchedVersions: []string{PolicyGVK.Version}}
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	h := newHarness(t, funcs, append(workloadPods(), isolation("inc"))...)
	watches := 0
	h.r.watchPolicies = func() error { watches++; return nil }

	result := h.reconcile("inc")

	h.assertEnforced("inc", metav1.ConditionFalse, v1alpha1.ReasonCiliumNotInstalled)
	if result.RequeueAfter == 0 {
		t.Fatal("the controller must notice Cilium being installed later")
	}
	if watches != 0 {
		t.Fatal("watching a kind the cluster does not serve would stop the manager")
	}
}

func TestPolicyWatchStartsOnceWhenCiliumIsPresent(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, append(workloadPods(), isolation("inc"))...)
	watches := 0
	h.r.watchPolicies = func() error { watches++; return nil }

	h.reconcile("inc")
	h.reconcile("inc")

	if watches != 1 {
		t.Fatalf("policy watch started %d times, want 1", watches)
	}
}

func TestFailedPolicyWriteIsReportedAndRetried(t *testing.T) {
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		return errors.New("admission denied")
	}}
	h := newHarness(t, funcs, append(workloadPods(), isolation("inc"))...)

	_, err := h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "inc"}})

	if err == nil {
		t.Fatal("the error must be returned so the write is retried with backoff")
	}
	h.assertEnforced("inc", metav1.ConditionUnknown, v1alpha1.ReasonPolicyApplyFailed)
}

func TestReconcileUnknownIsolationIsNotAnError(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{})
	h.reconcile("gone")
}

func TestIsolationsCoveringMapsOnlyMatchingNamespaces(t *testing.T) {
	h := newHarness(t, interceptor.Funcs{}, isolation("inc"))

	got := h.r.isolationsForPod(context.Background(), pod("tenant-b", "any", nil))
	if len(got) != 1 || got[0].Name != "inc" {
		t.Fatalf("requests = %v", got)
	}
	if got := h.r.isolationsForPod(context.Background(), pod("shared", "any", nil)); len(got) != 0 {
		t.Fatalf("unrelated namespace produced requests: %v", got)
	}
}

func specsOf(t *testing.T, policy *unstructured.Unstructured) string {
	t.Helper()
	data, err := policy.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
