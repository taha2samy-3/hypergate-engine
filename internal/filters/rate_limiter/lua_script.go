package rate_limiter

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"

	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
)

// luaScript runs a Lua script through EVALSHA, loading it with SCRIPT LOAD on a
// NOSCRIPT reply (e.g. after a Redis restart or failover). It is safe for
// concurrent use: the SHA is swapped atomically.
type luaScript struct {
	body string
	sha  atomic.Pointer[string]
}

func newLuaScript(body string) *luaScript {
	sum := sha1.Sum([]byte(body))
	sha := hex.EncodeToString(sum[:])
	s := &luaScript{body: body}
	s.sha.Store(&sha)
	return s
}

// Eval executes the script with numKeys keys followed by the remaining arguments.
func (s *luaScript) Eval(ctx context.Context, client redis.Client, rcv interface{}, numKeys int, keysAndArgs ...interface{}) error {
	args := make([]interface{}, 0, 2+len(keysAndArgs))
	args = append(args, *s.sha.Load(), strconv.Itoa(numKeys))
	args = append(args, keysAndArgs...)

	err := client.DoCmd(ctx, rcv, "EVALSHA", "", args...)
	if err == nil || !strings.Contains(err.Error(), "NOSCRIPT") {
		return err
	}

	mylogger.Info("EVALSHA NOSCRIPT, loading Lua script", zap.String("sha", *s.sha.Load()))
	var newSha string
	if err := client.DoCmd(ctx, &newSha, "SCRIPT", "", "LOAD", s.body); err != nil {
		return fmt.Errorf("SCRIPT LOAD failed: %w", err)
	}
	s.sha.Store(&newSha)
	args[0] = newSha
	return client.DoCmd(ctx, rcv, "EVALSHA", "", args...)
}
