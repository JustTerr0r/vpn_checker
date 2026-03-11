package pool

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rawSetKey     = "pool:raw"
	checkedSetKey = "pool:checked"
)

// RedisClient wraps go-redis with the operations needed by the pool.
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient connects to Redis using the given DSN and verifies the connection.
func NewRedisClient(dsn string) (*RedisClient, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse redis DSN: %w", err)
	}
	c := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisClient{client: c}, nil
}

// AddURIs adds URIs to the pool:raw Set using SADD.
// Returns the count of newly added (previously unseen) members.
func (r *RedisClient) AddURIs(ctx context.Context, uris []string) (int64, error) {
	if len(uris) == 0 {
		return 0, nil
	}
	members := make([]interface{}, len(uris))
	for i, u := range uris {
		members[i] = u
	}
	n, err := r.client.SAdd(ctx, rawSetKey, members...).Result()
	if err != nil {
		return 0, fmt.Errorf("SADD %s: %w", rawSetKey, err)
	}
	return n, nil
}

// GetRawURIs returns all URIs from pool:raw via SMEMBERS.
func (r *RedisClient) GetRawURIs(ctx context.Context) ([]string, error) {
	uris, err := r.client.SMembers(ctx, rawSetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("SMEMBERS %s: %w", rawSetKey, err)
	}
	return uris, nil
}

// RemoveRawURI removes a single URI from pool:raw via SREM.
func (r *RedisClient) RemoveRawURI(ctx context.Context, uri string) error {
	if err := r.client.SRem(ctx, rawSetKey, uri).Err(); err != nil {
		return fmt.Errorf("SREM %s: %w", rawSetKey, err)
	}
	return nil
}

// AddCheckedURI adds a live URI to pool:checked (Sorted Set) with score = latency in ms.
func (r *RedisClient) AddCheckedURI(ctx context.Context, uri string, latencyMs float64) error {
	if err := r.client.ZAdd(ctx, checkedSetKey, redis.Z{Score: latencyMs, Member: uri}).Err(); err != nil {
		return fmt.Errorf("ZADD %s: %w", checkedSetKey, err)
	}
	return nil
}

// GetCheckedURIs returns URIs from pool:checked sorted by latency (score asc).
func (r *RedisClient) GetCheckedURIs(ctx context.Context) ([]string, error) {
	uris, err := r.client.ZRange(ctx, checkedSetKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("ZRANGE %s: %w", checkedSetKey, err)
	}
	return uris, nil
}

// GetCheckedURIsTop returns top N URIs from pool:checked sorted by latency (score asc).
// If n <= 0, returns all URIs.
func (r *RedisClient) GetCheckedURIsTop(ctx context.Context, n int) ([]string, error) {
	stop := int64(-1)
	if n > 0 {
		stop = int64(n - 1)
	}
	uris, err := r.client.ZRange(ctx, checkedSetKey, 0, stop).Result()
	if err != nil {
		return nil, fmt.Errorf("ZRANGE %s: %w", checkedSetKey, err)
	}
	return uris, nil
}

// RawCount returns the number of URIs in pool:raw.
func (r *RedisClient) RawCount(ctx context.Context) (int64, error) {
	n, err := r.client.SCard(ctx, rawSetKey).Result()
	if err != nil {
		return 0, fmt.Errorf("SCARD %s: %w", rawSetKey, err)
	}
	return n, nil
}

// CheckedCount returns the number of URIs in pool:checked.
func (r *RedisClient) CheckedCount(ctx context.Context) (int64, error) {
	n, err := r.client.ZCard(ctx, checkedSetKey).Result()
	if err != nil {
		return 0, fmt.Errorf("ZCARD %s: %w", checkedSetKey, err)
	}
	return n, nil
}

// RemoveCheckedURI removes a single URI from pool:checked via ZREM.
func (r *RedisClient) RemoveCheckedURI(ctx context.Context, uri string) error {
	if err := r.client.ZRem(ctx, checkedSetKey, uri).Err(); err != nil {
		return fmt.Errorf("ZREM %s: %w", checkedSetKey, err)
	}
	return nil
}

// ClearRaw deletes all URIs from pool:raw (DEL key).
func (r *RedisClient) ClearRaw(ctx context.Context) error {
	if err := r.client.Del(ctx, rawSetKey).Err(); err != nil {
		return fmt.Errorf("DEL %s: %w", rawSetKey, err)
	}
	return nil
}

// ClearChecked deletes all URIs from pool:checked (DEL key).
func (r *RedisClient) ClearChecked(ctx context.Context) error {
	if err := r.client.Del(ctx, checkedSetKey).Err(); err != nil {
		return fmt.Errorf("DEL %s: %w", checkedSetKey, err)
	}
	return nil
}

// Close closes the underlying Redis connection.
func (r *RedisClient) Close() error {
	return r.client.Close()
}
