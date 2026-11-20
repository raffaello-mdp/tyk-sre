package isolation

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/raffaello-mdp/tyk-sre/api/v1alpha1"
)

func rules(t *testing.T, policy *unstructured.Unstructured) []map[string]any {
	t.Helper()
	specs, ok := policy.Object["specs"].([]any)
	if !ok || len(specs) != 2 {
		t.Fatalf("specs = %v, want one rule set per side", policy.Object["specs"])
	}
	return []map[string]any{specs[0].(map[string]any), specs[1].(map[string]any)}
}

func TestBuildPolicyDeniesBothDirectionsOnBothEnds(t *testing.T) {
	iso := isolation("inc")
	a := endpointSelector(iso.Spec.A)
	b := endpointSelector(iso.Spec.B)

	got := rules(t, BuildPolicy(iso))

	for i, want := range []struct{ self, peer map[string]any }{{a, b}, {b, a}} {
		rule := got[i]
		if !equality.Semantic.DeepEqual(rule["endpointSelector"], want.self) {
			t.Errorf("rule %d selects %v", i, rule["endpointSelector"])
		}
		ingress := rule["ingressDeny"].([]any)[0].(map[string]any)["fromEndpoints"].([]any)[0]
		egress := rule["egressDeny"].([]any)[0].(map[string]any)["toEndpoints"].([]any)[0]
		if !equality.Semantic.DeepEqual(ingress, want.peer) || !equality.Semantic.DeepEqual(egress, want.peer) {
			t.Errorf("rule %d does not deny its peer: ingress %v, egress %v", i, ingress, egress)
		}
	}
}

// Tricky: Without this setting, a deny-only policy puts the selected pods in default-deny
// and cuts all of their other traffic, which is what the brief forbids.
func TestBuildPolicyKeepsOtherTrafficWorking(t *testing.T) {
	for i, rule := range rules(t, BuildPolicy(isolation("inc"))) {
		want := map[string]any{"ingress": false, "egress": false}
		if !equality.Semantic.DeepEqual(rule["enableDefaultDeny"], want) {
			t.Errorf("rule %d: enableDefaultDeny = %v", i, rule["enableDefaultDeny"])
		}
		if _, ok := rule["ingress"]; ok {
			t.Errorf("rule %d has allow rules", i)
		}
		if _, ok := rule["egress"]; ok {
			t.Errorf("rule %d has allow rules", i)
		}
	}
}

func TestEndpointSelectorScopesToNamespacesAndLabels(t *testing.T) {
	got := endpointSelector(v1alpha1.WorkloadSelector{
		Namespaces: []v1alpha1.NamespaceName{"tenant-b", "tenant-a"},
		Selector: v1alpha1.PodSelector{
			MatchLabels: map[string]v1alpha1.LabelValue{"app": "gateway"},
			MatchExpressions: []v1alpha1.LabelSelectorRequirement{
				{Key: "tier", Operator: v1alpha1.LabelSelectorOpIn, Values: []v1alpha1.LabelValue{"b", "a"}},
				{Key: "canary", Operator: v1alpha1.LabelSelectorOpDoesNotExist},
			},
		},
	})

	want := map[string]any{
		"matchLabels": map[string]any{"app": "gateway"},
		"matchExpressions": []any{
			map[string]any{"key": ciliumNamespaceLabel, "operator": "In", "values": []any{"tenant-a", "tenant-b"}},
			map[string]any{"key": "canary", "operator": "DoesNotExist"},
			map[string]any{"key": "tier", "operator": "In", "values": []any{"a", "b"}},
		},
	}
	if !equality.Semantic.DeepEqual(got, want) {
		t.Fatalf("selector = %v\nwant %v", got, want)
	}
}

// Tricky: An empty selector must still be limited to the namespaces of the side: without
// the namespace expression it would match every pod in the cluster.
func TestEndpointSelectorWithEmptySelectorStaysInItsNamespaces(t *testing.T) {
	got := endpointSelector(v1alpha1.WorkloadSelector{Namespaces: []v1alpha1.NamespaceName{"tenant-a"}})

	want := map[string]any{"matchExpressions": []any{
		map[string]any{"key": ciliumNamespaceLabel, "operator": "In", "values": []any{"tenant-a"}},
	}}
	if !equality.Semantic.DeepEqual(got, want) {
		t.Fatalf("selector = %v", got)
	}
}

func TestPolicyMatchesIgnoresOrderOfSpecLists(t *testing.T) {
	first := isolation("inc")
	first.Spec.A.Namespaces = []v1alpha1.NamespaceName{"x", "y"}
	second := isolation("inc")
	second.Spec.A.Namespaces = []v1alpha1.NamespaceName{"y", "x"}

	if !policyMatches(BuildPolicy(first), BuildPolicy(second)) {
		t.Fatal("reordering namespaces must not rewrite the policy")
	}
}

func TestBuiltPolicyCanBeDeepCopied(t *testing.T) {
	// Unstructured content must only hold JSON types, or DeepCopy panics.
	BuildPolicy(isolation("inc")).DeepCopy()
}
