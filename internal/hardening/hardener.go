package hardening

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/raffaello-mdp/tyk-sre/internal/guard"
)

// FieldManager is the owner of the fields set by the tool.
const FieldManager = "tyk-sre-hardening"

// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;patch

// Status is the outcome for a namespace or a workload.
type Status string

const (
	// StatusPatched means values were added to the workload.
	StatusPatched Status = "Patched"
	// StatusUnchanged means nothing was missing.
	StatusUnchanged Status = "Unchanged"
	// StatusSkipped means the tool must not act on it.
	StatusSkipped Status = "Skipped"
	// StatusProcessed means the workloads of the namespace were attempted.
	StatusProcessed Status = "Processed"
	// StatusFailed means the operation did not complete.
	StatusFailed Status = "Failed"
)

// WorkloadResult is the outcome for one workload.
type WorkloadResult struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Status  Status   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Changes *Changes `json:"changes,omitempty"`
}

// NamespaceResult is the outcome for one namespace.
type NamespaceResult struct {
	Namespace string           `json:"namespace"`
	Status    Status           `json:"status"`
	Reason    string           `json:"reason,omitempty"`
	Workloads []WorkloadResult `json:"workloads,omitempty"`
}

// Failures counts the failed workloads, plus one if the namespace itself failed.
func (r NamespaceResult) Failures() int {
	failures := 0
	if r.Status == StatusFailed {
		failures++
	}
	for _, workload := range r.Workloads {
		if workload.Status == StatusFailed {
			failures++
		}
	}
	return failures
}

// Hardener fills in the resources and security settings missing from the
// workloads of a namespace.
type Hardener struct {
	// Reader must read from the API server, not from a cache: plans have to be
	// based on the current state, and the workloads need no watches.
	Reader   client.Reader
	Writer   client.Client
	Guard    guard.Guard
	Defaults Defaults
}

// Harden processes each namespace in turn. A failure is reported in the results
// and does not stop the remaining namespaces and workloads. With dryRun, the API
// server computes the changes but persists nothing.
func (h *Hardener) Harden(ctx context.Context, namespaces []string, dryRun bool) []NamespaceResult {
	results := make([]NamespaceResult, 0, len(namespaces))
	for _, namespace := range namespaces {
		results = append(results, h.hardenNamespace(ctx, namespace, dryRun))
	}
	return results
}

func (h *Hardener) hardenNamespace(ctx context.Context, name string, dryRun bool) NamespaceResult {
	result := NamespaceResult{Namespace: name, Status: StatusProcessed}

	namespace := &corev1.Namespace{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
		result.Status = StatusFailed
		result.Reason = fmt.Sprintf("reading namespace: %v", err)
		return result
	}
	if h.Guard.IsNamespaceProtected(namespace.Name, namespace.Labels) {
		result.Status = StatusSkipped
		result.Reason = "namespace is protected"
		return result
	}

	var listErrors []error
	for _, kind := range workloadKinds {
		workloads, err := h.hardenKind(ctx, kind, name, dryRun)
		if err != nil {
			listErrors = append(listErrors, fmt.Errorf("listing %ss: %w", kind.name, err))
		}
		result.Workloads = append(result.Workloads, workloads...)
	}
	if len(listErrors) > 0 {
		result.Status = StatusFailed
		result.Reason = joinErrors(listErrors)
	}
	return result
}

// hardenKind processes the workloads of one kind in a namespace.
func (h *Hardener) hardenKind(ctx context.Context, kind workloadKind, namespace string, dryRun bool) ([]WorkloadResult, error) {
	list := kind.newList()
	if err := h.Reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}

	results := make([]WorkloadResult, 0, len(items))
	for _, item := range items {
		results = append(results, h.hardenWorkload(ctx, kind, item.(client.Object), dryRun))
	}
	return results, nil
}

// hardenWorkload plans and applies the changes for one workload. A conflict means
// the workload changed after it was read, so the plan is made again from the
// current state.
func (h *Hardener) hardenWorkload(ctx context.Context, kind workloadKind, object client.Object, dryRun bool) WorkloadResult {
	result := WorkloadResult{Kind: kind.name, Name: object.GetName()}
	key := client.ObjectKeyFromObject(object)

	firstAttempt := true
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if !firstAttempt {
			object = kind.newObject()
			if err := h.Reader.Get(ctx, key, object); err != nil {
				return err
			}
		}
		firstAttempt = false

		var err error
		result, err = h.applyPlan(ctx, kind.wrap(object), dryRun)
		result.Kind, result.Name = kind.name, key.Name
		return err
	})

	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		result = WorkloadResult{Kind: kind.name, Name: key.Name, Status: StatusSkipped, Reason: "workload was deleted"}
	default:
		result = WorkloadResult{Kind: kind.name, Name: key.Name, Status: StatusFailed, Reason: err.Error()}
	}

	if result.Status == StatusPatched {
		log.FromContext(ctx).Info("hardened workload", "kind", kind.name, "workload", key,
			"changes", result.Changes, "dryRun", dryRun)
	}
	return result
}

// applyPlan decides what the workload needs and applies it. Ownership conflicts
// are not forced, so a value set by someone else is never overwritten.
func (h *Hardener) applyPlan(ctx context.Context, target workload, dryRun bool) (WorkloadResult, error) {
	if guard.IsWorkloadOptedOut(target.object.GetLabels()) {
		return WorkloadResult{Status: StatusSkipped, Reason: "workload opted out"}, nil
	}

	changes, notes := Plan(target.podSpec, h.Defaults)
	if changes.IsEmpty() {
		return WorkloadResult{Status: StatusUnchanged, Notes: notes}, nil
	}

	config, err := target.applyConfig(changes)
	if err != nil {
		return WorkloadResult{}, fmt.Errorf("reading owned fields: %w", err)
	}
	options := []client.ApplyOption{client.FieldOwner(FieldManager)}
	if dryRun {
		// The API server runs defaulting and admission but persists nothing.
		options = append(options, client.DryRunAll)
	}
	if err := h.Writer.Apply(ctx, config, options...); err != nil {
		return WorkloadResult{}, err
	}
	return WorkloadResult{Status: StatusPatched, Notes: notes, Changes: &changes}, nil
}

func joinErrors(errs []error) string {
	message := errs[0].Error()
	for _, err := range errs[1:] {
		message += "; " + err.Error()
	}
	return message
}
