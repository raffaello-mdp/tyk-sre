// Package guard decides which namespaces the tool must never act on.
package guard

// OptOutLabel excludes the namespace or workload carrying it, when set to "true".
const OptOutLabel = "sre.tyk.io/opt-out"

// defaultProtectedNamespaces are the system and CNI namespaces.
var defaultProtectedNamespaces = []string{
	"cilium",
	"cilium-secrets",
	"kube-node-lease",
	"kube-public",
	"kube-system",
	"local-path-storage",
}

// Guard is the set of protected namespaces.
type Guard struct {
	protected map[string]struct{}
}

// New returns a Guard protecting the default namespaces and the given ones.
func New(namespaces ...string) Guard {
	protected := make(map[string]struct{}, len(defaultProtectedNamespaces)+len(namespaces))
	for _, ns := range defaultProtectedNamespaces {
		protected[ns] = struct{}{}
	}
	for _, ns := range namespaces {
		protected[ns] = struct{}{}
	}
	return Guard{protected: protected}
}

// IsNamespaceProtected reports whether the tool should act on the namespace
// with the given name and labels.
func (g Guard) IsNamespaceProtected(name string, labels map[string]string) bool {
	_, protected := g.protected[name]
	return protected || labels[OptOutLabel] == "true"
}
