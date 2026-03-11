package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"vpn_checker/internal/parser"
)

// CheckedEntry is a live config result ready to display in the dashboard.
type CheckedEntry struct {
	RawURI    string `json:"RawURI"`
	Name      string `json:"Name"`
	Protocol  string `json:"Protocol"`
	Server    string `json:"Server"`
	Port      int    `json:"Port"`
	LatencyMs int64  `json:"LatencyMs"`
	ExitIP    string `json:"ExitIP"`
	Country   string `json:"Country"`
}

// Stats is a snapshot of pool state and current session progress.
type Stats struct {
	TotalRaw     int64 `json:"TotalRaw"`     // current SCARD pool:raw
	AliveCount   int64 `json:"AliveCount"`   // current ZCARD pool:checked
	DeadCount    int64 `json:"DeadCount"`    // removed dead in this session
	Unchecked    int64 `json:"Unchecked"`    // TotalRaw - SessionDone
	SessionDone  int64 `json:"SessionDone"`  // checked in current pass
	SessionTotal int64 `json:"SessionTotal"` // total to check in current pass
}

// GrabberStats is published via SSE and returned from /grabber/status.
type GrabberStats struct {
	Running    bool     `json:"running"`
	URLs       []string `json:"urls"`
	Interval   string   `json:"interval"`
	LastRun    string   `json:"last_run,omitempty"`
	LastAdded  int64    `json:"last_added"`
	TotalAdded int64    `json:"total_added"`
}

// RecheckStats is published via SSE for the pool:checked recheck loop.
type RecheckStats struct {
	Running  bool   `json:"running"`
	Interval string `json:"interval"`
	Workers  int    `json:"workers"`
	LastRun  string `json:"last_run,omitempty"`
	Removed  int64  `json:"removed"`
	Updated  int64  `json:"updated"`
}

// CheckerStats is published via SSE for the raw pool checker loop.
type CheckerStats struct {
	Running  bool   `json:"running"`
	Workers  int    `json:"workers"`
	LastRun  string `json:"last_run,omitempty"`
}

// GrabberCallbacks are provided by main to start/stop the grabber goroutine.
type GrabberCallbacks struct {
	Start        func(urls []string, interval time.Duration) error
	Stop         func()
	ClearRaw     func(ctx context.Context) error
	ClearChecked func(ctx context.Context) error

	RecheckStart func(interval time.Duration, workers int) error
	RecheckStop  func()

	CheckerStart func(workers int) error
	CheckerStop  func()
}

// sseEvent is the wire format for Server-Sent Events.
type sseEvent struct {
	Type        string        `json:"type"` // "alive" | "dead" | "stats" | "done" | "grabber" | "recheck" | "checker" | "config_limit"
	Entry       *CheckedEntry `json:"entry,omitempty"`
	URI         string        `json:"uri,omitempty"`
	Stats       Stats         `json:"stats"`
	CheckedAt   string        `json:"checked_at,omitempty"`
	Grabber     *GrabberStats `json:"grabber,omitempty"`
	Recheck     *RecheckStats `json:"recheck,omitempty"`
	Checker     *CheckerStats `json:"checker,omitempty"`
	ConfigLimit int           `json:"config_limit,omitempty"`
}

type serverState struct {
	entries    []CheckedEntry
	uriCountry map[string]string // rawURI → country code
	stats      Stats
	checking   bool
	checkedAt  string
}

// Server is an HTTP dashboard with live SSE updates for the redis-checker.
type Server struct {
	mu    sync.RWMutex
	state serverState

	getCheckedURIs func(ctx context.Context, limit int) ([]string, error)

	// config limit (0 = all)
	configLimitMu sync.RWMutex
	configLimit   int

	// grabber state
	grabMu   sync.Mutex
	grabStat GrabberStats
	grabCbs  GrabberCallbacks

	// recheck state
	recheckMu   sync.Mutex
	recheckStat RecheckStats

	// raw checker state
	checkerMu   sync.Mutex
	checkerStat CheckerStats

	sseMu      sync.Mutex
	sseClients map[chan []byte]struct{}
}

// NewServer creates a dashboard Server.
// getCheckedURIs is called on each GET /configs request to return sorted live URIs.
func NewServer(getCheckedURIs func(ctx context.Context, limit int) ([]string, error), cbs GrabberCallbacks) *Server {
	return &Server{
		getCheckedURIs: getCheckedURIs,
		grabCbs:        cbs,
		grabStat:       GrabberStats{URLs: []string{}},
		sseClients:     make(map[chan []byte]struct{}),
		state:          serverState{uriCountry: make(map[string]string)},
	}
}

// SetChecking marks the start of a new check pass.
func (s *Server) SetChecking(sessionTotal int64) {
	s.mu.Lock()
	s.state.checking = true
	s.state.stats.SessionTotal = sessionTotal
	s.state.stats.SessionDone = 0
	s.mu.Unlock()
}

// PublishAlive records a live result and broadcasts an SSE "alive" event.
func (s *Server) PublishAlive(e CheckedEntry, stats Stats) {
	s.mu.Lock()
	s.state.entries = append(s.state.entries, e)
	s.state.stats = stats
	if e.Country != "" {
		s.state.uriCountry[e.RawURI] = e.Country
	}
	s.mu.Unlock()
	s.broadcast(sseEvent{Type: "alive", Entry: &e, Stats: stats})
}

// PublishDead broadcasts an SSE "dead" event (entry not added to table).
func (s *Server) PublishDead(uri string, stats Stats) {
	s.mu.Lock()
	s.state.stats = stats
	s.mu.Unlock()
	s.broadcast(sseEvent{Type: "dead", URI: uri, Stats: stats})
}

// SetDone marks the pass as finished and broadcasts a "done" SSE event.
func (s *Server) SetDone(stats Stats) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	s.mu.Lock()
	s.state.checking = false
	s.state.stats = stats
	s.state.checkedAt = now
	s.mu.Unlock()
	s.broadcast(sseEvent{Type: "done", Stats: stats, CheckedAt: now})
}

// PublishGrabber updates grabber state and broadcasts it via SSE.
func (s *Server) PublishGrabber(gs GrabberStats) {
	s.grabMu.Lock()
	s.grabStat = gs
	s.grabMu.Unlock()
	s.broadcast(sseEvent{Type: "grabber", Grabber: &gs})
}

// PublishRecheck updates recheck state and broadcasts it via SSE.
func (s *Server) PublishRecheck(rs RecheckStats) {
	s.recheckMu.Lock()
	s.recheckStat = rs
	s.recheckMu.Unlock()
	s.broadcast(sseEvent{Type: "recheck", Recheck: &rs})
}

// PublishChecker updates raw checker state and broadcasts it via SSE.
func (s *Server) PublishChecker(cs CheckerStats) {
	s.checkerMu.Lock()
	s.checkerStat = cs
	s.checkerMu.Unlock()
	s.broadcast(sseEvent{Type: "checker", Checker: &cs})
}

// Serve starts the HTTP dashboard on addr and blocks.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/configs", s.handleConfigs)
	mux.HandleFunc("/grabber/start", s.handleGrabberStart)
	mux.HandleFunc("/grabber/stop", s.handleGrabberStop)
	mux.HandleFunc("/grabber/status", s.handleGrabberStatus)
	mux.HandleFunc("/pool/clear-raw", s.handleClearRaw)
	mux.HandleFunc("/pool/clear-checked", s.handleClearChecked)
	mux.HandleFunc("/recheck/start", s.handleRecheckStart)
	mux.HandleFunc("/recheck/stop", s.handleRecheckStop)
	mux.HandleFunc("/checker/start", s.handleCheckerStart)
	mux.HandleFunc("/checker/stop", s.handleCheckerStop)
	mux.HandleFunc("/configs/limit", s.handleConfigsLimit)
	return http.ListenAndServe(addr, mux)
}

// ---- grabber HTTP handlers ----

type grabStartRequest struct {
	URLs     []string `json:"urls"`
	Interval string   `json:"interval"`
}

func (s *Server) handleGrabberStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req grabStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		http.Error(w, "urls required", http.StatusBadRequest)
		return
	}
	interval := 10 * time.Minute
	if req.Interval != "" {
		if d, err := time.ParseDuration(req.Interval); err == nil {
			interval = d
		}
	}

	if err := s.grabCbs.Start(req.URLs, interval); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) handleGrabberStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.grabCbs.Stop()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

func (s *Server) handleGrabberStatus(w http.ResponseWriter, r *http.Request) {
	s.grabMu.Lock()
	gs := s.grabStat
	s.grabMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gs)
}

func (s *Server) handleClearRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := s.grabCbs.ClearRaw(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func (s *Server) handleClearChecked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := s.grabCbs.ClearChecked(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Also clear in-memory entries so dashboard table resets.
	s.mu.Lock()
	s.state.entries = nil
	s.state.uriCountry = make(map[string]string)
	s.state.stats.AliveCount = 0
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

// ---- recheck HTTP handlers ----

type recheckStartRequest struct {
	Interval string `json:"interval"`
	Workers  int    `json:"workers"`
}

func (s *Server) handleRecheckStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req recheckStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	interval := 30 * time.Minute
	if req.Interval != "" {
		if d, err := time.ParseDuration(req.Interval); err == nil {
			interval = d
		}
	}
	workers := req.Workers
	if workers <= 0 {
		workers = 5
	}
	if err := s.grabCbs.RecheckStart(interval, workers); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) handleRecheckStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.grabCbs.RecheckStop()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

// ---- raw checker HTTP handlers ----

type checkerStartRequest struct {
	Workers int `json:"workers"`
}

func (s *Server) handleCheckerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req checkerStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	workers := req.Workers
	if workers <= 0 {
		workers = 10
	}
	if err := s.grabCbs.CheckerStart(workers); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) handleCheckerStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.grabCbs.CheckerStop()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

// ---- configs limit HTTP handler ----

type configsLimitRequest struct {
	Limit int `json:"limit"`
}

func (s *Server) handleConfigsLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req configsLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Limit < 0 {
		req.Limit = 0
	}
	s.configLimitMu.Lock()
	s.configLimit = req.Limit
	s.configLimitMu.Unlock()
	s.broadcast(sseEvent{Type: "config_limit", ConfigLimit: req.Limit})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"limit": req.Limit})
}

// ---- SSE broker ----

func (s *Server) broadcast(ev sseEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	msg := append([]byte("data: "), data...)
	msg = append(msg, '\n', '\n')

	s.sseMu.Lock()
	for ch := range s.sseClients {
		select {
		case ch <- msg:
		default:
		}
	}
	s.sseMu.Unlock()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 64)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
	}()

	// Send current snapshot to late-joiner.
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	for _, e := range st.entries {
		ev := sseEvent{Type: "alive", Entry: &e, Stats: st.stats}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	{
		ev := sseEvent{Type: "stats", Stats: st.stats}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	if !st.checking && st.checkedAt != "" {
		ev := sseEvent{Type: "done", Stats: st.stats, CheckedAt: st.checkedAt}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	// Send grabber snapshot.
	s.grabMu.Lock()
	gs := s.grabStat
	s.grabMu.Unlock()
	{
		ev := sseEvent{Type: "grabber", Grabber: &gs}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	// Send recheck snapshot.
	s.recheckMu.Lock()
	rs := s.recheckStat
	s.recheckMu.Unlock()
	{
		ev := sseEvent{Type: "recheck", Recheck: &rs}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	// Send checker snapshot.
	s.checkerMu.Lock()
	cs := s.checkerStat
	s.checkerMu.Unlock()
	{
		ev := sseEvent{Type: "checker", Checker: &cs}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	// Send config limit snapshot.
	s.configLimitMu.RLock()
	cl := s.configLimit
	s.configLimitMu.RUnlock()
	{
		ev := sseEvent{Type: "config_limit", ConfigLimit: cl}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	flusher.Flush()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			w.Write(msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, htmlPage)
}

func (s *Server) handleConfigs(w http.ResponseWriter, r *http.Request) {
	// query param ?limit=N overrides server-side limit
	limit := 0
	if qs := r.URL.Query().Get("limit"); qs != "" {
		if n, err := strconv.Atoi(qs); err == nil && n >= 0 {
			limit = n
		}
	} else {
		s.configLimitMu.RLock()
		limit = s.configLimit
		s.configLimitMu.RUnlock()
	}

	uris, err := s.getCheckedURIs(r.Context(), limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("redis error: %v", err), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	countryMap := s.state.uriCountry
	s.mu.RUnlock()

	renamed := make([]string, len(uris))
	for i, uri := range uris {
		country := countryMap[uri]
		name := buildName(country)
		renamed[i] = parser.RenameURI(uri, name)
	}

	// Expire = 10 years from now, effectively unlimited.
	expire := time.Now().AddDate(10, 0, 0).Unix()

	announce := "base64:" + base64.StdEncoding.EncodeToString(
		[]byte("Вы пользуетесь бесплатной версией Babyl0n"))

	body := strings.Join(renamed, "\n")

	// Happ requires lowercase header names — bypass Go's canonicalization
	// by writing the raw HTTP response directly.
	hdr := w.Header()
	hdr["Content-Type"] = []string{"text/plain; charset=utf-8"}
	hdr["profile-title"] = []string{"Babyl0n Free"}
	hdr["profile-update-interval"] = []string{"12"}
	hdr["support-url"] = []string{"https://t.me/vabes"}
	hdr["subscription-userinfo"] = []string{
		fmt.Sprintf("upload=0; download=0; total=0; expire=%d", expire)}
	hdr["announce"] = []string{announce}
	hdr["content-disposition"] = []string{`attachment; filename="Babyl0n Free"`}
	hdr["hide-settings"] = []string{"1"}
	fmt.Fprint(w, body)
}

const configNameSuffix = " t.me/vpn0̸y - всегда рабочий VPN"

func buildName(country string) string {
	if country == "" {
		return "🌐 VPN" + configNameSuffix
	}
	return countryFlag(country) + " " + country + configNameSuffix
}

func countryFlag(code string) string {
	if len(code) != 2 {
		return "🌐"
	}
	r0 := rune(code[0]-'A') + 0x1F1E6
	r1 := rune(code[1]-'A') + 0x1F1E6
	return string([]rune{r0, r1})
}

const htmlPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Redis Pool Checker — Live</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#c9d1d9;padding:2rem;min-height:100vh}
h1{font-size:1.4rem;font-weight:700;color:#58a6ff;margin-bottom:.25rem}
h2{font-size:1rem;font-weight:600;color:#8b949e;margin-bottom:.75rem}
.meta{font-size:.82rem;color:#484f58;margin-bottom:1.2rem}
/* Stat cards */
.stats-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.75rem;margin-bottom:1.5rem}
.stat-card{background:#161b22;border:1px solid #21262d;border-radius:8px;padding:.9rem 1rem}
.stat-value{font-size:1.6rem;font-weight:700;font-variant-numeric:tabular-nums;color:#c9d1d9;line-height:1}
.stat-value.green{color:#3fb950}
.stat-value.red{color:#f85149}
.stat-value.blue{color:#58a6ff}
.stat-value.gray{color:#8b949e}
.stat-value.yellow{color:#d29922}
.stat-label{font-size:.72rem;color:#484f58;margin-top:.35rem}
/* Status bar */
.status-bar{display:flex;align-items:center;gap:.75rem;margin-bottom:1.2rem;flex-wrap:wrap}
.status-label{font-size:.8rem;color:#8b949e}
.progress-wrap{flex:1;min-width:120px;max-width:320px;background:#21262d;border-radius:6px;height:8px;overflow:hidden}
.progress-fill{height:100%;background:#1f6feb;border-radius:6px;transition:width .3s}
.pulse{display:inline-block;width:8px;height:8px;border-radius:50%;background:#3fb950;animation:pulse 1.2s ease-in-out infinite}
.pulse.done{background:#484f58;animation:none}
.pulse.grab{background:#d29922;animation:pulse 1.2s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}
/* Grabber panel */
.grabber-panel{background:#0f1923;border:1px solid #21262d;border-radius:10px;padding:1.2rem 1.4rem;margin-bottom:1.5rem}
.grabber-panel h2{color:#d29922}
.grabber-stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:.6rem;margin-bottom:1rem}
.grab-stat-card{background:#161b22;border:1px solid #21262d;border-radius:6px;padding:.6rem .8rem}
.grab-stat-card .stat-value{font-size:1.2rem}
.grabber-form{display:flex;flex-direction:column;gap:.6rem}
.grabber-form label{font-size:.78rem;color:#8b949e}
.grabber-form textarea{width:100%;height:80px;background:#161b22;border:1px solid #30363d;border-radius:6px;color:#c9d1d9;font-family:monospace;font-size:.75rem;padding:.5rem;resize:vertical;outline:none}
.grabber-form textarea:focus{border-color:#388bfd}
.grabber-row{display:flex;gap:.6rem;align-items:center;flex-wrap:wrap}
.grabber-row input{background:#161b22;border:1px solid #30363d;border-radius:6px;color:#c9d1d9;font-size:.8rem;padding:.35rem .6rem;width:90px;outline:none}
.grabber-row input:focus{border-color:#388bfd}
.grab-status{font-size:.78rem;color:#8b949e}
/* Actions */
.actions{display:flex;align-items:center;gap:1rem;margin-bottom:1.25rem;flex-wrap:wrap}
.btn{cursor:pointer;padding:.4rem 1rem;border:none;border-radius:6px;font-size:.82rem;font-weight:600;background:#1f6feb;color:#fff;transition:background .15s}
.btn:hover{background:#388bfd}
.btn:disabled{background:#21262d;color:#484f58;cursor:default}
.btn-sm{padding:.25rem .65rem;font-size:.75rem;background:#21262d;color:#8b949e;border:1px solid #30363d}
.btn-sm:hover{background:#30363d;color:#c9d1d9}
.btn-danger{background:#b91c1c}
.btn-danger:hover{background:#dc2626}
.btn-warning{background:#92400e}
.btn-warning:hover{background:#b45309}
a.link{color:#58a6ff;font-size:.82rem;text-decoration:none}
a.link:hover{text-decoration:underline}
.alive-label{font-size:.82rem;color:#8b949e;margin-left:auto}
/* Table */
table{width:100%;border-collapse:collapse;font-size:.8rem;table-layout:fixed}
thead th{background:#161b22;color:#8b949e;font-weight:600;text-align:left;padding:.45rem .5rem;border-bottom:1px solid #21262d;white-space:nowrap;overflow:hidden}
tbody td{padding:.38rem .5rem;border-bottom:1px solid #161b22;vertical-align:middle;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
tbody tr:hover td{background:#161b22}
tbody tr.new-row{animation:fadeIn .4s ease}
@keyframes fadeIn{from{background:#0d2a4a}to{background:transparent}}
col.c-num{width:2.5rem}
col.c-name{width:12rem}
col.c-proto{width:6rem}
col.c-server{width:11rem}
col.c-latency{width:5rem}
col.c-ip{width:8rem}
col.c-country{width:7rem}
col.c-uri{width:auto}
.badge{display:inline-block;padding:.12rem .45rem;border-radius:12px;font-size:.7rem;font-weight:700;letter-spacing:.02em}
.badge.vless{background:#1a3a6e;color:#79c0ff}
.badge.shadowsocks{background:#0d3326;color:#56d364}
.badge.vmess{background:#3a2010;color:#ffa657}
.badge.trojan{background:#2d1a4a;color:#d2a8ff}
.latency{color:#3fb950;font-variant-numeric:tabular-nums}
.server{font-family:monospace;font-size:.75rem;color:#8b949e}
.name-cell{overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
.uri-cell{overflow:hidden}
.uri-text{font-family:monospace;font-size:.7rem;color:#484f58;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;display:block;width:100%}
.copy-row{display:flex;align-items:center;gap:.3rem}
.toast{position:fixed;bottom:1.5rem;right:1.5rem;background:#238636;color:#fff;padding:.5rem 1rem;border-radius:8px;font-size:.82rem;opacity:0;transition:opacity .3s;pointer-events:none;z-index:999}
.toast.show{opacity:1}
.section-title{font-size:1rem;font-weight:600;color:#8b949e;margin-bottom:.75rem;margin-top:1.5rem}
</style>
</head>
<body>
<h1>Redis Pool Checker — Live</h1>
<p class="meta" id="checkedAt">Connecting…</p>

<div class="stats-grid">
  <div class="stat-card">
    <div class="stat-value blue" id="statRaw">–</div>
    <div class="stat-label">Total in pool:raw</div>
    <button class="btn btn-danger" style="margin-top:.6rem;font-size:.72rem;padding:.2rem .6rem" onclick="clearRaw()">Clear pool:raw</button>
  </div>
  <div class="stat-card">
    <div class="stat-value green" id="statAlive">–</div>
    <div class="stat-label">Alive (pool:checked)</div>
    <button class="btn btn-danger" style="margin-top:.6rem;font-size:.72rem;padding:.2rem .6rem" onclick="clearChecked()">Clear pool:checked</button>
  </div>
  <div class="stat-card">
    <div class="stat-value red" id="statDead">–</div>
    <div class="stat-label">Dead this session</div>
  </div>
  <div class="stat-card">
    <div class="stat-value gray" id="statUnchecked">–</div>
    <div class="stat-label">Unchecked yet</div>
  </div>
</div>

<div class="status-bar">
  <span class="pulse" id="pulse"></span>
  <span class="status-label" id="statusLabel">checking…</span>
  <div class="progress-wrap"><div class="progress-fill" id="progressFill" style="width:0%"></div></div>
  <span class="status-label" id="progressText"></span>
</div>

<!-- Grabber panel -->
<div class="grabber-panel">
  <h2>Grabber — Link Pool</h2>
  <div class="grabber-stats">
    <div class="grab-stat-card">
      <div class="stat-value yellow" id="grabTotal">–</div>
      <div class="stat-label">Total added to pool</div>
    </div>
    <div class="grab-stat-card">
      <div class="stat-value yellow" id="grabLast">–</div>
      <div class="stat-label">Added last run</div>
    </div>
    <div class="grab-stat-card">
      <div class="stat-value gray" id="grabLastRun">–</div>
      <div class="stat-label">Last run</div>
    </div>
    <div class="grab-stat-card" style="min-width:200px">
      <div style="display:flex;align-items:center;gap:.5rem;margin-bottom:.3rem">
        <span class="pulse done" id="grabPulse"></span>
        <span class="stat-value gray" style="font-size:.9rem" id="grabStatus">stopped</span>
      </div>
      <div class="stat-label">Grabber status</div>
    </div>
  </div>
  <div class="grabber-form">
    <label>URLs to grab from (one per line or comma-separated):</label>
    <textarea id="grabURLs" placeholder="https://raw.githubusercontent.com/.../configs.txt&#10;https://..."></textarea>
    <div class="grabber-row">
      <label style="margin:0">Interval:</label>
      <input type="text" id="grabInterval" value="10m" title="e.g. 5m, 1h, 30s">
      <button class="btn btn-warning" id="grabStartBtn" onclick="grabberStart()">Start Grabber</button>
      <button class="btn btn-danger" id="grabStopBtn" onclick="grabberStop()" disabled>Stop Grabber</button>
      <span class="grab-status" id="grabMsg"></span>
    </div>
  </div>
</div>

<!-- Recheck panel -->
<div class="grabber-panel" style="border-color:#1a3a6e">
  <h2 style="color:#79c0ff">Recheck — pool:checked актуализация</h2>
  <div class="grabber-stats">
    <div class="grab-stat-card">
      <div class="stat-value blue" id="recheckUpdated">–</div>
      <div class="stat-label">Обновлено (живые)</div>
    </div>
    <div class="grab-stat-card">
      <div class="stat-value red" id="recheckRemoved">–</div>
      <div class="stat-label">Удалено (мёртвые)</div>
    </div>
    <div class="grab-stat-card">
      <div class="stat-value gray" id="recheckLastRun">–</div>
      <div class="stat-label">Последний запуск</div>
    </div>
    <div class="grab-stat-card">
      <div style="display:flex;align-items:center;gap:.5rem;margin-bottom:.3rem">
        <span class="pulse done" id="recheckPulse"></span>
        <span class="stat-value gray" style="font-size:.9rem" id="recheckStatus">stopped</span>
      </div>
      <div class="stat-label">Статус</div>
    </div>
  </div>
  <div class="grabber-form">
    <div class="grabber-row">
      <label style="margin:0">Интервал:</label>
      <input type="text" id="recheckInterval" value="30m" title="e.g. 5m, 30m, 1h">
      <label style="margin:0">Воркеры:</label>
      <input type="text" id="recheckWorkers" value="5" style="width:55px">
      <button class="btn" style="background:#1a3a6e" id="recheckStartBtn" onclick="recheckStart()">Start Recheck</button>
      <button class="btn btn-danger" id="recheckStopBtn" onclick="recheckStop()" disabled>Stop</button>
      <span class="grab-status" id="recheckMsg"></span>
    </div>
  </div>
</div>

<!-- Raw Checker panel -->
<div class="grabber-panel" style="border-color:#0d3326">
  <h2 style="color:#56d364">Raw Checker — pool:raw проверка</h2>
  <div class="grabber-stats">
    <div class="grab-stat-card">
      <div class="stat-value gray" id="checkerLastRun">–</div>
      <div class="stat-label">Последний запуск</div>
    </div>
    <div class="grab-stat-card">
      <div style="display:flex;align-items:center;gap:.5rem;margin-bottom:.3rem">
        <span class="pulse done" id="checkerPulse"></span>
        <span class="stat-value gray" style="font-size:.9rem" id="checkerStatus">stopped</span>
      </div>
      <div class="stat-label">Статус</div>
    </div>
  </div>
  <div class="grabber-form">
    <div class="grabber-row">
      <label style="margin:0">Воркеры:</label>
      <input type="text" id="checkerWorkers" value="10" style="width:55px">
      <button class="btn" style="background:#0d3326;border:1px solid #56d364" id="checkerStartBtn" onclick="checkerStart()">Start Checker</button>
      <button class="btn btn-danger" id="checkerStopBtn" onclick="checkerStop()" disabled>Stop</button>
      <span class="grab-status" id="checkerMsg"></span>
    </div>
  </div>
</div>

<div class="actions">
  <button class="btn" onclick="copyAll()">Copy all URIs</button>
  <a class="link" href="/configs" target="_blank">/configs (plain text, sorted by latency)</a>
  <input type="number" id="configLimit" value="0" min="0" style="width:60px;background:#161b22;border:1px solid #30363d;border-radius:6px;color:#c9d1d9;font-size:.8rem;padding:.3rem .5rem;outline:none" title="0 = все конфиги">
  <button class="btn btn-sm" onclick="setConfigLimit()">Применить</button>
  <span id="configLimitLabel" style="font-size:.78rem;color:#8b949e"></span>
  <span class="alive-label"><span id="aliveCount">0</span> alive in table</span>
</div>

<table>
  <colgroup>
    <col class="c-num"><col class="c-name"><col class="c-proto"><col class="c-server">
    <col class="c-latency"><col class="c-ip"><col class="c-country"><col class="c-uri">
  </colgroup>
  <thead>
    <tr>
      <th>#</th><th>Name</th><th>Protocol</th><th>Server</th>
      <th>Latency</th><th>Exit IP</th><th>Country</th><th>URI</th>
    </tr>
  </thead>
  <tbody id="tbody"></tbody>
</table>

<div class="toast" id="toast">Copied!</div>

<script>
var rows = {};
var allURIs = {};
var rowCount = 0;

function badgeClass(p) {
  return {vless:'vless',shadowsocks:'shadowsocks',vmess:'vmess',trojan:'trojan'}[p] || p;
}
function esc(s) {
  return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function addRow(e) {
  var key = e.RawURI || (e.Server + ':' + e.Port);
  if (rows[key]) return;
  rowCount++;
  allURIs[key] = e.RawURI;
  var tr = document.createElement('tr');
  tr.className = 'new-row';
  tr.dataset.key = key;
  tr.innerHTML =
    '<td>' + rowCount + '</td>' +
    '<td class="name-cell" title="' + esc(e.Name) + '">' + esc(e.Name) + '</td>' +
    '<td><span class="badge ' + badgeClass(e.Protocol) + '">' + esc(e.Protocol) + '</span></td>' +
    '<td class="server" title="' + esc(e.Server) + ':' + e.Port + '">' + esc(e.Server) + ':' + e.Port + '</td>' +
    '<td class="latency">' + e.LatencyMs + 'ms</td>' +
    '<td class="server">' + esc(e.ExitIP) + '</td>' +
    '<td>' + esc(e.Country) + '</td>' +
    '<td class="uri-cell"><div class="copy-row">' +
      '<span class="uri-text" title="' + esc(e.RawURI) + '">' + esc(e.RawURI) + '</span>' +
      '<button class="btn btn-sm" style="flex-shrink:0" onclick="copyText(' + JSON.stringify(e.RawURI) + ')">Copy</button>' +
    '</div></td>';
  document.getElementById('tbody').appendChild(tr);
  rows[key] = tr;
  document.getElementById('aliveCount').textContent = rowCount;
}

function updateStats(s) {
  document.getElementById('statRaw').textContent       = s.TotalRaw;
  document.getElementById('statAlive').textContent     = s.AliveCount;
  document.getElementById('statDead').textContent      = s.DeadCount;
  document.getElementById('statUnchecked').textContent = Math.max(0, s.SessionTotal - s.SessionDone);
  if (s.SessionTotal > 0) {
    var pct = Math.round(s.SessionDone / s.SessionTotal * 100);
    document.getElementById('progressFill').style.width = pct + '%';
    document.getElementById('progressText').textContent = s.SessionDone + ' / ' + s.SessionTotal;
    document.getElementById('statusLabel').textContent  = 'checking… (' + pct + '%)';
  }
}

function updateGrabber(g) {
  document.getElementById('grabTotal').textContent   = g.total_added !== undefined ? g.total_added : '–';
  document.getElementById('grabLast').textContent    = g.last_added !== undefined ? g.last_added : '–';
  document.getElementById('grabLastRun').textContent = g.last_run || '–';
  var pulse = document.getElementById('grabPulse');
  var status = document.getElementById('grabStatus');
  var startBtn = document.getElementById('grabStartBtn');
  var stopBtn  = document.getElementById('grabStopBtn');
  if (g.running) {
    pulse.className = 'pulse grab';
    status.textContent = 'running';
    startBtn.disabled = true;
    stopBtn.disabled  = false;
    // Pre-fill URLs if we received them from server
    if (g.urls && g.urls.length > 0) {
      var ta = document.getElementById('grabURLs');
      if (!ta.value) ta.value = g.urls.join('\n');
    }
    if (g.interval) document.getElementById('grabInterval').value = g.interval;
  } else {
    pulse.className = 'pulse done';
    status.textContent = 'stopped';
    startBtn.disabled = false;
    stopBtn.disabled  = true;
  }
}

function grabberStart() {
  var raw = document.getElementById('grabURLs').value;
  var urls = raw.split(/[\n,]+/).map(function(u){return u.trim();}).filter(Boolean);
  if (!urls.length) { setGrabMsg('Enter at least one URL'); return; }
  var interval = document.getElementById('grabInterval').value.trim() || '10m';
  fetch('/grabber/start', {
    method: 'POST',
    headers: {'Content-Type':'application/json'},
    body: JSON.stringify({urls: urls, interval: interval})
  }).then(function(r){ return r.json(); }).then(function(d){
    setGrabMsg(d.status || d.error || '');
  }).catch(function(e){ setGrabMsg('error: '+e); });
}

function grabberStop() {
  fetch('/grabber/stop', {method:'POST'}).then(function(r){return r.json();}).then(function(d){
    setGrabMsg(d.status || '');
  }).catch(function(e){ setGrabMsg('error: '+e); });
}

function clearRaw() {
  if (!confirm('Delete all URIs from pool:raw?')) return;
  fetch('/pool/clear-raw', {method:'POST'}).then(function(r){return r.json();}).then(function(d){
    setGrabMsg(d.status || d.error || '');
  }).catch(function(e){ setGrabMsg('error: '+e); });
}

function clearChecked() {
  if (!confirm('Delete all URIs from pool:checked? The live table will also reset.')) return;
  fetch('/pool/clear-checked', {method:'POST'}).then(function(r){return r.json();}).then(function(d){
    if (d.status === 'cleared') {
      rows = {}; allURIs = {}; rowCount = 0;
      document.getElementById('tbody').innerHTML = '';
      document.getElementById('aliveCount').textContent = '0';
      document.getElementById('statAlive').textContent = '0';
    }
    setGrabMsg(d.status || d.error || '');
  }).catch(function(e){ setGrabMsg('error: '+e); });
}

function setGrabMsg(msg) {
  var el = document.getElementById('grabMsg');
  el.textContent = msg;
  setTimeout(function(){ el.textContent = ''; }, 3000);
}

function setMsg(id, msg) {
  var el = document.getElementById(id);
  el.textContent = msg;
  setTimeout(function(){ el.textContent = ''; }, 3000);
}

function updateRecheck(r) {
  document.getElementById('recheckUpdated').textContent = r.updated !== undefined ? r.updated : '–';
  document.getElementById('recheckRemoved').textContent = r.removed !== undefined ? r.removed : '–';
  document.getElementById('recheckLastRun').textContent = r.last_run || '–';
  var pulse = document.getElementById('recheckPulse');
  var status = document.getElementById('recheckStatus');
  var startBtn = document.getElementById('recheckStartBtn');
  var stopBtn  = document.getElementById('recheckStopBtn');
  if (r.running) {
    pulse.className = 'pulse'; pulse.style.background = '#79c0ff';
    status.textContent = 'running (' + (r.interval || '') + ')';
    startBtn.disabled = true; stopBtn.disabled = false;
    if (r.workers) document.getElementById('recheckWorkers').value = r.workers;
    if (r.interval) document.getElementById('recheckInterval').value = r.interval;
  } else {
    pulse.className = 'pulse done'; pulse.style.background = '';
    status.textContent = 'stopped';
    startBtn.disabled = false; stopBtn.disabled = true;
  }
}

function updateChecker(c) {
  document.getElementById('checkerLastRun').textContent = c.last_run || '–';
  var pulse = document.getElementById('checkerPulse');
  var status = document.getElementById('checkerStatus');
  var startBtn = document.getElementById('checkerStartBtn');
  var stopBtn  = document.getElementById('checkerStopBtn');
  if (c.running) {
    pulse.className = 'pulse'; pulse.style.background = '#56d364';
    status.textContent = 'running';
    startBtn.disabled = true; stopBtn.disabled = false;
    if (c.workers) document.getElementById('checkerWorkers').value = c.workers;
  } else {
    pulse.className = 'pulse done'; pulse.style.background = '';
    status.textContent = 'stopped';
    startBtn.disabled = false; stopBtn.disabled = true;
  }
}

function recheckStart() {
  var interval = document.getElementById('recheckInterval').value.trim() || '30m';
  var workers  = parseInt(document.getElementById('recheckWorkers').value) || 5;
  fetch('/recheck/start', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({interval: interval, workers: workers})
  }).then(function(r){ return r.json(); }).then(function(d){
    setMsg('recheckMsg', d.status || d.error || '');
  }).catch(function(e){ setMsg('recheckMsg', 'error: '+e); });
}

function recheckStop() {
  fetch('/recheck/stop', {method: 'POST'}).then(function(r){ return r.json(); }).then(function(d){
    setMsg('recheckMsg', d.status || '');
  }).catch(function(e){ setMsg('recheckMsg', 'error: '+e); });
}

function checkerStart() {
  var workers = parseInt(document.getElementById('checkerWorkers').value) || 10;
  fetch('/checker/start', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({workers: workers})
  }).then(function(r){ return r.json(); }).then(function(d){
    setMsg('checkerMsg', d.status || d.error || '');
  }).catch(function(e){ setMsg('checkerMsg', 'error: '+e); });
}

function checkerStop() {
  fetch('/checker/stop', {method: 'POST'}).then(function(r){ return r.json(); }).then(function(d){
    setMsg('checkerMsg', d.status || '');
  }).catch(function(e){ setMsg('checkerMsg', 'error: '+e); });
}

function setConfigLimit() {
  var n = parseInt(document.getElementById('configLimit').value) || 0;
  fetch('/configs/limit', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({limit: n})
  }).then(function(r){ return r.json(); }).then(function(d){
    updateConfigLimit(d.limit !== undefined ? d.limit : n);
  }).catch(function(e){ console.error('config limit error:', e); });
}

function updateConfigLimit(n) {
  document.getElementById('configLimit').value = n;
  var label = document.getElementById('configLimitLabel');
  label.textContent = n > 0 ? 'топ-' + n : 'все';
}

function connect() {
  var es = new EventSource('/events');
  es.onmessage = function(e) {
    var ev = JSON.parse(e.data);
    if (ev.stats) updateStats(ev.stats);
    if (ev.type === 'alive' && ev.entry) {
      addRow(ev.entry);
    } else if (ev.type === 'done') {
      document.getElementById('pulse').className = 'pulse done';
      document.getElementById('statusLabel').textContent = 'done';
      document.getElementById('progressFill').style.width = '100%';
      if (ev.checked_at) document.getElementById('checkedAt').textContent = 'Last checked: ' + ev.checked_at;
    } else if (ev.type === 'grabber' && ev.grabber) {
      updateGrabber(ev.grabber);
    } else if (ev.type === 'recheck' && ev.recheck) {
      updateRecheck(ev.recheck);
    } else if (ev.type === 'checker' && ev.checker) {
      updateChecker(ev.checker);
    } else if (ev.type === 'config_limit') {
      updateConfigLimit(ev.config_limit || 0);
    }
  };
  es.onerror = function() {
    document.getElementById('statusLabel').textContent = 'reconnecting…';
    es.close();
    setTimeout(connect, 3000);
  };
}
connect();

function copyText(s) {
  navigator.clipboard.writeText(s).then(showToast).catch(function() {
    var t = document.createElement('textarea');
    t.value = s; document.body.appendChild(t); t.select();
    document.execCommand('copy'); document.body.removeChild(t);
    showToast();
  });
}
function copyAll() { copyText(Object.values(allURIs).join('\n')); }
function showToast() {
  var el = document.getElementById('toast');
  el.classList.add('show');
  setTimeout(function(){ el.classList.remove('show'); }, 1800);
}
</script>
</body>
</html>`
