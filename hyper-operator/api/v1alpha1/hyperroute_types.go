package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TrafficClass says whether a request entered through a gateway (NorthSouth) or
// is a call between workloads (EastWest).
// +kubebuilder:validation:Enum=NorthSouth;EastWest;Any
type TrafficClass string

const (
	TrafficNorthSouth TrafficClass = "NorthSouth"
	TrafficEastWest   TrafficClass = "EastWest"
	TrafficAny        TrafficClass = "Any"
)

// SourceSelectorPattern and DestinationSelectorPattern are the schema patterns of
// MatchRule.Sources and MatchRule.Destinations (kept in sync with the CRD by a test).
// The engine's selector parser does the full validation.
const (
	SourceSelectorPattern      = `^(any|external|ip:[0-9A-Fa-f.:]+|cidr:[0-9A-Fa-f.:]+/[0-9]{1,3}|service:[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9]*[a-z0-9])?|namespace:[a-z0-9]([-a-z0-9]*[a-z0-9])?|sa:[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?|spiffe://[^/]+/ns/[^/]+/sa/[^/]+|labels:[^ ]+=[^ ]*)$`
	DestinationSelectorPattern = `^(any|ip:[0-9A-Fa-f.:]+|cidr:[0-9A-Fa-f.:]+/[0-9]{1,3}|host:(\*\.)?[A-Za-z0-9_.-]+|service:[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9]*[a-z0-9])?|namespace:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`
)

// +kubebuilder:object:generate=true
type MatchRule struct {
	// Traffic restricts the match to one traffic class. Default Any.
	// +optional
	Traffic TrafficClass `json:"traffic,omitempty"`

	// Sources lists the callers this match applies to, as prefix:value selectors
	// (ip:, cidr:, any; service:, namespace:, sa:, spiffe:, labels: and external
	// once workload identity is available). Any entry may match. Empty means any caller.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Sources []string `json:"sources,omitempty"`

	// Destinations lists the targets this match applies to, as prefix:value
	// selectors (ip:, cidr:, host:, any; service: and namespace: once workload
	// identity is available). Any entry may match. Empty means any target.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Destinations []string `json:"destinations,omitempty"`

	// +optional
	PathPrefix string `json:"pathPrefix,omitempty"`

	// +optional
	PathRegexPattern string `json:"pathRegexPattern,omitempty"`

	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// +kubebuilder:object:generate=true
// HyperRouteSpec defines the desired state of HyperRoute
type HyperRouteSpec struct {
	// +kubebuilder:validation:Required
	Priority int `json:"priority"`

	// +kubebuilder:validation:Required
	TargetPolicy string `json:"targetPolicy"`

	// +kubebuilder:validation:Required
	Matches []MatchRule `json:"matches"`
}

// HyperRoute status states.
const (
	HyperRouteStateReady   = "Ready"
	HyperRouteStateInvalid = "Invalid"
)

// +kubebuilder:object:generate=true
// HyperRouteStatus defines the observed state of HyperRoute
type HyperRouteStatus struct {
	// State is Ready when the route is compiled into the engine configuration, or
	// Invalid when it is skipped (Message says why).
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Target Policy",type="string",JSONPath=".spec.targetPolicy"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"

// HyperRoute is the Schema for the hyperroutes API
type HyperRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HyperRouteSpec   `json:"spec,omitempty"`
	Status HyperRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HyperRouteList contains a list of HyperRoute
type HyperRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HyperRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HyperRoute{}, &HyperRouteList{})
}
