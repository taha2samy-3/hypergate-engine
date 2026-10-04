package webhook

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/hyper-operator/internal/routes"
)

var hyperRouteLog = logf.Log.WithName("hyperroute-validation-webhook")

// HyperRouteValidator rejects HyperRoutes the compiler would have to skip: an
// empty targetPolicy, an invalid regex, traffic value or selector. Skipped routes
// let their traffic fall through to weaker policy, so they are stopped at admission.
type HyperRouteValidator struct {
	Client client.Client
	// Routes says what engines can evaluate (workload selectors need identity).
	Routes routes.Options
}

var _ admission.Validator[*hyperv1alpha1.HyperRoute] = &HyperRouteValidator{}

func (v *HyperRouteValidator) ValidateCreate(ctx context.Context, obj *hyperv1alpha1.HyperRoute) (admission.Warnings, error) {
	hyperRouteLog.Info("validating HyperRoute on CREATE", "name", obj.Name)
	return v.validate(ctx, obj)
}

func (v *HyperRouteValidator) ValidateUpdate(ctx context.Context, _, newObj *hyperv1alpha1.HyperRoute) (admission.Warnings, error) {
	hyperRouteLog.Info("validating HyperRoute on UPDATE", "name", newObj.Name)
	return v.validate(ctx, newObj)
}

func (v *HyperRouteValidator) ValidateDelete(context.Context, *hyperv1alpha1.HyperRoute) (admission.Warnings, error) {
	return nil, nil
}

func (v *HyperRouteValidator) validate(ctx context.Context, hr *hyperv1alpha1.HyperRoute) (admission.Warnings, error) {
	if err := routes.Validate(&hr.Spec, v.Routes); err != nil {
		return nil, fmt.Errorf("HyperRoute '%s' is invalid: %w", hr.Name, err)
	}

	// A missing target chain is allowed (it may be created next), but matching
	// requests are rejected until it exists.
	var chain hyperv1alpha1.HyperChain
	err := v.Client.Get(ctx, client.ObjectKey{Name: hr.Spec.TargetPolicy}, &chain)
	switch {
	case client.IgnoreNotFound(err) != nil:
		return nil, fmt.Errorf("failed to look up HyperChain '%s': %w", hr.Spec.TargetPolicy, err)
	case err != nil:
		return admission.Warnings{fmt.Sprintf(
			"HyperChain '%s' does not exist; requests matching this route are rejected with 503 until it is created",
			hr.Spec.TargetPolicy)}, nil
	}
	return nil, nil
}

// SetupHyperRouteWebhookWithManager registers the HyperRoute validating webhook.
func SetupHyperRouteWebhookWithManager(mgr ctrl.Manager, opts routes.Options) error {
	return ctrl.NewWebhookManagedBy(mgr, &hyperv1alpha1.HyperRoute{}).
		WithValidator(&HyperRouteValidator{Client: mgr.GetClient(), Routes: opts}).
		Complete()
}
