// Package v1alpha1 contains the hyper.io v1alpha1 API types.
// +kubebuilder:object:generate=true
// +groupName=hyper.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "hyper.io", Version: "v1alpha1"}

	//nolint:staticcheck
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&CorrelationIdFilter{}, &CorrelationIdFilterList{},
		&RedisMetadataEnricherFilter{}, &RedisMetadataEnricherFilterList{},
		&ExternalAuthFilter{}, &ExternalAuthFilterList{},
		&FirewallFilter{}, &FirewallFilterList{},
	)
}
