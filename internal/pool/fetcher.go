package pool

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vpn_checker/internal/parser"
)

const fetchTimeout = 30 * time.Second

// FetchResult holds the outcome of fetching a single URL.
type FetchResult struct {
	URL  string
	URIs []string // valid raw URI strings (parsed successfully by parser.ParseLine)
	Err  error
}

// FetchURL downloads the URL, scans it line-by-line, and returns only lines
// that parser.ParseLine accepts as valid VPN configs.
func FetchURL(ctx context.Context, client *http.Client, url string) FetchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchResult{URL: url, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("User-Agent", "vpn-pool-worker/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{URL: url, Err: fmt.Errorf("http get: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FetchResult{URL: url, Err: fmt.Errorf("http status %d", resp.StatusCode)}
	}

	var uris []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, err := parser.ParseLine(line); err == nil {
			uris = append(uris, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return FetchResult{URL: url, Err: fmt.Errorf("scan body: %w", err)}
	}

	return FetchResult{URL: url, URIs: uris}
}

func logf(format string, args ...any) {
	fmt.Printf("[%s] "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
}
