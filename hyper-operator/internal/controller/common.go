package controller

import (
	"os"
	"sort"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
)

const (
	defaultTargetNamespace = "hyper-system"
	engineConfigMapName    = "hyper-engine-config"

	// engineGRPCPort and engineHealthPort must match the compiled server settings.
	engineGRPCPort   = 9001
	engineHealthPort = 9003

	udsVolumeName      = "uds-sockets"
	udsVolumeMountPath = "/var/run/hypergate/"

	jwtSecretsRoot            = "/etc/hypergate/secrets"
	jwtLocalSecretFile        = "local-secret"
	jwtIntrospectionAuthFile  = "introspection-auth"
	hyperConfigStateReady     = "Ready"
	hyperConfigStateConflict  = "Conflict"
	fallbackEngineImageBase   = "ghcr.io/taha2samy-3/hyper-engine"
	engineImageEnv            = "ENGINE_IMAGE"
	degradedChainStatusCode   = 503
	degradedChainResponseBody = "Service Unavailable"
)

// DefaultEngineImageTag is the engine version released together with this operator.
// Override at build time with -ldflags "-X .../controller.DefaultEngineImageTag=1.2.3".
var DefaultEngineImageTag = "1.0.0"

// defaultEngineImage returns the engine image used when HyperConfig.spec.engineImage
// is empty: ENGINE_IMAGE (set by the Helm chart to the matching release), otherwise
// the version this operator was built with. Never ":latest".
func defaultEngineImage() string {
	if img := os.Getenv(engineImageEnv); img != "" {
		return img
	}
	return fallbackEngineImageBase + ":" + DefaultEngineImageTag
}

func targetNamespaceOf(hc *hyperv1alpha1.HyperConfig) string {
	if hc.Spec.TargetNamespace == "" {
		return defaultTargetNamespace
	}
	return hc.Spec.TargetNamespace
}

// activeHyperConfigs picks one HyperConfig per target namespace: the oldest wins
// (then lowest name). It returns the winners keyed by namespace and, for every
// losing HyperConfig, the name of the one that owns its namespace.
func activeHyperConfigs(items []hyperv1alpha1.HyperConfig) (map[string]*hyperv1alpha1.HyperConfig, map[string]string) {
	sorted := make([]*hyperv1alpha1.HyperConfig, len(items))
	for i := range items {
		sorted[i] = &items[i]
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return sorted[i].Name < sorted[j].Name
	})

	winners := make(map[string]*hyperv1alpha1.HyperConfig)
	conflicts := make(map[string]string)
	for _, hc := range sorted {
		ns := targetNamespaceOf(hc)
		if owner, taken := winners[ns]; taken {
			conflicts[hc.Name] = owner.Name
			continue
		}
		winners[ns] = hc
	}
	return winners, conflicts
}

func jwtSecretDir(filterName string) string {
	return jwtSecretsRoot + "/jwt-" + filterName
}
