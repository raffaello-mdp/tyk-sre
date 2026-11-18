package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionEnforced is the condition type reporting whether the network policy
// of an Isolation is in place.
const ConditionEnforced = "Enforced"

// Reasons used with ConditionEnforced.
const (
	// ReasonPolicyApplied means the policy matches the current spec.
	ReasonPolicyApplied = "PolicyApplied"
	// ReasonLastValidPolicyKept means the current spec failed the safety checks
	// and the policy of the last valid spec is still enforced.
	ReasonLastValidPolicyKept = "LastValidPolicyKept"
	// ReasonPodsMatchBothSides means no policy was created because at least one
	// pod is selected by both sides.
	ReasonPodsMatchBothSides = "PodsMatchBothSides"
	// ReasonCiliumNotInstalled means the cluster does not serve the Cilium policy API.
	ReasonCiliumNotInstalled = "CiliumNotInstalled"
)

// NamespaceName is the name of a namespace (an RFC 1123 label).
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
type NamespaceName string

// LabelKey is a label key: an optional DNS subdomain prefix and a name.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=317
// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
// +kubebuilder:validation:XValidation:rule=`self.indexOf('/') <= 253`,message="label key prefix must be at most 253 characters"
type LabelKey string

// LabelValue is a label value. It may be empty.
//
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`
type LabelValue string

// LabelSelectorOperator is the relation between a label key and a set of values.
//
// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
type LabelSelectorOperator string

// Operators of a LabelSelectorRequirement.
const (
	LabelSelectorOpIn           LabelSelectorOperator = "In"
	LabelSelectorOpNotIn        LabelSelectorOperator = "NotIn"
	LabelSelectorOpExists       LabelSelectorOperator = "Exists"
	LabelSelectorOpDoesNotExist LabelSelectorOperator = "DoesNotExist"
)

// LabelSelectorRequirement is a single label requirement of a PodSelector.
//
// +kubebuilder:validation:XValidation:rule=`self.operator in ['In', 'NotIn'] ? has(self.values) : !has(self.values)`,message="values must be set for In and NotIn, and must be omitted for Exists and DoesNotExist"
type LabelSelectorRequirement struct {
	// key is the label key the requirement applies to.
	// +required
	Key LabelKey `json:"key"`

	// operator is one of In, NotIn, Exists and DoesNotExist.
	// +required
	Operator LabelSelectorOperator `json:"operator"`

	// values is the set of label values used by In and NotIn.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Values []LabelValue `json:"values,omitempty"`
}

// PodSelector selects pods by label. It mirrors metav1.LabelSelector, with the
// bounds and syntax checks the API server needs to validate it at admission.
// All requirements are ANDed. An empty selector matches every pod.
type PodSelector struct {
	// matchLabels selects pods carrying all of these labels.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule=`self.all(k, k.size() <= 317 && k.indexOf('/') <= 253 && k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$'))`,message="matchLabels keys must be valid label keys"
	MatchLabels map[string]LabelValue `json:"matchLabels,omitempty"`

	// matchExpressions selects pods satisfying all of these requirements. Each
	// label key may appear once, which makes the comparison of the two sides
	// independent of the order of the requirements.
	// +optional
	// +listType=map
	// +listMapKey=key
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// WorkloadSelector defines one side of an Isolation.
type WorkloadSelector struct {
	// namespaces lists the namespaces this side covers.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Namespaces []NamespaceName `json:"namespaces"`

	// selector selects the pods of this side within its namespaces. It is
	// required so that covering whole namespaces is an explicit choice: use an
	// empty selector ({}) to do so.
	// +required
	Selector PodSelector `json:"selector"`
}

// IsolationSpec defines the two workloads to keep apart.
//
// CEL types each side by its schema path, so comparing them requires dyn().
//
// +kubebuilder:validation:XValidation:rule="dyn(self.a) != dyn(self.b)",message="sides a and b must differ"
type IsolationSpec struct {
	// reason records why the isolation exists, for example an incident reference.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`\S`
	Reason string `json:"reason"`

	// a is the first side of the isolation.
	// +required
	A WorkloadSelector `json:"a"`

	// b is the second side of the isolation.
	// +required
	B WorkloadSelector `json:"b"`
}

// MatchedPods counts the pods currently selected by each side.
type MatchedPods struct {
	// a is the number of pods selected by side a.
	// +required
	// +kubebuilder:validation:Minimum=0
	A int32 `json:"a"`

	// b is the number of pods selected by side b.
	// +required
	// +kubebuilder:validation:Minimum=0
	B int32 `json:"b"`
}

// IsolationStatus reports the observed state of an Isolation.
type IsolationStatus struct {
	// observedGeneration is the most recent generation seen by the controller.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the current state of the isolation.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// matchedPods counts the pods currently selected by each side.
	// +optional
	MatchedPods MatchedPods `json:"matchedPods,omitzero"`

	// warnings lists problems that do not prevent enforcement, such as a side
	// matching no pods.
	// +optional
	// +listType=atomic
	Warnings []string `json:"warnings,omitempty"`
}

// Isolation blocks all network traffic between two workloads for as long as it
// exists. Deleting it restores their previous connectivity.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="metadata.name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Enforced",type=string,JSONPath=`.status.conditions[?(@.type=="Enforced")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Enforced")].reason`
// +kubebuilder:printcolumn:name="Pods A",type=integer,JSONPath=`.status.matchedPods.a`
// +kubebuilder:printcolumn:name="Pods B",type=integer,JSONPath=`.status.matchedPods.b`
// +kubebuilder:printcolumn:name="Warnings",type=string,JSONPath=`.status.warnings`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Isolation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the two workloads to keep apart.
	// +required
	Spec IsolationSpec `json:"spec"`

	// status reports the observed state of the isolation.
	// +optional
	Status IsolationStatus `json:"status,omitzero"`
}

// IsolationList is a list of Isolation resources.
//
// +kubebuilder:object:root=true
type IsolationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Isolation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Isolation{}, &IsolationList{})
}
