package controller

import (
	"context"
	"fmt"
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

	hyperv1alpha1 "github.com/taha/myprog/hyper-operator/api/v1alpha1"
	"github.com/taha/myprog/internal/config"
	mylogger "github.com/taha/myprog/internal/logger"
)

// HyperChainMasterCompilerReconciler reconciles all state to build config.yaml
type HyperChainMasterCompilerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperredises,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperchains,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=hyperchains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=ratelimitfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=headermodifierfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=denyfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=correlationidfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=redismetadataenricherfilters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hyper.io,resources=apikeyfilters,verbs=get;list;watch;update;patch
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

	activeConfig := configList.Items[0]

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

	// Sort routes by priority descending
	routes := routeList.Items
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Spec.Priority > routes[j].Spec.Priority
	})

	// Mapping to Engine Structs
	engineConfig := config.Config{
		Version: "v1",
		Server: config.ServerConfig{
			Address:              activeConfig.Spec.ServerAddress,
			MaxConcurrentStreams: activeConfig.Spec.MaxConcurrentStreams,
		},
		Telemetry: config.TelemetryConfig{
			Logging: mylogger.LoggingConfig{
				Level: string(activeConfig.Spec.LogLevel),
			},
		},
		Redis:  make(map[string]config.RedisServiceConfig),
		Chains: make(map[string]config.Chain),
		Router: config.RouterConfig{
			Routes:       []config.RouteConfig{},
			DefaultChain: activeConfig.Spec.DefaultChain,
		},
	}

	// Map HyperRedis list
	for _, hr := range redisList.Items {
		engineConfig.Redis[hr.Name] = config.RedisServiceConfig{
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

	// Map HyperChain list and handle status bubbling
	validChains := make(map[string]bool)
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
			statusCopy.State = "Degraded"
			statusCopy.Message = failMsg
		} else {
			statusCopy.State = "Ready"
			statusCopy.Message = "Chain successfully compiled"
			engineConfig.Chains[chainObj.Name] = chain
			validChains[chainObj.Name] = true
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
		// Only include routes pointing to successfully compiled (valid) chains
		if !validChains[hr.Spec.TargetPolicy] {
			continue
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
		engineConfig.Router.Routes = append(engineConfig.Router.Routes, config.RouteConfig{
			Name:        hr.Name,
			TargetChain: hr.Spec.TargetPolicy,
			Matches:     matchConfigs,
		})
	}

	// YAML Generation
	yamlBytes, err := yaml.Marshal(&engineConfig)
	if err != nil {
		reqLogger.Error(err, "unable to marshal config to YAML")
		return ctrl.Result{}, err
	}

	targetNS := activeConfig.Spec.TargetNamespace
	if targetNS == "" {
		targetNS = "hyper-system"
	}

	// ConfigMap Write
	cm := &corev1.ConfigMap{}
	cmName := types.NamespacedName{
		Name:      "hyper-engine-config",
		Namespace: targetNS,
	}

	err = r.Get(ctx, cmName, cm)
	if err != nil && errors.IsNotFound(err) {
		// Create ConfigMap
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName.Name,
				Namespace: cmName.Namespace,
			},
			Data: map[string]string{
				"config.yaml": string(yamlBytes),
			},
		}
		if err := r.Create(ctx, cm); err != nil {
			reqLogger.Error(err, "unable to create ConfigMap")
			return ctrl.Result{}, err
		}
		reqLogger.Info("Created ConfigMap hyper-engine-config")
	} else if err == nil {
		// Update ConfigMap
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["config.yaml"] = string(yamlBytes)
		if err := r.Update(ctx, cm); err != nil {
			reqLogger.Error(err, "unable to update ConfigMap")
			return ctrl.Result{}, err
		}
		reqLogger.Info("Updated ConfigMap hyper-engine-config")
	} else {
		reqLogger.Error(err, "unable to get ConfigMap")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HyperChainMasterCompilerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	triggerFunc := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "global", Namespace: "default"}}}
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
		Complete(r)
}
