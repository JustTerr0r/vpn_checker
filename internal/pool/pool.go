package pool

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Config holds Pool constructor parameters.
type Config struct {
	URLs        []string
	Parallelism int
	RedisDSN    string
}

// Pool orchestrates periodic fetching of VPN config lists and storing them in Redis.
type Pool struct {
	urls        []string
	parallelism int
	redis       *RedisClient
	httpClient  *http.Client
}

// New creates a Pool and verifies the Redis connection.
func New(cfg Config) (*Pool, error) {
	rc, err := NewRedisClient(cfg.RedisDSN)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &Pool{
		urls:        cfg.URLs,
		parallelism: cfg.Parallelism,
		redis:       rc,
		httpClient:  &http.Client{Timeout: fetchTimeout},
	}, nil
}

// RunOnce fetches all configured URLs, collects valid URIs and SADDs them into Redis.
// Returns the count of newly added URIs.
func (p *Pool) RunOnce(ctx context.Context) (int64, error) {
	logf("fetching %d URL(s) with %d worker(s)…", len(p.urls), p.parallelism)

	results := FetchAll(ctx, p.httpClient, p.urls, p.parallelism)

	var allURIs []string
	for _, r := range results {
		if r.Err != nil {
			logf("WARN fetch %s: %v", r.URL, r.Err)
			continue
		}
		logf("fetched %s → %d valid URIs", r.URL, len(r.URIs))
		allURIs = append(allURIs, r.URIs...)
	}

	if len(allURIs) == 0 {
		logf("WARN no valid URIs collected this round")
		return 0, nil
	}

	added, err := p.redis.AddURIs(ctx, allURIs)
	if err != nil {
		return 0, fmt.Errorf("redis SADD: %w", err)
	}
	logf("pool:raw  +%d new  (batch total: %d)", added, len(allURIs))
	return added, nil
}

// Run executes RunOnce immediately, then repeats every interval until ctx is cancelled.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	if _, err := p.RunOnce(ctx); err != nil {
		logf("ERROR RunOnce: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logf("interval tick — starting fetch")
			if _, err := p.RunOnce(ctx); err != nil {
				logf("ERROR RunOnce: %v", err)
			}
			logf("next run in %s", interval)
		}
	}
}

// Close releases the Redis connection.
func (p *Pool) Close() error {
	return p.redis.Close()
}
