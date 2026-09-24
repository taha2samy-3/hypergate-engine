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

func TestHeaderKeyNameIsCaseInsensitive(t *testing.T) {
	cfg := config.APIKeyFilterConfig{KeyNames: []string{"X-API-Key"}, HashAlgorithm: "none", ValueFormat: "plain"}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatal(err)
	}
	client := redistest.New()
	client.Set("apikey:k1", "acme")
	f := NewAPIKeyFilter("api_key", cfg, client)

	ctx := &engine.RequestContext{
		Path:             "/",
		Headers:          map[string]string{"x-api-key": "k1"},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	if err := f.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Blocked {
		t.Fatalf("mixed-case key_names must match the lower-cased header: %s", ctx.ResponseBody)
	}
}

func TestHashFormatWithoutFieldsChecksExistence(t *testing.T) {
	cfg := config.APIKeyFilterConfig{HashAlgorithm: "none", ValueFormat: "hash"}
	if err := cfg.ApplyDefaults(); err != nil {
		t.Fatal(err)
	}
	client := redistest.New()
	client.Set("apikey:known", "x")
	f := NewAPIKeyFilter("api_key", cfg, client)

	for key, wantBlocked := range map[string]bool{"known": false, "unknown": true} {
		ctx := &engine.RequestContext{
			Headers:          map[string]string{"x-api-key": key},
			UpstreamShadow:   map[string]string{},
			DownstreamShadow: map[string]string{},
		}
		if err := f.Execute(ctx); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if ctx.Blocked != wantBlocked {
			t.Fatalf("%s: blocked=%v status=%d", key, ctx.Blocked, ctx.ResponseStatus)
		}
	}
}

func TestStatusCheckRejectedForPlainFormat(t *testing.T) {
	cfg := config.APIKeyFilterConfig{ValueFormat: "plain", StatusCheck: config.APIKeyStatusCheck{Enabled: true, FieldName: "s"}}
	if err := cfg.ApplyDefaults(); err == nil {
		t.Fatal("status_check with plain format must be rejected")
	}
}
