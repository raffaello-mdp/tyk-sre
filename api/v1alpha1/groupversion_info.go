// Package v1alpha1 contains the API types of the sre.tyk.io group.
//
// +kubebuilder:object:generate=true
// +groupName=sre.tyk.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of the types in this package.
	GroupVersion = schema.GroupVersion{Group: "sre.tyk.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types in this package with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this package to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
