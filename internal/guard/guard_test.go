package guard

import "testing"

func TestIsNamespaceProtected(t *testing.T) {
	g := New("tyk-sre")

	tests := map[string]struct {
		namespace string
		labels    map[string]string
		want      bool
	}{
		"system namespace":      {namespace: "kube-system", want: true},
		"cni namespace":         {namespace: "cilium", want: true},
		"cni namespace secrets": {namespace: "cilium-secrets", want: true},
		"tool namespace":        {namespace: "tyk-sre", want: true},
		"opted out":             {namespace: "tenant-a", labels: map[string]string{OptOutLabel: "true"}, want: true},
		"opt-out set to false":  {namespace: "tenant-a", labels: map[string]string{OptOutLabel: "false"}},
		"ordinary namespace":    {namespace: "tenant-a"},
		"similar name is free":  {namespace: "kube-system-2"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := g.IsNamespaceProtected(test.namespace, test.labels); got != test.want {
				t.Fatalf("IsNamespaceProtected = %v, want %v", got, test.want)
			}
		})
	}
}
