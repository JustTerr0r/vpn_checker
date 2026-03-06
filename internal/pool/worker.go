package pool

import (
	"context"
	"net/http"
	"sync"
)

// FetchAll fetches all URLs in parallel using up to parallelism goroutines.
// Results are returned in arbitrary order (one per URL).
func FetchAll(ctx context.Context, client *http.Client, urls []string, parallelism int) []FetchResult {
	if parallelism > len(urls) {
		parallelism = len(urls)
	}
	if parallelism < 1 {
		parallelism = 1
	}

	jobs := make(chan string, len(urls))
	results := make(chan FetchResult, len(urls))

	var wg sync.WaitGroup
	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for url := range jobs {
				results <- FetchURL(ctx, client, url)
			}
		}()
	}

	for _, u := range urls {
		jobs <- u
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]FetchResult, 0, len(urls))
	for r := range results {
		out = append(out, r)
	}
	return out
}
