package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CorsFilterSpec defines the desired state of CorsFilter. Field names mirror the
// engine's "cors" filter options.
type CorsFilterSpec struct {
	// AllowOrigins lists exact origins (scheme://host[:port]) or "*".
	// +optional
	AllowOrigins []string `json:"allowOrigins,omitempty" yaml:"allow_origins,omitempty"`

	// AllowOriginRegex lists RE2 patterns matched against the whole origin.
	// +optional
	AllowOriginRegex []string `json:"allowOriginRegex,omitempty" yaml:"allow_origin_regex,omitempty"`

	// AllowMethods are returned to preflights; "*" echoes the requested method.
	// Default: GET, HEAD, POST.
	// +optional
	AllowMethods []string `json:"allowMethods,omitempty" yaml:"allow_methods,omitempty"`

	// AllowHeaders are returned to preflights; "*" echoes the requested headers.
	// +optional
	AllowHeaders []string `json:"allowHeaders,omitempty" yaml:"allow_headers,omitempty"`

	// ExposeHeaders lists response headers readable by browser JavaScript.
	// +optional
	ExposeHeaders []string `json:"exposeHeaders,omitempty" yaml:"expose_headers,omitempty"`

	// AllowCredentials allows cookies and Authorization headers. Cannot be combined
	// with allowOrigins "*".
	// +optional
	AllowCredentials bool `json:"allowCredentials,omitempty" yaml:"allow_credentials,omitempty"`

	// MaxAge is how long, in seconds, browsers may cache a preflight result.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxAge int32 `json:"maxAge,omitempty" yaml:"max_age,omitempty"`

	// BlockDisallowedOrigins rejects non-preflight requests from other origins with 403.
	// +optional
	BlockDisallowedOrigins bool `json:"blockDisallowedOrigins,omitempty" yaml:"block_disallowed_origins,omitempty"`
}

// CorsFilterStatus defines the observed state of CorsFilter
type CorsFilterStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=corsf

// CorsFilter is the Schema for the corsfilters API
type CorsFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CorsFilterSpec   `json:"spec,omitempty"`
	Status CorsFilterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CorsFilterList contains a list of CorsFilter
type CorsFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CorsFilter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CorsFilter{}, &CorsFilterList{})
}
