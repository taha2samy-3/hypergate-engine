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

// ResolveExecutor instantiates the executor strategy selected by `algorithm`
// on top of the given Redis client.
func ResolveExecutor(
	algorithm string,
	client redis.Client,
	filterOpts FilterOptions,
) (RateLimitExecutor, error) {
	if client == nil {
		return nil, fmt.Errorf("rate limiter requires a redis client")
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
