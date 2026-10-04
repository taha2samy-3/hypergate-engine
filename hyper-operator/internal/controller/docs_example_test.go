package controller

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/filters"
)

const filterChainsPage = "../../../website/docs/concepts/filter-chains.md"

func docsBlock(t *testing.T, title string) []byte {
	t.Helper()
	data, err := os.ReadFile(filterChainsPage)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "```yaml title=\""+title+"\"\n")
	if !ok {
		t.Fatalf("no code block titled %q", title)
	}
	block, _, _ := strings.Cut(rest, "\n```")
	return []byte(block)
}

// The CRD example on the filter chains page must compile to the same chains and
// routes as the engine example on that page.
func TestDocsFilterChainCRDsCompileLikeEngineExample(t *testing.T) {
	scheme := testScheme(t)
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()

	var objs []client.Object
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(docsBlock(t, "crds.yaml"))))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj, _, err := decoder.Decode(doc, nil, nil)
		if err != nil {
			t.Fatalf("decode: %v\n%s", err, doc)
		}
		objs = append(objs, obj.(client.Object))
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&hyperv1alpha1.HyperChain{}, &hyperv1alpha1.HyperRoute{}).Build()
	r := &HyperChainMasterCompilerReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "hyper-system", Name: engineConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	got, err := config.ParseBytes([]byte(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatalf("compiled config does not parse: %v", err)
	}
	want, err := config.ParseBytes(docsBlock(t, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for name, chain := range want.Chains {
		var gotTypes, wantTypes []string
		for _, f := range got.Chains[name] {
			gotTypes = append(gotTypes, f.Type)
		}
		for _, f := range chain {
			wantTypes = append(wantTypes, f.Type)
		}
		if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
			t.Errorf("chain %s: operator compiles %v, engine example has %v", name, gotTypes, wantTypes)
		}
		for _, f := range got.Chains[name] {
			if f.Type == "deny" && f.Options["status_code"] == degradedChainStatusCode {
				t.Errorf("chain %s compiled as a degraded chain", name)
			}
			switch f.Type {
			case "cors", "correlation_id", "header_modifier", "deny":
				if _, err := filters.CreateFilter(f.Type, f.Options, nil); err != nil {
					t.Errorf("chain %s: compiled %s options rejected: %v", name, f.Type, err)
				}
			}
		}
	}
	if len(got.Router.Routes) != len(want.Router.Routes) || got.Router.DefaultChain != want.Router.DefaultChain {
		t.Fatalf("router: operator %+v, engine example %+v", got.Router, want.Router)
	}
	for i := range want.Router.Routes {
		g, w := got.Router.Routes[i], want.Router.Routes[i]
		if g.Name != w.Name || g.TargetChain != w.TargetChain || g.Matches[0].PathPrefix != w.Matches[0].PathPrefix {
			t.Errorf("route %d: operator %+v, engine example %+v", i, g, w)
		}
	}

	for _, name := range []string{"web-api", "admin", "public"} {
		var hc hyperv1alpha1.HyperChain
		_ = c.Get(context.Background(), client.ObjectKey{Name: name}, &hc)
		if hc.Status.State != "Ready" {
			t.Errorf("HyperChain %s: %s %s", name, hc.Status.State, hc.Status.Message)
		}
	}
}
