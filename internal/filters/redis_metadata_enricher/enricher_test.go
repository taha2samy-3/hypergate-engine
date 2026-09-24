package redis_metadata_enricher

import (
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/redis/redistest"
)

func TestInvalidRegexIsAnErrorNotAPanic(t *testing.T) {
	_, err := NewRedisMetadataEnricherFilter("e", config.RedisMetadataEnricherConfig{
		KeyPattern: "k:{v}",
		Variables:  map[string]config.Variable{"v": {Source: "{path}", RegexPattern: "("}},
	}, redistest.New())
	if err == nil {
		t.Fatal("invalid regex must be rejected")
	}
}

func TestEnrichesFromRedisJSON(t *testing.T) {
	client := redistest.New()
	client.Set("user_meta:alice", `{"profile":{"tier":"gold"}}`)
	f, err := NewRedisMetadataEnricherFilter("e", config.RedisMetadataEnricherConfig{
		KeyPattern: "user_meta:{user}",
		Variables:  map[string]config.Variable{"user": {Source: "{header:X-User-ID}", Default: "anonymous"}},
		OutputMappings: []config.OutputMappingSpec{
			{JSONPath: "profile.tier", TargetHeader: "X-User-Tier"},
		},
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &engine.RequestContext{
		Headers:          map[string]string{"x-user-id": "Alice"},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	if err := f.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ctx.GetHeader("x-user-tier"); got != "gold" {
		t.Fatalf("expected gold (variables are lower-cased), got %q", got)
	}
}
