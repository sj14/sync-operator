package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SyncObjectSpec defines the desired state of SyncObject
// +kubebuilder:validation:XValidation:rule="!has(self.targetNamespaces) || !has(self.ignoreNamespaces) || !self.targetNamespaces.exists(n, n in self.ignoreNamespaces)",message="a namespace cannot be in both targetNamespaces and ignoreNamespaces"
type SyncObjectSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Reference Reference `json:"reference"`
	// If no target namespaces are defined, all namespaces will be used.
	// +kubebuilder:validation:MaxItems=1000
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`
	// Explicitly skip replication to the specified namespaces.
	// +kubebuilder:validation:MaxItems=1000
	IgnoreNamespaces []string `json:"ignoreNamespaces,omitempty"`
	// Don't add a finalizer which would clean up the replicas when this SyncObject gets deleted.
	DisableFinalizer bool `json:"disableFinalizer,omitempty"`
	// ResyncInterval is how often the reference resource is re-checked and
	// re-applied even without a detected change. Changes to the reference
	// resource itself are synced immediately via a watch; this interval only
	// matters as a drift-correction fallback (e.g. a replica was edited
	// directly, a new target namespace appeared, or the watch could not yet
	// be established).
	//
	// Zero means the default. A negative value would silently disable the
	// resync altogether, and anything under a second would hammer the API
	// server, so both are rejected.
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || duration(self) >= duration('1s')",message="resyncInterval must be at least 1s, or 0 to use the default"
	ResyncInterval metav1.Duration `json:"resyncInterval,omitempty"`
}

type Reference struct {
	// Group of the referenced resource, empty for the core group.
	Group string `json:"group"`
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// GroupVersionKind returns the GroupVersionKind the reference points at.
func (r Reference) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// SyncObjectStatus defines the observed state of SyncObject
type SyncObjectStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// AppliedReference is the reference whose replicas were last created.
	// It's what lets a change to spec.reference clean up the replicas of
	// the previous reference, which are named after it and would otherwise
	// be orphaned.
	// +optional
	AppliedReference *Reference `json:"appliedReference,omitempty"`

	// ObservedGeneration is the metadata.generation this status was last
	// reconciled from. When it trails metadata.generation, the most recent
	// change to the spec has not been acted on yet.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the Ready condition, which reports whether the last
	// sync succeeded and, when it didn't, why.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConditionReady is set on a SyncObject to report whether its last sync
// succeeded.
const ConditionReady = "Ready"

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.reference.kind`
//+kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.reference.name`
//+kubebuilder:printcolumn:name="Source-Namespace",type=string,JSONPath=`.spec.reference.namespace`,priority=1
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
//+kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SyncObject is the Schema for the syncobjects API
type SyncObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SyncObjectSpec   `json:"spec,omitempty"`
	Status SyncObjectStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SyncObjectList contains a list of SyncObject
type SyncObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SyncObject `json:"items"`
}
