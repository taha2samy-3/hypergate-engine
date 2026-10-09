package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=true
// FilterReference defines a reference to a specific filter CRD.
type FilterReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=RateLimitFilter;HeaderModifierFilter;DenyFilter;CorrelationIdFilter;RedisMetadataEnricherFilter;ApiKeyFilter;ExternalAuthFilter;FirewallFilter;JwtAuthFilter;CorsFilter
	Kind string `json:"kind"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Audit overrides the chain's mode for this filter: true records its denials
	// without enforcing them, false enforces it even in an Audit chain.
	// +optional
	Audit *bool `json:"audit,omitempty"`
}

// ChainMode says whether a chain's denials are enforced.
// +kubebuilder:validation:Enum=Enforce;Audit
type ChainMode string

const (
	ChainEnforce ChainMode = "Enforce"
	ChainAudit   ChainMode = "Audit"
)

// LimitAction says what happens when a chain hits a limit.
// +kubebuilder:validation:Enum=Deny;Allow
type LimitAction string

const (
	LimitDeny  LimitAction = "Deny"
	LimitAllow LimitAction = "Allow"
)

// +kubebuilder:object:generate=true
// HyperChainSpec defines the desired state of HyperChain
type HyperChainSpec struct {
	// +kubebuilder:validation:Required
	Filters []FilterReference `json:"filters"`

	// Mode is Enforce (default) or Audit: in Audit, denials and failures are
	// counted (hypergate_audit_denies_total) and logged but not enforced.
	// +kubebuilder:default=Enforce
	// +optional
	Mode ChainMode `json:"mode,omitempty"`

	// Timeout bounds the chain's work for each ext_proc message (e.g. 300ms).
	// Keep it below Envoy's message timeout.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m))+$`
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// MaxConcurrency limits how many requests may run this chain's filters at
	// the same time on one engine. 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxConcurrency int32 `json:"maxConcurrency,omitempty"`

	// OnTimeout is Deny (default, 503) or Allow (continue without the remaining filters).
	// +optional
	OnTimeout LimitAction `json:"onTimeout,omitempty"`

	// OnOverload is Deny (default, 503 with Retry-After) or Allow (skip the chain).
	// +optional
	OnOverload LimitAction `json:"onOverload,omitempty"`
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
