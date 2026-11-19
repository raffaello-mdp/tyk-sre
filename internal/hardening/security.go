package hardening

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// The security settings follow the Pod Security Standards "restricted" profile.
// Workloads are assumed to run on Linux. The settings come in two tiers:
//
//   - allowPrivilegeEscalation: false and seccompProfile: RuntimeDefault are set
//     whenever missing. They only stop a process gaining privileges it did not
//     start with, and block rarely used system calls.
//   - runAsNonRoot: true and capabilities.drop: [ALL] can break an image that
//     runs as root (e.g. nginx binding port 80, or chown at start-up). The image's
//     user cannot be read from the API, so they are set only when the pod already
//     runs as a non-root user ID. A non-root process starts with no capabilities,
//     and with no privilege escalation it cannot gain any, so dropping them all
//     changes nothing for it.
//
// readOnlyRootFilesystem is never set: whether an application writes to its
// filesystem cannot be told from the spec.

// planPodSecurity returns the pod-level settings to add, or nil.
func planPodSecurity(pod *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	if pod != nil && pod.SeccompProfile != nil {
		return nil
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// planContainerSecurity returns the container-level settings to add, or nil.
// Container settings take precedence over pod settings, so a value set at either
// level counts as set.
func planContainerSecurity(container *corev1.SecurityContext, pod *corev1.PodSecurityContext) *corev1.SecurityContext {
	if container == nil {
		container = &corev1.SecurityContext{}
	}
	if pod == nil {
		pod = &corev1.PodSecurityContext{}
	}
	added := &corev1.SecurityContext{}

	// The API server rejects allowPrivilegeEscalation: false next to CAP_SYS_ADMIN.
	if container.AllowPrivilegeEscalation == nil && !addsCapability(container, "SYS_ADMIN") {
		added.AllowPrivilegeEscalation = ptr.To(false)
	}

	if runsAsNonRootUser(container, pod) {
		if container.RunAsNonRoot == nil && pod.RunAsNonRoot == nil {
			added.RunAsNonRoot = ptr.To(true)
		}
		// drop is replaced as a whole, so an existing list is left as it is.
		if container.Capabilities == nil || container.Capabilities.Drop == nil {
			added.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
		}
	}

	if *added == (corev1.SecurityContext{}) {
		return nil
	}
	return added
}

// runsAsNonRootUser reports whether the container is set to run as a user ID other than 0.
func runsAsNonRootUser(container *corev1.SecurityContext, pod *corev1.PodSecurityContext) bool {
	user := container.RunAsUser
	if user == nil {
		user = pod.RunAsUser
	}
	return user != nil && *user > 0
}

// addsCapability reports whether the container adds the capability, with or
// without the CAP_ prefix.
func addsCapability(container *corev1.SecurityContext, name corev1.Capability) bool {
	if container.Capabilities == nil {
		return false
	}
	return slices.ContainsFunc(container.Capabilities.Add, func(added corev1.Capability) bool {
		return added == name || added == "CAP_"+name
	})
}

// isPrivileged reports whether the container runs privileged. It is meant to,
// and restricting it would at best do nothing.
func isPrivileged(container *corev1.Container) bool {
	return container.SecurityContext != nil && ptr.Deref(container.SecurityContext.Privileged, false)
}
