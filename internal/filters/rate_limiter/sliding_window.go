package rate_limiter

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
	"go.uber.org/zap"
)

type slidingWindowExecutor struct {
	client     redis.Client
	options    FilterOptions
	localCache *freecache.Cache // Embedded L1 cache bypass
	script     *luaScript
}

// getSlidingLuaScriptBody implements an atomic check-then-increment sliding window counter.
func getSlidingLuaScriptBody() string {
	return `
local current_key = KEYS[1]
local previous_key = KEYS[2]
local limit = tonumber(ARGV[1])
local cost = tonumber(ARGV[2])
local weight = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local previous_count = tonumber(redis.call('GET', previous_key) or "0")
local current_count = tonumber(redis.call('GET', current_key) or "0")

local estimated_count = math.ceil(previous_count * weight + current_count)

if estimated_count + cost <= limit then
    redis.call('INCRBY', current_key, cost)
    redis.call('EXPIRE', current_key, ttl)
    return {1, limit - (estimated_count + cost)}
else
    return {0, limit - estimated_count}
end
`
}

// NewSlidingWindowExecutor compiles the Lua script and instantiates a high-performance sliding window counter.
// It pre-sorts configured descriptors to prioritize specific (value-based) rules over generic fallback ones.
func NewSlidingWindowExecutor(client redis.Client, opts FilterOptions, localCache *freecache.Cache) RateLimitExecutor {
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

	return &slidingWindowExecutor{
		client:     client,
		options:    opts,
		localCache: localCache,
		script:     newLuaScript(getSlidingLuaScriptBody()),
	}
}

// Evaluate processes the extracted request headers against the compiled composite sliding window policies.
func (e *slidingWindowExecutor) Evaluate(ctx context.Context, descriptors []DescriptorEntry, cost int64) (Decision, error) {
	var finalDecision Decision
	finalDecision.LimitRemaining = ^uint32(0) // Start with max uint32 for tracking the lowest limit remaining

	now := time.Now().Unix()
	var batchBuf [1024]byte
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

		// getDivider is resolved package-wide from fixed_window.go
		windowSize := getDivider(policy.Unit)
		currentRoundedTimestamp := (now / windowSize) * windowSize
		previousRoundedTimestamp := currentRoundedTimestamp - windowSize

		// 1. Zero-Allocation Composite Key Generation with Hash Tag Pinning {...}
		// Hash Tag enclosing is mandatory to force both keys to reside on the same Redis Cluster shard.
		estimatedSize := (len(e.options.Domain) + 30) * 2
		for _, entry := range policy.Entries {
			estimatedSize += (len(entry.Key) + len(extracted[entry.Key]) + 2) * 2
		}

		var currentKeyStr, previousKeyStr string

		if offset+estimatedSize <= len(batchBuf) {
			bufC := batchBuf[offset:offset]
			bufC = append(bufC, '{') // Hash Tag Start
			bufC = append(bufC, e.options.Domain...)
			for _, entry := range policy.Entries {
				bufC = append(bufC, '_')
				bufC = append(bufC, entry.Key...)
				bufC = append(bufC, '_')
				bufC = append(bufC, extracted[entry.Key]...)
			}
			bufC = append(bufC, '}') // Hash Tag End
			bufC = append(bufC, '_')
			prefixLen := len(bufC)

			bufC = strconv.AppendInt(bufC, currentRoundedTimestamp, 10)
			currentKeyStr = unsafe.String(unsafe.SliceData(bufC), len(bufC))
			offset += len(bufC)

			bufP := batchBuf[offset:offset]
			bufP = append(bufP, batchBuf[offset-len(bufC):offset-len(bufC)+prefixLen]...)
			bufP = strconv.AppendInt(bufP, previousRoundedTimestamp, 10)
			previousKeyStr = unsafe.String(unsafe.SliceData(bufP), len(bufP))
			offset += len(bufP)
		} else {
			bufC := make([]byte, 0, estimatedSize/2)
			bufC = append(bufC, '{')
			bufC = append(bufC, e.options.Domain...)
			for _, entry := range policy.Entries {
				bufC = append(bufC, '_')
				bufC = append(bufC, entry.Key...)
				bufC = append(bufC, '_')
				bufC = append(bufC, extracted[entry.Key]...)
			}
			bufC = append(bufC, '}')
			bufC = append(bufC, '_')
			prefixBytes := make([]byte, len(bufC))
			copy(prefixBytes, bufC)

			bufC = strconv.AppendInt(bufC, currentRoundedTimestamp, 10)
			currentKeyStr = string(bufC)

			bufP := strconv.AppendInt(prefixBytes, previousRoundedTimestamp, 10)
			previousKeyStr = string(bufP)
		}

		// 2. L1 Local Cache Fast-Bypass Check
		if e.localCache != nil {
			if _, err := e.localCache.Get(unsafe.Slice(unsafe.StringData(currentKeyStr), len(currentKeyStr))); err == nil {
				mylogger.Debug("L1 cache hit: request blocked", zap.String("key", currentKeyStr))
				return Decision{
					Blocked:        true,
					Limit:          policy.Limit,
					LimitRemaining: 0,
					ResetDuration:  time.Duration(windowSize-(now%windowSize)) * time.Second,
				}, nil
			}
		}

		// 3. Sliding window parameter calculations
		elapsed := now % windowSize
		weight := float64(windowSize-elapsed) / float64(windowSize)
		ttlSeconds := 2 * windowSize

		// 4. Format string arguments for Lua execution
		limitStr := strconv.FormatUint(uint64(policy.Limit), 10)
		costStr := strconv.FormatInt(cost, 10)
		weightStr := strconv.FormatFloat(weight, 'f', 4, 64)
		ttlStr := strconv.FormatInt(ttlSeconds, 10)

		var result []interface{}

		// Execute atomic check-then-increment script via EVALSHA
		err := e.script.Eval(ctx, e.client, &result, 2, currentKeyStr, previousKeyStr, limitStr, costStr, weightStr, ttlStr)

		if err != nil {
			mylogger.Error("Redis EVALSHA sliding window execution failed", zap.Error(err), zap.String("key", currentKeyStr))
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("redis sliding window evaluate failed for key %s: %w", currentKeyStr, err)
		}

		if len(result) < 2 {
			if policy.FailOpen {
				continue
			}
			return Decision{}, fmt.Errorf("invalid lua script response length")
		}

		// Parse Lua Response: {allowed (1/0), remaining_quota}
		var allowed int64
		if a, ok := result[0].(int64); ok {
			allowed = a
		}

		var remainingQuota int64
		if r, ok := result[1].(int64); ok {
			remainingQuota = r
		}
		remain := uint32(math.Max(0, float64(remainingQuota)))

		resetSeconds := windowSize - elapsed

		// 5. Decision Mapping & Cache Enforcement
		if allowed == 0 { // Blocked
			if policy.ShadowMode {
				mylogger.Debug("Sliding window metric: ShadowMode violation", zap.String("key", currentKeyStr))
				if 0 < finalDecision.LimitRemaining {
					finalDecision.LimitRemaining = 0
					finalDecision.Limit = policy.Limit
					finalDecision.ResetDuration = time.Duration(resetSeconds) * time.Second
				}
				continue
			}

			// Add blocked key to L1 local cache to protect Redis from brute-force DDoS
			if e.localCache != nil && resetSeconds > 0 {
				_ = e.localCache.Set(unsafe.Slice(unsafe.StringData(currentKeyStr), len(currentKeyStr)), []byte{1}, int(resetSeconds))
			}

			return Decision{
				Blocked:        true,
				Limit:          policy.Limit,
				LimitRemaining: 0,
				ResetDuration:  time.Duration(resetSeconds) * time.Second,
			}, nil
		}

		// Allowed request
		mylogger.Debug("Sliding window metric: WithinLimit", zap.String("key", currentKeyStr), zap.Uint32("remaining", remain))
		if remain < finalDecision.LimitRemaining {
			finalDecision.LimitRemaining = remain
			finalDecision.Limit = policy.Limit
			finalDecision.ResetDuration = time.Duration(resetSeconds) * time.Second
		}
	}

	if finalDecision.LimitRemaining == ^uint32(0) {
		finalDecision.LimitRemaining = 0
		finalDecision.Limit = 0
	}

	finalDecision.Blocked = false
	return finalDecision, nil
}
