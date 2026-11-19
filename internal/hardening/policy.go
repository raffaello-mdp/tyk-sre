// Package hardening fills in the resource requests and limits, and the security
// settings, missing from the workloads of a namespace.
package hardening

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Defaults are the values given to containers that have none.
type Defaults struct {
	CPURequest    resource.Quantity
	MemoryRequest resource.Quantity
	MemoryLimit   resource.Quantity
}

// DefaultResources returns the standard defaults. There is deliberately no CPU
// limit, because it throttles a workload even when the node has spare CPU.
func DefaultResources() Defaults {
	return Defaults{
		CPURequest:    resource.MustParse("50m"),
		MemoryRequest: resource.MustParse("128Mi"),
		MemoryLimit:   resource.MustParse("512Mi"),
	}
}

// Changes lists the values to add to a pod template.
type Changes struct {
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	Containers         []ContainerChange          `json:"containers,omitempty"`
}

// IsEmpty reports whether there is nothing to add.
func (c Changes) IsEmpty() bool {
	return c.PodSecurityContext == nil && len(c.Containers) == 0
}

// ContainerChange lists the values to add to one container.
type ContainerChange struct {
	Container       string                  `json:"container"`
	Init            bool                    `json:"init,omitempty"`
	Requests        corev1.ResourceList     `json:"requests,omitempty"`
	Limits          corev1.ResourceList     `json:"limits,omitempty"`
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
}

// Plan returns the values missing from a pod spec, and notes on the parts it
// deliberately left alone. Existing values are never changed, and a spec with
// nothing missing yields no changes.
func Plan(spec *corev1.PodSpec, defaults Defaults) (Changes, []string) {
	var notes []string

	planResources := spec.Resources == nil
	if !planResources {
		// Defaults added to the containers could exceed the budget of the pod.
		notes = append(notes, "resources left alone: pod-level resources are set")
	}
	changes := Changes{PodSecurityContext: planPodSecurity(spec.SecurityContext)}

	plan := func(container *corev1.Container, init bool) {
		change := ContainerChange{Container: container.Name, Init: init}
		if planResources {
			change.Requests, change.Limits = planContainerResources(container, defaults)
		}
		if isPrivileged(container) {
			notes = append(notes, "securityContext of container "+container.Name+" left alone: privileged")
		} else {
			change.SecurityContext = planContainerSecurity(container.SecurityContext, spec.SecurityContext)
		}
		if len(change.Requests) > 0 || len(change.Limits) > 0 || change.SecurityContext != nil {
			changes.Containers = append(changes.Containers, change)
		}
	}
	for i := range spec.InitContainers {
		plan(&spec.InitContainers[i], true)
	}
	for i := range spec.Containers {
		plan(&spec.Containers[i], false)
	}
	return changes, notes
}

// planContainerResources returns the requests and limits missing from a container.
//
// A resource with a limit but no request is left alone: Kubernetes already uses
// the limit as the request, and adding a lower one would demote a Guaranteed pod
// to Burstable, while a higher one would be rejected.
func planContainerResources(container *corev1.Container, defaults Defaults) (addRequests, addLimits corev1.ResourceList) {
	requests, limits := container.Resources.Requests, container.Resources.Limits

	add := func(list *corev1.ResourceList, name corev1.ResourceName, value resource.Quantity) {
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = value
	}

	if !has(requests, corev1.ResourceCPU) && !has(limits, corev1.ResourceCPU) {
		add(&addRequests, corev1.ResourceCPU, defaults.CPURequest)
	}

	memoryRequest, requested := requests[corev1.ResourceMemory]
	if !requested && !has(limits, corev1.ResourceMemory) {
		memoryRequest = defaults.MemoryRequest
		add(&addRequests, corev1.ResourceMemory, memoryRequest)
	}
	if !has(limits, corev1.ResourceMemory) {
		// A limit below the request would be rejected by the API server.
		memoryLimit := defaults.MemoryLimit
		if memoryRequest.Cmp(memoryLimit) > 0 {
			memoryLimit = memoryRequest
		}
		add(&addLimits, corev1.ResourceMemory, memoryLimit)
	}
	return addRequests, addLimits
}

func has(list corev1.ResourceList, name corev1.ResourceName) bool {
	_, found := list[name]
	return found
}
