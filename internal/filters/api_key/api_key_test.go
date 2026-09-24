package api_key

import (
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/redis/redistest"
)

func TestHideCredentialsStripsQueryKeyUpstream(t *testing.T) {
	cfg := config.APIKeyFilterConfig{
		KeyNames:        []string{"api_key"},
		KeyInQuery:      true,
		HideCredentials: true,
		HashAlgorithm:   "none",
		ValueFormat:     "plain",
		OutputMappings:  []config.APIKeyOutputMapping{{TargetHeader: "x-tenant"}},
	}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatal(err)
	}
	client := redistest.New()
	client.Set("apikey:secret123", "acme")
	f := NewAPIKeyFilter("api_key", cfg, client)

	ctx := &engine.RequestContext{
		Path:             "/orders?api_key=secret123&page=2",
		Headers:          map[string]string{},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	if err := f.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Blocked {
		t.Fatalf("valid key blocked: %s", ctx.ResponseBody)
	}
	var path string
	for _, h := range ctx.HeadersToAdd {
		if h.Key == ":path" {
			path = h.Value
		}
	}
	if path != "/orders?page=2" {
		t.Fatalf("expected :path mutation without the key, got %q", path)
	}
}
