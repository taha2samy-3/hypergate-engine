package webhook

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hyperv1alpha1 "github.com/taha/myprog/hyper-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

var hyperChainLog = logf.Log.WithName("hyperchain-validation-webhook")

// HyperChainValidator validates HyperChain resources by ensuring all referenced
// filters actually exist in the cluster before allowing CREATE or UPDATE.
type HyperChainValidator struct {
	Client client.Client
}

// Confirm type implements admission.Validator[*hyperv1alpha1.HyperChain]
var _ admission.Validator[*hyperv1alpha1.HyperChain] = &HyperChainValidator{}

// ValidateCreate validates a new HyperChain on creation.
func (v *HyperChainValidator) ValidateCreate(ctx context.Context, obj *hyperv1alpha1.HyperChain) (admission.Warnings, error) {
	hyperChainLog.Info("validating HyperChain on CREATE", "name", obj.Name)
	return v.validateFilters(ctx, obj)
}

// ValidateUpdate validates a HyperChain on update.
func (v *HyperChainValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *hyperv1alpha1.HyperChain) (admission.Warnings, error) {
	hyperChainLog.Info("validating HyperChain on UPDATE", "name", newObj.Name)
	return v.validateFilters(ctx, newObj)
}

func (v *HyperChainValidator) ValidateDelete(ctx context.Context, obj *hyperv1alpha1.HyperChain) (admission.Warnings, error) {
	hyperChainLog.Info("validating HyperChain on DELETE", "name", obj.Name)

	var configList hyperv1alpha1.HyperConfigList
	if err := v.Client.List(ctx, &configList); err != nil {
		return nil, fmt.Errorf("failed to list HyperConfigs: %w", err)
	}

	for _, hc := range configList.Items {
		if hc.Spec.DefaultChain == obj.Name {
			return nil, fmt.Errorf("cannot delete HyperChain '%s' because it is referenced as the defaultChain in HyperConfig '%s'", obj.Name, hc.Name)
		}
	}

	return nil, nil
}

// validateFilters iterates over spec.filters and verifies each referenced
// filter resource exists in the cluster.
func (v *HyperChainValidator) validateFilters(ctx context.Context, chain *hyperv1alpha1.HyperChain) (admission.Warnings, error) {
	for _, ref := range chain.Spec.Filters {
		key := types.NamespacedName{Name: ref.Name}

		var lookupErr error
		switch ref.Kind {
		case "RateLimitFilter":
			obj := &hyperv1alpha1.RateLimitFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		case "HeaderModifierFilter":
			obj := &hyperv1alpha1.HeaderModifierFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		case "DenyFilter":
			obj := &hyperv1alpha1.DenyFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		case "CorrelationIdFilter":
			obj := &hyperv1alpha1.CorrelationIdFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		case "RedisMetadataEnricherFilter":
			obj := &hyperv1alpha1.RedisMetadataEnricherFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		case "ApiKeyFilter":
			obj := &hyperv1alpha1.ApiKeyFilter{}
			lookupErr = v.Client.Get(ctx, key, obj)
		default:
			return nil, fmt.Errorf("unknown filter kind '%s' in HyperChain '%s'", ref.Kind, chain.Name)
		}

		if lookupErr != nil {
			if client.IgnoreNotFound(lookupErr) == nil {
				// Resource simply does not exist
				return nil, fmt.Errorf("referenced %s '%s' does not exist in the cluster", ref.Kind, ref.Name)
			}
			// Unexpected API error — surface it so the request is not silently accepted
			return nil, fmt.Errorf("failed to look up %s '%s': %w", ref.Kind, ref.Name, lookupErr)
		}
	}

	return nil, nil
}

// SetupHyperChainWebhookWithManager registers the HyperChain validating webhook
// with the controller-runtime manager.
func SetupHyperChainWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.HyperChain{}).
		WithValidator(&HyperChainValidator{Client: mgr.GetClient()}).
		Complete()
}
