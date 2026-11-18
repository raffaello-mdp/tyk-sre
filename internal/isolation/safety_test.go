package isolation

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
)

func podValue(namespace, name string) corev1.Pod {
	return *pod(namespace, name, nil)
}

func TestAssessFindsPodsOnBothSides(t *testing.T) {
	got := Assess(
		[]corev1.Pod{podValue("x", "shared"), podValue("x", "only-a")},
		[]corev1.Pod{podValue("x", "shared"), podValue("x", "only-b")},
	)

	if !slices.Equal(got.BothSides, []string{"x/shared"}) {
		t.Fatalf("BothSides = %v", got.BothSides)
	}
	if got.MatchedPods != (v1alpha1.MatchedPods{A: 2, B: 2}) {
		t.Fatalf("MatchedPods = %+v", got.MatchedPods)
	}
}

// Pods with the same name in different namespaces are different pods.
func TestAssessComparesNamespaceAndName(t *testing.T) {
	got := Assess([]corev1.Pod{podValue("x", "web")}, []corev1.Pod{podValue("y", "web")})

	if len(got.BothSides) != 0 {
		t.Fatalf("BothSides = %v", got.BothSides)
	}
}

// A finished Job pod left behind must not block an isolation during an incident.
func TestAssessIgnoresFinishedPods(t *testing.T) {
	finished := podValue("x", "job")
	finished.Status.Phase = corev1.PodSucceeded
	failed := podValue("x", "crashed")
	failed.Status.Phase = corev1.PodFailed

	got := Assess([]corev1.Pod{finished, failed, podValue("x", "a")}, []corev1.Pod{finished, failed, podValue("x", "b")})

	if len(got.BothSides) != 0 {
		t.Fatalf("BothSides = %v", got.BothSides)
	}
	if got.MatchedPods != (v1alpha1.MatchedPods{A: 1, B: 1}) {
		t.Fatalf("MatchedPods = %+v", got.MatchedPods)
	}
}

func TestAssessWarnsAboutEmptySides(t *testing.T) {
	got := Assess(nil, nil)

	want := []string{"side a matches no pods", "side b matches no pods"}
	if !slices.Equal(got.Warnings, want) {
		t.Fatalf("Warnings = %v", got.Warnings)
	}
}

func TestAssessWarnsAboutHostNetworkPods(t *testing.T) {
	host := podValue("x", "agent")
	host.Spec.HostNetwork = true

	got := Assess([]corev1.Pod{host}, []corev1.Pod{podValue("y", "web")})

	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "x/agent") {
		t.Fatalf("Warnings = %v", got.Warnings)
	}
}

func TestSummarizeCapsLongLists(t *testing.T) {
	got := summarize([]string{"a", "b", "c", "d", "e", "f", "g"})

	if got != "a, b, c, d, e and 2 more" {
		t.Fatalf("summarize = %q", got)
	}
}

func TestLabelSelectorMatchesLikeTheSpec(t *testing.T) {
	selector, err := labelSelector(v1alpha1.PodSelector{
		MatchLabels: map[string]v1alpha1.LabelValue{"app": "gateway"},
		MatchExpressions: []v1alpha1.LabelSelectorRequirement{
			{Key: "tier", Operator: v1alpha1.LabelSelectorOpIn, Values: []v1alpha1.LabelValue{"front", "edge"}},
			{Key: "canary", Operator: v1alpha1.LabelSelectorOpDoesNotExist},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		labels labels.Set
		want   bool
	}{
		"all requirements met": {labels.Set{"app": "gateway", "tier": "edge"}, true},
		"wrong app":            {labels.Set{"app": "api", "tier": "edge"}, false},
		"wrong tier":           {labels.Set{"app": "gateway", "tier": "back"}, false},
		"excluded label":       {labels.Set{"app": "gateway", "tier": "edge", "canary": "yes"}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := selector.Matches(test.labels); got != test.want {
				t.Fatalf("Matches = %v, want %v", got, test.want)
			}
		})
	}
}

// An empty selector means the whole namespace, not "no pods".
func TestLabelSelectorEmptyMatchesEverything(t *testing.T) {
	selector, err := labelSelector(v1alpha1.PodSelector{})
	if err != nil {
		t.Fatal(err)
	}
	if !selector.Matches(labels.Set{"any": "label"}) {
		t.Fatal("an empty selector must match every pod")
	}
}
