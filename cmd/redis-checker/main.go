package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"vpn_checker/internal/checker"
	"vpn_checker/internal/dashboard"
	"vpn_checker/internal/parser"
	"vpn_checker/internal/pool"
)

func main() {
	redisDSN        := flag.String("redis", "", "Redis DSN (default: $REDIS_URL)")
	workers         := flag.Int("workers", 10, "concurrent check workers")
	timeout         := flag.Duration("timeout", 15*time.Second, "timeout per config check")
	serve           := flag.String("serve", ":8081", "dashboard address")
	recheck         := flag.Bool("recheck", false, "loop forever: restart from pool:raw after each full pass")
	recheckInterval := flag.Duration("recheck-interval", 0, "interval to recheck pool:checked (0 = disabled)")
	recheckWorkers  := flag.Int("recheck-workers", 5, "workers for pool:checked recheck")
	flag.Parse()

	dsn := *redisDSN
	if dsn == "" {
		dsn = os.Getenv("REDIS_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "error: Redis DSN not set (use -redis or $REDIS_URL)")
		os.Exit(1)
	}

	// Railway injects $PORT; use it if -serve flag was not explicitly set.
	if *serve == ":8081" {
		if port := os.Getenv("PORT"); port != "" {
			*serve = ":" + port
		}
	}

	rc, err := pool.NewRedisClient(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect Redis: %v\n", err)
		os.Exit(1)
	}
	defer rc.Close()

	// Grabber state — controlled by dashboard HTTP handlers.
	var grabMu     sync.Mutex
	var grabCancel context.CancelFunc // non-nil while running

	grabberStart := func(urls []string, interval time.Duration) error {
		grabMu.Lock()
		defer grabMu.Unlock()
		if grabCancel != nil {
			return fmt.Errorf("grabber already running")
		}
		p, err := pool.New(pool.Config{
			URLs:        urls,
			Parallelism: 5,
			RedisDSN:    dsn,
		})
		if err != nil {
			return fmt.Errorf("init pool: %w", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		grabCancel = cancel
		go func() {
			defer p.Close()
			p.Run(ctx, interval)
		}()
		return nil
	}

	grabberStop := func() {
		grabMu.Lock()
		defer grabMu.Unlock()
		if grabCancel != nil {
			grabCancel()
			grabCancel = nil
		}
	}

	// The dashboard wraps grabber callbacks so it can publish SSE grabber events.
	// We wrap the callbacks to also update dashboard grabber state.
	var srv *dashboard.Server

	var grabTotalAdded atomic.Int64
	var grabLastAdded  atomic.Int64
	var grabLastRun    atomic.Value // stores string

	// Recheck state — controlled by dashboard HTTP handlers.
	var recheckMu     sync.Mutex
	var recheckCancel context.CancelFunc

	// Raw checker state — controlled by dashboard HTTP handlers.
	var checkerMu     sync.Mutex
	var checkerCancel context.CancelFunc

	cbs := dashboard.GrabberCallbacks{
		Start: func(urls []string, interval time.Duration) error {
			grabMu.Lock()
			if grabCancel != nil {
				grabMu.Unlock()
				return fmt.Errorf("grabber already running")
			}
			p, err := pool.New(pool.Config{
				URLs:        urls,
				Parallelism: 5,
				RedisDSN:    dsn,
			})
			if err != nil {
				grabMu.Unlock()
				return fmt.Errorf("init pool: %w", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			grabCancel = cancel
			grabMu.Unlock()

			gs := dashboard.GrabberStats{
				Running:    true,
				URLs:       urls,
				Interval:   interval.String(),
				LastAdded:  grabLastAdded.Load(),
				TotalAdded: grabTotalAdded.Load(),
				LastRun:    lastRunStr(grabLastRun),
			}
			srv.PublishGrabber(gs)

			go func() {
				defer p.Close()
				runGrabberLoop(ctx, p, interval, urls, srv, &grabTotalAdded, &grabLastAdded, &grabLastRun, &grabMu, &grabCancel)
				gs2 := dashboard.GrabberStats{
					Running:    false,
					URLs:       urls,
					Interval:   interval.String(),
					LastAdded:  grabLastAdded.Load(),
					TotalAdded: grabTotalAdded.Load(),
					LastRun:    lastRunStr(grabLastRun),
				}
				srv.PublishGrabber(gs2)
			}()
			return nil
		},
		Stop: func() {
			grabberStop()
		},
		ClearRaw:     rc.ClearRaw,
		ClearChecked: rc.ClearChecked,

		RecheckStart: func(interval time.Duration, workers int) error {
			recheckMu.Lock()
			defer recheckMu.Unlock()
			if recheckCancel != nil {
				return fmt.Errorf("recheck already running")
			}
			ctx, cancel := context.WithCancel(context.Background())
			recheckCancel = cancel
			srv.PublishRecheck(dashboard.RecheckStats{Running: true, Interval: interval.String(), Workers: workers})
			go func() {
				runRecheckLoop(ctx, rc, srv, workers, *timeout, interval)
				recheckMu.Lock()
				recheckCancel = nil
				recheckMu.Unlock()
				srv.PublishRecheck(dashboard.RecheckStats{Running: false, Interval: interval.String(), Workers: workers})
			}()
			return nil
		},
		RecheckStop: func() {
			recheckMu.Lock()
			defer recheckMu.Unlock()
			if recheckCancel != nil {
				recheckCancel()
				recheckCancel = nil
			}
		},

		CheckerStart: func(workers int) error {
			checkerMu.Lock()
			defer checkerMu.Unlock()
			if checkerCancel != nil {
				return fmt.Errorf("checker already running")
			}
			ctx, cancel := context.WithCancel(context.Background())
			checkerCancel = cancel
			srv.PublishChecker(dashboard.CheckerStats{Running: true, Workers: workers})
			go func() {
				runCheckLoop(ctx, rc, srv, workers, *timeout, true)
				checkerMu.Lock()
				checkerCancel = nil
				checkerMu.Unlock()
				srv.PublishChecker(dashboard.CheckerStats{Running: false, Workers: workers})
			}()
			return nil
		},
		CheckerStop: func() {
			checkerMu.Lock()
			defer checkerMu.Unlock()
			if checkerCancel != nil {
				checkerCancel()
				checkerCancel = nil
			}
		},
	}
	_ = grabberStart // replaced by cbs.Start above

	srv = dashboard.NewServer(rc.GetCheckedURIsTop, cbs)

	// Determine public URL for display
	publicHost := os.Getenv("PUBLIC_HOST")
	if publicHost == "" {
		publicHost = "localhost"
	}
	port := (*serve)
	if len(port) > 0 && port[0] == ':' {
		port = port[1:]
	}
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "┌─────────────────────────────────────────┐\n")
	fmt.Fprintf(os.Stderr, "│  Dashboard:  http://%s:%s/          \n", publicHost, port)
	fmt.Fprintf(os.Stderr, "│  Configs:    http://%s:%s/configs   \n", publicHost, port)
	fmt.Fprintf(os.Stderr, "└─────────────────────────────────────────┘\n")
	fmt.Fprintf(os.Stderr, "\n")

	go func() {
		if err := srv.Serve(*serve); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard error: %v\n", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *recheckInterval > 0 {
		go runRecheckLoop(ctx, rc, srv, *recheckWorkers, *timeout, *recheckInterval)
	}

	logf("[redis-checker] started — workers=%d timeout=%s recheck=%v recheck-interval=%s", *workers, *timeout, *recheck, *recheckInterval)
	runCheckLoop(ctx, rc, srv, *workers, *timeout, *recheck)
	logf("[redis-checker] stopped")
}

// runGrabberLoop runs pool.RunOnce immediately then on a ticker, publishing stats after each run.
func runGrabberLoop(
	ctx context.Context,
	p *pool.Pool,
	interval time.Duration,
	urls []string,
	srv *dashboard.Server,
	totalAdded, lastAdded *atomic.Int64,
	lastRun *atomic.Value,
	grabMu *sync.Mutex,
	grabCancel *context.CancelFunc,
) {
	publish := func(running bool) {
		gs := dashboard.GrabberStats{
			Running:    running,
			URLs:       urls,
			Interval:   interval.String(),
			LastAdded:  lastAdded.Load(),
			TotalAdded: totalAdded.Load(),
			LastRun:    lastRunStr(*lastRun),
		}
		srv.PublishGrabber(gs)
	}

	runOnce := func() {
		added, err := p.RunOnce(ctx)
		if err != nil {
			logf("[grabber] ERROR RunOnce: %v", err)
			return
		}
		lastAdded.Store(added)
		totalAdded.Add(added)
		lastRun.Store(time.Now().Format("2006-01-02 15:04:05"))
		publish(true)
		logf("[grabber] run done: +%d new URIs (total this session: %d)", added, totalAdded.Load())
	}

	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func lastRunStr(v atomic.Value) string {
	if s, ok := v.Load().(string); ok {
		return s
	}
	return ""
}

func runCheckLoop(ctx context.Context, rc *pool.RedisClient, srv *dashboard.Server,
	workers int, timeout time.Duration, recheck bool) {

	for {
		if ctx.Err() != nil {
			return
		}

		uris, err := rc.GetRawURIs(ctx)
		if err != nil {
			logf("ERROR GetRawURIs: %v — retrying in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		if len(uris) == 0 {
			logf("pool:raw is empty — waiting 30s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				continue
			}
		}

		total := int64(len(uris))
		logf("starting pass: %d URIs, %d workers", total, workers)
		srv.SetChecking(total)

		jobs := make(chan string, total)
		for _, u := range uris {
			jobs <- u
		}
		close(jobs)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			done    atomic.Int64
			deadCnt atomic.Int64
		)
		_ = mu

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for uri := range jobs {
					if ctx.Err() != nil {
						return
					}
					processURI(ctx, rc, srv, uri, timeout, &done, &deadCnt, total)
				}
			}()
		}
		wg.Wait()

		rawCnt, _ := rc.RawCount(ctx)
		aliveCnt, _ := rc.CheckedCount(ctx)
		d := deadCnt.Load()
		finalStats := dashboard.Stats{
			TotalRaw:     rawCnt,
			AliveCount:   aliveCnt,
			DeadCount:    d,
			SessionDone:  done.Load(),
			SessionTotal: total,
			Unchecked:    max64(0, rawCnt-aliveCnt),
		}
		srv.SetDone(finalStats)
		logf("pass done — alive=%d dead=%d raw_remaining=%d", aliveCnt, d, rawCnt)

		if !recheck {
			return
		}
		logf("recheck: starting next pass")
	}
}

func processURI(
	ctx context.Context,
	rc *pool.RedisClient,
	srv *dashboard.Server,
	uri string,
	timeout time.Duration,
	done *atomic.Int64,
	deadCnt *atomic.Int64,
	total int64,
) {
	cfg, err := parser.ParseLine(uri)
	if err != nil {
		_ = rc.RemoveRawURI(ctx, uri)
		n := done.Add(1)
		d := deadCnt.Add(1)
		stats := quickStats(ctx, rc, n, d, total)
		srv.PublishDead(uri, stats)
		return
	}

	idx := int(done.Load()) + 1
	result := checker.CheckConfig(idx, cfg, timeout)
	n := done.Add(1)

	if result.Alive {
		_ = rc.AddCheckedURI(ctx, uri, float64(result.Latency.Milliseconds()))
		entry := dashboard.CheckedEntry{
			RawURI:    uri,
			Name:      result.Name,
			Protocol:  result.Protocol,
			Server:    result.Server,
			Port:      result.Port,
			LatencyMs: result.Latency.Milliseconds(),
			ExitIP:    result.ExitIP,
			Country:   result.Country,
		}
		stats := quickStats(ctx, rc, n, deadCnt.Load(), total)
		srv.PublishAlive(entry, stats)
		logf("✔  %-35s %s  %dms  %s → %s", trunc(result.Name, 35), result.Protocol,
			result.Latency.Milliseconds(), result.ExitIP, result.Country)
	} else {
		_ = rc.RemoveRawURI(ctx, uri)
		d := deadCnt.Add(1)
		stats := quickStats(ctx, rc, n, d, total)
		srv.PublishDead(uri, stats)
		logf("✘  %-35s %s  %s", trunc(result.Name, 35), result.Protocol, trunc(result.Error, 50))
	}
}

// quickStats builds a Stats snapshot using in-memory counters + lightweight Redis calls.
func quickStats(ctx context.Context, rc *pool.RedisClient, done, dead, total int64) dashboard.Stats {
	rawCnt, _ := rc.RawCount(ctx)
	aliveCnt, _ := rc.CheckedCount(ctx)
	return dashboard.Stats{
		TotalRaw:     rawCnt,
		AliveCount:   aliveCnt,
		DeadCount:    dead,
		Unchecked:    max64(0, total-done),
		SessionDone:  done,
		SessionTotal: total,
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func runRecheckLoop(ctx context.Context, rc *pool.RedisClient, srv *dashboard.Server, workers int, timeout, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var totalRemoved, totalUpdated atomic.Int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		uris, err := rc.GetCheckedURIs(ctx)
		if err != nil || len(uris) == 0 {
			continue
		}
		logf("[recheck] starting pass: %d checked URIs", len(uris))

		jobs := make(chan string, len(uris))
		for _, u := range uris {
			jobs <- u
		}
		close(jobs)

		var wg sync.WaitGroup
		var removed, updated atomic.Int64

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for uri := range jobs {
					if ctx.Err() != nil {
						return
					}
					cfg, err := parser.ParseLine(uri)
					if err != nil {
						_ = rc.RemoveCheckedURI(ctx, uri)
						removed.Add(1)
						continue
					}
					result := checker.CheckConfig(0, cfg, timeout)
					if result.Alive {
						_ = rc.AddCheckedURI(ctx, uri, float64(result.Latency.Milliseconds()))
						updated.Add(1)
					} else {
						_ = rc.RemoveCheckedURI(ctx, uri)
						removed.Add(1)
						logf("[recheck] ✘ removed: %s", trunc(result.Name, 40))
					}
				}
			}()
		}
		wg.Wait()

		totalRemoved.Add(removed.Load())
		totalUpdated.Add(updated.Load())
		lastRun := time.Now().Format("2006-01-02 15:04:05")
		logf("[recheck] pass done — updated=%d removed=%d", updated.Load(), removed.Load())
		srv.PublishRecheck(dashboard.RecheckStats{
			Running:  true,
			Interval: interval.String(),
			Workers:  workers,
			LastRun:  lastRun,
			Removed:  totalRemoved.Load(),
			Updated:  totalUpdated.Load(),
		})
	}
}

func ts() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] "+format+"\n", append([]any{ts()}, args...)...)
}
