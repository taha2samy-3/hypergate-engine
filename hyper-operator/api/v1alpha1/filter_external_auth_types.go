/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=true

// SidecarContainerSpec defines the desired container spec for the external auth sidecar.
type SidecarContainerSpec struct {
	// Image is the container image for the sidecar.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ImagePullPolicy is the policy for pulling the image.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets is a list of secrets to use for pulling the image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Args are the command-line arguments to pass to the container.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is the list of environment variables to set in the container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom is the list of sources to populate environment variables in the container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Resources defines the compute resource requirements for the container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// SocketEnvKey is the name of the environment variable that the sidecar expects
	// to receive the Unix Domain Socket path in.
	// +optional
	SocketEnvKey string `json:"socketEnvKey,omitempty"`
}

// +kubebuilder:object:generate=true

// AuthSuccessRules defines what actions to take when external auth succeeds.
// These fields map 1:1 to the engine's config.AuthSuccessRules.
type AuthSuccessRules struct {
	// UpstreamHeadersToAdd is the list of headers from the auth response to inject upstream.
	// +optional
	UpstreamHeadersToAdd []string `json:"upstreamHeadersToAdd,omitempty" yaml:"upstream_headers_to_add"`

	// UpstreamHeadersToRemove is the list of headers to strip before forwarding upstream.
	// +optional
	UpstreamHeadersToRemove []string `json:"upstreamHeadersToRemove,omitempty" yaml:"upstream_headers_to_remove"`
}

// +kubebuilder:object:generate=true

// AuthFailureRules defines what actions to take when external auth fails.
// These fields map 1:1 to the engine's config.AuthFailureRules.
type AuthFailureRules struct {
	// DownstreamPassThroughHeaders is the list of headers from the auth response
	// to pass through to the downstream (caller) on failure.
	// +optional
	DownstreamPassThroughHeaders []string `json:"downstreamPassThroughHeaders,omitempty" yaml:"downstream_pass_through_headers"`
}

// +kubebuilder:object:generate=true

// EngineRulesSpec defines the mapping rules compiled into the Engine's config.yaml.
type EngineRulesSpec struct {
	// Timeout is the maximum duration for a single auth request (e.g. "2s").
	// +optional
	// +kubebuilder:default="2s"
	Timeout string `json:"timeout,omitempty"`

	// ForwardHeaders is the list of incoming request headers to forward to the auth sidecar.
	// +optional
	ForwardHeaders []string `json:"forwardHeaders,omitempty"`

	// OnSuccess defines header manipulation rules applied when auth succeeds (2xx).
	// +optional
	OnSuccess AuthSuccessRules `json:"onSuccess,omitempty"`

	// OnFailure defines header pass-through rules applied when auth fails (non-2xx).
	// +optional
	OnFailure AuthFailureRules `json:"onFailure,omitempty"`
}

// ExternalAuthFilterSpec defines the desired state of ExternalAuthFilter
type ExternalAuthFilterSpec struct {
	// Protocol is the communication protocol with the sidecar.
	// Only "http" is currently implemented. "grpc" is planned.
	// +kubebuilder:validation:Enum=http;grpc
	// +kubebuilder:default="http"
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// Container holds the sidecar container specification.
	// +kubebuilder:validation:Required
	Container SidecarContainerSpec `json:"container"`

	// EngineRules holds the mapping rules compiled into the Engine's config.yaml.
	// +optional
	EngineRules EngineRulesSpec `json:"engineRules,omitempty"`
}

// ExternalAuthFilterStatus defines the observed state of ExternalAuthFilter
type ExternalAuthFilterStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eaf
// +kubebuilder:printcolumn:name="Protocol",type="string",JSONPath=".spec.protocol"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.container.image"

// ExternalAuthFilter is the Schema for the externalauthfilters API
type ExternalAuthFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExternalAuthFilterSpec   `json:"spec,omitempty"`
	Status ExternalAuthFilterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExternalAuthFilterList contains a list of ExternalAuthFilter
type ExternalAuthFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalAuthFilter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExternalAuthFilter{}, &ExternalAuthFilterList{})
}
