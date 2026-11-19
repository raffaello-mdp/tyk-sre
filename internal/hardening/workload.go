package hardening

import (
	"maps"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workload is the view of a Deployment, StatefulSet or DaemonSet used for hardening.
type workload struct {
	object  client.Object
	podSpec *corev1.PodSpec
	// applyConfig builds the Server-Side Apply request adding the given changes.
	applyConfig func(changes Changes) (runtime.ApplyConfiguration, error)
}

// workloadKind describes how to read one kind of workload.
type workloadKind struct {
	name      string
	newList   func() client.ObjectList
	newObject func() client.Object
	wrap      func(object client.Object) workload
}

// workloadKinds are the kinds in scope, in the order they are processed.
var workloadKinds = []workloadKind{
	{
		name:      "Deployment",
		newList:   func() client.ObjectList { return &appsv1.DeploymentList{} },
		newObject: func() client.Object { return &appsv1.Deployment{} },
		wrap:      wrapDeployment,
	},
	{
		name:      "StatefulSet",
		newList:   func() client.ObjectList { return &appsv1.StatefulSetList{} },
		newObject: func() client.Object { return &appsv1.StatefulSet{} },
		wrap:      wrapStatefulSet,
	},
	{
		name:      "DaemonSet",
		newList:   func() client.ObjectList { return &appsv1.DaemonSetList{} },
		newObject: func() client.Object { return &appsv1.DaemonSet{} },
		wrap:      wrapDaemonSet,
	},
}

// The apply configurations below start from the fields the tool already owns.
// Server-Side Apply removes owned fields missing from a request, so sending only
// the new values would delete those set by an earlier run. The resource version
// makes the request fail if the workload changed since it was planned.

func wrapDeployment(object client.Object) workload {
	deployment := object.(*appsv1.Deployment)
	return workload{
		object:  deployment,
		podSpec: &deployment.Spec.Template.Spec,
		applyConfig: func(changes Changes) (runtime.ApplyConfiguration, error) {
			config, err := appsv1ac.ExtractDeployment(deployment, FieldManager)
			if err != nil {
				return nil, err
			}
			config.WithResourceVersion(deployment.ResourceVersion)
			if config.Spec == nil {
				config.WithSpec(appsv1ac.DeploymentSpec())
			}
			config.Spec.Template = withChanges(config.Spec.Template, changes)
			return config, nil
		},
	}
}

func wrapStatefulSet(object client.Object) workload {
	statefulSet := object.(*appsv1.StatefulSet)
	return workload{
		object:  statefulSet,
		podSpec: &statefulSet.Spec.Template.Spec,
		applyConfig: func(changes Changes) (runtime.ApplyConfiguration, error) {
			config, err := appsv1ac.ExtractStatefulSet(statefulSet, FieldManager)
			if err != nil {
				return nil, err
			}
			config.WithResourceVersion(statefulSet.ResourceVersion)
			if config.Spec == nil {
				config.WithSpec(appsv1ac.StatefulSetSpec())
			}
			config.Spec.Template = withChanges(config.Spec.Template, changes)
			return config, nil
		},
	}
}

func wrapDaemonSet(object client.Object) workload {
	daemonSet := object.(*appsv1.DaemonSet)
	return workload{
		object:  daemonSet,
		podSpec: &daemonSet.Spec.Template.Spec,
		applyConfig: func(changes Changes) (runtime.ApplyConfiguration, error) {
			config, err := appsv1ac.ExtractDaemonSet(daemonSet, FieldManager)
			if err != nil {
				return nil, err
			}
			config.WithResourceVersion(daemonSet.ResourceVersion)
			if config.Spec == nil {
				config.WithSpec(appsv1ac.DaemonSetSpec())
			}
			config.Spec.Template = withChanges(config.Spec.Template, changes)
			return config, nil
		},
	}
}

// withChanges adds the changes to the pod template of an apply configuration.
func withChanges(template *corev1ac.PodTemplateSpecApplyConfiguration, changes Changes) *corev1ac.PodTemplateSpecApplyConfiguration {
	if template == nil {
		template = corev1ac.PodTemplateSpec()
	}
	if template.Spec == nil {
		template.WithSpec(corev1ac.PodSpec())
	}
	if pod := changes.PodSecurityContext; pod != nil && pod.SeccompProfile != nil {
		if template.Spec.SecurityContext == nil {
			template.Spec.WithSecurityContext(corev1ac.PodSecurityContext())
		}
		template.Spec.SecurityContext.WithSeccompProfile(corev1ac.SeccompProfile().WithType(pod.SeccompProfile.Type))
	}
	for _, change := range changes.Containers {
		containers := &template.Spec.Containers
		if change.Init {
			containers = &template.Spec.InitContainers
		}
		mergeContainer(containers, change)
	}
	return template
}

// mergeContainer adds a change to the entry of its container, creating it if needed.
func mergeContainer(containers *[]corev1ac.ContainerApplyConfiguration, change ContainerChange) {
	index := slices.IndexFunc(*containers, func(c corev1ac.ContainerApplyConfiguration) bool {
		return c.Name != nil && *c.Name == change.Container
	})
	if index < 0 {
		*containers = append(*containers, *corev1ac.Container().WithName(change.Container))
		index = len(*containers) - 1
	}
	container := &(*containers)[index]

	// Empty objects are not sent: applying one would claim ownership of it.
	if len(change.Requests) > 0 || len(change.Limits) > 0 {
		if container.Resources == nil {
			container.WithResources(corev1ac.ResourceRequirements())
		}
		if len(change.Requests) > 0 {
			container.Resources.Requests = merged(container.Resources.Requests, change.Requests)
		}
		if len(change.Limits) > 0 {
			container.Resources.Limits = merged(container.Resources.Limits, change.Limits)
		}
	}

	if added := change.SecurityContext; added != nil {
		if container.SecurityContext == nil {
			container.WithSecurityContext(corev1ac.SecurityContext())
		}
		security := container.SecurityContext
		if added.AllowPrivilegeEscalation != nil {
			security.WithAllowPrivilegeEscalation(*added.AllowPrivilegeEscalation)
		}
		if added.RunAsNonRoot != nil {
			security.WithRunAsNonRoot(*added.RunAsNonRoot)
		}
		if added.Capabilities != nil && added.Capabilities.Drop != nil {
			if security.Capabilities == nil {
				security.WithCapabilities(corev1ac.Capabilities())
			}
			security.Capabilities.Drop = slices.Clone(added.Capabilities.Drop)
		}
	}
}

// merged returns a new list holding the values of both lists.
func merged(existing *corev1.ResourceList, added corev1.ResourceList) *corev1.ResourceList {
	result := corev1.ResourceList{}
	if existing != nil {
		maps.Copy(result, *existing)
	}
	maps.Copy(result, added)
	return &result
}
