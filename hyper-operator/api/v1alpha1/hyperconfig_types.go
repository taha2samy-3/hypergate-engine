package v1alpha1

import (
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
	// +kubebuilder:default="taha/myprog-engine:latest"
	// +optional
	EngineImage string `json:"engineImage,omitempty"`

	// +kubebuilder:validation:Required
	RedisServiceRef string `json:"redisServiceRef"`

	// +optional
	DefaultChain string `json:"defaultChain,omitempty"`
}

// +kubebuilder:object:generate=true
// HyperConfigStatus defines the observed state of HyperConfig
type HyperConfigStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Server Address",type="string",JSONPath=".spec.serverAddress"
// +kubebuilder:printcolumn:name="Log Level",type="string",JSONPath=".spec.logLevel"
// +kubebuilder:printcolumn:name="Redis Ref",type="string",JSONPath=".spec.redisServiceRef"

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
