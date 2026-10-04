package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
)

func webhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := hyperv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func route(target string, m ...hyperv1alpha1.MatchRule) *hyperv1alpha1.HyperRoute {
	return &hyperv1alpha1.HyperRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r"},
		Spec:       hyperv1alpha1.HyperRouteSpec{TargetPolicy: target, Matches: m},
	}
}

func TestHyperRouteValidator(t *testing.T) {
	chain := &hyperv1alpha1.HyperChain{ObjectMeta: metav1.ObjectMeta{Name: "partner"}}
	v := &HyperRouteValidator{Client: fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(chain).Build()}
	ctx := context.Background()

	valid := route("partner", hyperv1alpha1.MatchRule{
		Traffic:      hyperv1alpha1.TrafficNorthSouth,
		Sources:      []string{"cidr:203.0.113.0/24"},
		Destinations: []string{"host:*.example.com"},
	})
	if w, err := v.ValidateCreate(ctx, valid); err != nil || len(w) != 0 {
		t.Fatalf("valid route rejected: %v %v", w, err)
	}

	invalid := route("partner", hyperv1alpha1.MatchRule{Sources: []string{"host:api.example.com"}})
	if _, err := v.ValidateUpdate(ctx, valid, invalid); err == nil || !strings.Contains(err.Error(), "cannot be used as a source") {
		t.Fatalf("wrong-side selector accepted: %v", err)
	}

	workload := route("partner", hyperv1alpha1.MatchRule{Sources: []string{"service:shop/checkout"}})
	if _, err := v.ValidateCreate(ctx, workload); err == nil || !strings.Contains(err.Error(), "identity map") {
		t.Fatalf("workload selector accepted before identity support: %v", err)
	}

	missing := route("does-not-exist", hyperv1alpha1.MatchRule{PathPrefix: "/"})
	w, err := v.ValidateCreate(ctx, missing)
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "503") {
		t.Fatalf("missing chain should be a warning, got %v %v", w, err)
	}
}

func TestHyperChainDeleteProtectsDefaultChains(t *testing.T) {
	hc := &hyperv1alpha1.HyperConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "main"},
		Spec: hyperv1alpha1.HyperConfigSpec{
			DefaultChains: &hyperv1alpha1.DefaultChains{EastWest: "internal"},
		},
	}
	v := &HyperChainValidator{Client: fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(hc).Build()}

	_, err := v.ValidateDelete(context.Background(), &hyperv1alpha1.HyperChain{ObjectMeta: metav1.ObjectMeta{Name: "internal"}})
	if err == nil || !strings.Contains(err.Error(), "defaultChains.eastWest") {
		t.Fatalf("expected delete to be refused, got %v", err)
	}
	if _, err := v.ValidateDelete(context.Background(), &hyperv1alpha1.HyperChain{ObjectMeta: metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatalf("unreferenced chain delete refused: %v", err)
	}
}
