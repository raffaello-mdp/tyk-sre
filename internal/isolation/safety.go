package isolation

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
)

// maxListedPods caps the pod names quoted in a status message.
const maxListedPods = 5

// Assessment is the result of comparing the two sides with the running pods.
type Assessment struct {
	// MatchedPods counts the active pods of each side.
	MatchedPods v1alpha1.MatchedPods
	// BothSides lists the pods selected by both sides, as namespace/name.
	BothSides []string
	// Warnings lists problems that do not prevent enforcement.
	Warnings []string
}

// Assess counts the active pods of each side, finds the pods selected by both
// sides, and collects warnings. Pods that have finished are ignored, since they
// no longer send or receive traffic.
func Assess(podsA, podsB []corev1.Pod) Assessment {
	activePodsInA, hostNetworkA := activePods(podsA)
	activePodsInB, hostNetworkB := activePods(podsB)

	assessment := Assessment{
		MatchedPods: v1alpha1.MatchedPods{A: int32(len(activePodsInA)), B: int32(len(activePodsInB))},
	}
	for _, name := range activePodsInA {
		if slices.Contains(activePodsInB, name) {
			assessment.BothSides = append(assessment.BothSides, name)
		}
	}

	if len(activePodsInA) == 0 {
		assessment.Warnings = append(assessment.Warnings, "side a matches no pods")
	}
	if len(activePodsInB) == 0 {
		assessment.Warnings = append(assessment.Warnings, "side b matches no pods")
	}
	hostNetwork := slices.Compact(slices.Sorted(slices.Values(append(hostNetworkA, hostNetworkB...))))
	if len(hostNetwork) > 0 {
		assessment.Warnings = append(assessment.Warnings,
			"hostNetwork pods are not covered by the policy: "+summarize(hostNetwork))
	}
	return assessment
}

// activePods returns the sorted names of the active pods, and of those among
// them using the host network.
func activePods(pods []corev1.Pod) (names, hostNetwork []string) {
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		name := pod.Namespace + "/" + pod.Name
		names = append(names, name)
		if pod.Spec.HostNetwork {
			hostNetwork = append(hostNetwork, name)
		}
	}
	slices.Sort(names)
	return names, hostNetwork
}

// labelSelector converts the selector of a side for use with the Kubernetes API.
func labelSelector(selector v1alpha1.PodSelector) (labels.Selector, error) {
	converted := &metav1.LabelSelector{}
	for key, value := range selector.MatchLabels {
		metav1.AddLabelToSelector(converted, key, string(value))
	}
	for _, requirement := range selector.MatchExpressions {
		values := make([]string, 0, len(requirement.Values))
		for _, value := range requirement.Values {
			values = append(values, string(value))
		}
		converted.MatchExpressions = append(converted.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      string(requirement.Key),
			Operator: metav1.LabelSelectorOperator(requirement.Operator),
			Values:   values,
		})
	}
	return metav1.LabelSelectorAsSelector(converted)
}

// summarize joins names for a status message, keeping it short.
func summarize(names []string) string {
	if len(names) <= maxListedPods {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxListedPods], ", "), len(names)-maxListedPods)
}
