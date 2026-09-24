package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"strings"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// HyperChainMasterCompilerReconciler reconciles all state to build config.yaml
type HyperChainMasterCompilerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperredis,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperchains,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperchains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=ratelimitfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=headermodifierfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=denyfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=correlationidfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=redismetadataenricherfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=apikeyfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=externalauthfilters,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=firewallfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=jwtauthfilters,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=apikeyfilters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile aggregated configurations and generate the final config.yaml
func (r *HyperChainMasterCompilerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqLogger := log.FromContext(ctx)

	// Fetch all instances of HyperConfig
	var configList hyperv1alpha1.HyperConfigList
	if err := r.List(ctx, &configList); err != nil {
		reqLogger.Error(err, "unable to list HyperConfigs")
		return ctrl.Result{}, err
	}

	// Abort check: need at least one config
	if len(configList.Items) == 0 {
		reqLogger.Info("No HyperConfig exists, skipping compilation")
		return ctrl.Result{}, nil
	}

	// One compiled ConfigMap per target namespace, from the HyperConfig that owns it.
	activeConfigs, _ := activeHyperConfigs(configList.Items)

	// Fetch HyperRedis list
	var redisList hyperv1alpha1.HyperRedisList
	if err := r.List(ctx, &redisList); err != nil {
		reqLogger.Error(err, "unable to list HyperRedis")
		return ctrl.Result{}, err
	}

	// Fetch HyperRoute list
	var routeList hyperv1alpha1.HyperRouteList
	if err := r.List(ctx, &routeList); err != nil {
		reqLogger.Error(err, "unable to list HyperRoutes")
		return ctrl.Result{}, err
	}

	// Fetch HyperChain list
	var chainList hyperv1alpha1.HyperChainList
	if err := r.List(ctx, &chainList); err != nil {
		reqLogger.Error(err, "unable to list HyperChains")
		return ctrl.Result{}, err
	}

	// Fetch Filters
	var rateLimitList hyperv1alpha1.RateLimitFilterList
	if err := r.List(ctx, &rateLimitList); err != nil {
		reqLogger.Error(err, "unable to list RateLimitFilters")
		return ctrl.Result{}, err
	}

	var headerModifierList hyperv1alpha1.HeaderModifierFilterList
	if err := r.List(ctx, &headerModifierList); err != nil {
		reqLogger.Error(err, "unable to list HeaderModifierFilters")
		return ctrl.Result{}, err
	}

	var denyList hyperv1alpha1.DenyFilterList
	if err := r.List(ctx, &denyList); err != nil {
		reqLogger.Error(err, "unable to list DenyFilters")
		return ctrl.Result{}, err
	}

	var correlationIdList hyperv1alpha1.CorrelationIdFilterList
	if err := r.List(ctx, &correlationIdList); err != nil {
		reqLogger.Error(err, "unable to list CorrelationIdFilters")
		return ctrl.Result{}, err
	}

	var redisMetadataEnricherList hyperv1alpha1.RedisMetadataEnricherFilterList
	if err := r.List(ctx, &redisMetadataEnricherList); err != nil {
		reqLogger.Error(err, "unable to list RedisMetadataEnricherFilters")
		return ctrl.Result{}, err
	}

	var apiKeyList hyperv1alpha1.ApiKeyFilterList
	if err := r.List(ctx, &apiKeyList); err != nil {
		reqLogger.Error(err, "unable to list ApiKeyFilters")
		return ctrl.Result{}, err
	}

	var externalAuthList hyperv1alpha1.ExternalAuthFilterList
	if err := r.List(ctx, &externalAuthList); err != nil {
		reqLogger.Error(err, "unable to list ExternalAuthFilters")
		return ctrl.Result{}, err
	}

	var firewallList hyperv1alpha1.FirewallFilterList
	if err := r.List(ctx, &firewallList); err != nil {
		reqLogger.Error(err, "unable to list FirewallFilters")
		return ctrl.Result{}, err
	}

	var jwtList hyperv1alpha1.JwtAuthFilterList
	if err := r.List(ctx, &jwtList); err != nil {
		reqLogger.Error(err, "unable to list JwtAuthFilters")
		return ctrl.Result{}, err
	}

	// Sort routes by priority descending
	routes := routeList.Items
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Spec.Priority > routes[j].Spec.Priority
	})

	// Cluster-wide policy shared by every engine deployment.
	redisConfigs := make(map[string]config.RedisServiceConfig)
	chainConfigs := make(map[string]config.Chain)
	var routeConfigs []config.RouteConfig

	// Map HyperRedis list
	for _, hr := range redisList.Items {
		redisConfigs[hr.Name] = config.RedisServiceConfig{
			URL:                   hr.Spec.Url,
			Type:                  string(hr.Spec.Type),
			PoolSize:              hr.Spec.PoolSize,
			Timeout:               hr.Spec.Timeout,
			ActiveConnHealthCheck: hr.Spec.ActiveConnHealthCheck,
		}
	}

	// Maps of filters for lookup
	rateLimitMap := make(map[string]*hyperv1alpha1.RateLimitFilter)
	for i := range rateLimitList.Items {
		rateLimitMap[rateLimitList.Items[i].Name] = &rateLimitList.Items[i]
	}

	headerModifierMap := make(map[string]*hyperv1alpha1.HeaderModifierFilter)
	for i := range headerModifierList.Items {
		headerModifierMap[headerModifierList.Items[i].Name] = &headerModifierList.Items[i]
	}

	denyMap := make(map[string]*hyperv1alpha1.DenyFilter)
	for i := range denyList.Items {
		denyMap[denyList.Items[i].Name] = &denyList.Items[i]
	}

	correlationIdMap := make(map[string]*hyperv1alpha1.CorrelationIdFilter)
	for i := range correlationIdList.Items {
		correlationIdMap[correlationIdList.Items[i].Name] = &correlationIdList.Items[i]
	}

	redisMetadataEnricherMap := make(map[string]*hyperv1alpha1.RedisMetadataEnricherFilter)
	for i := range redisMetadataEnricherList.Items {
		redisMetadataEnricherMap[redisMetadataEnricherList.Items[i].Name] = &redisMetadataEnricherList.Items[i]
	}

	apiKeyMap := make(map[string]*hyperv1alpha1.ApiKeyFilter)
	for i := range apiKeyList.Items {
		apiKeyMap[apiKeyList.Items[i].Name] = &apiKeyList.Items[i]
	}

	externalAuthMap := make(map[string]*hyperv1alpha1.ExternalAuthFilter)
	for i := range externalAuthList.Items {
		externalAuthMap[externalAuthList.Items[i].Name] = &externalAuthList.Items[i]
	}

	firewallMap := make(map[string]*hyperv1alpha1.FirewallFilter)
	for i := range firewallList.Items {
		firewallMap[firewallList.Items[i].Name] = &firewallList.Items[i]
	}

	jwtMap := make(map[string]*hyperv1alpha1.JwtAuthFilter)
	for i := range jwtList.Items {
		jwtMap[jwtList.Items[i].Name] = &jwtList.Items[i]
	}

	// Map HyperChain list and handle status bubbling
	for _, chainObj := range chainList.Items {
		var chain config.Chain
		failed := false
		var failMsg string

		for _, filterRef := range chainObj.Spec.Filters {
			var resolvedOptions interface{}
			var filterType string

			switch filterRef.Kind {
			case "RateLimitFilter":
				filterType = "embedded_rate_limiter"
				f, exists := rateLimitMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "HeaderModifierFilter":
				filterType = "header_modifier"
				f, exists := headerModifierMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "DenyFilter":
				filterType = "deny"
				f, exists := denyMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "CorrelationIdFilter":
				filterType = "correlation_id"
				f, exists := correlationIdMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "RedisMetadataEnricherFilter":
				filterType = "redis_metadata_enricher"
				f, exists := redisMetadataEnricherMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "ApiKeyFilter":
				filterType = "api_key"
				f, exists := apiKeyMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				resolvedOptions = f.Spec
			case "ExternalAuthFilter":
				filterType = "external_auth"
				f, exists := externalAuthMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind %s not found", filterRef.Name, filterRef.Kind)
					break
				}
				// Build a map that exactly matches the engine's ExternalAuthConfig YAML tags.
				// The socket path is deterministically derived from the CRD name.
				socketPath := fmt.Sprintf("/var/run/hypergate/ext-auth-%s.sock", f.Name)
				resolvedOptions = map[string]interface{}{
					"protocol":        f.Spec.Protocol,
					"socket_path":     socketPath,
					"timeout":         f.Spec.EngineRules.Timeout,
					"forward_headers": f.Spec.EngineRules.ForwardHeaders,
					"on_success": map[string]interface{}{
						"upstream_headers_to_add":    f.Spec.EngineRules.OnSuccess.UpstreamHeadersToAdd,
						"upstream_headers_to_remove": f.Spec.EngineRules.OnSuccess.UpstreamHeadersToRemove,
					},
					"on_failure": map[string]interface{}{
						"downstream_pass_through_headers": f.Spec.EngineRules.OnFailure.DownstreamPassThroughHeaders,
					},
				}
			case "FirewallFilter":
				filterType = "firewall"
				f, exists := firewallMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind FirewallFilter not found", filterRef.Name)
					break
				}
				socketPath := fmt.Sprintf("/var/run/hypergate/fw-%s.sock", f.Name)
				resolvedOptions = map[string]interface{}{
					"protocol":         f.Spec.Protocol,
					"socket_path":      socketPath,
					"timeout":          f.Spec.EngineRules.Timeout,
					"inspect_body":     f.Spec.EngineRules.InspectBody,
					"max_body_size_kb": f.Spec.EngineRules.MaxBodySizeKB,
					"forward_headers":  f.Spec.EngineRules.ForwardHeaders,
					"on_success": map[string]interface{}{
						"upstream_headers_to_add":    f.Spec.EngineRules.OnSuccess.UpstreamHeadersToAdd,
						"upstream_headers_to_remove": f.Spec.EngineRules.OnSuccess.UpstreamHeadersToRemove,
					},
					"on_failure": map[string]interface{}{
						"downstream_pass_through_headers": f.Spec.EngineRules.OnFailure.DownstreamPassThroughHeaders,
					},
				}
			case "JwtAuthFilter":
				filterType = "jwt_auth"
				f, exists := jwtMap[filterRef.Name]
				if !exists {
					failed = true
					failMsg = fmt.Sprintf("Filter %s of Kind JwtAuthFilter not found", filterRef.Name)
					break
				}
				resolvedOptions = jwtAuthOptions(f)
			default:
				failed = true
				failMsg = fmt.Sprintf("Unknown filter Kind %s", filterRef.Kind)
			}

			if failed {
				break
			}

			// Translate CRD spec into engine options map[string]interface{} via YAML marshal/unmarshal
			// to preserve snake_case keys that match the engine's yaml unmarshaler.
			specBytes, err := yaml.Marshal(resolvedOptions)
			if err != nil {
				failed = true
				failMsg = fmt.Sprintf("Failed to marshal filter options: %v", err)
				break
			}

			var opts map[string]interface{}
			if err := yaml.Unmarshal(specBytes, &opts); err != nil {
				failed = true
				failMsg = fmt.Sprintf("Failed to unmarshal filter options into map: %v", err)
				break
			}

			chain = append(chain, config.FilterConfig{
				Type:    filterType,
				Options: opts,
			})
		}

		// Update Status
		statusCopy := chainObj.Status.DeepCopy()
		if failed {
			// Fail closed: a degraded chain keeps its name but rejects every request,
			// instead of disappearing and letting its routes fall through to a
			// weaker (or no) policy.
			statusCopy.State = "Degraded"
			statusCopy.Message = failMsg
			chainConfigs[chainObj.Name] = degradedChain()
		} else {
			statusCopy.State = "Ready"
			statusCopy.Message = "Chain successfully compiled"
			chainConfigs[chainObj.Name] = chain
		}

		if statusCopy.State != chainObj.Status.State || statusCopy.Message != chainObj.Status.Message {
			chainObj.Status = *statusCopy
			if err := r.Status().Update(ctx, &chainObj); err != nil {
				reqLogger.Error(err, "unable to update HyperChain status", "chain", chainObj.Name)
				return ctrl.Result{}, err
			}
		}
	}

	// Map HyperRoute list
	for _, hr := range routes {
		// The engine rejects a whole config containing an invalid route, which would
		// freeze every later update; drop the broken route and report it instead.
		if hr.Spec.TargetPolicy == "" {
			reqLogger.Error(nil, "HyperRoute has no targetPolicy, skipping", "route", hr.Name)
			continue
		}
		if err := validateRouteRegexes(&hr); err != nil {
			reqLogger.Error(err, "HyperRoute has an invalid regex, skipping", "route", hr.Name)
			continue
		}

		// A route to a chain that does not exist fails closed as well.
		if _, ok := chainConfigs[hr.Spec.TargetPolicy]; !ok {
			reqLogger.Info("HyperRoute targets a missing HyperChain, requests will be rejected", "route", hr.Name, "chain", hr.Spec.TargetPolicy)
			chainConfigs[hr.Spec.TargetPolicy] = degradedChain()
		}

		var matchConfigs []config.MatchConfig
		for _, m := range hr.Spec.Matches {
			var hmConfigs map[string]config.HeaderMatchConfig
			if m.Headers != nil {
				hmConfigs = make(map[string]config.HeaderMatchConfig)
				for k, v := range m.Headers {
					hmConfigs[strings.ToLower(k)] = config.HeaderMatchConfig{Exact: v}
				}
			}

			matchConfigs = append(matchConfigs, config.MatchConfig{
				PathPrefix:       m.PathPrefix,
				PathRegexPattern: m.PathRegexPattern,
				Headers:          hmConfigs,
			})
		}
		routeConfigs = append(routeConfigs, config.RouteConfig{
			Name:        hr.Name,
			TargetChain: hr.Spec.TargetPolicy,
			Matches:     matchConfigs,
		})
	}

	for targetNS, hc := range activeConfigs {
		chains := chainConfigs
		if dc := hc.Spec.DefaultChain; dc != "" {
			if _, ok := chainConfigs[dc]; !ok {
				reqLogger.Info("HyperConfig defaultChain does not exist, unmatched requests will be rejected", "hyperconfig", hc.Name, "chain", dc)
				chains = make(map[string]config.Chain, len(chainConfigs)+1)
				for k, v := range chainConfigs {
					chains[k] = v
				}
				chains[dc] = degradedChain()
			}
		}

		engineConfig := config.Config{
			Version: "v1",
			Server: config.ServerConfig{
				Address:                 hc.Spec.ServerAddress,
				MaxConcurrentStreams:    hc.Spec.MaxConcurrentStreams,
				PoolPrewarmSize:         int(hc.Spec.PoolPrewarmSize),
				InitialHeaderCapacity:   int(hc.Spec.InitialHeaderCapacity),
				PreallocBodyBufferBytes: int(hc.Spec.PreallocBodyBufferBytes),
				HealthAddress:           fmt.Sprintf(":%d", engineHealthPort),
				ClientIP:                config.ClientIPConfig{TrustedProxyHops: int(hc.Spec.TrustedProxyHops)},
			},
			Telemetry: config.TelemetryConfig{
				Logging: mylogger.LoggingConfig{
					Level: string(hc.Spec.LogLevel),
				},
			},
			Redis:  redisConfigs,
			Chains: chains,
			Router: config.RouterConfig{
				Routes:       routeConfigs,
				DefaultChain: hc.Spec.DefaultChain,
			},
		}
		if engineConfig.Router.Routes == nil {
			engineConfig.Router.Routes = []config.RouteConfig{}
		}

		if err := r.writeConfigMap(ctx, targetNS, &engineConfig); err != nil {
			reqLogger.Error(err, "unable to write engine ConfigMap", "namespace", targetNS)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *HyperChainMasterCompilerReconciler) writeConfigMap(ctx context.Context, namespace string, engineConfig *config.Config) error {
	yamlBytes, err := yaml.Marshal(engineConfig)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: engineConfigMapName, Namespace: namespace}
	err = r.Get(ctx, key, cm)
	switch {
	case errors.IsNotFound(err):
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Data:       map[string]string{"config.yaml": string(yamlBytes)},
		}
		if err := r.Create(ctx, cm); err != nil {
			return fmt.Errorf("create configmap: %w", err)
		}
		log.FromContext(ctx).Info("Created ConfigMap", "namespace", namespace, "name", key.Name)
	case err != nil:
		return fmt.Errorf("get configmap: %w", err)
	default:
		if cm.Data["config.yaml"] == string(yamlBytes) {
			return nil // unchanged: avoid a needless engine reload
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["config.yaml"] = string(yamlBytes)
		if err := r.Update(ctx, cm); err != nil {
			return fmt.Errorf("update configmap: %w", err)
		}
		log.FromContext(ctx).Info("Updated ConfigMap", "namespace", namespace, "name", key.Name)
	}
	return nil
}

// degradedChain rejects every request. It replaces chains that failed to compile
// and chains that are referenced but do not exist.
func degradedChain() config.Chain {
	return config.Chain{{
		Type: "deny",
		Options: map[string]interface{}{
			"status_code": degradedChainStatusCode,
			"body":        degradedChainResponseBody,
		},
	}}
}

// jwtAuthOptions maps a JwtAuthFilter to engine options. Secret references become
// file paths of the Secrets the HyperConfig controller mounts into the engine pod.
func jwtAuthOptions(f *hyperv1alpha1.JwtAuthFilter) map[string]interface{} {
	opts := map[string]interface{}{}
	specBytes, err := yaml.Marshal(f.Spec)
	if err == nil {
		_ = yaml.Unmarshal(specBytes, &opts)
	}
	if ref := f.Spec.LocalSecretRef; ref != nil {
		opts["local_secret_file"] = jwtSecretFile(f.Name, ref, jwtLocalSecretFile)
	}
	if ref := f.Spec.IntrospectionAuthSecretRef; ref != nil {
		opts["introspection_auth_header_file"] = jwtSecretFile(f.Name, ref, jwtIntrospectionAuthFile)
	}
	return opts
}

// SetupWithManager sets up the controller with the Manager.
func (r *HyperChainMasterCompilerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	triggerFunc := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var routeList hyperv1alpha1.HyperRouteList
		if err := mgr.GetClient().List(ctx, &routeList); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, len(routeList.Items))
		for i, route := range routeList.Items {
			reqs[i] = reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      route.Name,
					Namespace: route.Namespace,
				},
			}
		}
		// Also ensure a default reconciliation runs in case no routes exist yet
		// to build the base config with the default chain.
		if len(reqs) == 0 {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "global", Namespace: "default"},
			})
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1alpha1.HyperRoute{}).
		Watches(&hyperv1alpha1.HyperChain{}, triggerFunc).
		Watches(&hyperv1alpha1.HyperConfig{}, triggerFunc).
		Watches(&hyperv1alpha1.HyperRedis{}, triggerFunc).
		Watches(&hyperv1alpha1.RateLimitFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.HeaderModifierFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.DenyFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.CorrelationIdFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.RedisMetadataEnricherFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.ApiKeyFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.ExternalAuthFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.FirewallFilter{}, triggerFunc).
		Watches(&hyperv1alpha1.JwtAuthFilter{}, triggerFunc).
		Complete(r)
}

func validateRouteRegexes(hr *hyperv1alpha1.HyperRoute) error {
	for i, m := range hr.Spec.Matches {
		if m.PathRegexPattern == "" {
			continue
		}
		if _, err := regexp.Compile(m.PathRegexPattern); err != nil {
			return fmt.Errorf("matches[%d].pathRegexPattern: %w", i, err)
		}
	}
	return nil
}
