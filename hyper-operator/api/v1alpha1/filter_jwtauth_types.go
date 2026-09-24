package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef points at one key of a Secret in the engine's target namespace.
// The operator mounts the Secret into the engine pod; the value never appears in
// the generated ConfigMap.
type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key inside the Secret.
	Key string `json:"key"`
}

// +kubebuilder:object:generate=true
// JwtAuthFilterSpec defines the desired state of JwtAuthFilter
type JwtAuthFilterSpec struct {
	// Source is where the token is read from: header (default), query or cookie.
	// +kubebuilder:validation:Enum=header;query;cookie
	// +optional
	Source string `json:"source,omitempty" yaml:"source,omitempty"`

	// HeaderName is the header holding the token. Default: authorization ("Bearer " is stripped).
	// +optional
	HeaderName string `json:"headerName,omitempty" yaml:"header_name,omitempty"`

	// QueryParam is the query parameter holding the token when source is query.
	// +optional
	QueryParam string `json:"queryParam,omitempty" yaml:"query_param,omitempty"`

	// CookieName is the cookie holding the token when source is cookie.
	// +optional
	CookieName string `json:"cookieName,omitempty" yaml:"cookie_name,omitempty"`

	// JWKSEndpoint is the URL of the JSON Web Key Set used to verify signatures.
	// +optional
	JWKSEndpoint string `json:"jwksEndpoint,omitempty" yaml:"jwks_endpoint,omitempty"`

	// JWKSRefreshInterval is how often the key set is refreshed, e.g. "5m".
	// +optional
	JWKSRefreshInterval string `json:"jwksRefreshInterval,omitempty" yaml:"jwks_refresh_interval,omitempty"`

	// LocalSecretRef references the shared HMAC secret (HS256/HS384/HS512) when no JWKS is used.
	// +optional
	LocalSecretRef *SecretKeyRef `json:"localSecretRef,omitempty" yaml:"-"`

	// Algorithm is the expected signing algorithm for localSecretRef, e.g. HS256.
	// +optional
	Algorithm string `json:"algorithm,omitempty" yaml:"algorithm,omitempty"`

	// Issuer is the required "iss" claim. Not validated when empty.
	// +optional
	Issuer string `json:"issuer,omitempty" yaml:"issuer,omitempty"`

	// Audience is the required "aud" claim. Not validated when empty.
	// +optional
	Audience string `json:"audience,omitempty" yaml:"audience,omitempty"`

	// ClaimMappings maps claim names to upstream header names, e.g. {sub: x-user-id}.
	// +optional
	ClaimMappings map[string]string `json:"claimMappings,omitempty" yaml:"claim_mappings,omitempty"`

	// IntrospectionEndpoint is an RFC 7662 endpoint used when local validation fails.
	// +optional
	IntrospectionEndpoint string `json:"introspectionEndpoint,omitempty" yaml:"introspection_endpoint,omitempty"`

	// IntrospectionAuthSecretRef references the Authorization header value sent to the
	// introspection endpoint.
	// +optional
	IntrospectionAuthSecretRef *SecretKeyRef `json:"introspectionAuthSecretRef,omitempty" yaml:"-"`

	// IntrospectionTimeout bounds the introspection call, e.g. "2s".
	// +optional
	IntrospectionTimeout string `json:"introspectionTimeout,omitempty" yaml:"introspection_timeout,omitempty"`

	// FailOpen lets requests through when token validation cannot be completed. Default false.
	// +optional
	FailOpen bool `json:"failOpen,omitempty" yaml:"fail_open,omitempty"`

	// StripToken removes the token header before the request is forwarded upstream.
	// +optional
	StripToken bool `json:"stripToken,omitempty" yaml:"strip_token,omitempty"`
}

// +kubebuilder:object:generate=true
// JwtAuthFilterStatus defines the observed state of JwtAuthFilter
type JwtAuthFilterStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// JwtAuthFilter is the Schema for the jwtauthfilters API
type JwtAuthFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JwtAuthFilterSpec   `json:"spec,omitempty"`
	Status JwtAuthFilterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// JwtAuthFilterList contains a list of JwtAuthFilter
type JwtAuthFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JwtAuthFilter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JwtAuthFilter{}, &JwtAuthFilterList{})
}
