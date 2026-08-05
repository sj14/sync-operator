package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SyncObjectSpec defines the desired state of SyncObject
type SyncObjectSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Reference Reference `json:"reference"`
	// If no target namespaces are defined, all namespaces will be used.
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`
	// Explicitly skip replication to the specified namespaces.
	IgnoreNamespaces []string `json:"ignoreNamespaces,omitempty"`
	// Don't add a finalizer which would clean up the replicas when this SyncObject gets deleted.
	DisableFinalizer bool `json:"disableFinalizer,omitempty"`
	// ResyncInterval is how often the reference resource is re-checked and
	// re-applied even without a detected change. Changes to the reference
	// resource itself are synced immediately via a watch; this interval only
	// matters as a drift-correction fallback (e.g. a replica was edited
	// directly, a new target namespace appeared, or the watch could not yet
	// be established).
	// +kubebuilder:default="1h"
	ResyncInterval metav1.Duration `json:"resyncInterval,omitempty"`
}

type Reference struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// SyncObjectStatus defines the observed state of SyncObject
type SyncObjectStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

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
