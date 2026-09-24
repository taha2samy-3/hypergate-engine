package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=true
// CorrelationIdFilterSpec defines the desired state of CorrelationIdFilter
type CorrelationIdFilterSpec struct {
	// HeaderName is the name of the HTTP header.
	// +optional
	HeaderName string `json:"headerName,omitempty" yaml:"header_name,omitempty"`

	// Algorithm used for ID generation.
	// +optional
	Algorithm string `json:"algorithm,omitempty" yaml:"algorithm,omitempty"`

	// Mode determines when to generate an ID.
	// +optional
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// Prefix to prepend to generated IDs.
	// +optional
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// PropagateToUpstream sends the ID to the upstream service. Default true.
	// The yaml tag has no omitempty so an explicit false reaches the engine
	// (whose own default is true).
	// +kubebuilder:default=true
	// +optional
	PropagateToUpstream bool `json:"propagateToUpstream" yaml:"propagate_to_upstream"`

	// PropagateToDownstream returns the ID to the client. Default true.
	// +kubebuilder:default=true
	// +optional
	PropagateToDownstream bool `json:"propagateToDownstream" yaml:"propagate_to_downstream"`

	// InputHeaderName the input header name.
	// +optional
	InputHeaderName string `json:"inputHeaderName,omitempty" yaml:"input_header_name,omitempty"`

	// ResponseHeaderName the response header name.
	// +optional
	ResponseHeaderName string `json:"responseHeaderName,omitempty" yaml:"response_header_name,omitempty"`

	// ValidationRegex validates incoming correlation IDs.
	// +optional
	ValidationRegex string `json:"validationRegex,omitempty" yaml:"validation_regex,omitempty"`
}

// +kubebuilder:object:generate=true
// CorrelationIdFilterStatus defines the observed state of CorrelationIdFilter
type CorrelationIdFilterStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// CorrelationIdFilter is the Schema for the correlationidfilters API
type CorrelationIdFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CorrelationIdFilterSpec   `json:"spec,omitempty"`
	Status CorrelationIdFilterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CorrelationIdFilterList contains a list of CorrelationIdFilter
type CorrelationIdFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CorrelationIdFilter `json:"items"`
}
