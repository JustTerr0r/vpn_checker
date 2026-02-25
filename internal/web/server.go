package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"vpn_checker/internal/checker"
)

// AliveEntry pairs a successful check result with its original raw URI.
type AliveEntry struct {
	Result checker.Result
	RawURI string
}

type templateData struct {
	GeneratedAt string
	Count       int
	Entries     []AliveEntry
}

var tmpl = template.Must(
	template.New("alive").Funcs(template.FuncMap{
		"inc":       func(i int) int { return i + 1 },
		"latencyMs": func(d time.Duration) int64 { return d.Milliseconds() },
		"truncate": func(s string, n int) string {
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n-1]) + "…"
		},
	}).Parse(htmlTemplate),
)

// Serve starts an HTTP server on addr and blocks until it exits.
func Serve(addr string, entries []AliveEntry) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex(entries))
	mux.HandleFunc("/configs", handleConfigs(entries))
	return http.ListenAndServe(addr, mux)
}

func handleIndex(entries []AliveEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data := templateData{
			GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
			Count:       len(entries),
			Entries:     entries,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, fmt.Sprintf("template error: %v", err), http.StatusInternalServerError)
		}
	}
}

func handleConfigs(entries []AliveEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		uris := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.RawURI != "" {
				uris = append(uris, e.RawURI)
			}
		}
		fmt.Fprint(w, strings.Join(uris, "\n"))
	}
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>VPN Checker — Alive Configs</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#c9d1d9;padding:2rem;min-height:100vh}
h1{font-size:1.4rem;font-weight:700;color:#58a6ff;margin-bottom:.25rem}
.meta{font-size:.82rem;color:#484f58;margin-bottom:1.5rem}
.actions{display:flex;align-items:center;gap:1rem;margin-bottom:1.25rem;flex-wrap:wrap}
.btn{cursor:pointer;padding:.4rem 1rem;border:none;border-radius:6px;font-size:.82rem;font-weight:600;
     background:#1f6feb;color:#fff;transition:background .15s}
.btn:hover{background:#388bfd}
.btn-sm{padding:.25rem .65rem;font-size:.75rem;background:#21262d;color:#8b949e;border:1px solid #30363d}
.btn-sm:hover{background:#30363d;color:#c9d1d9}
a.link{color:#58a6ff;font-size:.82rem;text-decoration:none}
a.link:hover{text-decoration:underline}
.stats{font-size:.82rem;color:#8b949e;margin-left:auto}
table{width:100%;border-collapse:collapse;font-size:.83rem}
thead th{background:#161b22;color:#8b949e;font-weight:600;text-align:left;
          padding:.55rem .75rem;border-bottom:1px solid #21262d;white-space:nowrap}
tbody td{padding:.48rem .75rem;border-bottom:1px solid #161b22;vertical-align:middle}
tbody tr:hover td{background:#161b22}
.badge{display:inline-block;padding:.15rem .55rem;border-radius:12px;font-size:.72rem;font-weight:700;letter-spacing:.02em}
.badge.vless{background:#1a3a6e;color:#79c0ff}
.badge.shadowsocks{background:#0d3326;color:#56d364}
.badge.vmess{background:#3a2010;color:#ffa657}
.badge.trojan{background:#2d1a4a;color:#d2a8ff}
.latency{color:#3fb950;font-variant-numeric:tabular-nums}
.server{font-family:monospace;font-size:.78rem;color:#8b949e}
.uri-cell{max-width:260px}
.uri-text{font-family:monospace;font-size:.72rem;color:#484f58;white-space:nowrap;
           overflow:hidden;text-overflow:ellipsis;max-width:220px;display:inline-block;vertical-align:middle}
.copy-row{display:flex;align-items:center;gap:.4rem}
.toast{position:fixed;bottom:1.5rem;right:1.5rem;background:#238636;color:#fff;
        padding:.5rem 1rem;border-radius:8px;font-size:.82rem;opacity:0;
        transition:opacity .3s;pointer-events:none;z-index:999}
.toast.show{opacity:1}
</style>
</head>
<body>
<h1>VPN Checker — Alive Configs</h1>
<p class="meta">Generated {{.GeneratedAt}}</p>

<div class="actions">
  <button class="btn" onclick="copyAll()">Copy all URIs</button>
  <a class="link" href="/configs" target="_blank">/configs (plain text)</a>
  <span class="stats">{{.Count}} alive</span>
</div>

<table>
  <thead>
    <tr>
      <th>#</th>
      <th>Name</th>
      <th>Protocol</th>
      <th>Server</th>
      <th>Latency</th>
      <th>Exit IP</th>
      <th>Country</th>
      <th>URI</th>
    </tr>
  </thead>
  <tbody>
  {{range $i, $e := .Entries}}
    <tr>
      <td>{{inc $i}}</td>
      <td>{{$e.Result.Name}}</td>
      <td><span class="badge {{$e.Result.Protocol}}">{{$e.Result.Protocol}}</span></td>
      <td class="server">{{$e.Result.Server}}:{{$e.Result.Port}}</td>
      <td class="latency">{{latencyMs $e.Result.Latency}}ms</td>
      <td class="server">{{$e.Result.ExitIP}}</td>
      <td>{{$e.Result.Country}}</td>
      <td class="uri-cell">
        <div class="copy-row">
          <span class="uri-text" title="{{$e.RawURI}}">{{truncate $e.RawURI 55}}</span>
          <button class="btn btn-sm" onclick='copyText({{$e.RawURI | js}})'>Copy</button>
        </div>
      </td>
    </tr>
  {{end}}
  </tbody>
</table>

<div class="toast" id="toast">Copied!</div>

<script>
var allURIs = [{{range .Entries}}{{.RawURI | js}},{{end}}];

function copyText(s) {
  navigator.clipboard.writeText(s).then(showToast).catch(function() {
    var t = document.createElement('textarea');
    t.value = s;
    document.body.appendChild(t);
    t.select();
    document.execCommand('copy');
    document.body.removeChild(t);
    showToast();
  });
}

function copyAll() {
  copyText(allURIs.join('\n'));
}

function showToast() {
  var el = document.getElementById('toast');
  el.classList.add('show');
  setTimeout(function(){ el.classList.remove('show'); }, 1800);
}
</script>
</body>
</html>`
