package hardening

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func quantities(pairs ...string) corev1.ResourceList {
	list := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		list[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return list
}

func TestPlanContainerResources(t *testing.T) {
	tests := map[string]struct {
		resources    corev1.ResourceRequirements
		wantRequests corev1.ResourceList
		wantLimits   corev1.ResourceList
	}{
		"nothing set gets every default": {
			wantRequests: quantities("cpu", "50m", "memory", "128Mi"),
			wantLimits:   quantities("memory", "512Mi"),
		},
		"complete container is left alone": {
			resources: corev1.ResourceRequirements{
				Requests: quantities("cpu", "1", "memory", "1Gi"),
				Limits:   quantities("memory", "2Gi"),
			},
		},
		"no CPU limit is ever added": {
			resources: corev1.ResourceRequirements{
				Requests: quantities("cpu", "100m", "memory", "64Mi"),
				Limits:   quantities("memory", "64Mi"),
			},
		},
		// Tricky: the default limit would sit below the request, which the API server rejects.
		"memory limit is raised to a request above the default": {
			resources:  corev1.ResourceRequirements{Requests: quantities("cpu", "1", "memory", "2Gi")},
			wantLimits: quantities("memory", "2Gi"),
		},
		// Tricky: Kubernetes uses the limit as the request, so adding one would change the QoS class.
		"limit without request keeps its implicit request": {
			resources: corev1.ResourceRequirements{Limits: quantities("cpu", "10m", "memory", "32Mi")},
		},
		"explicit zero counts as set": {
			resources: corev1.ResourceRequirements{
				Requests: quantities("cpu", "0", "memory", "0"),
				Limits:   quantities("memory", "64Mi"),
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			container := corev1.Container{Name: "app", Resources: test.resources}

			requests, limits := planContainerResources(&container, DefaultResources())

			if !equality.Semantic.DeepEqual(requests, test.wantRequests) {
				t.Errorf("requests = %v, want %v", requests, test.wantRequests)
			}
			if !equality.Semantic.DeepEqual(limits, test.wantLimits) {
				t.Errorf("limits = %v, want %v", limits, test.wantLimits)
			}
		})
	}
}

func TestPlanContainerSecurity(t *testing.T) {
	dropAll := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}

	tests := map[string]struct {
		container *corev1.SecurityContext
		pod       *corev1.PodSecurityContext
		want      *corev1.SecurityContext
	}{
		// No evidence of a non-root user: only the low-risk setting.
		"nothing set": {
			want: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
		},
		"non-root user on the pod gets the full set": {
			pod: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)},
			want: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				RunAsNonRoot:             ptr.To(true),
				Capabilities:             dropAll,
			},
		},
		"non-root user on the container gets the full set": {
			container: &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000)},
			want: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				RunAsNonRoot:             ptr.To(true),
				Capabilities:             dropAll,
			},
		},
		// Tricky: the container setting wins over the pod one, so this container runs as root.
		"container overriding the pod to root": {
			container: &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)},
			pod:       &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)},
			want:      &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
		},
		"explicit allowPrivilegeEscalation is kept": {
			container: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(true)},
		},
		// Tricky: the API server rejects allowPrivilegeEscalation: false next to CAP_SYS_ADMIN.
		"CAP_SYS_ADMIN prevents allowPrivilegeEscalation": {
			container: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CAP_SYS_ADMIN"}}},
		},
		"SYS_ADMIN without prefix too": {
			container: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}},
		},
		"runAsNonRoot set to false on the pod is respected": {
			pod: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsNonRoot: ptr.To(false)},
			want: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             dropAll,
			},
		},
		"an existing drop list is not replaced": {
			container: &corev1.SecurityContext{
				RunAsUser:    ptr.To[int64](1000),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}},
			},
			want: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				RunAsNonRoot:             ptr.To(true),
			},
		},
		"added capabilities survive dropping the rest": {
			container: &corev1.SecurityContext{
				RunAsUser:    ptr.To[int64](1000),
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}},
			},
			want: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				RunAsNonRoot:             ptr.To(true),
				Capabilities:             dropAll,
			},
		},
		"already restricted": {
			container: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             dropAll,
			},
			pod: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsNonRoot: ptr.To(true)},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := planContainerSecurity(test.container, test.pod)

			if !equality.Semantic.DeepEqual(got, test.want) {
				t.Errorf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestPlanPodSecurity(t *testing.T) {
	if got := planPodSecurity(nil); got == nil || got.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("missing seccomp profile: got %+v, want RuntimeDefault", got)
	}
	unconfined := &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
	if got := planPodSecurity(unconfined); got != nil {
		t.Errorf("explicit seccomp profile was replaced with %+v", got)
	}
}

func TestPlanCoversInitContainers(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init"}},
		Containers:     []corev1.Container{{Name: "app"}},
	}

	changes, _ := Plan(&spec, DefaultResources())

	if len(changes.Containers) != 2 {
		t.Fatalf("got %d container changes, want 2", len(changes.Containers))
	}
	if changes.Containers[0].Container != "init" || !changes.Containers[0].Init {
		t.Errorf("first change = %+v, want the init container", changes.Containers[0])
	}
	if changes.Containers[1].Container != "app" || changes.Containers[1].Init {
		t.Errorf("second change = %+v, want the regular container", changes.Containers[1])
	}
}

func TestPlanLeavesPartsAlone(t *testing.T) {
	tests := map[string]struct {
		spec          corev1.PodSpec
		wantResources bool
		wantSecurity  bool
		wantNote      string
	}{
		"pod-level resources": {
			spec:         corev1.PodSpec{Resources: &corev1.ResourceRequirements{Requests: quantities("memory", "64Mi")}},
			wantSecurity: true,
			wantNote:     "resources left alone: pod-level resources are set",
		},
		"privileged container": {
			spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:            "app",
				SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			}}},
			wantResources: true,
			wantNote:      "securityContext of container app left alone: privileged",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			spec := test.spec
			if len(spec.Containers) == 0 {
				spec.Containers = []corev1.Container{{Name: "app"}}
			}

			changes, notes := Plan(&spec, DefaultResources())

			container := ContainerChange{}
			if len(changes.Containers) > 0 {
				container = changes.Containers[0]
			}
			if gotResources := len(container.Requests) > 0; gotResources != test.wantResources {
				t.Errorf("resources planned = %v, want %v", gotResources, test.wantResources)
			}
			if gotSecurity := container.SecurityContext != nil; gotSecurity != test.wantSecurity {
				t.Errorf("container security planned = %v, want %v", gotSecurity, test.wantSecurity)
			}
			if !slices.Contains(notes, test.wantNote) {
				t.Errorf("notes = %q, want %q", notes, test.wantNote)
			}
		})
	}
}
