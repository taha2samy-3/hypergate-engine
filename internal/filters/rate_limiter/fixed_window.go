package rate_limiter

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/coocood/freecache"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
	"go.uber.org/zap"
)

type fixedWindowExecutor struct {
	client                     redis.Client
	options                    FilterOptions
	localCache                 *freecache.Cache // Embedded L1 cache bypass
	jitterRand                 *rand.Rand
	expirationJitterMaxSeconds int64
}

// NewFixedWindowExecutor compiles and instantiates a high-performance fixed window rate limiter.
// It pre-sorts configured descriptors so that specific (value-based) rules are evaluated before generic fallback rules.
func NewFixedWindowExecutor(client redis.Client, opts FilterOptions, localCache *freecache.Cache, jitterRand *rand.Rand, jitterMax int64) RateLimitExecutor {
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
		// Order by total entries count (composite complexity), then by specific values count
		if len(opts.Descriptors[i].Entries) != len(opts.Descriptors[j].Entries) {
			return len(opts.Descriptors[i].Entries) > len(opts.Descriptors[j].Entries)
		}
		return countI > countJ
	})

	return &fixedWindowExecutor{
		client:                     client,
		options:                    opts,
		localCache:                 localCache,
		jitterRand:                 jitterRand,
		expirationJitterMaxSeconds: jitterMax,
	}
}

// getDivider converts a unit string into its corresponding seconds representation.
func getDivider(unit string) int64 {
	switch strings.ToLower(unit) {
	case "second":
		return 1
	case "minute":
		return 60
	case "hour":
		return 3600
	case "day":
		return 86400
	case "week":
		return 604800
	case "month":
		return 2592000 // approx 30 days
	case "year":
		return 31536000
	default:
		return 60 // default to minute if unrecognized
	}
}

// Evaluate processes the extracted request headers against the compiled composite rate limit policies.
func (e *fixedWindowExecutor) Evaluate(ctx context.Context, descriptors []DescriptorEntry, cost int64) (Decision, error) {
	var finalDecision Decision
	finalDecision.LimitRemaining = ^uint32(0) // Start with max uint32 to find the minimum remaining quota

	now := time.Now().Unix()
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
			// If the policy defines an explicit value, it must match the client's runtime value.
			// If the policy value is empty, it acts as a wildcard (OTHER/fallback) and matches any value.
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

		divider := getDivider(policy.Unit)
		roundedTimestamp := (now / divider) * divider
		resetSeconds := divider - (now % divider)

		// 1. Zero-Allocation Composite Key Generation
		estimatedSize := len(e.options.Domain) + 30
		for _, entry := range policy.Entries {
			estimatedSize += len(entry.Key) + len(extracted[entry.Key]) + 2
		}

		var keyStr string
		var buf []byte

		if offset+estimatedSize <= len(batchBuf) {
			buf = batchBuf[offset : offset : offset+estimatedSize]
			buf = append(buf, e.options.Domain...)
			for _, entry := range policy.Entries {
				buf = append(buf, '_')
				buf = append(buf, entry.Key...)
				buf = append(buf, '_')
				buf = append(buf, extracted[entry.Key]...) // Use the actual extracted runtime value for the Redis key!
			}
			buf = append(buf, '_')
			buf = strconv.AppendInt(buf, roundedTimestamp, 10)

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
			buf = append(buf, '_')
			buf = strconv.AppendInt(buf, roundedTimestamp, 10)

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
					ResetDuration:  time.Duration(resetSeconds) * time.Second,
				}, nil
			}
		}

		// 3. Optimized Redis Pipelining Execution
		var count uint64
		var p redis.Pipeline
		p = e.client.PipeAppend(p, &count, "INCRBY", keyStr, cost)

		ttl := resetSeconds
		if e.expirationJitterMaxSeconds > 0 && e.jitterRand != nil {
			ttl += e.jitterRand.Int63n(e.expirationJitterMaxSeconds)
		}
		p = e.client.PipeAppend(p, nil, "EXPIRE", keyStr, ttl)

		if err := e.client.PipeDo(ctx, p); err != nil {
			mylogger.Error("Redis pipeline execution failed", zap.Error(err), zap.String("key", keyStr))

			// Direct struct field access to safely process fail-open logic
			if policy.FailOpen {
				continue // Fallback to allowing the request if fail_open is enabled
			}

			// Safe default if fail_open is not explicitly checked or configured
			return Decision{}, fmt.Errorf("redis pipeline fail for key %s: %w", keyStr, err)
		}

		// 4. Metrics Logging & Enforcement Decision
		if count > uint64(policy.Limit) {
			mylogger.Debug("Rate limit metric: OverLimit", zap.String("key", keyStr), zap.Uint64("count", count))

			if policy.ShadowMode {
				mylogger.Debug("Rate limit metric: ShadowMode violation", zap.String("key", keyStr))
				remain := uint32(0)
				if remain < finalDecision.LimitRemaining {
					finalDecision.LimitRemaining = remain
					finalDecision.Limit = policy.Limit
					finalDecision.ResetDuration = time.Duration(resetSeconds) * time.Second
				}
				continue
			}

			// Add blocked key to L1 cache to protect Redis from downstream load
			if e.localCache != nil {
				if cost == 1 || (count-uint64(cost)) >= uint64(policy.Limit) {
					_ = e.localCache.Set(buf, []byte{1}, int(resetSeconds))
				}
			}

			return Decision{
				Blocked:        true,
				Limit:          policy.Limit,
				LimitRemaining: 0,
				ResetDuration:  time.Duration(resetSeconds) * time.Second,
			}, nil
		}

		// Update the final decision with the most restrictive observed quota remaining
		remain := policy.Limit - uint32(count)
		if remain < finalDecision.LimitRemaining {
			finalDecision.LimitRemaining = remain
			finalDecision.Limit = policy.Limit
			finalDecision.ResetDuration = time.Duration(resetSeconds) * time.Second
		}
	}

	// Normalize LimitRemaining if no policies matched during evaluation
	if finalDecision.LimitRemaining == ^uint32(0) {
		finalDecision.LimitRemaining = 0
		finalDecision.Limit = 0
	}

	finalDecision.Blocked = false
	return finalDecision, nil
}
