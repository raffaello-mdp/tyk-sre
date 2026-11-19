package hardening

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/raffaello-mdp/tyk-sre/internal/guard"
)

const testNamespace = "tenant-a"

func newHardener(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) (*Hardener, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		WithReturnManagedFields().
		Build()
	return &Hardener{Reader: c, Writer: c, Guard: guard.New("tyk-sre"), Defaults: DefaultResources()}, c
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func podTemplate(containers ...corev1.Container) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo"}},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

// The workloads below have no selector on purpose: the fake client sends the required
// field as null in an apply, which a real API server never sees, and reports a conflict.

func deployment(name string, containers ...corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: podTemplate(containers...),
		},
	}
}

func getDeployment(t *testing.T, c client.Client, name string) *appsv1.Deployment {
	t.Helper()
	found := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, found); err != nil {
		t.Fatal(err)
	}
	return found
}

// singleWorkload runs the hardener on the test namespace and returns its only workload.
func singleWorkload(t *testing.T, h *Hardener) WorkloadResult {
	t.Helper()
	results := h.Harden(context.Background(), []string{testNamespace}, false)
	if len(results) != 1 || len(results[0].Workloads) != 1 {
		t.Fatalf("results = %+v, want one namespace with one workload", results)
	}
	return results[0].Workloads[0]
}

func TestHardenPatchesEveryWorkloadKind(t *testing.T) {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNamespace},
		Spec: appsv1.StatefulSetSpec{
			Template: podTemplate(corev1.Container{Name: "app", Image: "db:1"}),
		},
	}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: testNamespace},
		Spec: appsv1.DaemonSetSpec{
			Template: podTemplate(corev1.Container{Name: "app", Image: "agent:1"}),
		},
	}
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil),
		deployment("web", corev1.Container{Name: "app", Image: "web:1"}), statefulSet, daemonSet)

	results := h.Harden(context.Background(), []string{testNamespace}, false)

	if len(results[0].Workloads) != 3 {
		t.Fatalf("workloads = %+v, want 3", results[0].Workloads)
	}
	for _, workload := range results[0].Workloads {
		if workload.Status != StatusPatched {
			t.Errorf("%s/%s status = %s (%s), want patched", workload.Kind, workload.Name, workload.Status, workload.Reason)
		}
	}

	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(statefulSet), statefulSet); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(daemonSet), daemonSet); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]corev1.PodSpec{
		"Deployment":  getDeployment(t, c, "web").Spec.Template.Spec,
		"StatefulSet": statefulSet.Spec.Template.Spec,
		"DaemonSet":   daemonSet.Spec.Template.Spec,
	} {
		container := spec.Containers[0]
		if !container.Resources.Requests.Memory().Equal(h.Defaults.MemoryRequest) {
			t.Errorf("%s memory request = %s", name, container.Resources.Requests.Memory())
		}
		if container.SecurityContext == nil || !ptr.Equal(container.SecurityContext.AllowPrivilegeEscalation, ptr.To(false)) {
			t.Errorf("%s allowPrivilegeEscalation not set to false: %+v", name, container.SecurityContext)
		}
		if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil {
			t.Errorf("%s has no seccomp profile", name)
		}
		if container.Image == "" {
			t.Errorf("%s lost its image", name)
		}
	}
}

func TestHardenKeepsExistingValues(t *testing.T) {
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil), deployment("web", corev1.Container{
		Name:  "app",
		Image: "web:1",
		Resources: corev1.ResourceRequirements{
			Requests: quantities("memory", "32Mi"),
			Limits:   quantities("cpu", "500m", "memory", "128Mi"),
		},
	}))

	got := singleWorkload(t, h)
	if got.Status != StatusPatched {
		t.Fatalf("status = %s (%s), want patched with the security settings", got.Status, got.Reason)
	}
	if change := got.Changes.Containers[0]; change.Requests != nil || change.Limits != nil {
		t.Errorf("resources planned for a container that has them: %+v", change)
	}

	resources := getDeployment(t, c, "web").Spec.Template.Spec.Containers[0].Resources
	if !resources.Requests.Memory().Equal(quantities("memory", "32Mi")["memory"]) {
		t.Errorf("memory request changed to %s", resources.Requests.Memory())
	}
	if _, found := resources.Requests[corev1.ResourceCPU]; found {
		t.Errorf("CPU request was added below an existing limit: %s", resources.Requests.Cpu())
	}
}

func TestHardenIsIdempotent(t *testing.T) {
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil),
		deployment("web", corev1.Container{Name: "app", Image: "web:1"}))

	if got := singleWorkload(t, h); got.Status != StatusPatched {
		t.Fatalf("first run status = %s (%s), want patched", got.Status, got.Reason)
	}
	generation := getDeployment(t, c, "web").ResourceVersion

	if got := singleWorkload(t, h); got.Status != StatusUnchanged {
		t.Fatalf("second run status = %s (%s), want unchanged", got.Status, got.Reason)
	}
	if after := getDeployment(t, c, "web").ResourceVersion; after != generation {
		t.Errorf("second run wrote to the workload: resourceVersion %s -> %s", generation, after)
	}
}

// Tricky: Server-Side Apply deletes the fields a manager owned and no longer sends.
// A second run that sent only what is newly missing would strip the values of the first.
func TestHardenSecondRunKeepsValuesFromFirstRun(t *testing.T) {
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil),
		deployment("web", corev1.Container{Name: "app", Image: "web:1"}))
	ctx := context.Background()

	if got := singleWorkload(t, h); got.Status != StatusPatched {
		t.Fatalf("first run status = %s (%s), want patched", got.Status, got.Reason)
	}

	// Someone removes one of the values, so the next run has exactly one field to add.
	edited := getDeployment(t, c, "web")
	delete(edited.Spec.Template.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)
	edited.ManagedFields = nil
	if err := c.Update(ctx, edited, client.FieldOwner("someone-else")); err != nil {
		t.Fatal(err)
	}

	got := singleWorkload(t, h)
	if got.Status != StatusPatched {
		t.Fatalf("second run status = %s (%s), want patched", got.Status, got.Reason)
	}

	container := getDeployment(t, c, "web").Spec.Template.Spec.Containers[0]
	resources := container.Resources
	if container.SecurityContext == nil || !ptr.Equal(container.SecurityContext.AllowPrivilegeEscalation, ptr.To(false)) {
		t.Errorf("security context = %+v, the first run's value was lost", container.SecurityContext)
	}
	if !resources.Limits.Memory().Equal(h.Defaults.MemoryLimit) {
		t.Errorf("memory limit = %s, want it restored", resources.Limits.Memory())
	}
	if !resources.Requests.Cpu().Equal(h.Defaults.CPURequest) {
		t.Errorf("CPU request = %s, the first run's value was lost", resources.Requests.Cpu())
	}
	if !resources.Requests.Memory().Equal(h.Defaults.MemoryRequest) {
		t.Errorf("memory request = %s, the first run's value was lost", resources.Requests.Memory())
	}
}

// Tricky: an operator sets a value between the read and the write. The retry must
// plan again from the current state and keep the operator's value.
func TestHardenReplansAfterConflict(t *testing.T) {
	conflicts := 0
	funcs := interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if conflicts == 0 {
				conflicts++
				current := &appsv1.Deployment{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "web"}, current); err != nil {
					return err
				}
				current.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: quantities("cpu", "2", "memory", "1Gi"),
					Limits:   quantities("memory", "1Gi"),
				}
				current.ManagedFields = nil
				if err := c.Update(ctx, current, client.FieldOwner("operator")); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("changed"))
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
	h, c := newHardener(t, funcs, namespace(testNamespace, nil),
		deployment("web", corev1.Container{Name: "app", Image: "web:1"}))

	got := singleWorkload(t, h)

	if got.Status != StatusPatched {
		t.Fatalf("status = %s (%s), want patched with the security settings", got.Status, got.Reason)
	}
	if change := got.Changes.Containers[0]; change.Requests != nil || change.Limits != nil {
		t.Errorf("replanned change still adds resources: %+v", change)
	}
	resources := getDeployment(t, c, "web").Spec.Template.Spec.Containers[0].Resources
	if !resources.Requests.Cpu().Equal(quantities("cpu", "2")["cpu"]) {
		t.Errorf("CPU request = %s, the operator's value was overwritten", resources.Requests.Cpu())
	}
}

func TestHardenSkipsOptedOutWorkload(t *testing.T) {
	optedOut := deployment("opted-out", corev1.Container{Name: "app", Image: "web:1"})
	optedOut.Labels = map[string]string{guard.OptOutLabel: "true"}
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil), optedOut)

	if got := singleWorkload(t, h); got.Status != StatusSkipped {
		t.Errorf("status = %s, want skipped", got.Status)
	}
	spec := getDeployment(t, c, "opted-out").Spec.Template.Spec
	if len(spec.Containers[0].Resources.Requests) != 0 || spec.Containers[0].SecurityContext != nil || spec.SecurityContext != nil {
		t.Errorf("opted-out workload was patched: %+v", spec)
	}
}

func TestHardenReportsPartsLeftAlone(t *testing.T) {
	podLevel := deployment("pod-level", corev1.Container{Name: "app", Image: "web:1"})
	podLevel.Spec.Template.Spec.Resources = &corev1.ResourceRequirements{Requests: quantities("memory", "64Mi")}
	h, c := newHardener(t, interceptor.Funcs{}, namespace(testNamespace, nil), podLevel)

	got := singleWorkload(t, h)

	if got.Status != StatusPatched || len(got.Notes) != 1 {
		t.Fatalf("result = %+v, want patched with one note", got)
	}
	container := getDeployment(t, c, "pod-level").Spec.Template.Spec.Containers[0]
	if len(container.Resources.Requests) != 0 {
		t.Errorf("container resources added under pod-level resources: %v", container.Resources.Requests)
	}
	if container.SecurityContext == nil {
		t.Error("security settings were not applied")
	}
}

func TestHardenNamespaceOutcomes(t *testing.T) {
	protectedWorkload := deployment("web", corev1.Container{Name: "app", Image: "web:1"})
	protectedWorkload.Namespace = "kube-system"
	optedOutWorkload := deployment("web", corev1.Container{Name: "app", Image: "web:1"})
	optedOutWorkload.Namespace = "opted-out"

	h, c := newHardener(t, interceptor.Funcs{},
		namespace("kube-system", nil),
		namespace("opted-out", map[string]string{guard.OptOutLabel: "true"}),
		protectedWorkload, optedOutWorkload)

	results := h.Harden(context.Background(), []string{"kube-system", "opted-out", "missing"}, false)

	want := []Status{StatusSkipped, StatusSkipped, StatusFailed}
	for i, result := range results {
		if result.Status != want[i] {
			t.Errorf("%s status = %s, want %s", result.Namespace, result.Status, want[i])
		}
	}
	for _, object := range []*appsv1.Deployment{protectedWorkload, optedOutWorkload} {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
		if len(object.Spec.Template.Spec.Containers[0].Resources.Requests) != 0 {
			t.Errorf("workload in protected namespace %s was patched", object.Namespace)
		}
	}
}

// The fake client ignores dry run on Apply, so this checks that the request asks
// the API server for one; the API server is what guarantees nothing is persisted.
func TestHardenDryRunAsksForServerSideDryRun(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
			var sent []string
			funcs := interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					applyOptions := &client.ApplyOptions{}
					applyOptions.ApplyOptions(opts)
					sent = applyOptions.DryRun
					if len(sent) > 0 {
						return nil
					}
					return c.Apply(ctx, obj, opts...)
				},
			}
			h, _ := newHardener(t, funcs, namespace(testNamespace, nil),
				deployment("web", corev1.Container{Name: "app", Image: "web:1"}))

			results := h.Harden(context.Background(), []string{testNamespace}, dryRun)

			got := results[0].Workloads[0]
			if got.Status != StatusPatched || got.Changes == nil {
				t.Fatalf("result = %+v, want the changes", got)
			}
			if want := dryRun; slices.Equal(sent, []string{metav1.DryRunAll}) != want {
				t.Errorf("dryRun option sent = %v, want dry run %v", sent, want)
			}
		})
	}
}

func TestHardenContinuesAfterWorkloadFailure(t *testing.T) {
	funcs := interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if config, ok := obj.(interface{ GetName() *string }); ok && *config.GetName() == "broken" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "broken", errors.New("denied"))
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
	h, c := newHardener(t, funcs, namespace(testNamespace, nil),
		deployment("broken", corev1.Container{Name: "app", Image: "web:1"}),
		deployment("healthy", corev1.Container{Name: "app", Image: "web:1"}))

	results := h.Harden(context.Background(), []string{testNamespace}, false)

	if got := results[0].Failures(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
	statuses := map[string]Status{}
	for _, workload := range results[0].Workloads {
		statuses[workload.Name] = workload.Status
	}
	if statuses["broken"] != StatusFailed || statuses["healthy"] != StatusPatched {
		t.Fatalf("statuses = %v, want broken failed and healthy patched", statuses)
	}
	if len(getDeployment(t, c, "healthy").Spec.Template.Spec.Containers[0].Resources.Requests) == 0 {
		t.Error("healthy workload was not patched")
	}
}
