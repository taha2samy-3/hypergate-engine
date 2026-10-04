package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
)

var optionalKinds = []schema.GroupVersionKind{envoyExtensionPolicyGVK, referenceGrantGVK, httpRouteGVK, gatewayGVK, ciliumExtProcFilterGVK}

// extProcEnv builds a reconciler whose cluster serves the given optional kinds.
func extProcEnv(t *testing.T, installed []schema.GroupVersionKind, objs ...client.Object) (*ExtProcReconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	extra := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range installed {
		extra.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper := meta.MultiRESTMapper{testrestmapper.TestOnlyStaticRESTMapper(scheme), extra}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&hyperv1alpha1.HyperConfig{}).
		Build()
	rec := events.NewFakeRecorder(20)
	return &ExtProcReconciler{Client: c, Scheme: scheme, Recorder: rec}, c, rec
}

func unstructuredObj(gvk schema.GroupVersionKind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	if spec != nil {
		u.Object["spec"] = spec
	}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func routeReferencingHypergate(ns, name string) *unstructured.Unstructured {
	return unstructuredObj(httpRouteGVK, ns, name, map[string]any{
		"parentRefs": []any{map[string]any{"group": "", "kind": "Service", "name": "ledger"}},
		"rules": []any{map[string]any{
			"filters": []any{map[string]any{
				"type":         "ExtensionRef",
				"extensionRef": map[string]any{"group": "cilium.io", "kind": "CiliumEnvoyExtProcFilter", "name": "hypergate"},
			}},
		}},
	})
}

func extProcConfig(spec *hyperv1alpha1.ExtProcSpec) *hyperv1alpha1.HyperConfig {
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.ExtProc = spec
	return &hc
}

func reconcileExtProc(t *testing.T, r *ExtProcReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}}); err != nil {
		t.Fatal(err)
	}
}

func getUnstructured(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, false
		}
		t.Fatal(err)
	}
	return u, true
}

func condition(t *testing.T, c client.Client, condType string) metav1.Condition {
	t.Helper()
	var hc hyperv1alpha1.HyperConfig
	if err := c.Get(context.Background(), client.ObjectKey{Name: "main"}, &hc); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(hc.Status.Conditions, condType)
	if cond == nil {
		t.Fatalf("condition %s missing in %+v", condType, hc.Status.Conditions)
	}
	return *cond
}

func TestExtProc_GeneratesPoliciesFiltersAndReferenceGrant(t *testing.T) {
	hc := extProcConfig(&hyperv1alpha1.ExtProcSpec{
		Gateways:   []hyperv1alpha1.GatewayRef{{Namespace: "edge", Name: "public"}},
		Namespaces: []string{"shop", "payments"},
	})
	okRoute := routeReferencingHypergate("shop", "ledger")
	badRoute := routeReferencingHypergate("billing", "invoices")
	otherRoute := unstructuredObj(httpRouteGVK, "billing", "plain", map[string]any{"rules": []any{}})
	r, c, rec := extProcEnv(t, optionalKinds, hc,
		unstructuredObj(gatewayGVK, "edge", "public", map[string]any{}), okRoute, badRoute, otherRoute)

	before, _ := getUnstructured(t, c, httpRouteGVK, "billing", "invoices")
	routeVersion := before.GetResourceVersion()

	reconcileExtProc(t, r)

	// Envoy Gateway policy in the Gateway's namespace.
	p, ok := getUnstructured(t, c, envoyExtensionPolicyGVK, "edge", "hypergate-public")
	if !ok {
		t.Fatal("EnvoyExtensionPolicy not created")
	}
	target, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if target[0].(map[string]any)["name"] != "public" || target[0].(map[string]any)["kind"] != "Gateway" {
		t.Fatalf("targetRefs = %v", target)
	}
	ext, _, _ := unstructured.NestedSlice(p.Object, "spec", "extProc")
	e := ext[0].(map[string]any)
	backend := e["backendRefs"].([]any)[0].(map[string]any)
	if backend["name"] != engineServiceName || backend["namespace"] != "hyper-system" || backend["port"] != int64(engineGRPCPort) {
		t.Fatalf("backendRefs = %v", backend)
	}
	if e["failOpen"] != false || e["messageTimeout"] != "2500ms" {
		t.Fatalf("failure settings = %v %v", e["failOpen"], e["messageTimeout"])
	}
	attrs, _, _ := unstructured.NestedStringSlice(e, "processingMode", "request", "attributes")
	if strings.Join(attrs, ",") != "source.address,destination.address" {
		t.Fatalf("attributes = %v", attrs)
	}
	if allow, _, _ := unstructured.NestedBool(e, "processingMode", "allowModeOverride"); !allow {
		t.Fatal("allowModeOverride must be true")
	}
	if refs := p.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "main" {
		t.Fatalf("owner references = %v", refs)
	}

	// Cilium filters in each listed namespace.
	for _, ns := range []string{"shop", "payments"} {
		f, ok := getUnstructured(t, c, ciliumExtProcFilterGVK, ns, CiliumFilterName)
		if !ok {
			t.Fatalf("CiliumEnvoyExtProcFilter missing in %s", ns)
		}
		if name, _, _ := unstructured.NestedString(f.Object, "spec", "backendRef", "name"); name != engineServiceName {
			t.Fatalf("backendRef = %v", f.Object["spec"])
		}
	}

	// One ReferenceGrant in the engine namespace allowing both kinds.
	g, ok := getUnstructured(t, c, referenceGrantGVK, "hyper-system", extProcReferenceGrantName)
	if !ok {
		t.Fatal("ReferenceGrant not created")
	}
	from, _, _ := unstructured.NestedSlice(g.Object, "spec", "from")
	if len(from) != 3 {
		t.Fatalf("ReferenceGrant from = %v", from)
	}

	// HTTPRoutes are only read: problems are reported, the route is untouched.
	if cnd := condition(t, c, ConditionHTTPRoutes); cnd.Status != metav1.ConditionFalse || !strings.Contains(cnd.Message, "billing/invoices") {
		t.Fatalf("HTTPRoutes condition = %+v", cnd)
	}
	after, _ := getUnstructured(t, c, httpRouteGVK, "billing", "invoices")
	if after.GetResourceVersion() != routeVersion || len(after.GetOwnerReferences()) != 0 || len(after.GetLabels()) != 0 {
		t.Fatalf("HTTPRoute was modified: %+v", after.Object["metadata"])
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "HypergateFilterUnavailable") {
			t.Fatalf("unexpected event %q", ev)
		}
	default:
		t.Fatal("no Event recorded for the invalid HTTPRoute")
	}

	if cnd := condition(t, c, ConditionEnvoyGatewayPolicies); cnd.Status != metav1.ConditionTrue {
		t.Fatalf("EnvoyGatewayPolicies = %+v", cnd)
	}
	if cnd := condition(t, c, ConditionCiliumFilters); cnd.Status != metav1.ConditionTrue {
		t.Fatalf("CiliumFilters = %+v", cnd)
	}

	// Removing entries prunes the generated objects and narrows the grant.
	var current hyperv1alpha1.HyperConfig
	_ = c.Get(context.Background(), client.ObjectKey{Name: "main"}, &current)
	current.Spec.ExtProc.Gateways = nil
	current.Spec.ExtProc.Namespaces = []string{"shop"}
	if err := c.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	reconcileExtProc(t, r)
	if _, ok := getUnstructured(t, c, envoyExtensionPolicyGVK, "edge", "hypergate-public"); ok {
		t.Fatal("policy for an unlisted Gateway was not deleted")
	}
	if _, ok := getUnstructured(t, c, ciliumExtProcFilterGVK, "payments", CiliumFilterName); ok {
		t.Fatal("filter in an unlisted namespace was not deleted")
	}
	g, _ = getUnstructured(t, c, referenceGrantGVK, "hyper-system", extProcReferenceGrantName)
	if from, _, _ := unstructured.NestedSlice(g.Object, "spec", "from"); len(from) != 1 {
		t.Fatalf("ReferenceGrant not narrowed: %v", from)
	}
}

func TestExtProc_FailOpenTimeoutAndMissingGateway(t *testing.T) {
	hc := extProcConfig(&hyperv1alpha1.ExtProcSpec{
		Gateways:       []hyperv1alpha1.GatewayRef{{Namespace: "edge", Name: "not-yet"}},
		FailureMode:    hyperv1alpha1.ExtProcFailOpen,
		MessageTimeout: "5s",
	})
	r, c, _ := extProcEnv(t, optionalKinds, hc)
	reconcileExtProc(t, r)

	p, ok := getUnstructured(t, c, envoyExtensionPolicyGVK, "edge", "hypergate-not-yet")
	if !ok {
		t.Fatal("policy must be created even before the Gateway exists")
	}
	ext, _, _ := unstructured.NestedSlice(p.Object, "spec", "extProc")
	if e := ext[0].(map[string]any); e["failOpen"] != true || e["messageTimeout"] != "5s" {
		t.Fatalf("settings not applied: %v", e)
	}
	if cnd := condition(t, c, ConditionEnvoyGatewayPolicies); cnd.Reason != "GatewayNotFound" {
		t.Fatalf("condition = %+v", cnd)
	}
}

func TestExtProc_WithoutOptionalCRDs(t *testing.T) {
	hc := extProcConfig(&hyperv1alpha1.ExtProcSpec{
		Gateways:   []hyperv1alpha1.GatewayRef{{Namespace: "edge", Name: "public"}},
		Namespaces: []string{"shop"},
	})
	r, c, _ := extProcEnv(t, nil, hc)
	reconcileExtProc(t, r)

	for condType, reason := range map[string]string{
		ConditionEnvoyGatewayPolicies: "CRDNotInstalled",
		ConditionCiliumFilters:        "CRDNotInstalled",
		ConditionHTTPRoutes:           "CRDNotInstalled",
	} {
		if cnd := condition(t, c, condType); cnd.Status != metav1.ConditionFalse || cnd.Reason != reason {
			t.Errorf("%s = %+v", condType, cnd)
		}
	}
	if cnd := condition(t, c, ConditionCiliumFilters); !strings.Contains(cnd.Message, "46479") {
		t.Errorf("Cilium condition should point at the upstream PR: %q", cnd.Message)
	}
}

func TestExtProc_NeverTakesOverForeignObjects(t *testing.T) {
	hc := extProcConfig(&hyperv1alpha1.ExtProcSpec{Gateways: []hyperv1alpha1.GatewayRef{{Namespace: "edge", Name: "public"}}})
	foreign := unstructuredObj(envoyExtensionPolicyGVK, "edge", "hypergate-public", map[string]any{"targetRefs": []any{}})
	r, c, _ := extProcEnv(t, optionalKinds, hc, foreign)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}})
	if err == nil || !strings.Contains(err.Error(), "not managed by HyperConfig") {
		t.Fatalf("expected refusal, got %v", err)
	}
	p, _ := getUnstructured(t, c, envoyExtensionPolicyGVK, "edge", "hypergate-public")
	if refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs"); len(refs) != 0 {
		t.Fatal("foreign object was overwritten")
	}
}

func TestExtProc_NotConfigured(t *testing.T) {
	r, c, _ := extProcEnv(t, optionalKinds, extProcConfig(nil))
	reconcileExtProc(t, r)
	if cnd := condition(t, c, ConditionEnvoyGatewayPolicies); cnd.Reason != "NotConfigured" {
		t.Fatalf("condition = %+v", cnd)
	}
	if _, ok := getUnstructured(t, c, referenceGrantGVK, "hyper-system", extProcReferenceGrantName); ok {
		t.Fatal("ReferenceGrant created with nothing to grant")
	}
}

// Both CRD copies must describe extProc and status.conditions.
func TestHyperConfigCRDHasExtProc(t *testing.T) {
	for _, path := range []string{"../../../charts/hyper-operator/crds/hyperconfig_crd.yaml", "../../deploy/CRDs/hyperconfig_crd.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		schema := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)
		spec := schema["spec"].(map[string]any)["properties"].(map[string]any)
		ext, ok := spec["extProc"].(map[string]any)
		if !ok {
			t.Fatalf("%s: spec.extProc missing", path)
		}
		for _, f := range []string{"gateways", "namespaces", "failureMode", "messageTimeout"} {
			if _, ok := ext["properties"].(map[string]any)[f]; !ok {
				t.Errorf("%s: spec.extProc.%s missing", path, f)
			}
		}
		status := schema["status"].(map[string]any)["properties"].(map[string]any)
		if _, ok := status["conditions"]; !ok {
			t.Errorf("%s: status.conditions missing", path)
		}
	}
}
