package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"vpn_checker/internal/parser"
	xrayrunner "vpn_checker/internal/xray"
)

// Result holds the outcome of checking a single proxy config
type Result struct {
	Index    int
	Name     string
	Protocol string
	Server   string
	Port     int
	Alive    bool
	Latency  time.Duration
	ExitIP   string
	Country  string
	Error    string
}

type ipAPIResponse struct {
	Query       string `json:"query"`
	CountryName string `json:"country"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}

// CheckConfig checks a single proxy config and returns a Result
func CheckConfig(idx int, cfg parser.ProxyConfig, timeout time.Duration) Result {
	result := Result{
		Index:    idx,
		Name:     cfg.GetName(),
		Protocol: cfg.GetProtocol(),
		Server:   cfg.GetServer(),
		Port:     cfg.GetPort(),
	}

	// Find a free local port for SOCKS5
	socksPort, err := freePort()
	if err != nil {
		result.Error = fmt.Sprintf("no free port: %v", err)
		return result
	}

	// Generate xray config
	configJSON, err := xrayrunner.GenerateConfig(cfg, socksPort)
	if err != nil {
		result.Error = fmt.Sprintf("config gen: %v", err)
		return result
	}

	// Start xray
	cmd, err := xrayrunner.Start(configJSON)
	if err != nil {
		result.Error = fmt.Sprintf("xray start: %v", err)
		return result
	}
	defer xrayrunner.Stop(cmd)

	// Wait for xray SOCKS5 to become ready
	if err := waitForPort("127.0.0.1", socksPort, 3*time.Second); err != nil {
		result.Error = fmt.Sprintf("xray not ready: %v", err)
		return result
	}

	// Create SOCKS5 dialer
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		result.Error = fmt.Sprintf("socks5 dialer: %v", err)
		return result
	}

	// Create HTTP client with SOCKS5 transport
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	// Measure latency via HTTP GET
	start := time.Now()
	resp, err := client.Get("http://ip-api.com/json")
	if err != nil {
		result.Error = fmt.Sprintf("http get: %v", err)
		return result
	}
	defer resp.Body.Close()
	result.Latency = time.Since(start)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Error = fmt.Sprintf("read body: %v", err)
		return result
	}

	var apiResp ipAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		result.Error = fmt.Sprintf("json parse: %v", err)
		return result
	}

	if apiResp.Status != "success" {
		result.Error = fmt.Sprintf("ip-api: %s", apiResp.Message)
		return result
	}

	result.Alive = true
	result.ExitIP = apiResp.Query
	result.Country = apiResp.CountryName
	return result
}

// CheckAll runs CheckConfig concurrently with the given number of workers.
// onResult is called (under a mutex) immediately after each config finishes — use it for live progress output.
func CheckAll(configs []parser.ProxyConfig, workers int, timeout time.Duration, onResult func(Result, int, int)) []Result {
	total := len(configs)
	results := make([]Result, total)
	jobs := make(chan int, total)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		done    int
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				r := CheckConfig(idx+1, configs[idx], timeout)
				mu.Lock()
				results[idx] = r
				done++
				if onResult != nil {
					onResult(r, done, total)
				}
				mu.Unlock()
			}
		}()
	}

	for i := range configs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	return results
}

// freePort finds an available TCP port on localhost
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// waitForPort polls until the given TCP address is accepting connections or timeout
func waitForPort(host string, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}
