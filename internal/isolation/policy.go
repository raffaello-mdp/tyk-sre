// Package isolation implements the controller behind the Isolation resource.
package isolation

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
)

const (
	// IsolationLabel marks a policy with the name of the Isolation it enforces.
	IsolationLabel = "sre.tyk.io/isolation"

	policyNamePrefix     = "isolation-"
	ciliumNamespaceLabel = "k8s:io.kubernetes.pod.namespace"
)

// PolicyGVK identifies the Cilium policy enforcing an Isolation. The policy is
// handled as unstructured data so the tool does not depend on the Cilium module.
var PolicyGVK = schema.GroupVersionKind{
	Group:   "cilium.io",
	Version: "v2",
	Kind:    "CiliumClusterwideNetworkPolicy",
}

func newPolicy() *unstructured.Unstructured {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(PolicyGVK)
	return policy
}

// PolicyName returns the name of the policy owned by the named Isolation.
func PolicyName(isolationName string) string {
	return policyNamePrefix + isolationName
}

// BuildPolicy returns the policy enforcing iso. It holds one rule set per side,
// so that the block is enforced on both ends of a connection.
func BuildPolicy(iso *v1alpha1.Isolation) *unstructured.Unstructured {
	policy := newPolicy()
	policy.SetName(PolicyName(iso.Name))
	policy.SetLabels(map[string]string{IsolationLabel: iso.Name})
	policy.SetOwnerReferences([]metav1.OwnerReference{ownerReference(iso)})
	policy.Object["specs"] = []any{
		ruleSet(fmt.Sprintf("isolation %s: side a denies side b", iso.Name), iso.Spec.A, iso.Spec.B),
		ruleSet(fmt.Sprintf("isolation %s: side b denies side a", iso.Name), iso.Spec.B, iso.Spec.A),
	}
	return policy
}

// policyMatches reports whether the existing policy already enforces desired.
func policyMatches(existing, desired *unstructured.Unstructured) bool {
	_, hasSingleSpec := existing.Object["spec"]
	return !hasSingleSpec &&
		existing.GetLabels()[IsolationLabel] == desired.GetLabels()[IsolationLabel] &&
		equality.Semantic.DeepEqual(existing.Object["specs"], desired.Object["specs"])
}

func ownerReference(iso *v1alpha1.Isolation) metav1.OwnerReference {
	isController := true
	blockOwnerDeletion := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "Isolation",
		Name:               iso.Name,
		UID:                iso.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

// ruleSet denies all traffic between self and peer, from the side of self.
func ruleSet(description string, self, peer v1alpha1.WorkloadSelector) map[string]any {
	return map[string]any{
		"description":      description,
		"endpointSelector": endpointSelector(self),
		"enableDefaultDeny": map[string]any{"ingress": false, "egress": false},
		"ingressDeny":       []any{map[string]any{"fromEndpoints": []any{endpointSelector(peer)}}},
		"egressDeny":        []any{map[string]any{"toEndpoints": []any{endpointSelector(peer)}}},
	}
}

// endpointSelector renders a side as a Cilium selector. Lists are sorted, so
// reordering them in the spec does not rewrite the policy.
func endpointSelector(side v1alpha1.WorkloadSelector) map[string]any {
	namespaces := make([]string, 0, len(side.Namespaces))
	for _, ns := range side.Namespaces {
		namespaces = append(namespaces, string(ns))
	}
	expressions := []any{map[string]any{
		"key":      ciliumNamespaceLabel,
		"operator": "In",
		"values":   sortedValues(namespaces),
	}}

	requirements := slices.Clone(side.Selector.MatchExpressions)
	slices.SortFunc(requirements, func(a, b v1alpha1.LabelSelectorRequirement) int {
		return compareStrings(string(a.Key), string(b.Key))
	})
	for _, requirement := range requirements {
		expression := map[string]any{
			"key":      string(requirement.Key),
			"operator": string(requirement.Operator),
		}
		if len(requirement.Values) > 0 {
			values := make([]string, 0, len(requirement.Values))
			for _, value := range requirement.Values {
				values = append(values, string(value))
			}
			expression["values"] = sortedValues(values)
		}
		expressions = append(expressions, expression)
	}

	selector := map[string]any{"matchExpressions": expressions}
	if len(side.Selector.MatchLabels) > 0 {
		matchLabels := make(map[string]any, len(side.Selector.MatchLabels))
		for key, value := range side.Selector.MatchLabels {
			matchLabels[key] = string(value)
		}
		selector["matchLabels"] = matchLabels
	}
	return selector
}

func sortedValues(values []string) []any {
	slices.Sort(values)
	sorted := make([]any, 0, len(values))
	for _, value := range values {
		sorted = append(sorted, value)
	}
	return sorted
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
