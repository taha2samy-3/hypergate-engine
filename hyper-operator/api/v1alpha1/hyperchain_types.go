package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=true
// FilterReference defines a reference to a specific filter CRD.
type FilterReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=RateLimitFilter;HeaderModifierFilter;DenyFilter;CorrelationIdFilter;RedisMetadataEnricherFilter
	Kind string `json:"kind"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// +kubebuilder:object:generate=true
// HyperChainSpec defines the desired state of HyperChain
type HyperChainSpec struct {
	// +kubebuilder:validation:Required
	Filters []FilterReference `json:"filters"`
}

// +kubebuilder:object:generate=true
// HyperChainStatus defines the observed state of HyperChain
type HyperChainStatus struct {
	// +kubebuilder:default="Pending"
	State string `json:"state,omitempty"`

	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message"

// HyperChain is the Schema for the hyperchains API
type HyperChain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HyperChainSpec   `json:"spec,omitempty"`
	Status HyperChainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HyperChainList contains a list of HyperChain
type HyperChainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HyperChain `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HyperChain{}, &HyperChainList{})
}
