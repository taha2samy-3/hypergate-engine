package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
)

// IdentitySettings describe the operator's identity server, which engines
// connect to for the workload identity map.
type IdentitySettings struct {
	// Enabled is true when the operator runs the identity server.
	Enabled bool
	// Address engines dial, host:port of the identity Service.
	Address string
	// ServerName is the name in the server certificate.
	ServerName string
	// CAFile is the operator-side path of the CA certificate copied to engines.
	CAFile string
	// Insecure: the server runs without TLS (development only).
	Insecure bool
}

const (
	identityCAConfigMap    = "hyper-identity-ca"
	identityCAVolume       = "identity-ca"
	identityCAMountDir     = "/etc/hypergate/identity"
	identityTokenVolume    = "identity-token"
	identityTokenMountDir  = "/var/run/secrets/hypergate/identity"
	identityTokenAudience  = "hypergate-identity"
	identityTokenExpirySec = 3600
	// identityCARefresh re-copies the CA certificate so a rotated CA reaches engines.
	identityCARefresh = 10 * time.Minute
)

// engineConfig returns the identity block of the compiled engine configuration.
func (s IdentitySettings) engineConfig() config.IdentityConfig {
	if !s.Enabled {
		return config.IdentityConfig{}
	}
	out := config.IdentityConfig{
		Enabled:    true,
		Address:    s.Address,
		ServerName: s.ServerName,
		TokenFile:  identityTokenMountDir + "/token",
		Insecure:   s.Insecure,
	}
	if !s.Insecure {
		out.CAFile = identityCAMountDir + "/ca.crt"
	}
	return out
}

// identityVolumes mounts the projected identity token and the CA certificate.
func identityVolumes(s IdentitySettings) ([]corev1.Volume, []corev1.VolumeMount) {
	if !s.Enabled {
		return nil, nil
	}
	volumes := []corev1.Volume{{
		Name: identityTokenVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			// Readable by the engine's user even when sidecars with other users share
			// the pod (kubelet then cannot hand the file to a single owner).
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience:          identityTokenAudience,
				ExpirationSeconds: ptr.To[int64](identityTokenExpirySec),
				Path:              "token",
			}}},
		}},
	}}
	mounts := []corev1.VolumeMount{{Name: identityTokenVolume, MountPath: identityTokenMountDir, ReadOnly: true}}
	if !s.Insecure {
		volumes = append(volumes, corev1.Volume{
			Name: identityCAVolume,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: identityCAConfigMap},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: identityCAVolume, MountPath: identityCAMountDir, ReadOnly: true})
	}
	return volumes, mounts
}

// reconcileIdentityCA copies the identity server's CA certificate into the
// engine namespace, where the engine mounts it.
func (r *HyperConfigReconciler) reconcileIdentityCA(ctx context.Context, hc *hyperv1alpha1.HyperConfig, namespace string) error {
	if !r.Identity.Enabled || r.Identity.Insecure {
		return nil
	}
	ca, err := os.ReadFile(r.Identity.CAFile)
	if err != nil {
		return fmt.Errorf("read identity CA %s: %w", r.Identity.CAFile, err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: identityCAConfigMap, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"ca.crt": string(ca)}
		return ctrl.SetControllerReference(hc, cm, r.Scheme)
	})
	return err
}
