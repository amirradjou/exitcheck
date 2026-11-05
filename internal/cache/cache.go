// Package cache keeps recent results in Redis, so a repeated address answers instantly, and makes concurrent
// requests for the same key share one computation. Redis is an accelerator, not a dependency: when it is down or
// hangs, requests are computed without it, and after a failure it is left alone for a while so that each answer
// does not pay the timeout again.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// prefix namespaces every key; bump the version when the stored shape changes.
const prefix = "rugcheck:v1:"

// Timeout bounds each Redis operation, and Cooldown is how long Redis is skipped after one fails. A local Redis
// answers in well under a millisecond; one that takes longer than Timeout is treated as down.
var (
	Timeout  = 250 * time.Millisecond
	Cooldown = 30 * time.Second
)

// Cache is a Redis-backed cache with request coalescing.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
	// OnError receives Redis failures (the request still succeeds without the cache). Nil drops them.
	OnError func(error)
	group   singleflight.Group
	// downUntil is when Redis may be tried again after a failure, in Unix nanoseconds.
	downUntil atomic.Int64
}

// New connects to the Redis at url (redis://host:port/db) and keeps entries for ttl unless told otherwise.
func New(url string, ttl time.Duration) (*Cache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = Timeout, Timeout, Timeout
	opts.MaxRetries = -1 // go-redis retries three times by default; a cache that is down must fail at once
	return &Cache{rdb: redis.NewClient(opts), ttl: ttl}, nil
}

// Ping checks that Redis answers.
func (c *Cache) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the connection pool.
func (c *Cache) Close() error {
	return c.rdb.Close()
}

// Fetch returns the cached value for key, or runs compute once for all concurrent callers asking for the same key
// and caches its result. compute returns the value and how long to keep it: zero means the cache's TTL, a negative
// duration not at all. hit reports whether the value came from Redis. Errors from compute are not cached.
//
// compute runs under the first caller's context, so callers that share keys should share a lifetime (the bot passes
// its own); each caller still stops waiting when its context ends.
func Fetch[T any](ctx context.Context, c *Cache, key string, compute func(context.Context) (T, time.Duration, error)) (v T, hit bool, err error) {
	if c == nil {
		v, _, err = compute(ctx)
		return v, false, err
	}
	if v, ok := get[T](ctx, c, key); ok {
		return v, true, nil
	}
	ch := c.group.DoChan(key, func() (any, error) {
		v, ttl, err := compute(ctx)
		if err != nil {
			return v, err
		}
		if ttl >= 0 {
			c.set(ctx, key, v, ttl)
		}
		return v, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			var zero T
			return zero, false, r.Err
		}
		return r.Val.(T), false, nil
	case <-ctx.Done():
		var zero T
		return zero, false, ctx.Err()
	}
}

// Put stores v under key for the cache's TTL. A nil cache drops it.
func Put(ctx context.Context, c *Cache, key string, v any) {
	if c != nil {
		c.set(ctx, key, v, 0)
	}
}

// PutFor stores v under key for ttl (0: the cache's TTL). A nil cache drops it.
func PutFor(ctx context.Context, c *Cache, key string, v any, ttl time.Duration) {
	if c != nil {
		c.set(ctx, key, v, ttl)
	}
}

// Delete removes keys, so the next request computes afresh.
func Delete(ctx context.Context, c *Cache, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = prefix + k
	}
	return c.rdb.Del(ctx, full...).Err()
}

// Get returns the cached value for key, if there is one. A nil cache has nothing.
func Get[T any](ctx context.Context, c *Cache, key string) (T, bool) {
	if c == nil {
		var zero T
		return zero, false
	}
	return get[T](ctx, c, key)
}

func get[T any](ctx context.Context, c *Cache, key string) (T, bool) {
	var v T
	if !c.up() {
		return v, false
	}
	raw, err := c.rdb.Get(ctx, prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return v, false
	}
	if err != nil {
		c.down(fmt.Errorf("cache get: %w", err))
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		c.report(fmt.Errorf("cache get %s: stale or corrupt entry: %w", key, err))
		return v, false
	}
	return v, true
}

func (c *Cache) set(ctx context.Context, key string, v any, ttl time.Duration) {
	if !c.up() {
		return
	}
	if ttl == 0 {
		ttl = c.ttl
	}
	raw, err := json.Marshal(v)
	if err != nil {
		c.report(fmt.Errorf("cache set %s: %w", key, err))
		return
	}
	if err := c.rdb.Set(ctx, prefix+key, raw, ttl).Err(); err != nil {
		c.down(fmt.Errorf("cache set: %w", err))
	}
}

func (c *Cache) up() bool {
	return time.Now().UnixNano() >= c.downUntil.Load()
}

// down records a Redis failure and stops using Redis for Cooldown.
func (c *Cache) down(err error) {
	c.downUntil.Store(time.Now().Add(Cooldown).UnixNano())
	c.report(err)
}

func (c *Cache) report(err error) {
	if c.OnError != nil {
		c.OnError(err)
	}
}
