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

	// extract []ProxyConfig for CheckAll (keeps checker package decoupled from raw URIs)
	configs := make([]parser.ProxyConfig, len(entries))
	for i, e := range entries {
		configs[i] = e.Config
	}

	total := len(entries)
	fmt.Fprintf(os.Stderr, "%s%sVPN Checker%s — %d configs, %d workers, timeout %s\n%s\n",
		boldOn, colorCyan, colorReset, total, *workers, *timeout,
		strings.Repeat("─", 80))

	startAll := time.Now()
	alive := 0

	// Progress callback — called under mutex after each result
	onResult := func(r checker.Result, done, total int) {
		// Clear the spinner/progress line
		fmt.Fprintf(os.Stderr, "\r\033[K")

		// Print result line immediately
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

		// Draw progress bar on next line
		if done < total {
			pct := float64(done) / float64(total)
			barW := 40
			filled := int(pct * float64(barW))
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
			fmt.Fprintf(os.Stderr, "%s[%s] %3.0f%%  %d/%d done%s",
				colorCyan, bar, pct*100, done, total, colorReset)
		}
	}

	// Initial progress bar
	fmt.Fprintf(os.Stderr, "%s[%s] %3d%%  0/%d done%s",
		colorCyan, strings.Repeat("░", 40), 0, total, colorReset)

	results := checker.CheckAll(configs, *workers, *timeout, onResult)

	// Clear progress bar line after done
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

	if *jsonOut {
		printJSON(results)
	} else {
		printTable(results)
	}

	if *serveAddr != "" {
		aliveEntries := buildAliveEntries(results, entries)
		if len(aliveEntries) == 0 {
			fmt.Fprintln(os.Stderr, "no alive configs to serve")
			return
		}
		fmt.Fprintf(os.Stderr, "\n%sServing %d alive configs:%s\n  http://localhost%s/\n  http://localhost%s/configs\n",
			colorCyan, len(aliveEntries), colorReset, *serveAddr, *serveAddr)
		if err := web.Serve(*serveAddr, aliveEntries); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}
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

func disableColors() {
	colorReset = ""
	colorGreen = ""
	colorRed = ""
	colorYellow = ""
	colorCyan = ""
	colorGray = ""
	boldOn = ""
}
