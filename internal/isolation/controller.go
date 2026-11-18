package isolation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
	"github.com/raffaello-mdp/tyk-sre/internal/guard"
)

// recheckInterval is how often an Isolation is retried while it waits and is not informed about changes.
const recheckInterval = 30 * time.Second

// +kubebuilder:rbac:groups=sre.tyk.io,resources=isolations,verbs=get;list;watch
// +kubebuilder:rbac:groups=sre.tyk.io,resources=isolations/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=sre.tyk.io,resources=isolations/finalizers,verbs=update
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumclusterwidenetworkpolicies,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=pods;namespaces,verbs=get;list;watch

// Reconciler keeps the policy of each Isolation in line with its spec, and
// reports the outcome in status.
type Reconciler struct {
	client.Client
	Guard guard.Guard

	watchPolicies func() error
	watchMu       sync.Mutex
	watching      bool
}

// outcome is the result of enforcing an Isolation.
type outcome struct {
	status   metav1.ConditionStatus
	reason   string
	message  string
	warnings []string
	recheck  bool
}

// SetupWithManager registers the controller and its watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Isolation{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Watch pod creation, deletion, update of label or phase
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.isolationsForPod),
			builder.WithPredicates(podChangedPredicate())).
		// Watch Namespace to react to label changes (sre.tyk.io/opt-out)
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.isolationsForNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Build(r)
	if err != nil {
		return err
	}

	r.watchPolicies = func() error {
		return c.Watch(source.Kind(mgr.GetCache(), newPolicy(),
			handler.TypedEnqueueRequestForOwner[*unstructured.Unstructured](
				mgr.GetScheme(), mgr.GetRESTMapper(), &v1alpha1.Isolation{}, handler.OnlyControllerOwner())))
	}
	return nil
}

// Reconcile enforces one Isolation and records the result in its status.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	iso := &v1alpha1.Isolation{}
	if err := r.Get(ctx, req.NamespacedName, iso); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !iso.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	assessment, err := r.assess(ctx, iso)
	if err != nil {
		return reconcile.Result{}, err
	}
	protected, err := r.protectedNamespaces(ctx, iso)
	if err != nil {
		return reconcile.Result{}, err
	}

	result, enforceErr := r.enforce(ctx, iso, assessment, protected)
	statusErr := r.updateStatus(ctx, iso, assessment, result)
	if err := errors.Join(enforceErr, statusErr); err != nil {
		return reconcile.Result{}, err
	}
	if result.recheck {
		return reconcile.Result{RequeueAfter: recheckInterval}, nil
	}
	return reconcile.Result{}, nil
}

// enforce brings the policy in line with the spec, unless the spec is unsafe.
// An unsafe spec never removes a policy.
func (r *Reconciler) enforce(
	ctx context.Context, iso *v1alpha1.Isolation, assessment Assessment, protected []string,
) (outcome, error) {
	desired := BuildPolicy(iso)
	existing := newPolicy()
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName()}, existing)
	switch {
	case meta.IsNoMatchError(err):
		return outcome{
			status:  metav1.ConditionFalse,
			reason:  v1alpha1.ReasonCiliumNotInstalled,
			message: "the cluster does not serve " + PolicyGVK.GroupKind().String(),
			recheck: true,
		}, nil
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return applyFailed(err), err
	}
	if err := r.ensurePolicyWatch(); err != nil {
		return applyFailed(err), err
	}

	if existing != nil && !metav1.IsControlledBy(existing, iso) {
		return outcome{
			status:  metav1.ConditionFalse,
			reason:  v1alpha1.ReasonPolicyConflict,
			message: fmt.Sprintf("policy %s exists and is not owned by this Isolation", desired.GetName()),
			recheck: true,
		}, nil
	}

	if reason, message := rejection(assessment, protected); reason != "" {
		switch {
		case existing == nil:
			return outcome{status: metav1.ConditionFalse, reason: reason, message: message}, nil
		case policyMatches(existing, desired):
			return outcome{
				status:   metav1.ConditionTrue,
				reason:   v1alpha1.ReasonPolicyApplied,
				message:  "the policy matches the current spec",
				warnings: []string{message},
			}, nil
		default:
			return outcome{
				status:  metav1.ConditionTrue,
				reason:  v1alpha1.ReasonLastValidPolicyKept,
				message: "spec rejected, the previous policy is still enforced: " + message,
			}, nil
		}
	}

	if err := r.applyPolicy(ctx, existing, desired); err != nil {
		return applyFailed(err), err
	}
	return outcome{
		status:  metav1.ConditionTrue,
		reason:  v1alpha1.ReasonPolicyApplied,
		message: "the policy matches the current spec",
	}, nil
}

// applyPolicy creates the policy or updates it in a single write, so an edit
// never opens a window in which the two sides can talk.
func (r *Reconciler) applyPolicy(ctx context.Context, existing, desired *unstructured.Unstructured) error {
	if existing == nil {
		return r.Create(ctx, desired)
	}
	if policyMatches(existing, desired) {
		return nil
	}
	delete(existing.Object, "spec")
	existing.Object["specs"] = desired.Object["specs"]
	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[IsolationLabel] = desired.GetLabels()[IsolationLabel]
	existing.SetLabels(labels)
	return r.Update(ctx, existing)
}

// assess lists the pods currently selected by each side of iso and returns their
// Assessment: the pod counts, the pods on both sides, and any warnings.
func (r *Reconciler) assess(ctx context.Context, iso *v1alpha1.Isolation) (Assessment, error) {
	podsA, err := r.podsOf(ctx, iso.Spec.A)
	if err != nil {
		return Assessment{}, fmt.Errorf("listing pods of side a: %w", err)
	}
	podsB, err := r.podsOf(ctx, iso.Spec.B)
	if err != nil {
		return Assessment{}, fmt.Errorf("listing pods of side b: %w", err)
	}
	return Assess(podsA, podsB), nil
}

func (r *Reconciler) podsOf(ctx context.Context, side v1alpha1.WorkloadSelector) ([]corev1.Pod, error) {
	selector, err := labelSelector(side.Selector)
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	for _, ns := range side.Namespaces {
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList, client.InNamespace(string(ns)),
			client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, err
		}
		pods = append(pods, podList.Items...)
	}
	return pods, nil
}

// protectedNamespaces returns the sorted namespaces of the isolation the tool must not act on.
func (r *Reconciler) protectedNamespaces(ctx context.Context, iso *v1alpha1.Isolation) ([]string, error) {
	var protected []string
	for _, name := range uniqueNamespacesOf(iso) {
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("reading namespace %s: %w", name, err)
		}
		if r.Guard.IsNamespaceProtected(name, ns.Labels) {
			protected = append(protected, name)
		}
	}
	return protected, nil
}

// updateStatus records the result, writing only when something changed.
func (r *Reconciler) updateStatus(
	ctx context.Context, iso *v1alpha1.Isolation, assessment Assessment, result outcome,
) error {
	original := iso.DeepCopy()
	iso.Status.ObservedGeneration = iso.Generation
	iso.Status.MatchedPods = assessment.MatchedPods
	iso.Status.Warnings = slices.Concat(assessment.Warnings, result.warnings)
	meta.SetStatusCondition(&iso.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionEnforced,
		Status:             result.status,
		ObservedGeneration: iso.Generation,
		Reason:             result.reason,
		Message:            result.message,
	})
	if equality.Semantic.DeepEqual(original.Status, iso.Status) {
		return nil
	}
	return r.Status().Patch(ctx, iso, client.MergeFrom(original))
}

// ensurePolicyWatch starts the policy watch the first time Cilium is seen.
func (r *Reconciler) ensurePolicyWatch() error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.watching || r.watchPolicies == nil {
		return nil
	}
	if err := r.watchPolicies(); err != nil {
		return fmt.Errorf("watching policies: %w", err)
	}
	r.watching = true
	return nil
}

// isolationsForPod maps a pod to the Isolations covering its namespace.
func (r *Reconciler) isolationsForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	return r.isolationsCovering(ctx, pod.GetNamespace())
}

// isolationsForNamespace maps a namespace to the Isolations covering it.
func (r *Reconciler) isolationsForNamespace(ctx context.Context, ns client.Object) []reconcile.Request {
	return r.isolationsCovering(ctx, ns.GetName())
}

func (r *Reconciler) isolationsCovering(ctx context.Context, namespace string) []reconcile.Request {
	list := &v1alpha1.IsolationList{}
	if err := r.List(ctx, list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing isolations", "namespace", namespace)
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		if slices.Contains(uniqueNamespacesOf(&list.Items[i]), namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
			})
		}
	}
	return requests
}

// podChangedPredicate keeps the pod updates that can change an assessment.
func podChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, okBefore := e.ObjectOld.(*corev1.Pod)
			after, okAfter := e.ObjectNew.(*corev1.Pod)
			if !okBefore || !okAfter {
				return false
			}
			return before.Status.Phase != after.Status.Phase ||
				!equality.Semantic.DeepEqual(before.Labels, after.Labels)
		},
	}
}

// rejection returns why the spec must not be enforced, if it must not.
func rejection(assessment Assessment, protected []string) (reason, message string) {
	if len(protected) > 0 {
		return v1alpha1.ReasonNamespaceProtected, "protected namespaces: " + strings.Join(protected, ", ")
	}
	if len(assessment.BothSides) > 0 {
		return v1alpha1.ReasonPodsMatchBothSides, "pods match both sides: " + summarize(assessment.BothSides)
	}
	return "", ""
}

// uniqueNamespacesOf returns the sorted, unique namespaces covered by iso.
func uniqueNamespacesOf(iso *v1alpha1.Isolation) []string {
	var namespaces []string
	for _, ns := range slices.Concat(iso.Spec.A.Namespaces, iso.Spec.B.Namespaces) {
		namespaces = append(namespaces, string(ns))
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}

func applyFailed(err error) outcome {
	return outcome{
		status:  metav1.ConditionUnknown,
		reason:  v1alpha1.ReasonPolicyApplyFailed,
		message: err.Error(),
	}
}
