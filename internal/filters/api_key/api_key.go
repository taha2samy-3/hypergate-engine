package api_key

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	"github.com/tidwall/gjson"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
)

type APIKeyFilter struct {
	name       string
	config     config.APIKeyFilterConfig
	client     redis.Client
	localCache *freecache.Cache
	cacheTTL   time.Duration
}

func NewAPIKeyFilter(name string, cfg config.APIKeyFilterConfig, client redis.Client) *APIKeyFilter {
	// Initialize L1 cache with 10MB default size
	cache := freecache.NewCache(10 * 1024 * 1024)
	return &APIKeyFilter{
		name:       name,
		config:     cfg,
		client:     client,
		localCache: cache,
		cacheTTL:   30 * time.Second,
	}
}

// Execute checks request headers/query parameters for an API key, hashes it,
// checks L1 cache, fetches/verifies from Redis, checks status, and injects/strips headers/query params.
func (f *APIKeyFilter) Execute(ctx *engine.RequestContext) error {
	var apiKey string
	var matchedKeyName string

	// Step A: Extract the API Key
	for _, name := range f.config.KeyNames {
		if f.config.KeyInHeader {
			if val := ctx.GetHeader(name); val != "" {
				apiKey = val
				matchedKeyName = name
				break
			}
		}
		if f.config.KeyInQuery {
			if val := f.extractQueryParam(ctx.Path, name); val != "" {
				apiKey = val
				matchedKeyName = name
				break
			}
		}
	}

	if apiKey == "" {
		ctx.Blocked = true
		ctx.ResponseStatus = 401
		ctx.ResponseBody = "Unauthorized: Missing API Key"
		mylogger.Warn("Unauthorized: Missing API Key")
		return nil
	}

	// Step B: Hash the Key
	var hashedKey string
	switch f.config.HashAlgorithm {
	case "sha256":
		// lgtm[go/weak-crypto] - Used for deterministic cache key generation, not password hashing
		h := sha256.Sum256([]byte(apiKey))
		var hexBuf [64]byte
		hex.Encode(hexBuf[:], h[:])
		hashedKey = string(hexBuf[:])
	case "md5":
		// lgtm[go/weak-crypto] - Used for deterministic cache key generation, not password hashing
		h := md5.Sum(append([]byte(nil), apiKey...))
		var hexBuf [32]byte
		hex.Encode(hexBuf[:], h[:])
		hashedKey = string(hexBuf[:])
	case "none", "":
		hashedKey = apiKey
	}

	// Concatenate config.RedisKeyPrefix + hashedKey using a stack-allocated byte buffer
	var stackBuf [256]byte
	prefixLen := len(f.config.RedisKeyPrefix)
	keyLen := len(hashedKey)
	var redisKeyBytes []byte
	if prefixLen+keyLen <= len(stackBuf) {
		redisKeyBytes = stackBuf[:prefixLen+keyLen]
		copy(redisKeyBytes[:prefixLen], f.config.RedisKeyPrefix)
		copy(redisKeyBytes[prefixLen:], hashedKey)
	} else {
		redisKeyBytes = []byte(f.config.RedisKeyPrefix + hashedKey)
	}
	redisKey := unsafe.String(unsafe.SliceData(redisKeyBytes), len(redisKeyBytes))

	// Step C: L1 Local Cache Check
	if f.localCache != nil {
		if cachedVal, err := f.localCache.Get(redisKeyBytes); err == nil {
			if len(cachedVal) > 0 && cachedVal[0] == 0 {
				// Cached block marker
				reason := string(cachedVal[1:])
				ctx.Blocked = true
				if strings.Contains(reason, "Forbidden") {
					ctx.ResponseStatus = 403
					ctx.ResponseBody = reason
				} else {
					ctx.ResponseStatus = 401
					ctx.ResponseBody = reason
				}
				return nil
			}
			// Cached metadata: format is TargetHeader1=Value1\nTargetHeader2=Value2...
			lines := strings.Split(string(cachedVal), "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					ctx.SetHeaderUpstream(parts[0], parts[1])
				}
			}
			// Perform credentials stripping if enabled
			f.stripCredentials(ctx, matchedKeyName)
			return nil
		}
	}

	var statusVal string
	headerValues := make(map[string]string)

	// Step D: Query Redis
	switch f.config.ValueFormat {
	case "plain":
		var reply string
		var p redis.Pipeline
		p = f.client.PipeAppend(p, &reply, "GET", redisKey)
		if err := f.client.PipeDo(ctx.Ctx, p); err != nil {
			ctx.Blocked = true
			ctx.ResponseStatus = 500
			return fmt.Errorf("redis connection error: %w", err)
		}
		if reply == "" {
			// Cache block marker in L1 for 10 seconds
			f.cacheBlock(redisKeyBytes, "Unauthorized: Invalid API Key", 10*time.Second)
			ctx.Blocked = true
			ctx.ResponseStatus = 401
			ctx.ResponseBody = "Unauthorized: Invalid API Key"
			return nil
		}
		// Treat the string value as delimited record
		delimiter := f.config.Delimiter
		if delimiter == "" {
			delimiter = "|"
		}
		parts := strings.Split(reply, delimiter)
		for i, mapping := range f.config.OutputMappings {
			if i < len(parts) {
				headerValues[mapping.TargetHeader] = parts[i]
			}
		}

	case "hash":
		// Gather target redis fields
		var statusIdx = -1
		var fields []interface{}
		for _, mapping := range f.config.OutputMappings {
			fields = append(fields, mapping.RedisField)
		}
		if f.config.StatusCheck.Enabled {
			statusIdx = len(fields)
			fields = append(fields, f.config.StatusCheck.FieldName)
		}

		var reply []string
		var p redis.Pipeline
		// client.PipeAppend is (pipeline, rcv, cmd, key, args...)
		p = f.client.PipeAppend(p, &reply, "HMGET", redisKey, fields...)
		if err := f.client.PipeDo(ctx.Ctx, p); err != nil {
			ctx.Blocked = true
			ctx.ResponseStatus = 500
			return fmt.Errorf("redis connection error: %w", err)
		}

		allEmpty := true
		for _, v := range reply {
			if v != "" {
				allEmpty = false
				break
			}
		}
		if len(reply) == 0 || allEmpty {
			f.cacheBlock(redisKeyBytes, "Unauthorized: Invalid API Key", 10*time.Second)
			ctx.Blocked = true
			ctx.ResponseStatus = 401
			ctx.ResponseBody = "Unauthorized: Invalid API Key"
			return nil
		}

		for i, mapping := range f.config.OutputMappings {
			if i < len(reply) {
				headerValues[mapping.TargetHeader] = reply[i]
			}
		}
		if statusIdx >= 0 && statusIdx < len(reply) {
			statusVal = reply[statusIdx]
		}

	case "json":
		var reply string
		var p redis.Pipeline
		p = f.client.PipeAppend(p, &reply, "GET", redisKey)
		if err := f.client.PipeDo(ctx.Ctx, p); err != nil {
			ctx.Blocked = true
			ctx.ResponseStatus = 500
			return fmt.Errorf("redis connection error: %w", err)
		}
		if reply == "" {
			f.cacheBlock(redisKeyBytes, "Unauthorized: Invalid API Key", 10*time.Second)
			ctx.Blocked = true
			ctx.ResponseStatus = 401
			ctx.ResponseBody = "Unauthorized: Invalid API Key"
			return nil
		}

		for _, mapping := range f.config.OutputMappings {
			res := gjson.Get(reply, mapping.JSONPath)
			if res.Exists() {
				headerValues[mapping.TargetHeader] = res.String()
			}
		}
		if f.config.StatusCheck.Enabled {
			res := gjson.Get(reply, f.config.StatusCheck.FieldName)
			if res.Exists() {
				statusVal = res.String()
			}
		}
	}

	// Step E: Account Status Verification
	if f.config.StatusCheck.Enabled {
		if statusVal != f.config.StatusCheck.ExpectedValue {
			reason := "Forbidden: Account is " + statusVal
			f.cacheBlock(redisKeyBytes, reason, 10*time.Second)
			ctx.Blocked = true
			ctx.ResponseStatus = 403
			ctx.ResponseBody = reason
			mylogger.Warn(reason)
			return nil
		}
	}

	// Inject resolved headers upstream
	for k, v := range headerValues {
		ctx.SetHeaderUpstream(k, v)
	}

	// Cache successful auth metadata in L1 cache
	if f.localCache != nil {
		var cacheSB strings.Builder
		for k, v := range headerValues {
			cacheSB.WriteString(k)
			cacheSB.WriteByte('=')
			cacheSB.WriteString(v)
			cacheSB.WriteByte('\n')
		}
		cacheStr := cacheSB.String()
		_ = f.localCache.Set(redisKeyBytes, []byte(cacheStr), int(f.cacheTTL.Seconds()))
	}

	// Step F: Credentials Stripping
	f.stripCredentials(ctx, matchedKeyName)

	return nil
}

func (f *APIKeyFilter) cacheBlock(key []byte, reason string, ttl time.Duration) {
	if f.localCache != nil {
		val := make([]byte, 1+len(reason))
		val[0] = 0
		copy(val[1:], reason)
		_ = f.localCache.Set(key, val, int(ttl.Seconds()))
	}
}

func (f *APIKeyFilter) stripCredentials(ctx *engine.RequestContext, keyName string) {
	if f.config.HideCredentials && keyName != "" {
		if f.config.KeyInHeader {
			ctx.RemoveHeaderUpstream(keyName)
		}
		if f.config.KeyInQuery {
			ctx.Path = f.stripQueryParam(ctx.Path, keyName)
		}
	}
}

func (f *APIKeyFilter) extractQueryParam(path, name string) string {
	idx := strings.IndexByte(path, '?')
	if idx == -1 {
		return ""
	}
	query := path[idx+1:]
	for query != "" {
		key := query
		if next := strings.IndexByte(query, '&'); next != -1 {
			key = query[:next]
			query = query[next+1:]
		} else {
			query = ""
		}
		if valIdx := strings.IndexByte(key, '='); valIdx != -1 {
			if key[:valIdx] == name {
				return key[valIdx+1:]
			}
		} else {
			if key == name {
				return ""
			}
		}
	}
	return ""
}

func (f *APIKeyFilter) stripQueryParam(path, name string) string {
	idx := strings.IndexByte(path, '?')
	if idx == -1 {
		return path
	}
	base := path[:idx]
	query := path[idx+1:]
	var newQueryParts []string
	for query != "" {
		part := query
		if next := strings.IndexByte(query, '&'); next != -1 {
			part = query[:next]
			query = query[next+1:]
		} else {
			query = ""
		}
		valIdx := strings.IndexByte(part, '=')
		k := part
		if valIdx != -1 {
			k = part[:valIdx]
		}
		if k != name {
			newQueryParts = append(newQueryParts, part)
		}
	}
	if len(newQueryParts) == 0 {
		return base
	}
	return base + "?" + strings.Join(newQueryParts, "&")
}
