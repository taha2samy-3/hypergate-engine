package rate_limiter

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	mylogger "github.com/taha/myprog/internal/logger"
	"github.com/taha/myprog/internal/redis"
	"go.uber.org/zap"
)

type tokenBucketExecutor struct {
	client     redis.Client
	options    FilterOptions
	localCache *freecache.Cache
	scriptSha1 string
	luaBody    string
}

// getTokenLuaScriptBody returns the atomic Lua script to evaluate token bucket limit checks.
func getTokenLuaScriptBody() string {
	return `
local key = KEYS[1]
local max_tokens = tonumber(ARGV[1])
local fill_rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])

local bucket = redis.call('HMGET', key, 't', 'l')
local tokens = max_tokens
local last_updated = now

if bucket[1] then
    tokens = tonumber(bucket[1])
end
if bucket[2] then
    last_updated = tonumber(bucket[2])
end

local elapsed = math.max(0, now - last_updated)
local replenished = tokens + (elapsed * fill_rate)
tokens = math.max(0, math.min(max_tokens, replenished)) 

local allowed = 0
if tokens >= cost then
    tokens = tokens - cost
    allowed = 1
    redis.call('HMSET', key, 't', tokens, 'l', now)
    redis.call('EXPIRE', key, ttl)
end

local reset_duration = 0
if fill_rate > 0 then
    if allowed == 1 then
        reset_duration = (max_tokens - tokens) / fill_rate
    else
        local missing = cost - tokens
        reset_duration = missing / fill_rate
    end
end

reset_duration = math.max(0, reset_duration)

return {allowed, tostring(tokens), math.ceil(reset_duration)}
`
}

// NewTokenBucketExecutor compiles the Lua script and instantiates a high-performance token bucket rate limiter.
// It pre-sorts configured descriptors to prioritize specific (value-based) rules over generic fallback ones.
func NewTokenBucketExecutor(client redis.Client, opts FilterOptions, localCache *freecache.Cache) RateLimitExecutor {
	body := getTokenLuaScriptBody()
	hasher := sha1.New()
	hasher.Write([]byte(body))
	sha := hex.EncodeToString(hasher.Sum(nil))

	// Pre-sort descriptors: Most Specific Rules (with explicit values) must come BEFORE generic/wildcard rules (OTHER)
	sort.Slice(opts.Descriptors, func(i, j int) bool {
		countI, countJ := 0, 0
		for _, entry := range opts.Descriptors[i].Entries {
			if entry.Value != "" {
				countI++
			}
		}
		for _, entry := range opts.Descriptors[j].Entries {
			if entry.Value != "" {
				countJ++
			}
		}
		if len(opts.Descriptors[i].Entries) != len(opts.Descriptors[j].Entries) {
			return len(opts.Descriptors[i].Entries) > len(opts.Descriptors[j].Entries)
		}
		return countI > countJ
	})

	return &tokenBucketExecutor{
		client:     client,
		options:    opts,
		localCache: localCache,
		scriptSha1: sha,
		luaBody:    body,
	}
}

// Evaluate processes the extracted request headers against the compiled composite token bucket policies.
func (e *tokenBucketExecutor) Evaluate(ctx context.Context, descriptors []DescriptorEntry, cost int64) (Decision, error) {
	var finalDecision Decision
	finalDecision.LimitRemaining = ^uint32(0) // Start with max uint32 to track the lowest limit remaining

	// Use float64 for highly precise Lua calculation of elapsed time
	nowFloat := float64(time.Now().UnixNano()) / float64(time.Second)
	var batchBuf [512]byte
	offset := 0

	// Map extracted client descriptor keys to their runtime values for O(1) lookup complexity
	extracted := make(map[string]string, len(descriptors))
	for _, entry := range descriptors {
		extracted[entry.Key] = entry.Value
	}

	// Track evaluated dimensions (e.g., "role_cycle") to short-circuit and prevent
	// evaluating less-specific fallback/OTHER policies of the same key combination in a single request.
	evaluatedDimensions := make(map[string]bool, len(e.options.Descriptors))

	// Iterate through the pre-sorted configured policies (First Match Wins for each rate-limiting dimension)
	for _, policy := range e.options.Descriptors {
		// Construct the unique dimension signature for this policy (e.g., "role_cycle")
		var dimBuf [128]byte
		db := dimBuf[:0]
		for i, entry := range policy.Entries {
			if i > 0 {
				db = append(db, '_')
			}
			db = append(db, entry.Key...)
		}
		dimension := unsafe.String(unsafe.SliceData(db), len(db))

		// Short-circuit: Skip if a more specific policy for this exact dimension has already been processed
		if evaluatedDimensions[dimension] {
			continue
		}

		matched := true

		// Verify if the request satisfies all conditions (Entries) of this composite policy
		for _, entry := range policy.Entries {
			clientVal, exists := extracted[entry.Key]
			if !exists {
				matched = false
				break
			}
			if entry.Value != "" && entry.Value != clientVal {
				matched = false
				break
			}
		}

		if !matched {
			continue // Skip to next policy if conditions are not met
		}

		// Mark this dimension as evaluated so that weaker fallback policies of this dimension are ignored
		evaluatedDimensions[dimension] = true

		// Calculate TTL: safe window to clean up idle keys from Redis memory
		ttlSeconds := int64(3600) // Default 1 hour TTL
		if policy.FillRate > 0 {
			ttlSeconds = int64(math.Ceil(policy.MaxTokens / policy.FillRate))
			if ttlSeconds < 60 {
				ttlSeconds = 60 // Minimum 1 minute TTL to prevent transient key eviction
			}
		}

		// 1. Zero-Allocation Composite Key Generation (with _token_bucket suffix)
		estimatedSize := len(e.options.Domain) + len("_token_bucket") + 30
		for _, entry := range policy.Entries {
			estimatedSize += len(entry.Key) + len(extracted[entry.Key]) + 2
		}

		var keyStr string
		var buf []byte

		if offset+estimatedSize <= len(batchBuf) {
			buf = batchBuf[offset : offset : offset+estimatedSize]
			buf = append(buf, e.options.Domain...)
			buf = append(buf, '_')
			for _, entry := range policy.Entries {
				buf = append(buf, entry.Key...)
				buf = append(buf, '_')
				buf = append(buf, extracted[entry.Key]...)
				buf = append(buf, '_')
			}
			buf = append(buf, "token_bucket"...)

			keyStr = unsafe.String(unsafe.SliceData(buf), len(buf))
			offset += len(buf)
		} else {
			buf = make([]byte, 0, estimatedSize)
			buf = append(buf, e.options.Domain...)
			for _, entry := range policy.Entries {
				buf = append(buf, '_')
				buf = append(buf, entry.Key...)
				buf = append(buf, '_')
				buf = append(buf, extracted[entry.Key]...)
			}
			buf = append(buf, "_token_bucket"...)

			keyStr = string(buf)
		}

		// 2. L1 Local Cache Fast-Bypass Check
		if e.localCache != nil {
			if _, err := e.localCache.Get(buf); err == nil {
				mylogger.Debug("L1 cache hit: request blocked", zap.String("key", keyStr))
				return Decision{
					Blocked:        true,
					Limit:          uint32(policy.MaxTokens),
					LimitRemaining: 0,
					ResetDuration:  time.Duration(ttlSeconds) * time.Second,
				}, nil
			}
		}

		// 3. Redis EVALSHA Execution & Fallback State Machine
		maxTokensStr := strconv.FormatFloat(policy.MaxTokens, 'f', -1, 64)
		fillRateStr := strconv.FormatFloat(policy.FillRate, 'f', -1, 64)
		costStr := strconv.FormatInt(cost, 10)
		nowStr := strconv.FormatFloat(nowFloat, 'f', 6, 64)
		ttlStr := strconv.FormatInt(ttlSeconds, 10)

		var result []interface{}

		// Execute atomic check-then-decrement script via EVALSHA
		err := e.client.DoCmd(&result, "EVALSHA", "", e.scriptSha1, "1", keyStr, maxTokensStr, fillRateStr, costStr, nowStr, ttlStr)

		// NOSCRIPT Fallback Loop
		if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
			mylogger.Info("EVALSHA NOSCRIPT error caught, loading script...", zap.String("sha", e.scriptSha1))

			var newSha string
			errLoad := e.client.DoCmd(&newSha, "SCRIPT", "", "LOAD", e.luaBody)
			if errLoad != nil {
				if policy.FailOpen {
					mylogger.Error("Failed to SCRIPT LOAD Lua token bucket rate limiter, failing open", zap.Error(errLoad))
					continue
				}
				return Decision{}, fmt.Errorf("failed to SCRIPT LOAD Lua token bucket rate limiter: %w", errLoad)
			}

			e.scriptSha1 = newSha // Update stored SHA locally for subsequent executions
			err = e.client.DoCmd(&result, "EVALSHA", "", e.scriptSha1, "1", keyStr, maxTokensStr, fillRateStr, costStr, nowStr, ttlStr)
		}

		if err != nil {
			mylogger.Error("Redis EVALSHA token bucket execution failed", zap.Error(err), zap.String("key", keyStr))
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("redis token bucket fail for key %s: %w", keyStr, err)
		}

		// Parse the Lua response array: {allowed (0/1), tokens (float/bytes), reset_duration (int)}
		if len(result) < 3 {
			mylogger.Error("Invalid Lua script response length", zap.Any("result", result))
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("invalid lua script response")
		}

		var allowed int64
		if a, ok := result[0].(int64); ok {
			allowed = a
		}

		var remainingTokensFloat float64
		switch v := result[1].(type) {
		case int64:
			remainingTokensFloat = float64(v)
		case []byte:
			remainingTokensFloat, _ = strconv.ParseFloat(string(v), 64)
		}
		remainingTokens := uint32(math.Max(0, remainingTokensFloat))

		var resetDuration int64
		if r, ok := result[2].(int64); ok {
			resetDuration = r
		}

		// 4. Decision Enforcement & Cache Injection
		if allowed == 0 {
			if policy.ShadowMode {
				mylogger.Debug("Token Bucket metric: ShadowMode violation", zap.String("key", keyStr))
				if 0 < finalDecision.LimitRemaining {
					finalDecision.LimitRemaining = 0
					finalDecision.Limit = uint32(policy.MaxTokens)
					finalDecision.ResetDuration = time.Duration(resetDuration) * time.Second
				}
				continue
			}

			// Blocked: Inject into L1 cache
			if e.localCache != nil && resetDuration > 0 {
				if remainingTokens == 0 || cost == 1 {
					_ = e.localCache.Set(buf, []byte{1}, int(resetDuration))
				}
			}

			return Decision{
				Blocked:        true,
				Limit:          uint32(policy.MaxTokens),
				LimitRemaining: remainingTokens,
				ResetDuration:  time.Duration(resetDuration) * time.Second,
			}, nil
		}

		// Allowed request
		mylogger.Debug("Token Bucket metric: WithinLimit", zap.String("key", keyStr), zap.Uint32("remaining", remainingTokens))
		if remainingTokens < finalDecision.LimitRemaining {
			finalDecision.LimitRemaining = remainingTokens
			finalDecision.Limit = uint32(policy.MaxTokens)
			finalDecision.ResetDuration = time.Duration(resetDuration) * time.Second
		}
	}

	if finalDecision.LimitRemaining == ^uint32(0) {
		finalDecision.LimitRemaining = 0
		finalDecision.Limit = 0
	}

	finalDecision.Blocked = false
	return finalDecision, nil
}
