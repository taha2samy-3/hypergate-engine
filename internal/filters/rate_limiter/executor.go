package rate_limiter

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/coocood/freecache"
	"github.com/taha2samy/hypergate/internal/redis"
)

// DescriptorEntry represents a compiled key-value pair extracted at runtime from the request headers
type DescriptorEntry struct {
	Key   string
	Value string
}

// Decision represents the unified evaluation result returned to the filter
type Decision struct {
	Blocked        bool          // True if the request must be denied (429)
	Limit          uint32        // The configured maximum quota for this window
	LimitRemaining uint32        // The remaining quota after evaluation
	ResetDuration  time.Duration // The remaining time until the current limit window resets
}

// RateLimitExecutor defines the execution contract for all rate-limiting strategies
type RateLimitExecutor interface {
	Evaluate(ctx context.Context, descriptors []DescriptorEntry, cost int64) (Decision, error)
}

// ResolveExecutor is a factory function that resolves and instantiates the correct
// high-performance backend executor strategy at application boot based on configuration.
// It inspects the `algorithm` string and queries the global redis.Manager for the
// requested `redisService`.
func ResolveExecutor(
	algorithm string,
	redisService string,
	redisManager *redis.Manager,
	filterOpts FilterOptions,
) (RateLimitExecutor, error) {

	// Query the global redis.Manager
	client, ok := redisManager.GetClient(redisService)
	if !ok {
		return nil, fmt.Errorf("configured redis service %s not found in manager", redisService)
	}

	// Inspect the algorithm string parameter (case-insensitive).
	algo := strings.ToLower(strings.TrimSpace(algorithm))

	switch algo {
	case "fixed_window":
		// Instantiate and return the fixed window executor
		localCache := freecache.NewCache(10 * 1024 * 1024) // 10MB local cache bypass
		jitterRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		return NewFixedWindowExecutor(client, filterOpts, localCache, jitterRand, 5), nil
	case "sliding_window_counter":
		// Instantiate and return the sliding window executor
		localCache := freecache.NewCache(10 * 1024 * 1024) // 10MB local cache bypass
		return NewSlidingWindowExecutor(client, filterOpts, localCache), nil
	case "token_bucket":
		// Instantiate and return the Token Bucket executor
		localCache := freecache.NewCache(10 * 1024 * 1024) // 10MB local cache bypass
		return NewTokenBucketExecutor(client, filterOpts, localCache), nil
	case "sliding_window_log":
		if filterOpts.DynamicCost.Enabled {
			return nil, fmt.Errorf("sliding_window_log algorithm does not support dynamic cost")
		}
		// Instantiate and return the Lua Sliding Window Log executor
		localCache := freecache.NewCache(10 * 1024 * 1024) // 10MB local cache bypass
		return NewSlidingWindowLogExecutor(client, filterOpts, localCache), nil
	case "leaky_bucket":
		// Instantiate and return the leaky bucket executor
		localCache := freecache.NewCache(10 * 1024 * 1024) // 10MB local cache bypass
		return NewLeakyBucketExecutor(client, filterOpts, localCache), nil
	default:
		return nil, fmt.Errorf("unsupported rate limiter algorithm: %q", algorithm)
	}
}
