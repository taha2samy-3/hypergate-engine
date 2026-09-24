package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
)

// HyperConfigReconciler reconciles a HyperConfig object
type HyperConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hyper.io,resources=externalauthfilters,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=firewallfilters,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=jwtauthfilters,verbs=get;list;watch

func (r *HyperConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var hyperConfig hyperv1alpha1.HyperConfig
	if err := r.Get(ctx, req.NamespacedName, &hyperConfig); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch HyperConfig")
		return ctrl.Result{}, err
	}

	// Only one HyperConfig may manage a namespace; the others would fight over the
	// same DaemonSet and ConfigMap.
	var configList hyperv1alpha1.HyperConfigList
	if err := r.List(ctx, &configList); err != nil {
		return ctrl.Result{}, err
	}
	_, conflicts := activeHyperConfigs(configList.Items)
	if owner, lost := conflicts[hyperConfig.Name]; lost {
		msg := fmt.Sprintf("targetNamespace %q is already managed by HyperConfig %q", targetNamespaceOf(&hyperConfig), owner)
		logger.Info("HyperConfig conflicts with an older HyperConfig, skipping", "owner", owner)
		return ctrl.Result{}, r.setStatus(ctx, &hyperConfig, hyperConfigStateConflict, msg)
	}

	namespace := targetNamespaceOf(&hyperConfig)
	if err := r.ensureNamespace(ctx, namespace); err != nil {
		return ctrl.Result{}, err
	}

	engineImage := hyperConfig.Spec.EngineImage
	if engineImage == "" {
		engineImage = defaultEngineImage()
	}

	saName := "hyper-engine-sa"
	roleName := "hyper-engine-config-reader"
	dsName := "hyper-engine"
	svcName := "hyper-engine-svc"

	// 1. ServiceAccount
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return ctrl.SetControllerReference(&hyperConfig, sa, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile ServiceAccount")
		return ctrl.Result{}, err
	}

	// 2. Role
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"configmaps"},
				ResourceNames: []string{engineConfigMapName},
				Verbs:         []string{"get", "watch"},
			},
		}
		return ctrl.SetControllerReference(&hyperConfig, role, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Role")
		return ctrl.Result{}, err
	}

	// 3. RoleBinding
	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName + "-binding", Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     roleName,
		}
		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		}
		return ctrl.SetControllerReference(&hyperConfig, roleBinding, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile RoleBinding")
		return ctrl.Result{}, err
	}

	// Filter CRDs are cluster-scoped, so they are listed cluster-wide. (Listing them
	// with a namespace returns nothing and no sidecar would ever be injected.)
	var externalAuthList hyperv1alpha1.ExternalAuthFilterList
	if err := r.List(ctx, &externalAuthList); err != nil {
		logger.Error(err, "failed to list ExternalAuthFilters")
		return ctrl.Result{}, err
	}
	var firewallList hyperv1alpha1.FirewallFilterList
	if err := r.List(ctx, &firewallList); err != nil {
		logger.Error(err, "failed to list FirewallFilters")
		return ctrl.Result{}, err
	}
	var jwtList hyperv1alpha1.JwtAuthFilterList
	if err := r.List(ctx, &jwtList); err != nil {
		logger.Error(err, "failed to list JwtAuthFilters")
		return ctrl.Result{}, err
	}
	// Deterministic ordering keeps the pod template stable and avoids needless rollouts.
	sort.Slice(externalAuthList.Items, func(i, j int) bool { return externalAuthList.Items[i].Name < externalAuthList.Items[j].Name })
	sort.Slice(firewallList.Items, func(i, j int) bool { return firewallList.Items[i].Name < firewallList.Items[j].Name })
	sort.Slice(jwtList.Items, func(i, j int) bool { return jwtList.Items[i].Name < jwtList.Items[j].Name })

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dsName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		labels := map[string]string{"app": "hyper-engine"}
		if ds.Spec.Selector == nil {
			ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
		ds.Spec.Template.Labels = labels
		ds.Spec.Template.Spec.ServiceAccountName = saName
		ds.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}

		podSpec := buildEnginePodSpec(&hyperConfig, engineImage, namespace, externalAuthList.Items, firewallList.Items, jwtList.Items)
		ds.Spec.Template.Spec.Containers = podSpec.Containers
		ds.Spec.Template.Spec.Volumes = podSpec.Volumes
		ds.Spec.Template.Spec.ImagePullSecrets = podSpec.ImagePullSecrets

		return ctrl.SetControllerReference(&hyperConfig, ds, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile DaemonSet")
		return ctrl.Result{}, err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = map[string]string{"app": "hyper-engine"}
		appProto := "kubernetes.io/h2c"
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:        "grpc",
				Port:        engineGRPCPort,
				TargetPort:  intstr.FromInt(engineGRPCPort),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: &appProto,
			},
		}
		td := "PreferSameNode"
		svc.Spec.TrafficDistribution = &td
		return ctrl.SetControllerReference(&hyperConfig, svc, r.Scheme)
	}); err != nil {
		logger.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setStatus(ctx, &hyperConfig, hyperConfigStateReady, "Engine resources reconciled in namespace "+namespace)
}

func (r *HyperConfigReconciler) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to fetch target namespace %q: %w", name, err)
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create target namespace %q: %w", name, err)
	}
	log.FromContext(ctx).Info("Created target namespace", "namespace", name)
	return nil
}

func (r *HyperConfigReconciler) setStatus(ctx context.Context, hc *hyperv1alpha1.HyperConfig, state, message string) error {
	if hc.Status.State == state && hc.Status.Message == message {
		return nil
	}
	hc.Status.State = state
	hc.Status.Message = message
	return r.Status().Update(ctx, hc)
}

// buildEnginePodSpec renders the engine container, the injected sidecars and the
// volumes they share.
func buildEnginePodSpec(
	hc *hyperv1alpha1.HyperConfig,
	engineImage, namespace string,
	externalAuths []hyperv1alpha1.ExternalAuthFilter,
	firewalls []hyperv1alpha1.FirewallFilter,
	jwts []hyperv1alpha1.JwtAuthFilter,
) corev1.PodSpec {
	engine := corev1.Container{
		Name:  "engine",
		Image: engineImage,
		Ports: []corev1.ContainerPort{
			{ContainerPort: engineGRPCPort, Name: "grpc"},
			{ContainerPort: engineHealthPort, Name: "health"},
		},
		Env: []corev1.EnvVar{
			{Name: "CONFIG_PROVIDER", Value: "K8S"},
			{Name: "CONFIG_K8S_NAME", Value: engineConfigMapName},
			{Name: "CONFIG_K8S_NAMESPACE", Value: namespace},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(engineHealthPort)}},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(engineHealthPort)}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
		Resources: engineResources(hc),
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](10001),
			RunAsGroup:               ptr.To[int64](10001),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	var volumes []corev1.Volume
	var sidecars []corev1.Container
	pullSecretSet := make(map[string]struct{})

	if len(externalAuths) > 0 || len(firewalls) > 0 {
		// UDS emptyDir volume shared between engine and all sidecars.
		volumes = append(volumes, corev1.Volume{
			Name:         udsVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		engine.VolumeMounts = append(engine.VolumeMounts, corev1.VolumeMount{Name: udsVolumeName, MountPath: udsVolumeMountPath})
	}

	for i := range externalAuths {
		eaf := &externalAuths[i]
		socketPath := udsVolumeMountPath + "ext-auth-" + eaf.Name + ".sock"
		for _, s := range eaf.Spec.Container.ImagePullSecrets {
			pullSecretSet[s.Name] = struct{}{}
		}

		sidecarEnv := append([]corev1.EnvVar(nil), eaf.Spec.Container.Env...)
		if eaf.Spec.Container.SocketEnvKey != "" {
			sidecarEnv = append(sidecarEnv, corev1.EnvVar{Name: eaf.Spec.Container.SocketEnvKey, Value: "unix://" + socketPath})
		} else if isOAuth2Proxy(eaf.Spec.Container.Image) {
			sidecarEnv = append(sidecarEnv, corev1.EnvVar{Name: "OAUTH2_PROXY_HTTP_ADDRESS", Value: "unix://" + socketPath})
		}

		sidecars = append(sidecars, corev1.Container{
			Name:            "ext-auth-" + eaf.Name,
			Image:           eaf.Spec.Container.Image,
			ImagePullPolicy: eaf.Spec.Container.ImagePullPolicy,
			Args:            substituteSocketPath(eaf.Spec.Container.Args, socketPath),
			Env:             sidecarEnv,
			EnvFrom:         eaf.Spec.Container.EnvFrom,
			Resources:       eaf.Spec.Container.Resources,
			VolumeMounts:    []corev1.VolumeMount{{Name: udsVolumeName, MountPath: udsVolumeMountPath}},
		})
	}

	for i := range firewalls {
		fw := &firewalls[i]
		socketPath := udsVolumeMountPath + "fw-" + fw.Name + ".sock"
		for _, s := range fw.Spec.Container.ImagePullSecrets {
			pullSecretSet[s.Name] = struct{}{}
		}

		sidecarEnv := append([]corev1.EnvVar(nil), fw.Spec.Container.Env...)
		socketEnvKey := fw.Spec.Container.SocketEnvKey
		if socketEnvKey == "" {
			socketEnvKey = "FIREWALL_SOCKET_PATH"
		}
		sidecarEnv = append(sidecarEnv, corev1.EnvVar{Name: socketEnvKey, Value: "unix://" + socketPath})

		volumeMounts := []corev1.VolumeMount{{Name: udsVolumeName, MountPath: udsVolumeMountPath}}
		if fw.Spec.RulesConfigMap != "" {
			volName := "fw-rules-cm-" + fw.Name
			volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: volName, MountPath: "/etc/firewall/rules/"})
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: fw.Spec.RulesConfigMap},
				}},
			})
		}
		if fw.Spec.RulesSecretRef != "" {
			volName := "fw-rules-sec-" + fw.Name
			volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: volName, MountPath: "/etc/firewall/secrets/"})
			volumes = append(volumes, corev1.Volume{
				Name:         volName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fw.Spec.RulesSecretRef}},
			})
		}

		sidecars = append(sidecars, corev1.Container{
			Name:            "fw-" + fw.Name,
			Image:           fw.Spec.Container.Image,
			ImagePullPolicy: fw.Spec.Container.ImagePullPolicy,
			Args:            substituteSocketPath(fw.Spec.Container.Args, socketPath),
			Env:             sidecarEnv,
			EnvFrom:         fw.Spec.Container.EnvFrom,
			Resources:       fw.Spec.Container.Resources,
			VolumeMounts:    volumeMounts,
		})
	}

	// JWT secrets are mounted read-only into the engine; the compiled config only
	// carries file paths, never the secret values.
	for i := range jwts {
		items := jwtSecretItems(&jwts[i])
		for secretName, keys := range items {
			volName := "jwt-" + jwts[i].Name + "-" + secretName
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  secretName,
					Items:       keys,
					DefaultMode: ptr.To[int32](0o440),
				}},
			})
			engine.VolumeMounts = append(engine.VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: jwtSecretDir(jwts[i].Name) + "/" + secretName,
				ReadOnly:  true,
			})
		}
	}

	pullSecrets := make([]string, 0, len(pullSecretSet))
	for name := range pullSecretSet {
		pullSecrets = append(pullSecrets, name)
	}
	sort.Strings(pullSecrets)
	var imagePullSecrets []corev1.LocalObjectReference
	for _, name := range pullSecrets {
		imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: name})
	}

	return corev1.PodSpec{
		Containers:       append([]corev1.Container{engine}, sidecars...),
		Volumes:          volumes,
		ImagePullSecrets: imagePullSecrets,
	}
}

// jwtSecretItems groups the secret keys a JwtAuthFilter needs by Secret name.
func jwtSecretItems(f *hyperv1alpha1.JwtAuthFilter) map[string][]corev1.KeyToPath {
	items := make(map[string][]corev1.KeyToPath)
	if ref := f.Spec.LocalSecretRef; ref != nil {
		items[ref.Name] = append(items[ref.Name], corev1.KeyToPath{Key: ref.Key, Path: jwtLocalSecretFile})
	}
	if ref := f.Spec.IntrospectionAuthSecretRef; ref != nil {
		items[ref.Name] = append(items[ref.Name], corev1.KeyToPath{Key: ref.Key, Path: jwtIntrospectionAuthFile})
	}
	return items
}

// jwtSecretFile is the path at which a referenced secret key is mounted in the engine.
func jwtSecretFile(filterName string, ref *hyperv1alpha1.SecretKeyRef, file string) string {
	return jwtSecretDir(filterName) + "/" + ref.Name + "/" + file
}

func engineResources(hc *hyperv1alpha1.HyperConfig) corev1.ResourceRequirements {
	if hc.Spec.EngineResources != nil {
		return *hc.Spec.EngineResources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

func substituteSocketPath(args []string, socketPath string) []string {
	var out []string
	for _, arg := range args {
		out = append(out, strings.ReplaceAll(arg, "{socket_path}", socketPath))
	}
	return out
}

// isOAuth2Proxy returns true when the container image string indicates an oauth2-proxy sidecar.
// Detection is image-substring based to cover both quay.io/oauth2-proxy/oauth2-proxy and
// custom registry mirrors.
func isOAuth2Proxy(image string) bool {
	return strings.Contains(image, "oauth2-proxy")
}

// SetupWithManager sets up the controller with the Manager.
func (r *HyperConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Any change to a filter that affects the pod template, or to another HyperConfig
	// (namespace ownership), re-reconciles every HyperConfig.
	allConfigs := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, o client.Object) []reconcile.Request {
			var configList hyperv1alpha1.HyperConfigList
			if err := mgr.GetClient().List(ctx, &configList); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, len(configList.Items))
			for i, hc := range configList.Items {
				reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: hc.Name}}
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1alpha1.HyperConfig{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&hyperv1alpha1.HyperConfig{}, allConfigs).
		Watches(&hyperv1alpha1.ExternalAuthFilter{}, allConfigs).
		Watches(&hyperv1alpha1.FirewallFilter{}, allConfigs).
		Watches(&hyperv1alpha1.JwtAuthFilter{}, allConfigs).
		Complete(r)
}
