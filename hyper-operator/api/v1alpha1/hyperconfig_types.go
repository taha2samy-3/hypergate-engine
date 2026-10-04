package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=DEBUG;INFO;WARN;ERROR
type LogLevel string

const (
	LogLevelDebug LogLevel = "DEBUG"
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

// +kubebuilder:object:generate=true
// HyperConfigSpec defines the desired state of HyperConfig
type HyperConfigSpec struct {
	// +kubebuilder:default="0.0.0.0:9001"
	// +optional
	ServerAddress string `json:"serverAddress,omitempty"`

	// +optional
	MaxConcurrentStreams uint32 `json:"maxConcurrentStreams,omitempty"`

	// +kubebuilder:default="INFO"
	// +optional
	LogLevel LogLevel `json:"logLevel,omitempty"`

	// TargetNamespace is the namespace where engine resources will be deployed.
	// +kubebuilder:default="hyper-system"
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// EngineImage is the full registry path of the engine container image.
	// Defaults to the engine image matching the operator release.
	// +optional
	EngineImage string `json:"engineImage,omitempty"`

	// EngineResources sets the engine container's resource requests and limits.
	// +optional
	EngineResources *corev1.ResourceRequirements `json:"engineResources,omitempty"`

	// TrustedProxyHops is the number of proxies/load balancers in front of Envoy whose
	// X-Forwarded-For entries are trusted when resolving the client IP. Default 0
	// (the client is Envoy's direct peer).
	// +kubebuilder:validation:Minimum=0
	// +optional
	TrustedProxyHops int32 `json:"trustedProxyHops,omitempty"`

	// RedisServiceRef is deprecated and ignored: every Redis-backed filter names its
	// HyperRedis in its own spec.
	// +optional
	RedisServiceRef string `json:"redisServiceRef,omitempty"`

	// DefaultChain runs for requests that match no HyperRoute and have no
	// defaultChains entry for their traffic class.
	// +optional
	DefaultChain string `json:"defaultChain,omitempty"`

	// DefaultChains picks the chain for unmatched requests by traffic class.
	// +optional
	DefaultChains *DefaultChains `json:"defaultChains,omitempty"`

	// ExtProc attaches this engine to Envoy through typed filter objects the
	// operator generates. Nothing is attached unless it is listed here. The
	// operator never creates or edits Gateway API HTTPRoutes.
	// +optional
	ExtProc *ExtProcSpec `json:"extProc,omitempty"`

	// PoolPrewarmSize specifies the number of RequestContext objects to pre-allocate at boot.
	// +optional
	// +kubebuilder:default=5000
	PoolPrewarmSize int32 `json:"poolPrewarmSize,omitempty" yaml:"pool_prewarm_size,omitempty"`

	// InitialHeaderCapacity defines the initial map/slice capacity for headers per request.
	// +optional
	// +kubebuilder:default=64
	InitialHeaderCapacity int32 `json:"initialHeaderCapacity,omitempty" yaml:"initial_header_capacity,omitempty"`

	// PreallocBodyBufferBytes defines the pre-allocated byte buffer size for HTTP request/response bodies per request.
	// +optional
	// +kubebuilder:default=65536
	PreallocBodyBufferBytes int32 `json:"preallocBodyBufferBytes,omitempty" yaml:"prealloc_body_buffer_bytes,omitempty"`
}

// ExtProcFailureMode says what Envoy does when the engine cannot be reached.
// +kubebuilder:validation:Enum=FailClosed;FailOpen
type ExtProcFailureMode string

const (
	ExtProcFailClosed ExtProcFailureMode = "FailClosed"
	ExtProcFailOpen   ExtProcFailureMode = "FailOpen"
)

// ExtProcSpec selects where the operator attaches the engine.
// +kubebuilder:object:generate=true
type ExtProcSpec struct {
	// Gateways lists the Envoy Gateway Gateways that get an EnvoyExtensionPolicy
	// pointing at this engine. Requires Envoy Gateway's CRDs.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Gateways []GatewayRef `json:"gateways,omitempty"`

	// Namespaces lists the namespaces that get a CiliumEnvoyExtProcFilter named
	// "hypergate", for HTTPRoutes (GAMMA or Gateway) to reference through an
	// ExtensionRef. Inactive until Cilium ships the CRD (cilium/cilium#46479).
	// +kubebuilder:validation:MaxItems=256
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// FailureMode is FailClosed (default: requests fail when the engine is
	// unreachable) or FailOpen (requests pass without policy).
	// +kubebuilder:default=FailClosed
	// +optional
	FailureMode ExtProcFailureMode `json:"failureMode,omitempty"`

	// MessageTimeout is Envoy's deadline for each ext_proc message, as a Gateway
	// API duration. Default 2500ms.
	// +kubebuilder:validation:Pattern=`^([0-9]{1,5}(h|m|s|ms)){1,4}$`
	// +optional
	MessageTimeout string `json:"messageTimeout,omitempty"`
}

// GatewayRef names a Gateway.
// +kubebuilder:object:generate=true
type GatewayRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// DefaultChains holds the fallback HyperChain per traffic class.
// +kubebuilder:object:generate=true
type DefaultChains struct {
	// NorthSouth runs for unmatched requests that entered through a gateway.
	// +optional
	NorthSouth string `json:"northSouth,omitempty"`
	// EastWest runs for unmatched calls between workloads.
	// +optional
	EastWest string `json:"eastWest,omitempty"`
}

// +kubebuilder:object:generate=true
// HyperConfigStatus defines the observed state of HyperConfig
type HyperConfigStatus struct {
	// State is Ready, or Conflict when another HyperConfig already manages the same
	// targetNamespace (the oldest one wins).
	// +optional
	State string `json:"state,omitempty"`
	// Message explains State.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions report the ext_proc attachment: EnvoyGatewayPolicies,
	// CiliumFilters and HTTPRoutes.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Server Address",type="string",JSONPath=".spec.serverAddress"
// +kubebuilder:printcolumn:name="Log Level",type="string",JSONPath=".spec.logLevel"
// +kubebuilder:printcolumn:name="Target Namespace",type="string",JSONPath=".spec.targetNamespace"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"

// HyperConfig is the Schema for the hyperconfigs API
type HyperConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HyperConfigSpec   `json:"spec,omitempty"`
	Status HyperConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HyperConfigList contains a list of HyperConfig
type HyperConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HyperConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HyperConfig{}, &HyperConfigList{})
}
