package webhook

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hyperv1alpha1 "github.com/taha/myprog/hyper-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var filterlog = logf.Log.WithName("filter-protection-webhook")

// FilterProtectionValidator prevents deletion of filters if referenced by any HyperChain
type FilterProtectionValidator[T client.Object] struct {
	Client client.Client
	Kind   string
}

var _ admission.Validator[*hyperv1alpha1.RateLimitFilter] = &FilterProtectionValidator[*hyperv1alpha1.RateLimitFilter]{Kind: "RateLimitFilter"}
var _ admission.Validator[*hyperv1alpha1.HeaderModifierFilter] = &FilterProtectionValidator[*hyperv1alpha1.HeaderModifierFilter]{Kind: "HeaderModifierFilter"}
var _ admission.Validator[*hyperv1alpha1.DenyFilter] = &FilterProtectionValidator[*hyperv1alpha1.DenyFilter]{Kind: "DenyFilter"}
var _ admission.Validator[*hyperv1alpha1.CorrelationIdFilter] = &FilterProtectionValidator[*hyperv1alpha1.CorrelationIdFilter]{Kind: "CorrelationIdFilter"}
var _ admission.Validator[*hyperv1alpha1.RedisMetadataEnricherFilter] = &FilterProtectionValidator[*hyperv1alpha1.RedisMetadataEnricherFilter]{Kind: "RedisMetadataEnricherFilter"}
var _ admission.Validator[*hyperv1alpha1.ApiKeyFilter] = &FilterProtectionValidator[*hyperv1alpha1.ApiKeyFilter]{Kind: "ApiKeyFilter"}
var _ admission.Validator[*hyperv1alpha1.ExternalAuthFilter] = &FilterProtectionValidator[*hyperv1alpha1.ExternalAuthFilter]{Kind: "ExternalAuthFilter"}

// ValidateCreate implements admission.Validator.
func (v *FilterProtectionValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate implements admission.Validator.
func (v *FilterProtectionValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete implements admission.Validator.
func (v *FilterProtectionValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	filterName := obj.GetName()
	filterlog.Info("validating delete for filter", "kind", v.Kind, "name", filterName)

	var chainList hyperv1alpha1.HyperChainList
	if err := v.Client.List(ctx, &chainList); err != nil {
		return nil, fmt.Errorf("failed to list HyperChains: %w", err)
	}

	for _, chain := range chainList.Items {
		for _, filterRef := range chain.Spec.Filters {
			if filterRef.Kind == v.Kind && filterRef.Name == filterName {
				return nil, fmt.Errorf("cannot delete %s '%s' because it is referenced by HyperChain '%s'", v.Kind, filterName, chain.Name)
			}
		}
	}

	return nil, nil
}

// SetupFiltersWebhookWithManager registers the webhook for the four filter types
func SetupFiltersWebhookWithManager(mgr ctrl.Manager) error {
	c := mgr.GetClient()

	// Register validator for RateLimitFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.RateLimitFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.RateLimitFilter]{Client: c, Kind: "RateLimitFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for HeaderModifierFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.HeaderModifierFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.HeaderModifierFilter]{Client: c, Kind: "HeaderModifierFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for DenyFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.DenyFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.DenyFilter]{Client: c, Kind: "DenyFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for CorrelationIdFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.CorrelationIdFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.CorrelationIdFilter]{Client: c, Kind: "CorrelationIdFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for RedisMetadataEnricherFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.RedisMetadataEnricherFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.RedisMetadataEnricherFilter]{Client: c, Kind: "RedisMetadataEnricherFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for ApiKeyFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.ApiKeyFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.ApiKeyFilter]{Client: c, Kind: "ApiKeyFilter"}).
		Complete(); err != nil {
		return err
	}

	// Register validator for ExternalAuthFilter
	if err := ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.ExternalAuthFilter{}).
		WithValidator(&FilterProtectionValidator[*hyperv1alpha1.ExternalAuthFilter]{Client: c, Kind: "ExternalAuthFilter"}).
		Complete(); err != nil {
		return err
	}

	return nil
}
