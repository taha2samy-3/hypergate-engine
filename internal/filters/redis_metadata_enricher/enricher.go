package redis_metadata_enricher

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
	"github.com/taha/myprog/internal/redis"
)

type RedisMetadataEnricherFilter struct {
	name          string
	redisService  string
	keyPattern    string
	client        redis.Client
	localCache    *freecache.Cache // Embedded L1 cache to bypass Redis completely on hits
	cacheTTL      time.Duration    // Thread-safe parsed cache expiration duration
	variables     map[string]VariableRule
	outputMapping []config.OutputMappingSpec
}

type VariableRule struct {
	Source        string
	DefaultValue  string
	CompiledRegex *regexp.Regexp
	JSONPath      string
}

// NewRedisMetadataEnricherFilter instantiates and compiles the advanced metadata enrichment filter.
// It parses the configurable cache timeout string into a time.Duration at boot-time.
// NewRedisMetadataEnricherFilter instantiates and compiles the advanced metadata enrichment filter.
// It dynamically evaluates the cache size and TTL from the configuration, falling back to safe defaults.
func NewRedisMetadataEnricherFilter(name string, cfg config.RedisMetadataEnricherConfig, client redis.Client) *RedisMetadataEnricherFilter {
	// 1. Calculate dynamic L1 Cache Size (Default is 10MB if omitted or zero)
	cacheSize := 10 * 1024 * 1024 // 10MB in bytes
	if cfg.CacheSizeMB > 0 {
		cacheSize = cfg.CacheSizeMB * 1024 * 1024 // Convert Operator's MB input to Bytes
	}
	localCache := freecache.NewCache(cacheSize)

	// 2. Calculate dynamic L1 Cache TTL (Default is 10 seconds if omitted or invalid)
	cacheTTL := 10 * time.Second
	if cfg.CacheTimeout != "" {
		if parsed, err := time.ParseDuration(cfg.CacheTimeout); err == nil {
			cacheTTL = parsed
		}
	}

	compiledVars := make(map[string]VariableRule, len(cfg.Variables))
	for varName, v := range cfg.Variables {
		var re *regexp.Regexp
		if v.RegexPattern != "" {
			re = regexp.MustCompile(v.RegexPattern)
		}
		compiledVars[varName] = VariableRule{
			Source:        v.Source,
			DefaultValue:  v.Default,
			CompiledRegex: re,
			JSONPath:      v.JSONPath,
		}
	}

	return &RedisMetadataEnricherFilter{
		name:          name,
		redisService:  cfg.RedisService,
		keyPattern:    cfg.KeyPattern,
		client:        client,
		localCache:    localCache,
		cacheTTL:      cacheTTL, // Pass the parsed dynamic TTL
		variables:     compiledVars,
		outputMapping: cfg.OutputMappings,
	}
}

// Execute performs dynamic variable resolution, L1 cache lookup, Redis fetching, and JSON path header injection.
// It implements the engine.Filter interface cleanly.
func (f *RedisMetadataEnricherFilter) Execute(ctx *engine.RequestContext) error {
	// Defensive nil check: if the stream context is uninitialized, fall back to a
	// safe background context to prevent a nil-pointer panic in PipeDo.
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	// Map of resolved variables during this request lifecycle
	resolvedVars := make(map[string]string, len(f.variables))

	// 1. Resolve all input variables, apply JSON Path / Regex, and fallback to defaults
	for name, rule := range f.variables {
		val := f.resolveSourceValue(ctx, rule.Source)

		// If source is a JSON string, extract value via gjson (Zero-Allocation JSON path)
		if rule.JSONPath != "" && val != "" {
			res := gjson.Get(val, rule.JSONPath)
			if res.Exists() {
				val = res.String()
			} else {
				val = ""
			}
		}

		// If regex pattern is defined, perform extraction
		if rule.CompiledRegex != nil && val != "" {
			matches := rule.CompiledRegex.FindStringSubmatch(val)
			if len(matches) > 1 {
				val = matches[1] // Extract the first capture group
			} else {
				val = ""
			}
		}

		// Fallback to default value if extraction yielded an empty string
		if val == "" {
			val = rule.DefaultValue
		}

		resolvedVars[name] = strings.ToLower(val)
	}

	// 2. Build the dynamic Redis Key using a stack-allocated buffer (Zero-Allocation)
	var batchBuf [512]byte
	keyBytes := f.compileRedisKey(batchBuf[:0], resolvedVars)
	keyStr := unsafe.String(unsafe.SliceData(keyBytes), len(keyBytes))

	var jsonStr string
	cacheHit := false

	// 3. Query L1 Local Cache (Bypass Redis entirely on hits)
	if f.localCache != nil {
		if cachedVal, err := f.localCache.Get(keyBytes); err == nil {
			jsonStr = unsafe.String(unsafe.SliceData(cachedVal), len(cachedVal))
			cacheHit = true
			mylogger.Debug("L1 metadata cache hit", zap.String("key", keyStr))
		}
	}

	// 4. Fallback: Query Redis if L1 cache missed
	if !cacheHit {
		// To ensure context timeout awareness and protect from blocking indefinitely,
		// we execute the GET command inside a pipeline via PipeDo with the request's context.
		var reply string
		var p redis.Pipeline
		p = f.client.PipeAppend(p, &reply, "GET", keyStr)

		err := f.client.PipeDo(reqCtx, p) // Timeout-aware & cancellable; uses safe reqCtx (never nil).
		if err != nil {
			mylogger.Error("Redis metadata fetch failed", zap.Error(err), zap.String("key", keyStr))
			return nil // Fail-open: Proceed with the request even if metadata resolution fails
		}

		jsonStr = reply

		// Write to L1 cache to protect Redis from redundant queries in subsequent hot paths
		if f.localCache != nil && jsonStr != "" {
			_ = f.localCache.Set(keyBytes, unsafe.Slice(unsafe.StringData(jsonStr), len(jsonStr)), int(f.cacheTTL.Seconds()))
		}
	}

	// 5. Output Mapping: Parse the JSON payload and inject variables as Upstream Headers
	if jsonStr != "" {
		for _, mapping := range f.outputMapping {
			var valToInject string

			// If jsonPath is empty, inject the entire raw JSON string
			if mapping.JSONPath == "" {
				valToInject = jsonStr
			} else {
				// Extract specific nested field via gjson dot-notation (Zero-Allocation)
				res := gjson.Get(jsonStr, mapping.JSONPath)
				if res.Exists() {
					valToInject = res.String()
				}
			}

			if valToInject != "" {
				ctx.SetHeaderUpstream(mapping.TargetHeader, valToInject)
			}
		}
	}

	return nil
}

// resolveSourceValue extracts the raw value from request context variables (e.g., {path}, {client_ip}).
func (f *RedisMetadataEnricherFilter) resolveSourceValue(ctx *engine.RequestContext, source string) string {
	switch source {
	case "{path}":
		return ctx.Path
	case "{method}":
		return ctx.Method
	case "{client_ip}":
		val := ctx.GetHeader("x-forwarded-for")
		if val == "" {
			val = ctx.GetHeader("x-real-ip")
		}
		return val
	default:
		// Check for custom header pattern: "{header:X-Tenant-ID}"
		if strings.HasPrefix(source, "{header:") && strings.HasSuffix(source, "}") {
			headerName := source[8 : len(source)-1]
			return ctx.GetHeader(headerName)
		}
		return ""
	}
}

// compileRedisKey dynamically substitutes placeholders with resolved variables using a stack buffer.
func (f *RedisMetadataEnricherFilter) compileRedisKey(buf []byte, vars map[string]string) []byte {
	i := 0
	for i < len(f.keyPattern) {
		start := strings.IndexByte(f.keyPattern[i:], '{')
		if start == -1 {
			buf = append(buf, f.keyPattern[i:]...)
			break
		}
		buf = append(buf, f.keyPattern[i:i+start]...)
		i += start

		end := strings.IndexByte(f.keyPattern[i:], '}')
		if end == -1 {
			buf = append(buf, f.keyPattern[i:]...)
			break
		}

		varName := f.keyPattern[i+1 : i+end]
		if resolved, ok := vars[varName]; ok {
			buf = append(buf, resolved...)
		} else {
			buf = append(buf, f.keyPattern[i:i+end+1]...) // Keep placeholder if unresolved
		}
		i += end + 1
	}
	return buf
}
