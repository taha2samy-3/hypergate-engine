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
	_ "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=true

// FirewallEngineRulesSpec defines the firewall rule execution parameters compiled into config.yaml.
type FirewallEngineRulesSpec struct {
	// Timeout is the maximum duration for a single firewall inspection request (e.g. "2s").
	// +optional
	// +kubebuilder:default="2s"
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// ForwardHeaders is the list of incoming request headers to forward to the firewall sidecar.
	// +optional
	ForwardHeaders []string `json:"forwardHeaders,omitempty" yaml:"forward_headers,omitempty"`

	// InspectBody toggles full HTTP request body inspection.
	// +optional
	// +kubebuilder:default=false
	InspectBody bool `json:"inspectBody,omitempty" yaml:"inspect_body,omitempty"`

	// MaxBodySizeKB specifies the maximum request body payload size in kilobytes passed to the firewall (default: 1024 KB).
	// +optional
	// +kubebuilder:default=1024
	MaxBodySizeKB int32 `json:"maxBodySizeKB,omitempty" yaml:"max_body_size_kb,omitempty"`

	// OnSuccess defines header manipulation rules applied when firewall inspection passes (2xx).
	// +optional
	OnSuccess AuthSuccessRules `json:"onSuccess,omitempty" yaml:"on_success,omitempty"`

	// OnFailure defines downstream pass-through headers applied when firewall inspection blocks a request (non-2xx).
	// +optional
	OnFailure AuthFailureRules `json:"onFailure,omitempty" yaml:"on_failure,omitempty"`
}

// +kubebuilder:object:generate=true

// FirewallFilterSpec defines the desired state of FirewallFilter.
type FirewallFilterSpec struct {
	// Protocol is the communication protocol with the sidecar ("http" or "grpc").
	// +kubebuilder:validation:Enum=http;grpc
	// +kubebuilder:default="grpc"
	// +optional
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`

	// Container holds the sidecar container specification.
	// +kubebuilder:validation:Required
	Container SidecarContainerSpec `json:"container" yaml:"container"`

	// EngineRules holds the request inspection and rules execution settings.
	// +optional
	EngineRules FirewallEngineRulesSpec `json:"engineRules,omitempty" yaml:"engine_rules,omitempty"`

	// RulesConfigMap is an optional ConfigMap name containing custom WAF rules to mount into the sidecar.
	// +optional
	RulesConfigMap string `json:"rulesConfigMap,omitempty" yaml:"rules_config_map,omitempty"`

	// RulesSecretRef is an optional Secret name containing sensitive WAF rules or credentials to mount into the sidecar.
	// +optional
	RulesSecretRef string `json:"rulesSecretRef,omitempty" yaml:"rules_secret_ref,omitempty"`
}

// FirewallFilterStatus defines the observed state of FirewallFilter.
type FirewallFilterStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fwf;firewall
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.container.image"
// +kubebuilder:printcolumn:name="InspectBody",type="boolean",JSONPath=".spec.engineRules.inspectBody"
// +kubebuilder:printcolumn:name="MaxBodySizeKB",type="integer",JSONPath=".spec.engineRules.maxBodySizeKB"

// FirewallFilter is the Schema for the firewallfilters API.
type FirewallFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FirewallFilterSpec   `json:"spec,omitempty"`
	Status FirewallFilterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FirewallFilterList contains a list of FirewallFilter.
type FirewallFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FirewallFilter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FirewallFilter{}, &FirewallFilterList{})
}
