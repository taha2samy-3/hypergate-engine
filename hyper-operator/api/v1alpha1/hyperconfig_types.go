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

	// +optional
	DefaultChain string `json:"defaultChain,omitempty"`

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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Server Address",type="string",JSONPath=".spec.serverAddress"
// +kubebuilder:printcolumn:name="Log Level",type="string",JSONPath=".spec.logLevel"
// +kubebuilder:printcolumn:name="Redis Ref",type="string",JSONPath=".spec.redisServiceRef"
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
