package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"vpn_checker/internal/checker"
)

// AliveEntry pairs a successful check result with its original raw URI.
type AliveEntry struct {
	Result checker.Result
	RawURI string
}

// CheckEvent is sent over SSE for each finished config check.
type CheckEvent struct {
	Type    string     `json:"type"` // "result" | "done" | "remove"
	Alive   bool       `json:"alive,omitempty"`
	Entry   *AliveEntry `json:"entry,omitempty"`
	Key     string     `json:"key,omitempty"` // for "remove"
	Done    int        `json:"done,omitempty"`
	Total   int        `json:"total,omitempty"`
	Checked string     `json:"checked_at,omitempty"`
}

type state struct {
	Entries     []AliveEntry
	GeneratedAt string
	NextCheckIn string
	Checking    bool
	Done        int
	Total       int
}

// Server holds shared state and exposes Update for periodic re-checks.
type Server struct {
	mu    sync.RWMutex
	state state

	// SSE broker
	sseClients map[chan []byte]struct{}
	sseMu      sync.Mutex
}

// NewServer creates a Server ready to serve (entries may be empty initially).
func NewServer(entries []AliveEntry) *Server {
	return &Server{
		state: state{
			Entries:     entries,
			GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		},
		sseClients: make(map[chan []byte]struct{}),
	}
}

// ---- state mutations ----

// SetChecking marks the server as "check in progress" with a known total.
func (s *Server) SetChecking(total int) {
	s.mu.Lock()
	s.state.Checking = true
	s.state.Total = total
	s.state.Done = 0
	s.mu.Unlock()
}

// SetDone marks the check as finished and broadcasts a "done" SSE event.
func (s *Server) SetDone() {
	s.mu.Lock()
	s.state.Checking = false
	s.state.GeneratedAt = time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	s.mu.Unlock()
	s.broadcast(CheckEvent{Type: "done", Checked: time.Now().UTC().Format("2006-01-02 15:04:05 UTC")})
}

// PublishResult is called after each individual config check.
// If alive, it appends to the list and broadcasts an SSE "result" event.
// If dead, it broadcasts a "result" event with Alive=false (no entry added).
func (s *Server) PublishResult(e AliveEntry, done, total int) {
	s.mu.Lock()
	s.state.Done = done
	s.state.Total = total
	if e.Result.Alive {
		key := entryKey(e)
		found := false
		for _, ex := range s.state.Entries {
			if entryKey(ex) == key {
				found = true
				break
			}
		}
		if !found {
			s.state.Entries = append(s.state.Entries, e)
		}
	}
	s.mu.Unlock()

	ev := CheckEvent{
		Type:  "result",
		Alive: e.Result.Alive,
		Done:  done,
		Total: total,
	}
	if e.Result.Alive {
		ev.Entry = &e
	}
	s.broadcast(ev)
}

// UpdateEntries atomically replaces the alive entries and resets the timestamp.
func (s *Server) UpdateEntries(entries []AliveEntry, nextCheckIn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state{
		Entries:     entries,
		GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		NextCheckIn: nextCheckIn,
	}
}

// AppendEntries merges newEntries into the existing list, deduplicating by RawURI.
func (s *Server) AppendEntries(newEntries []AliveEntry, nextCheckIn string) {
	s.mu.Lock()
	seen := make(map[string]struct{}, len(s.state.Entries))
	for _, e := range s.state.Entries {
		seen[entryKey(e)] = struct{}{}
	}
	merged := s.state.Entries
	for _, e := range newEntries {
		k := entryKey(e)
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			merged = append(merged, e)
		}
	}
	s.state.Entries = merged
	s.state.GeneratedAt = time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	s.state.NextCheckIn = nextCheckIn
	s.mu.Unlock()
}

// UpdateNextCheckIn updates only the countdown string.
func (s *Server) UpdateNextCheckIn(nextCheckIn string) {
	s.mu.Lock()
	s.state.NextCheckIn = nextCheckIn
	s.mu.Unlock()
}

// Entries returns a snapshot of the current alive entries (oldest first).
func (s *Server) Entries() []AliveEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]AliveEntry, len(s.state.Entries))
	copy(cp, s.state.Entries)
	return cp
}

// RemoveEntry removes the entry with the given key and broadcasts an SSE "remove" event.
func (s *Server) RemoveEntry(key string) {
	s.mu.Lock()
	out := s.state.Entries[:0]
	for _, e := range s.state.Entries {
		if entryKey(e) != key {
			out = append(out, e)
		}
	}
	s.state.Entries = out
	s.mu.Unlock()
	s.broadcast(CheckEvent{Type: "remove", Key: key})
}

func entryKey(e AliveEntry) string {
	if e.RawURI != "" {
		return e.RawURI
	}
	return fmt.Sprintf("%s:%d", e.Result.Server, e.Result.Port)
}

// ---- SSE broker ----

func (s *Server) broadcast(ev CheckEvent) {
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
		default: // slow client — skip
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

	ch := make(chan []byte, 32)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
	}()

	// Send current state immediately so late-joiners catch up.
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	for _, e := range st.Entries {
		ev := CheckEvent{Type: "result", Alive: true, Entry: &e, Done: st.Done, Total: st.Total}
		if data, err := json.Marshal(ev); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}
	if !st.Checking {
		ev := CheckEvent{Type: "done", Checked: st.GeneratedAt}
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

// ---- HTTP server ----

// Serve starts an HTTP server on addr and blocks until it exits.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/configs", s.handleConfigs)
	mux.HandleFunc("/events", s.handleEvents)
	return http.ListenAndServe(addr, mux)
}

// Serve is a convenience function for one-shot usage (no periodic updates).
func Serve(addr string, entries []AliveEntry) error {
	return NewServer(entries).Serve(addr)
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
	s.mu.RLock()
	entries := s.state.Entries
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	uris := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.RawURI != "" {
			uris = append(uris, e.RawURI)
		}
	}
	fmt.Fprint(w, strings.Join(uris, "\n"))
}

const htmlPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>VPN Checker — Live</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#c9d1d9;padding:2rem;min-height:100vh}
h1{font-size:1.4rem;font-weight:700;color:#58a6ff;margin-bottom:.25rem}
.meta{font-size:.82rem;color:#484f58;margin-bottom:.4rem}
.status-bar{display:flex;align-items:center;gap:.75rem;margin-bottom:1.2rem;flex-wrap:wrap}
.status-label{font-size:.8rem;color:#8b949e}
.progress-wrap{flex:1;min-width:120px;max-width:320px;background:#21262d;border-radius:6px;height:8px;overflow:hidden}
.progress-fill{height:100%;background:#1f6feb;border-radius:6px;transition:width .3s}
.pulse{display:inline-block;width:8px;height:8px;border-radius:50%;background:#3fb950;
       animation:pulse 1.2s ease-in-out infinite}
.pulse.done{background:#484f58;animation:none}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}
.next{font-size:.78rem;color:#3fb950;margin-bottom:1.2rem}
.actions{display:flex;align-items:center;gap:1rem;margin-bottom:1.25rem;flex-wrap:wrap}
.btn{cursor:pointer;padding:.4rem 1rem;border:none;border-radius:6px;font-size:.82rem;font-weight:600;
     background:#1f6feb;color:#fff;transition:background .15s}
.btn:hover{background:#388bfd}
.btn-sm{padding:.25rem .65rem;font-size:.75rem;background:#21262d;color:#8b949e;border:1px solid #30363d}
.btn-sm:hover{background:#30363d;color:#c9d1d9}
a.link{color:#58a6ff;font-size:.82rem;text-decoration:none}
a.link:hover{text-decoration:underline}
.stats{font-size:.82rem;color:#8b949e;margin-left:auto}
table{width:100%;border-collapse:collapse;font-size:.8rem;table-layout:fixed}
thead th{background:#161b22;color:#8b949e;font-weight:600;text-align:left;
          padding:.45rem .5rem;border-bottom:1px solid #21262d;white-space:nowrap;overflow:hidden}
tbody td{padding:.38rem .5rem;border-bottom:1px solid #161b22;vertical-align:middle;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
tbody tr{transition:background .15s}
tbody tr:hover td{background:#161b22}
tbody tr.new-row{animation:fadeIn .4s ease}
@keyframes fadeIn{from{background:#0d2a4a}to{background:transparent}}
/* column widths */
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
.name-cell{max-width:12rem;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
.uri-cell{overflow:hidden}
.uri-text{font-family:monospace;font-size:.7rem;color:#484f58;white-space:nowrap;
           overflow:hidden;text-overflow:ellipsis;display:block;width:100%}
.copy-row{display:flex;align-items:center;gap:.3rem}
.toast{position:fixed;bottom:1.5rem;right:1.5rem;background:#238636;color:#fff;
        padding:.5rem 1rem;border-radius:8px;font-size:.82rem;opacity:0;
        transition:opacity .3s;pointer-events:none;z-index:999}
.toast.show{opacity:1}
</style>
</head>
<body>
<h1>VPN Checker — Live</h1>
<p class="meta" id="checkedAt">Connecting…</p>

<div class="status-bar">
  <span class="pulse" id="pulse"></span>
  <span class="status-label" id="statusLabel">checking…</span>
  <div class="progress-wrap"><div class="progress-fill" id="progressFill" style="width:0%"></div></div>
  <span class="status-label" id="progressText"></span>
</div>

<div class="actions">
  <button class="btn" onclick="copyAll()">Copy all URIs</button>
  <a class="link" href="/configs" target="_blank">/configs (plain text)</a>
  <span class="stats"><span id="aliveCount">0</span> alive</span>
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
var rows = {}; // key -> tr element
var allURIs = {};
var rowCount = 0;

function badgeClass(proto) {
  var m = {'vless':'vless','shadowsocks':'shadowsocks','vmess':'vmess','trojan':'trojan'};
  return m[proto] || proto;
}

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function truncate(s, n) {
  if (!s) return '';
  var r = Array.from(s);
  return r.length <= n ? s : r.slice(0, n-1).join('') + '…';
}

function addRow(entry) {
  var key = entry.RawURI || (entry.Result.Server + ':' + entry.Result.Port);
  if (rows[key]) return; // dedup

  rowCount++;
  allURIs[key] = entry.RawURI;

  var r = entry.Result;
  var tr = document.createElement('tr');
  tr.className = 'new-row';
  tr.dataset.key = key;
  tr.innerHTML =
    '<td>' + rowCount + '</td>' +
    '<td class="name-cell" title="' + esc(r.Name) + '">' + esc(r.Name) + '</td>' +
    '<td><span class="badge ' + badgeClass(r.Protocol) + '">' + esc(r.Protocol) + '</span></td>' +
    '<td class="server" title="' + esc(r.Server) + ':' + r.Port + '">' + esc(r.Server) + ':' + r.Port + '</td>' +
    '<td class="latency">' + r.Latency/1000000 + 'ms</td>' +
    '<td class="server">' + esc(r.ExitIP) + '</td>' +
    '<td>' + esc(r.Country) + '</td>' +
    '<td class="uri-cell"><div class="copy-row">' +
      '<span class="uri-text" title="' + esc(entry.RawURI) + '">' + esc(entry.RawURI) + '</span>' +
      '<button class="btn btn-sm" style="flex-shrink:0" onclick="copyText(' + JSON.stringify(entry.RawURI) + ')">Copy</button>' +
    '</div></td>';

  document.getElementById('tbody').appendChild(tr);
  rows[key] = tr;
  document.getElementById('aliveCount').textContent = rowCount;
}

function removeRow(key) {
  var tr = rows[key];
  if (tr) {
    tr.remove();
    delete rows[key];
    delete allURIs[key];
    rowCount--;
    document.getElementById('aliveCount').textContent = rowCount;
    // Re-number
    var trs = document.querySelectorAll('#tbody tr');
    trs.forEach(function(r, i){ r.cells[0].textContent = i+1; });
  }
}

function connect() {
  var es = new EventSource('/events');

  es.onmessage = function(e) {
    var ev = JSON.parse(e.data);

    if (ev.type === 'result') {
      if (ev.alive && ev.entry) {
        addRow(ev.entry);
      }
      if (ev.total > 0) {
        var pct = Math.round(ev.done / ev.total * 100);
        document.getElementById('progressFill').style.width = pct + '%';
        document.getElementById('progressText').textContent = ev.done + ' / ' + ev.total;
        document.getElementById('statusLabel').textContent = 'checking… (' + pct + '%)';
      }
    } else if (ev.type === 'done') {
      document.getElementById('pulse').className = 'pulse done';
      document.getElementById('statusLabel').textContent = 'done';
      document.getElementById('progressFill').style.width = '100%';
      if (ev.checked_at) {
        document.getElementById('checkedAt').textContent = 'Last checked: ' + ev.checked_at;
      }
    } else if (ev.type === 'remove') {
      removeRow(ev.key);
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
function copyAll() {
  copyText(Object.values(allURIs).join('\n'));
}
function showToast() {
  var el = document.getElementById('toast');
  el.classList.add('show');
  setTimeout(function(){ el.classList.remove('show'); }, 1800);
}
</script>
</body>
</html>`
