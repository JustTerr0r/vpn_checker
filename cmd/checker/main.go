package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"vpn_checker/internal/checker"
	"vpn_checker/internal/parser"
	"vpn_checker/internal/web"
)

// ConfigEntry pairs the original raw URI line with its parsed form.
type ConfigEntry struct {
	RawURI string
	Config parser.ProxyConfig
}

var (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	boldOn      = "\033[1m"
)

func main() {
	file := flag.String("f", "", "path to file with VPN configs (one per line); reads stdin if not set")
	workers := flag.Int("w", 5, "number of concurrent workers")
	timeout := flag.Duration("t", 10*time.Second, "timeout per config check")
	jsonOut := flag.Bool("json", false, "output results as JSON")
	noColor := flag.Bool("no-color", false, "disable ANSI colors")
	serveAddr := flag.String("serve", "", "serve alive configs on this address after check (e.g. :8080)")
	interval := flag.Duration("interval", 5*time.Minute, "how often to re-check configs for changes (0 = no auto re-check; requires -f)")
	recheck := flag.Duration("recheck", 10*time.Minute, "how often to re-validate already-alive configs and drop dead ones (0 = disabled)")
	flag.Parse()

	if *noColor {
		disableColors()
	}

	entries, err := readConfigs(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading configs: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no valid configs found")
		os.Exit(1)
	}

	// Create the web server immediately — it will serve live progress via SSE.
	srv := web.NewServer(nil)

	if *serveAddr != "" {
		fmt.Fprintf(os.Stderr, "\n%sServing live results:%s\n  http://localhost%s/\n  http://localhost%s/configs\n\n",
			colorCyan, colorReset, *serveAddr, *serveAddr)
		go func() {
			if err := srv.Serve(*serveAddr); err != nil {
				fmt.Fprintf(os.Stderr, "server error: %v\n", err)
				os.Exit(1)
			}
		}()
	}

	results := runCheck(entries, *workers, *timeout, srv)

	if *jsonOut {
		printJSON(results)
	} else {
		printTable(results)
	}

	if *serveAddr == "" {
		return
	}

	// Launch background watcher if -interval > 0 and a file path was given.
	if *interval > 0 && *file != "" {
		go watchAndRecheck(*file, *workers, *timeout, *interval, srv)
	} else if *interval > 0 && *file == "" {
		fmt.Fprintln(os.Stderr, "note: -interval ignored when reading from stdin")
	}

	// Launch background re-validator for already-alive configs.
	if *recheck > 0 {
		go recheckLoop(*timeout, *recheck, srv)
	}

	// Block forever (server already running in goroutine).
	select {}
}

// watchAndRecheck polls the file every interval. When the file's mtime changes
// it re-reads configs, runs a fresh check, and updates the web server.
func watchAndRecheck(filePath string, workers int, timeout, interval time.Duration, srv *web.Server) {
	lastMtime := fileMtime(filePath)

	for {
		nextAt := time.Now().Add(interval)

		// Update "next check in" countdown on the web page every 30s while waiting.
		ticker := time.NewTicker(30 * time.Second)
		timer := time.NewTimer(interval)

	waitLoop:
		for {
			select {
			case <-timer.C:
				ticker.Stop()
				break waitLoop
			case <-ticker.C:
				remaining := time.Until(nextAt).Round(time.Second)
				srv.UpdateNextCheckIn(remaining.String())
			}
		}

		// Check if file has changed.
		mtime := fileMtime(filePath)
		if mtime.Equal(lastMtime) {
			fmt.Fprintf(os.Stderr, "\n%s[watcher]%s %s — no changes detected, skipping re-check\n",
				colorGray, colorReset, time.Now().Format("15:04:05"))
			srv.UpdateNextCheckIn(interval.String())
			continue
		}

		lastMtime = mtime
		fmt.Fprintf(os.Stderr, "\n%s[watcher]%s %s — file changed, re-checking configs…\n",
			colorCyan, colorReset, time.Now().Format("15:04:05"))

		entries, err := readConfigs(filePath)
		if err != nil || len(entries) == 0 {
			fmt.Fprintf(os.Stderr, "%s[watcher]%s error reading configs: %v\n", colorRed, colorReset, err)
			continue
		}

		results := runCheck(entries, workers, timeout, srv)
		aliveEntries := buildAliveEntries(results, entries)

		nextCheckIn := interval.String()
		srv.AppendEntries(aliveEntries, nextCheckIn)

		fmt.Fprintf(os.Stderr, "%s[watcher]%s updated web server — %d alive configs\n",
			colorGreen, colorReset, len(aliveEntries))
	}
}

// recheckLoop cycles through alive entries from oldest to newest, re-validates
// each one with a pause of (interval / total) between checks so the full cycle
// takes approximately interval. Dead configs are removed from the web server.
func recheckLoop(timeout, interval time.Duration, srv *web.Server) {
	// pos tracks where in the list we left off across iterations.
	pos := 0

	for {
		entries := srv.Entries()
		if len(entries) == 0 {
			time.Sleep(30 * time.Second)
			pos = 0
			continue
		}

		// Wrap around when we've reached the end.
		if pos >= len(entries) {
			pos = 0
			entries = srv.Entries()
			if len(entries) == 0 {
				time.Sleep(30 * time.Second)
				continue
			}
		}

		e := entries[pos]
		pos++

		// Re-parse the raw URI to get a fresh ProxyConfig.
		cfg, err := parser.ParseLine(e.RawURI)
		if err != nil {
			// Can't parse — treat as dead and remove.
			srv.RemoveEntry(aliveEntryKey(e))
			continue
		}

		r := checker.CheckConfig(0, cfg, timeout)
		key := aliveEntryKey(e)

		if r.Alive {
			fmt.Fprintf(os.Stderr, "%s[recheck]%s ✔  %s — still alive (%dms)\n",
				colorGreen, colorReset, truncate(e.Result.Name, 35), r.Latency.Milliseconds())
		} else {
			fmt.Fprintf(os.Stderr, "%s[recheck]%s ✘  %s — dead, removing (%s)\n",
				colorRed, colorReset, truncate(e.Result.Name, 35), truncate(r.Error, 40))
			srv.RemoveEntry(key)
		}

		// Spread checks evenly across the interval.
		pause := interval / time.Duration(len(entries))
		if pause < time.Second {
			pause = time.Second
		}
		time.Sleep(pause)
	}
}

// fileMtime returns the modification time of a file, or zero on error.
func fileMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// runCheck runs the full check pipeline and prints progress + summary to stderr.
// If srv is non-nil, each result is published via SSE in real time.
func runCheck(entries []ConfigEntry, workers int, timeout time.Duration, srv *web.Server) []checker.Result {
	configs := make([]parser.ProxyConfig, len(entries))
	for i, e := range entries {
		configs[i] = e.Config
	}

	total := len(entries)
	fmt.Fprintf(os.Stderr, "%s%sVPN Checker%s — %d configs, %d workers, timeout %s\n%s\n",
		boldOn, colorCyan, colorReset, total, workers, timeout,
		strings.Repeat("─", 80))

	if srv != nil {
		srv.SetChecking(total)
	}

	startAll := time.Now()
	alive := 0

	onResult := func(r checker.Result, done, total int) {
		fmt.Fprintf(os.Stderr, "\r\033[K")

		if r.Alive {
			alive++
			fmt.Fprintf(os.Stderr, "%s[%3d/%-3d]%s %s✔%s  %-30s %s%-12s%s %s%dms%s  %s → %s%s\n",
				colorGray, done, total, colorReset,
				colorGreen, colorReset,
				truncate(r.Name, 30),
				colorGray, r.Protocol, colorReset,
				colorYellow, r.Latency.Milliseconds(), colorReset,
				r.ExitIP, r.Country,
				colorReset,
			)
		} else {
			fmt.Fprintf(os.Stderr, "%s[%3d/%-3d]%s %s✘%s  %-30s %s%-12s%s  %s%s%s\n",
				colorGray, done, total, colorReset,
				colorRed, colorReset,
				truncate(r.Name, 30),
				colorGray, r.Protocol, colorReset,
				colorRed, truncate(r.Error, 45), colorReset,
			)
		}

		if done < total {
			pct := float64(done) / float64(total)
			barW := 40
			filled := int(pct * float64(barW))
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
			fmt.Fprintf(os.Stderr, "%s[%s] %3.0f%%  %d/%d done%s",
				colorCyan, bar, pct*100, done, total, colorReset)
		}

		if srv != nil {
			rawURI := ""
			if r.Index >= 1 && r.Index <= len(entries) {
				rawURI = entries[r.Index-1].RawURI
			}
			srv.PublishResult(web.AliveEntry{Result: r, RawURI: rawURI}, done, total)
		}
	}

	fmt.Fprintf(os.Stderr, "%s[%s] %3d%%  0/%d done%s",
		colorCyan, strings.Repeat("░", 40), 0, total, colorReset)

	results := checker.CheckAll(configs, workers, timeout, onResult)

	fmt.Fprintf(os.Stderr, "\r\033[K")

	elapsed := time.Since(startAll)
	dead := total - alive
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("─", 80))
	fmt.Fprintf(os.Stderr, "%s%sDone in %s%s  Total: %d  %s✔ Alive: %d%s  %s✘ Dead: %d%s\n\n",
		boldOn, colorCyan, elapsed.Round(time.Millisecond), colorReset,
		total,
		colorGreen, alive, colorReset,
		colorRed, dead, colorReset,
	)

	if srv != nil {
		srv.SetDone()
	}

	return results
}

func readConfigs(filePath string) ([]ConfigEntry, error) {
	var src *os.File
	if filePath != "" {
		f, err := os.Open(filePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		src = f
	} else {
		src = os.Stdin
	}

	var entries []ConfigEntry
	scanner := bufio.NewScanner(src)
	for scanner.Scan() {
		line := scanner.Text()
		cfg, err := parser.ParseLine(line)
		if err != nil {
			continue
		}
		entries = append(entries, ConfigEntry{RawURI: line, Config: cfg})
	}
	return entries, scanner.Err()
}

func buildAliveEntries(results []checker.Result, entries []ConfigEntry) []web.AliveEntry {
	var out []web.AliveEntry
	for _, r := range results {
		if !r.Alive {
			continue
		}
		rawURI := ""
		if r.Index >= 1 && r.Index <= len(entries) {
			rawURI = entries[r.Index-1].RawURI
		}
		out = append(out, web.AliveEntry{Result: r, RawURI: rawURI})
	}
	return out
}

func printTable(results []checker.Result) {
	sep := strings.Repeat("─", 120)
	fmt.Printf("%s%-3s │ %-30s │ %-12s │ %-22s │ %-8s │ %-9s │ %-16s │ %s%s\n",
		boldOn, "#", "NAME", "PROTO", "SERVER", "STATUS", "LATENCY", "EXIT IP", "COUNTRY", colorReset)
	fmt.Println(sep)

	for _, r := range results {
		status := colorRed + "✘ FAIL" + colorReset
		latency := "-"
		exitIP := "-"
		country := "-"

		if r.Alive {
			status = colorGreen + "✔ OK  " + colorReset
			latency = fmt.Sprintf("%dms", r.Latency.Milliseconds())
			exitIP = r.ExitIP
			country = r.Country
		}

		server := fmt.Sprintf("%s:%d", r.Server, r.Port)
		name := r.Name

		fmt.Printf("%-3d │ %-30s │ %-12s │ %-22s │ %s │ %-9s │ %-16s │ %s\n",
			r.Index, truncate(name, 30), r.Protocol, truncate(server, 22),
			status, latency, exitIP, country)

		if !r.Alive && r.Error != "" {
			fmt.Printf("    │ %serror: %s%s\n", colorRed, truncate(r.Error, 100), colorReset)
		}
	}

	fmt.Println(sep)

	alive := 0
	for _, r := range results {
		if r.Alive {
			alive++
		}
	}
	fmt.Printf("%sTotal: %d  Alive: %d%s  Dead: %d\n",
		boldOn, len(results), alive, colorReset, len(results)-alive)
}

func printJSON(results []checker.Result) {
	type jsonResult struct {
		Index     int    `json:"index"`
		Name      string `json:"name"`
		Protocol  string `json:"protocol"`
		Server    string `json:"server"`
		Port      int    `json:"port"`
		Alive     bool   `json:"alive"`
		LatencyMs int64  `json:"latency_ms,omitempty"`
		ExitIP    string `json:"exit_ip,omitempty"`
		Country   string `json:"country,omitempty"`
		Error     string `json:"error,omitempty"`
	}

	out := make([]jsonResult, len(results))
	for i, r := range results {
		out[i] = jsonResult{
			Index:    r.Index,
			Name:     r.Name,
			Protocol: r.Protocol,
			Server:   r.Server,
			Port:     r.Port,
			Alive:    r.Alive,
			ExitIP:   r.ExitIP,
			Country:  r.Country,
			Error:    r.Error,
		}
		if r.Alive {
			out[i].LatencyMs = r.Latency.Milliseconds()
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func aliveEntryKey(e web.AliveEntry) string {
	if e.RawURI != "" {
		return e.RawURI
	}
	return fmt.Sprintf("%s:%d", e.Result.Server, e.Result.Port)
}

func disableColors() {
	colorReset = ""
	colorGreen = ""
	colorRed = ""
	colorYellow = ""
	colorCyan = ""
	colorGray = ""
	boldOn = ""
}
