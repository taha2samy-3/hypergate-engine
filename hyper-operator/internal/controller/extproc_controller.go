package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
)

// Kinds the ext_proc controller manages or reads. They come from other projects'
// CRDs, so they are handled as unstructured objects and every one is optional.
var (
	envoyExtensionPolicyGVK = schema.GroupVersionKind{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "EnvoyExtensionPolicy"}
	referenceGrantGVK       = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant"}
	httpRouteGVK            = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	gatewayGVK              = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}
	ciliumExtProcFilterGVK  = schema.GroupVersionKind{Group: "cilium.io", Version: "v2alpha1", Kind: "CiliumEnvoyExtProcFilter"}
)

const (
	// CiliumFilterName is the CiliumEnvoyExtProcFilter HTTPRoutes reference.
	CiliumFilterName = "hypergate"
	// extProcReferenceGrantName is the ReferenceGrant in the engine namespace.
	extProcReferenceGrantName = "hypergate-ext-proc"
	engineServiceName         = "hyper-engine-svc"
	defaultExtProcTimeout     = "2500ms"

	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelHyperConfig = "hyper.io/hyperconfig"
	managedByValue   = "hyper-operator"

	// Condition types on HyperConfig.status.conditions.
	ConditionEnvoyGatewayPolicies = "EnvoyGatewayPolicies"
	ConditionCiliumFilters        = "CiliumFilters"
	ConditionHTTPRoutes           = "HTTPRoutes"

	// extProcRecheck re-detects optional CRDs and re-validates HTTPRoutes.
	extProcRecheck = 5 * time.Minute
)

// ExtProcReconciler generates the objects that attach an engine to Envoy:
// EnvoyExtensionPolicies for listed Gateways (Envoy Gateway), CiliumEnvoyExtProcFilters
// in listed namespaces (once Cilium ships the CRD) and the ReferenceGrant that lets
// them point at the engine Service. It reads HTTPRoutes to report wrong references,
// and never creates, edits or deletes an HTTPRoute.
type ExtProcReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=envoyextensionpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes;gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumenvoyextprocfilters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hyper.io,resources=hyperconfigs/status,verbs=get;update;patch

func (r *ExtProcReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var hc hyperv1alpha1.HyperConfig
	if err := r.Get(ctx, req.NamespacedName, &hc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var all hyperv1alpha1.HyperConfigList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	if _, conflicts := activeHyperConfigs(all.Items); conflicts[hc.Name] != "" {
		return ctrl.Result{}, nil // the HyperConfig controller reports the conflict
	}

	spec := hc.Spec.ExtProc
	if spec == nil {
		spec = &hyperv1alpha1.ExtProcSpec{}
	}
	engineNS := targetNamespaceOf(&hc)

	var conds []metav1.Condition
	egCond, egNamespaces, err := r.reconcileEnvoyGateway(ctx, &hc, spec, engineNS)
	if err != nil {
		return ctrl.Result{}, err
	}
	conds = append(conds, egCond)

	ciliumCond, ciliumNamespaces, err := r.reconcileCilium(ctx, &hc, spec, engineNS)
	if err != nil {
		return ctrl.Result{}, err
	}
	conds = append(conds, ciliumCond)

	if err := r.reconcileReferenceGrant(ctx, &hc, engineNS, egNamespaces, ciliumNamespaces); err != nil {
		return ctrl.Result{}, err
	}

	routeCond, err := r.checkHTTPRoutes(ctx, spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	conds = append(conds, routeCond)

	if err := r.setConditions(ctx, &hc, conds); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("ext_proc attachment reconciled", "gateways", len(spec.Gateways), "namespaces", len(spec.Namespaces))
	return ctrl.Result{RequeueAfter: extProcRecheck}, nil
}

// installed reports whether the cluster serves a kind (its CRD is installed).
func (r *ExtProcReconciler) installed(gvk schema.GroupVersionKind) (bool, error) {
	_, err := r.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	}
	return false, err
}

// --- Envoy Gateway ---

func (r *ExtProcReconciler) reconcileEnvoyGateway(ctx context.Context, hc *hyperv1alpha1.HyperConfig, spec *hyperv1alpha1.ExtProcSpec, engineNS string) (metav1.Condition, []string, error) {
	cond := metav1.Condition{Type: ConditionEnvoyGatewayPolicies}
	ok, err := r.installed(envoyExtensionPolicyGVK)
	if err != nil {
		return cond, nil, err
	}
	if !ok {
		if len(spec.Gateways) > 0 {
			return falseCond(cond, "CRDNotInstalled", "extProc.gateways is set but Envoy Gateway's EnvoyExtensionPolicy CRD is not installed"), nil, nil
		}
		return falseCond(cond, "NotConfigured", "no Gateways listed in extProc.gateways"), nil, nil
	}

	desired := map[types.NamespacedName]*unstructured.Unstructured{}
	var missing []string
	gatewaysInstalled, err := r.installed(gatewayGVK)
	if err != nil {
		return cond, nil, err
	}
	for _, gw := range spec.Gateways {
		if gatewaysInstalled {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gatewayGVK)
			if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}, obj); err != nil {
				if !errors.IsNotFound(err) {
					return cond, nil, err
				}
				missing = append(missing, gw.Namespace+"/"+gw.Name)
			}
		}
		p := envoyExtensionPolicy(hc, gw, engineNS, spec)
		desired[client.ObjectKeyFromObject(p)] = p
	}
	if err := r.applyAndPrune(ctx, hc, envoyExtensionPolicyGVK, desired); err != nil {
		return cond, nil, err
	}

	if len(spec.Gateways) == 0 {
		return falseCond(cond, "NotConfigured", "no Gateways listed in extProc.gateways"), nil, nil
	}
	namespaces := gatewayNamespaces(spec.Gateways)
	if len(missing) > 0 {
		// The policy is still created: it takes effect as soon as the Gateway exists.
		return falseCond(cond, "GatewayNotFound", "EnvoyExtensionPolicies created, but these Gateways do not exist: "+strings.Join(missing, ", ")), namespaces, nil
	}
	cond.Status = metav1.ConditionTrue
	cond.Reason = "Applied"
	cond.Message = fmt.Sprintf("EnvoyExtensionPolicy applied to %d Gateway(s)", len(spec.Gateways))
	return cond, namespaces, nil
}

// envoyExtensionPolicy points the Gateway's ext_proc filter at the engine. Envoy
// Gateway cannot send static stream metadata, so the engine treats these
// requests as north-south (its default without x-hypergate-traffic), which is
// what Gateway traffic is.
func envoyExtensionPolicy(hc *hyperv1alpha1.HyperConfig, gw hyperv1alpha1.GatewayRef, engineNS string, spec *hyperv1alpha1.ExtProcSpec) *unstructured.Unstructured {
	p := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{
				"group": gatewayGVK.Group,
				"kind":  gatewayGVK.Kind,
				"name":  gw.Name,
			}},
			"extProc": []any{map[string]any{
				"backendRefs": []any{map[string]any{
					"group":     "",
					"kind":      "Service",
					"name":      engineServiceName,
					"namespace": engineNS,
					"port":      int64(engineGRPCPort),
				}},
				"messageTimeout": messageTimeout(spec),
				"failOpen":       spec.FailureMode == hyperv1alpha1.ExtProcFailOpen,
				"processingMode": map[string]any{
					"request":           map[string]any{"attributes": []any{"source.address", "destination.address"}},
					"response":          map[string]any{},
					"allowModeOverride": true,
				},
			}},
		},
	}}
	p.SetGroupVersionKind(envoyExtensionPolicyGVK)
	p.SetNamespace(gw.Namespace)
	p.SetName("hypergate-" + gw.Name)
	p.SetLabels(managedLabels(hc))
	return p
}

// --- Cilium ---

func (r *ExtProcReconciler) reconcileCilium(ctx context.Context, hc *hyperv1alpha1.HyperConfig, spec *hyperv1alpha1.ExtProcSpec, engineNS string) (metav1.Condition, []string, error) {
	cond := metav1.Condition{Type: ConditionCiliumFilters}
	ok, err := r.installed(ciliumExtProcFilterGVK)
	if err != nil {
		return cond, nil, err
	}
	if !ok {
		if len(spec.Namespaces) > 0 {
			return falseCond(cond, "CRDNotInstalled", "waiting for Cilium's CiliumEnvoyExtProcFilter CRD (cilium/cilium#46479); filters are created once it is installed"), nil, nil
		}
		return falseCond(cond, "NotConfigured", "no namespaces listed in extProc.namespaces"), nil, nil
	}

	desired := map[types.NamespacedName]*unstructured.Unstructured{}
	for _, ns := range uniqueSorted(spec.Namespaces) {
		f := ciliumExtProcFilter(hc, ns, engineNS, spec)
		desired[client.ObjectKeyFromObject(f)] = f
	}
	if err := r.applyAndPrune(ctx, hc, ciliumExtProcFilterGVK, desired); err != nil {
		return cond, nil, err
	}
	if len(desired) == 0 {
		return falseCond(cond, "NotConfigured", "no namespaces listed in extProc.namespaces"), nil, nil
	}
	cond.Status = metav1.ConditionTrue
	cond.Reason = "Applied"
	cond.Message = fmt.Sprintf("CiliumEnvoyExtProcFilter %q present in %d namespace(s)", CiliumFilterName, len(desired))
	return cond, uniqueSorted(spec.Namespaces), nil
}

// ciliumExtProcFilter follows the draft API of cilium/cilium#46479 (backendRef,
// processingMode, messageTimeout). The draft has no request attributes, stream
// metadata, failure mode or mode override yet; fields are re-aligned in phase R6.
func ciliumExtProcFilter(hc *hyperv1alpha1.HyperConfig, namespace, engineNS string, spec *hyperv1alpha1.ExtProcSpec) *unstructured.Unstructured {
	f := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"backendRef": map[string]any{
				"name":      engineServiceName,
				"namespace": engineNS,
				"port":      int64(engineGRPCPort),
			},
			"processingMode": map[string]any{
				"requestHeaderMode":   "SEND",
				"responseHeaderMode":  "SEND",
				"requestBodyMode":     "NONE",
				"responseBodyMode":    "NONE",
				"requestTrailerMode":  "SKIP",
				"responseTrailerMode": "SKIP",
			},
			"messageTimeout": messageTimeout(spec),
		},
	}}
	f.SetGroupVersionKind(ciliumExtProcFilterGVK)
	f.SetNamespace(namespace)
	f.SetName(CiliumFilterName)
	f.SetLabels(managedLabels(hc))
	return f
}

// --- ReferenceGrant ---

// reconcileReferenceGrant lets the generated objects in other namespaces
// reference the engine Service.
func (r *ExtProcReconciler) reconcileReferenceGrant(ctx context.Context, hc *hyperv1alpha1.HyperConfig, engineNS string, egNamespaces, ciliumNamespaces []string) error {
	ok, err := r.installed(referenceGrantGVK)
	if err != nil || !ok {
		return err
	}
	var from []any
	for _, ns := range egNamespaces {
		if ns != engineNS {
			from = append(from, map[string]any{"group": envoyExtensionPolicyGVK.Group, "kind": envoyExtensionPolicyGVK.Kind, "namespace": ns})
		}
	}
	for _, ns := range ciliumNamespaces {
		if ns != engineNS {
			from = append(from, map[string]any{"group": ciliumExtProcFilterGVK.Group, "kind": ciliumExtProcFilterGVK.Kind, "namespace": ns})
		}
	}
	desired := map[types.NamespacedName]*unstructured.Unstructured{}
	if len(from) > 0 {
		g := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{
				"from": from,
				"to":   []any{map[string]any{"group": "", "kind": "Service", "name": engineServiceName}},
			},
		}}
		g.SetGroupVersionKind(referenceGrantGVK)
		g.SetNamespace(engineNS)
		g.SetName(extProcReferenceGrantName)
		g.SetLabels(managedLabels(hc))
		desired[client.ObjectKeyFromObject(g)] = g
	}
	return r.applyAndPrune(ctx, hc, referenceGrantGVK, desired)
}

// --- HTTPRoutes (read only) ---

// checkHTTPRoutes finds HTTPRoutes that reference the Hypergate Cilium filter and
// reports references that cannot work. HTTPRoutes are never modified: problems go
// to the HyperConfig condition and to Events on the HTTPRoute.
func (r *ExtProcReconciler) checkHTTPRoutes(ctx context.Context, spec *hyperv1alpha1.ExtProcSpec) (metav1.Condition, error) {
	cond := metav1.Condition{Type: ConditionHTTPRoutes}
	ok, err := r.installed(httpRouteGVK)
	if err != nil {
		return cond, err
	}
	if !ok {
		return falseCond(cond, "CRDNotInstalled", "Gateway API HTTPRoute CRD is not installed"), nil
	}
	ciliumOK, err := r.installed(ciliumExtProcFilterGVK)
	if err != nil {
		return cond, err
	}

	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(httpRouteGVK.GroupVersion().WithKind("HTTPRouteList"))
	if err := r.List(ctx, routes); err != nil {
		return cond, err
	}
	listed := map[string]bool{}
	for _, ns := range spec.Namespaces {
		listed[ns] = true
	}

	var referencing int
	var problems []string
	for i := range routes.Items {
		route := &routes.Items[i]
		if !referencesHypergateFilter(route) {
			continue
		}
		referencing++
		var reason string
		switch {
		case !listed[route.GetNamespace()]:
			reason = fmt.Sprintf("namespace %q is not listed in a HyperConfig's extProc.namespaces, so the %q filter is not created there", route.GetNamespace(), CiliumFilterName)
		case !ciliumOK:
			reason = "the CiliumEnvoyExtProcFilter CRD is not installed (waiting for cilium/cilium#46479)"
		}
		if reason == "" {
			continue
		}
		problems = append(problems, route.GetNamespace()+"/"+route.GetName()+": "+reason)
		if r.Recorder != nil {
			r.Recorder.Eventf(route, nil, "Warning", "HypergateFilterUnavailable", "Validate",
				"References the Hypergate ext_proc filter, but %s", reason)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return falseCond(cond, "InvalidReferences", strings.Join(problems, "; ")), nil
	}
	cond.Status = metav1.ConditionTrue
	cond.Reason = "Valid"
	cond.Message = fmt.Sprintf("%d HTTPRoute(s) reference the Hypergate filter", referencing)
	return cond, nil
}

// referencesHypergateFilter reports whether any rule of the route has an
// ExtensionRef to the Hypergate CiliumEnvoyExtProcFilter.
func referencesHypergateFilter(route *unstructured.Unstructured) bool {
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	for _, rule := range rules {
		rm, _ := rule.(map[string]any)
		filters, _, _ := unstructured.NestedSlice(rm, "filters")
		for _, f := range filters {
			fm, _ := f.(map[string]any)
			ref, _, _ := unstructured.NestedMap(fm, "extensionRef")
			if ref["group"] == ciliumExtProcFilterGVK.Group && ref["kind"] == ciliumExtProcFilterGVK.Kind && ref["name"] == CiliumFilterName {
				return true
			}
		}
	}
	return false
}

// --- helpers ---

// applyAndPrune creates or updates the desired objects of one kind and deletes
// objects of that kind this HyperConfig created earlier but no longer wants.
func (r *ExtProcReconciler) applyAndPrune(ctx context.Context, hc *hyperv1alpha1.HyperConfig, gvk schema.GroupVersionKind, desired map[types.NamespacedName]*unstructured.Unstructured) error {
	for _, obj := range desired {
		if err := ctrl.SetControllerReference(hc, obj, r.Scheme); err != nil {
			return err
		}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(gvk)
		err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
		switch {
		case errors.IsNotFound(err):
			if err := r.Create(ctx, obj); err != nil {
				return fmt.Errorf("create %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
			continue
		case err != nil:
			return err
		}
		if !isManagedBy(existing, hc) {
			// Never take over an object someone else created under the same name.
			return fmt.Errorf("%s %s/%s exists and is not managed by HyperConfig %q", gvk.Kind, obj.GetNamespace(), obj.GetName(), hc.Name)
		}
		if equality.Semantic.DeepEqual(existing.Object["spec"], obj.Object["spec"]) &&
			equality.Semantic.DeepEqual(existing.GetLabels(), obj.GetLabels()) {
			continue
		}
		existing.Object["spec"] = obj.Object["spec"]
		existing.SetLabels(obj.GetLabels())
		existing.SetOwnerReferences(obj.GetOwnerReferences())
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("update %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.List(ctx, list, client.MatchingLabels{labelManagedBy: managedByValue, labelHyperConfig: hc.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, keep := desired[client.ObjectKeyFromObject(item)]; keep {
			continue
		}
		if err := r.Delete(ctx, item); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete %s %s/%s: %w", gvk.Kind, item.GetNamespace(), item.GetName(), err)
		}
	}
	return nil
}

func (r *ExtProcReconciler) setConditions(ctx context.Context, hc *hyperv1alpha1.HyperConfig, conds []metav1.Condition) error {
	patch := client.MergeFrom(hc.DeepCopy())
	changed := false
	for _, c := range conds {
		c.ObservedGeneration = hc.Generation
		if meta.SetStatusCondition(&hc.Status.Conditions, c) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, hc, patch)
}

func falseCond(c metav1.Condition, reason, msg string) metav1.Condition {
	c.Status = metav1.ConditionFalse
	c.Reason = reason
	c.Message = msg
	return c
}

func managedLabels(hc *hyperv1alpha1.HyperConfig) map[string]string {
	return map[string]string{labelManagedBy: managedByValue, labelHyperConfig: hc.Name}
}

func isManagedBy(obj client.Object, hc *hyperv1alpha1.HyperConfig) bool {
	l := obj.GetLabels()
	return l[labelManagedBy] == managedByValue && l[labelHyperConfig] == hc.Name
}

func messageTimeout(spec *hyperv1alpha1.ExtProcSpec) string {
	if spec.MessageTimeout != "" {
		return spec.MessageTimeout
	}
	return defaultExtProcTimeout
}

func gatewayNamespaces(gws []hyperv1alpha1.GatewayRef) []string {
	ns := make([]string, 0, len(gws))
	for _, g := range gws {
		ns = append(ns, g.Namespace)
	}
	return uniqueSorted(ns)
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

// SetupWithManager watches HyperConfigs, the generated objects and HTTPRoutes.
// Optional kinds are watched only when their CRD exists at start-up; otherwise
// the periodic re-check picks them up.
func (r *ExtProcReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allConfigs := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list hyperv1alpha1.HyperConfigList
		if err := mgr.GetClient().List(ctx, &list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, len(list.Items))
		for i := range list.Items {
			reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}}
		}
		return reqs
	})
	owner := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if name := o.GetLabels()[labelHyperConfig]; name != "" && o.GetLabels()[labelManagedBy] == managedByValue {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
		}
		return nil
	})

	b := ctrl.NewControllerManagedBy(mgr).
		Named("extproc").
		For(&hyperv1alpha1.HyperConfig{})
	for _, w := range []struct {
		gvk schema.GroupVersionKind
		h   handler.EventHandler
	}{
		{envoyExtensionPolicyGVK, owner},
		{ciliumExtProcFilterGVK, owner},
		{referenceGrantGVK, owner},
		{httpRouteGVK, allConfigs},
		{gatewayGVK, allConfigs},
	} {
		if ok, err := r.installed(w.gvk); err != nil || !ok {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(w.gvk)
		b = b.Watches(obj, w.h)
	}
	return b.Complete(r)
}
