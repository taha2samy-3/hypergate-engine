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
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
	"go.uber.org/zap"
)

var logCounter uint64

type slidingWindowLogExecutor struct {
	client     redis.Client
	options    FilterOptions
	localCache *freecache.Cache
	scriptSha1 string
	luaBody    string
}

// getLogLuaScriptBody returns the atomic Lua script to evaluate sliding window log checks via ZSET.
func getLogLuaScriptBody() string {
	return `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local member = ARGV[4]

redis.call("ZREMRANGEBYSCORE", key, 0, now - window)

local count = redis.call("ZCARD", key)

if count < limit then
    redis.call("ZADD", key, now, member)
    redis.call("EXPIRE", key, math.ceil(window / 1000))
    return {1, limit - count - 1}
else
    local oldest = redis.call("ZRANGE", key, 0, 0, "WITHSCORES")
    local reset_duration = window
    if oldest and oldest[2] then
        local oldest_ts = tonumber(oldest[2])
        reset_duration = window - (now - oldest_ts)
    end
    
    reset_duration = math.max(0, reset_duration)
    
    return {0, math.ceil(reset_duration)}
end
`
}

// NewSlidingWindowLogExecutor compiles the Lua script and instantiates a high-performance sliding window log executor.
// It pre-sorts configured descriptors to prioritize specific (value-based) rules over generic fallback ones.
func NewSlidingWindowLogExecutor(client redis.Client, opts FilterOptions, localCache *freecache.Cache) RateLimitExecutor {
	body := getLogLuaScriptBody()
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

	return &slidingWindowLogExecutor{
		client:     client,
		options:    opts,
		localCache: localCache,
		scriptSha1: sha,
		luaBody:    body,
	}
}

// Evaluate processes the extracted request headers against the compiled composite sliding window log policies.
func (e *slidingWindowLogExecutor) Evaluate(ctx context.Context, descriptors []DescriptorEntry, cost int64) (Decision, error) {
	var finalDecision Decision
	finalDecision.LimitRemaining = ^uint32(0)

	// ZSET Log calculates precision at the millisecond level
	nowMs := time.Now().UnixNano() / int64(time.Millisecond)
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

		// Resolved package-wide from fixed_window.go
		windowSeconds := getDivider(policy.Unit)
		windowMs := windowSeconds * 1000

		// 1. Zero-Allocation Composite Key Generation (with _sliding_log suffix)
		estimatedSize := len(e.options.Domain) + len("_sliding_log") + 30
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
			buf = append(buf, "sliding_log"...)

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
			buf = append(buf, "_sliding_log"...)

			keyStr = string(buf)
		}

		// 2. L1 Local Cache Fast-Bypass Check
		if e.localCache != nil {
			if _, err := e.localCache.Get(buf); err == nil {
				mylogger.Debug("L1 cache hit: request blocked", zap.String("key", keyStr))
				return Decision{
					Blocked:        true,
					Limit:          policy.Limit,
					LimitRemaining: 0,
					ResetDuration:  time.Duration(windowMs) * time.Millisecond,
				}, nil
			}
		}

		// 3. Dynamic ZSET Member ID Generation
		// Evaluated using UnixMicro timestamp combined with request ID or fallback atomic counter.
		limitStr := strconv.FormatUint(uint64(policy.Limit), 10)
		nowStr := strconv.FormatInt(nowMs, 10)
		windowStr := strconv.FormatInt(windowMs, 10)

		var reqID string
		if rid, ok := ctx.Value(requestIDKey).(string); ok {
			reqID = rid
		}
		var memberBuf [128]byte
		mb := memberBuf[:0]
		mb = strconv.AppendInt(mb, time.Now().UnixMicro(), 10)
		if reqID != "" {
			mb = append(mb, '_')
			mb = append(mb, reqID...)
		} else {
			mb = append(mb, '_')
			mb = strconv.AppendUint(mb, atomic.AddUint64(&logCounter, 1), 10)
		}
		memberID := unsafe.String(unsafe.SliceData(mb), len(mb))

		var result []interface{}

		// Execute atomic EVALSHA
		err := e.client.DoCmd(&result, "EVALSHA", "", e.scriptSha1, "1", keyStr, limitStr, nowStr, windowStr, memberID)

		// NOSCRIPT Fallback Loop
		if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
			mylogger.Info("EVALSHA NOSCRIPT error caught, loading log script...", zap.String("sha", e.scriptSha1))

			var newSha string
			errLoad := e.client.DoCmd(&newSha, "SCRIPT", "", "LOAD", e.luaBody)
			if errLoad != nil {
				if policy.FailOpen {
					mylogger.Error("Failed to SCRIPT LOAD Lua rate limiter, failing open", zap.Error(errLoad))
					continue
				}
				return Decision{}, fmt.Errorf("failed to SCRIPT LOAD sliding log lua script: %w", errLoad)
			}

			e.scriptSha1 = newSha
			err = e.client.DoCmd(&result, "EVALSHA", "", e.scriptSha1, "1", keyStr, limitStr, nowStr, windowStr, memberID)
		}

		if err != nil {
			mylogger.Error("Redis EVALSHA sliding log execution failed", zap.Error(err), zap.String("key", keyStr))
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("redis sliding log fail for key %s: %w", keyStr, err)
		}

		if len(result) < 2 {
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("invalid sliding log script response length")
		}

		var allowed int64
		if a, ok := result[0].(int64); ok {
			allowed = a
		}

		// 4. Decision Enforcement & Cache Injection
		if allowed == 0 {
			var resetDurationMs int64
			if r, ok := result[1].(int64); ok {
				resetDurationMs = r
			}

			if policy.ShadowMode {
				mylogger.Debug("Sliding log metric: ShadowMode violation", zap.String("key", keyStr))
				if 0 < finalDecision.LimitRemaining {
					finalDecision.LimitRemaining = 0
					finalDecision.Limit = policy.Limit
					finalDecision.ResetDuration = time.Duration(resetDurationMs) * time.Millisecond
				}
				continue
			}

			// Blocked: Inject into L1 cache
			if e.localCache != nil && resetDurationMs > 0 {
				cacheTTL := int(math.Ceil(float64(resetDurationMs) / 1000.0))
				if cacheTTL < 1 {
					cacheTTL = 1
				}
				_ = e.localCache.Set(buf, []byte{1}, cacheTTL)
			}

			return Decision{
				Blocked:        true,
				Limit:          policy.Limit,
				LimitRemaining: 0,
				ResetDuration:  time.Duration(resetDurationMs) * time.Millisecond,
			}, nil
		}

		// Allowed request
		var remainInt int64
		if r, ok := result[1].(int64); ok {
			remainInt = r
		}
		remain := uint32(math.Max(0, float64(remainInt)))

		if remain < finalDecision.LimitRemaining {
			finalDecision.LimitRemaining = remain
			finalDecision.Limit = policy.Limit
			finalDecision.ResetDuration = time.Duration(windowMs) * time.Millisecond
		}
	}

	if finalDecision.LimitRemaining == ^uint32(0) {
		finalDecision.LimitRemaining = 0
		finalDecision.Limit = 0
	}

	finalDecision.Blocked = false
	return finalDecision, nil
}
