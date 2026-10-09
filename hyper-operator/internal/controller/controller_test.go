package controller

import (
	"context"
	"os"
	"path/filepath"
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
	"github.com/taha2samy/hypergate/internal/filters"
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

func TestCompiler_CorsFilterCompilesToWorkingEngineFilter(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "web"
	corsF := &hyperv1alpha1.CorsFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "browser"},
		Spec: hyperv1alpha1.CorsFilterSpec{
			AllowOrigins:     []string{"https://app.example.com"},
			AllowMethods:     []string{"GET", "POST"},
			AllowCredentials: true,
			MaxAge:           600,
		},
	}
	chain := &hyperv1alpha1.HyperChain{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec:       hyperv1alpha1.HyperChainSpec{Filters: []hyperv1alpha1.FilterReference{{Kind: "CorsFilter", Name: "browser"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hc, corsF, chain).
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
	fc := cfg.Chains["web"][0]
	if fc.Type != "cors" {
		t.Fatalf("expected cors filter, got %q", fc.Type)
	}
	if _, err := filters.CreateFilter(fc.Type, fc.Options, nil); err != nil {
		t.Fatalf("compiled options do not build an engine filter: %v", err)
	}
	if fc.Options["max_age"] != 600 || fc.Options["allow_credentials"] != true {
		t.Fatalf("options not compiled with engine keys: %v", fc.Options)
	}
}

func compiledEngineConfig(t *testing.T, c client.Client) *config.Config {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: engineConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseBytes([]byte(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatalf("compiled config does not parse: %v", err)
	}
	return cfg
}

func TestCompiler_TrafficSelectorsAndDefaultChains(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "public"
	hc.Spec.DefaultChains = &hyperv1alpha1.DefaultChains{NorthSouth: "public", EastWest: "missing-internal"}
	public := &hyperv1alpha1.HyperChain{ObjectMeta: metav1.ObjectMeta{Name: "public"}}
	good := &hyperv1alpha1.HyperRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "partner"},
		Spec: hyperv1alpha1.HyperRouteSpec{
			Priority:     10,
			TargetPolicy: "public",
			Matches: []hyperv1alpha1.MatchRule{{
				Traffic:      hyperv1alpha1.TrafficNorthSouth,
				Sources:      []string{"cidr:203.0.113.0/24"},
				Destinations: []string{"host:api.example.com"},
			}},
		},
	}
	bad := &hyperv1alpha1.HyperRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec: hyperv1alpha1.HyperRouteSpec{
			Priority:     20,
			TargetPolicy: "public",
			Matches:      []hyperv1alpha1.MatchRule{{Sources: []string{"host:api.example.com"}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&hc, public, good, bad).
		WithStatusSubresource(&hyperv1alpha1.HyperChain{}, &hyperv1alpha1.HyperRoute{}).
		Build()
	r := &HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	cfg := compiledEngineConfig(t, c)
	if len(cfg.Router.Routes) != 1 || cfg.Router.Routes[0].Name != "partner" {
		t.Fatalf("only the valid route must be compiled, got %+v", cfg.Router.Routes)
	}
	m := cfg.Router.Routes[0].Matches[0]
	if m.Traffic != "north_south" || m.CompiledSources.String() != "cidr:203.0.113.0/24" || m.CompiledDestinations.String() != "host:api.example.com" {
		t.Fatalf("match not compiled: %+v", m)
	}
	if cfg.Router.DefaultChains.NorthSouth != "public" || cfg.Router.DefaultChains.EastWest != "missing-internal" {
		t.Fatalf("default chains not compiled: %+v", cfg.Router.DefaultChains)
	}
	if chain := cfg.Chains["missing-internal"]; len(chain) != 1 || chain[0].Type != "deny" {
		t.Fatalf("missing default chain must fail closed, got %+v", chain)
	}

	for name, want := range map[string]string{"partner": hyperv1alpha1.HyperRouteStateReady, "broken": hyperv1alpha1.HyperRouteStateInvalid} {
		var hr hyperv1alpha1.HyperRoute
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &hr); err != nil {
			t.Fatal(err)
		}
		if hr.Status.State != want {
			t.Errorf("%s: state %q, want %q (message %q)", name, hr.Status.State, want, hr.Status.Message)
		}
	}
}

func TestCompiler_IdentityEnablesWorkloadSelectors(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "c"
	hc.Spec.UnknownSource = hyperv1alpha1.UnknownSourceDefault
	chain := &hyperv1alpha1.HyperChain{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	route := &hyperv1alpha1.HyperRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-to-ledger"},
		Spec: hyperv1alpha1.HyperRouteSpec{
			TargetPolicy: "c",
			Matches: []hyperv1alpha1.MatchRule{{
				Traffic:      hyperv1alpha1.TrafficEastWest,
				Sources:      []string{"service:shop/checkout"},
				Destinations: []string{"service:payments/ledger"},
			}},
		},
	}
	build := func(identity IdentitySettings) (*config.Config, *hyperv1alpha1.HyperRoute) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hc.DeepCopy(), chain.DeepCopy(), route.DeepCopy()).
			WithStatusSubresource(&hyperv1alpha1.HyperChain{}, &hyperv1alpha1.HyperRoute{}).Build()
		r := &HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme, Identity: identity}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		var hr hyperv1alpha1.HyperRoute
		_ = c.Get(context.Background(), client.ObjectKey{Name: "checkout-to-ledger"}, &hr)
		return compiledEngineConfig(t, c), &hr
	}

	// Without the identity server the route is rejected.
	cfg, hr := build(IdentitySettings{})
	if len(cfg.Router.Routes) != 0 || hr.Status.State != hyperv1alpha1.HyperRouteStateInvalid || !strings.Contains(hr.Status.Message, "identity map") {
		t.Fatalf("workload route without identity: routes=%d status=%+v", len(cfg.Router.Routes), hr.Status)
	}
	if cfg.Identity.Enabled {
		t.Fatal("identity block compiled without the identity server")
	}

	// With it, the route compiles and engines get the identity block.
	cfg, hr = build(IdentitySettings{Enabled: true, Address: "hyper-operator-identity.hyper-system.svc:9444",
		ServerName: "hyper-operator-identity.hyper-system.svc", CAFile: "/tmp/ca.crt"})
	if len(cfg.Router.Routes) != 1 || hr.Status.State != hyperv1alpha1.HyperRouteStateReady {
		t.Fatalf("workload route with identity: routes=%d status=%+v", len(cfg.Router.Routes), hr.Status)
	}
	id := cfg.Identity
	if !id.Enabled || id.Address != "hyper-operator-identity.hyper-system.svc:9444" || id.CAFile != "/etc/hypergate/identity/ca.crt" ||
		id.TokenFile != "/var/run/secrets/hypergate/identity/token" || id.ServerName != "hyper-operator-identity.hyper-system.svc" {
		t.Fatalf("identity block = %+v", id)
	}
	if cfg.Router.UnknownSource != config.UnknownSourceDefault {
		t.Fatalf("unknown_source = %q", cfg.Router.UnknownSource)
	}
}

func TestHyperConfigReconcile_IdentityVolumesAndCA(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hc).WithStatusSubresource(&hyperv1alpha1.HyperConfig{}).Build()
	r := &HyperConfigReconciler{Client: c, Scheme: scheme, Identity: IdentitySettings{Enabled: true, CAFile: caFile}}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("identity CA must be refreshed periodically")
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: identityCAConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["ca.crt"], "BEGIN CERTIFICATE") {
		t.Fatalf("CA not copied: %v", cm.Data)
	}

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: "hyper-engine"}, &ds); err != nil {
		t.Fatal(err)
	}
	var token *corev1.ServiceAccountTokenProjection
	var caVolume bool
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken != nil {
			token = v.Projected.Sources[0].ServiceAccountToken
		}
		if v.ConfigMap != nil && v.ConfigMap.Name == identityCAConfigMap {
			caVolume = true
		}
	}
	if token == nil || token.Audience != "hypergate-identity" || !caVolume {
		t.Fatalf("identity volumes missing: token=%+v ca=%v", token, caVolume)
	}
	mounts := map[string]bool{}
	for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[m.MountPath] = true
	}
	if !mounts["/var/run/secrets/hypergate/identity"] || !mounts["/etc/hypergate/identity"] {
		t.Fatalf("identity mounts missing: %v", mounts)
	}
}

func TestHyperConfigReconcile_EngineRolloutSettings(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hc).WithStatusSubresource(&hyperv1alpha1.HyperConfig{}).Build()
	r := &HyperConfigReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}}); err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: "hyper-engine"}, &ds); err != nil {
		t.Fatal(err)
	}
	ru := ds.Spec.UpdateStrategy.RollingUpdate
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType || ru == nil ||
		ru.MaxSurge.IntValue() != 1 || ru.MaxUnavailable.IntValue() != 0 {
		t.Fatalf("update strategy = %+v", ds.Spec.UpdateStrategy)
	}
	if g := ds.Spec.Template.Spec.TerminationGracePeriodSeconds; g == nil || *g != engineTerminationGraceSeconds {
		t.Fatalf("terminationGracePeriodSeconds = %v", g)
	}
}

func TestEngineMetricsWiring(t *testing.T) {
	scheme := testScheme(t)
	hc := hyperConfig("main", "hyper-system", time.Now())
	hc.Spec.DefaultChain = "web"
	corsF := &hyperv1alpha1.CorsFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "browser"},
		Spec:       hyperv1alpha1.CorsFilterSpec{AllowOrigins: []string{"https://app.example.com"}},
	}
	chain := &hyperv1alpha1.HyperChain{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec:       hyperv1alpha1.HyperChainSpec{Filters: []hyperv1alpha1.FilterReference{{Kind: "CorsFilter", Name: "browser"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hc, corsF, chain).
		WithStatusSubresource(&hyperv1alpha1.HyperChain{}, &hyperv1alpha1.HyperConfig{}).Build()
	if _, err := (&HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme}).Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if name := compiledEngineConfig(t, c).Chains["web"][0].Name; name != "CorsFilter/browser" {
		t.Fatalf("filter name = %q, want CorsFilter/browser", name)
	}

	if _, err := (&HyperConfigReconciler{Client: c, Scheme: scheme}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}}); err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: "hyper-engine"}, &ds); err != nil {
		t.Fatal(err)
	}
	a := ds.Spec.Template.Annotations
	if a["prometheus.io/scrape"] != "true" || a["prometheus.io/port"] != "9003" || a["prometheus.io/path"] != "/metrics" {
		t.Fatalf("scrape annotations = %v", a)
	}
}
