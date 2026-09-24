// Package redistest provides an in-memory redis.Client for unit tests.
package redistest

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"github.com/taha2samy/hypergate/internal/redis"
)

type call struct {
	rcv  interface{}
	cmd  string
	key  string
	args []interface{}
}

// Client is a tiny Redis stand-in supporting GET/SET, INCRBY and EXPIRE. It records
// every key it sees so tests can assert on key layout.
type Client struct {
	mu      sync.Mutex
	data    map[string]string
	pending []call
	Keys    []string
	Closed  bool
	Err     error
}

func New() *Client { return &Client{data: map[string]string{}} }

func (c *Client) Set(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = val
}

func (c *Client) Get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[key]
}

func (c *Client) DoCmd(ctx context.Context, rcv interface{}, cmd, key string, args ...interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exec(call{rcv, cmd, key, args})
}

func (c *Client) PipeAppend(p redis.Pipeline, rcv interface{}, cmd, key string, args ...interface{}) redis.Pipeline {
	c.mu.Lock()
	c.pending = append(c.pending, call{rcv, cmd, key, args})
	c.mu.Unlock()
	return append(p, redis.PipelineAction{Key: key})
}

func (c *Client) PipeDo(ctx context.Context, p redis.Pipeline) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := c.pending
	c.pending = nil
	for _, cl := range calls {
		if err := c.exec(cl); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) exec(cl call) error {
	if c.Err != nil {
		return c.Err
	}
	if cl.key != "" {
		c.Keys = append(c.Keys, cl.key)
	}
	switch cl.cmd {
	case "GET":
		assign(cl.rcv, c.data[cl.key])
	case "SET":
		c.data[cl.key] = fmt.Sprint(cl.args[0])
	case "INCRBY":
		cur, _ := strconv.ParseInt(c.data[cl.key], 10, 64)
		by, _ := strconv.ParseInt(fmt.Sprint(cl.args[0]), 10, 64)
		cur += by
		c.data[cl.key] = strconv.FormatInt(cur, 10)
		assign(cl.rcv, cur)
	case "EXPIRE", "PING":
		assign(cl.rcv, "PONG")
	default:
		return fmt.Errorf("redistest: unsupported command %s", cl.cmd)
	}
	return nil
}

func assign(rcv interface{}, v interface{}) {
	if rcv == nil {
		return
	}
	dst := reflect.ValueOf(rcv)
	if dst.Kind() != reflect.Ptr || dst.IsNil() {
		return
	}
	target := dst.Elem()
	switch val := v.(type) {
	case string:
		if target.Kind() == reflect.String {
			target.SetString(val)
		}
	case int64:
		switch target.Kind() {
		case reflect.Uint64, reflect.Uint32, reflect.Uint:
			if val < 0 {
				val = 0
			}
			target.SetUint(uint64(val))
		case reflect.Int64, reflect.Int:
			target.SetInt(val)
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Closed = true
	return nil
}

func (c *Client) NumActiveConns() int { return 0 }
