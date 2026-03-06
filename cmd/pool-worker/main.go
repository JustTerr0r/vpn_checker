package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vpn_checker/internal/pool"
)

func main() {
	urlsFlag    := flag.String("urls", "", "comma-separated list of URLs to fetch")
	intervalFlag := flag.Duration("interval", 10*time.Minute, "how often to re-fetch all URLs")
	redisFlag   := flag.String("redis", "", "Redis DSN (default: $REDIS_URL)")
	workersFlag := flag.Int("workers", 5, "parallel HTTP fetchers")
	flag.Parse()

	// Parse URL list.
	var urls []string
	for _, u := range strings.Split(*urlsFlag, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "error: no URLs provided (use -urls url1,url2,...)")
		os.Exit(1)
	}

	// Redis DSN: flag → env.
	redisDSN := *redisFlag
	if redisDSN == "" {
		redisDSN = os.Getenv("REDIS_URL")
	}
	if redisDSN == "" {
		fmt.Fprintln(os.Stderr, "error: Redis DSN not set (use -redis or $REDIS_URL)")
		os.Exit(1)
	}

	p, err := pool.New(pool.Config{
		URLs:        urls,
		Parallelism: *workersFlag,
		RedisDSN:    redisDSN,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init pool: %v\n", err)
		os.Exit(1)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\n[pool-worker] shutting down…")
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "[%s] [pool-worker] started — %d URL(s), interval=%s, workers=%d\n",
		time.Now().Format("2006-01-02 15:04:05"), len(urls), *intervalFlag, *workersFlag)

	p.Run(ctx, *intervalFlag)

	fmt.Fprintln(os.Stderr, "[pool-worker] stopped")
}
