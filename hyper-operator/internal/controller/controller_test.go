package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := hyperv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func hyperConfig(name, ns string, created time.Time) hyperv1alpha1.HyperConfig {
	return hyperv1alpha1.HyperConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       hyperv1alpha1.HyperConfigSpec{TargetNamespace: ns, RedisServiceRef: "main"},
	}
}

func TestActiveHyperConfigs_OldestWinsPerNamespace(t *testing.T) {
	now := time.Now()
	items := []hyperv1alpha1.HyperConfig{
		hyperConfig("newer", "gw", now),
		hyperConfig("older", "gw", now.Add(-time.Hour)),
		hyperConfig("other", "edge", now),
	}
	winners, conflicts := activeHyperConfigs(items)
	if winners["gw"].Name != "older" || winners["edge"].Name != "other" {
		t.Fatalf("unexpected winners: %v", winners)
	}
	if conflicts["newer"] != "older" || len(conflicts) != 1 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
}

func TestHyperConfigReconcile_InjectsClusterScopedSidecars(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	eaf := &hyperv1alpha1.ExternalAuthFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "oauth"},
		Spec: hyperv1alpha1.ExternalAuthFilterSpec{
			Container: hyperv1alpha1.SidecarContainerSpec{
				Image: "quay.io/oauth2-proxy/oauth2-proxy:v7",
				Args:  []string{"--http-address={socket_path}"},
			},
		},
	}
	jwt := &hyperv1alpha1.JwtAuthFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "jwt"},
		Spec:       hyperv1alpha1.JwtAuthFilterSpec{LocalSecretRef: &hyperv1alpha1.SecretKeyRef{Name: "jwt-secret", Key: "hmac"}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&hc, eaf, jwt).
		WithStatusSubresource(&hyperv1alpha1.HyperConfig{}).
		Build()
	r := &HyperConfigReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}}); err != nil {
		t.Fatal(err)
	}

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: "hyper-engine"}, &ds); err != nil {
		t.Fatal(err)
	}
	containers := ds.Spec.Template.Spec.Containers
	if len(containers) != 2 || containers[1].Name != "ext-auth-oauth" {
		t.Fatalf("external auth sidecar not injected: %v", names(containers))
	}
	if got := containers[1].Args[0]; got != "--http-address=/var/run/hypergate/ext-auth-oauth.sock" {
		t.Fatalf("socket path not substituted: %q", got)
	}

	engine := containers[0]
	if engine.ReadinessProbe == nil || engine.LivenessProbe == nil {
		t.Fatal("engine container has no probes")
	}
	if engine.SecurityContext == nil || engine.SecurityContext.ReadOnlyRootFilesystem == nil || !*engine.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("engine container is not hardened")
	}
	if engine.Resources.Requests.Cpu().IsZero() {
		t.Fatal("engine container has no resource requests")
	}
	if strings.HasSuffix(engine.Image, ":latest") {
		t.Fatalf("engine image must be pinned, got %s", engine.Image)
	}
	mounted := false
	for _, m := range engine.VolumeMounts {
		if m.MountPath == "/etc/hypergate/secrets/jwt-jwt/jwt-secret" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("jwt secret not mounted into engine: %v", engine.VolumeMounts)
	}

	var updated hyperv1alpha1.HyperConfig
	_ = c.Get(context.Background(), client.ObjectKey{Name: "main"}, &updated)
	if updated.Status.State != hyperConfigStateReady {
		t.Fatalf("expected Ready status, got %q", updated.Status.State)
	}
}

func TestHyperConfigReconcile_ConflictingConfigIsSkipped(t *testing.T) {
	scheme := testScheme(t)
	older := hyperConfig("older", "gw", time.Now().Add(-time.Hour))
	newer := hyperConfig("newer", "gw", time.Now())
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&older, &newer).
		WithStatusSubresource(&hyperv1alpha1.HyperConfig{}).
		Build()
	r := &HyperConfigReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "newer"}}); err != nil {
		t.Fatal(err)
	}
	var got hyperv1alpha1.HyperConfig
	_ = c.Get(context.Background(), client.ObjectKey{Name: "newer"}, &got)
	if got.Status.State != hyperConfigStateConflict {
		t.Fatalf("expected Conflict, got %q", got.Status.State)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "gw", Name: "hyper-engine"}, &ds); err == nil {
		t.Fatal("conflicting HyperConfig must not create a DaemonSet")
	}
}

func TestCompiler_DegradedAndMissingChainsFailClosed(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "does-not-exist"
	broken := &hyperv1alpha1.HyperChain{
		ObjectMeta: metav1.ObjectMeta{Name: "auth"},
		Spec: hyperv1alpha1.HyperChainSpec{Filters: []hyperv1alpha1.FilterReference{
			{Kind: "ApiKeyFilter", Name: "missing-filter"},
		}},
	}
	route := &hyperv1alpha1.HyperRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: hyperv1alpha1.HyperRouteSpec{
			TargetPolicy: "auth",
			Matches:      []hyperv1alpha1.MatchRule{{PathPrefix: "/admin"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&hc, broken, route).
		WithStatusSubresource(&hyperv1alpha1.HyperChain{}).
		Build()
	r := &HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "admin"}}); err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: engineConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseBytes([]byte(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatalf("compiled config does not parse: %v", err)
	}
	if len(cfg.Router.Routes) != 1 || cfg.Router.Routes[0].TargetChain != "auth" {
		t.Fatalf("route to degraded chain must be kept, got %+v", cfg.Router.Routes)
	}
	for _, name := range []string{"auth", "does-not-exist"} {
		chain := cfg.Chains[name]
		if len(chain) != 1 || chain[0].Type != "deny" {
			t.Fatalf("chain %q should be a fail-closed deny chain, got %+v", name, chain)
		}
	}
	if cfg.Server.HealthAddress != ":9003" {
		t.Fatalf("health address not set: %q", cfg.Server.HealthAddress)
	}
}

func names(cs []corev1.Container) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func TestCompiler_CorrelationPropagateFalseReachesEngine(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "c"
	corr := &hyperv1alpha1.CorrelationIdFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "rid"},
		Spec:       hyperv1alpha1.CorrelationIdFilterSpec{PropagateToUpstream: true, PropagateToDownstream: false},
	}
	chain := &hyperv1alpha1.HyperChain{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec:       hyperv1alpha1.HyperChainSpec{Filters: []hyperv1alpha1.FilterReference{{Kind: "CorrelationIdFilter", Name: "rid"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hc, corr, chain).
		WithStatusSubresource(&hyperv1alpha1.HyperChain{}).Build()
	r := &HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: engineConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseBytes([]byte(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.Chains["c"][0].Options["propagate_to_downstream"]; !ok || v != false {
		t.Fatalf("explicit false must be compiled, got %v (present=%v)", v, ok)
	}
}
