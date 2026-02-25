package parser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ProxyConfig is the common interface for all proxy types
type ProxyConfig interface {
	GetName() string
	GetProtocol() string
	GetServer() string
	GetPort() int
}

// VlessConfig holds parsed vless:// URI parameters
type VlessConfig struct {
	Name       string
	UUID       string
	Server     string
	Port       int
	Security   string
	Type       string
	SNI        string
	Host       string
	Path       string
	Fp         string
	Encryption string
	Flow       string
	PublicKey  string // reality pbk
	ShortID    string // reality sid
}

func (v *VlessConfig) GetName() string     { return v.Name }
func (v *VlessConfig) GetProtocol() string { return "vless" }
func (v *VlessConfig) GetServer() string   { return v.Server }
func (v *VlessConfig) GetPort() int        { return v.Port }

// SSConfig holds parsed ss:// URI parameters
type SSConfig struct {
	Name     string
	Method   string
	Password string
	Server   string
	Port     int
}

func (s *SSConfig) GetName() string     { return s.Name }
func (s *SSConfig) GetProtocol() string { return "shadowsocks" }
func (s *SSConfig) GetServer() string   { return s.Server }
func (s *SSConfig) GetPort() int        { return s.Port }

// VmessConfig holds parsed vmess:// URI parameters (JSON payload in base64)
type VmessConfig struct {
	Name     string
	UUID     string
	Server   string
	Port     int
	Aid      int
	Security string // cipher: auto, aes-128-gcm, chacha20-poly1305, none
	Network  string // net: tcp, ws, grpc, h2, kcp
	TLS      string // tls / ""
	SNI      string
	Host     string
	Path     string
}

func (v *VmessConfig) GetName() string     { return v.Name }
func (v *VmessConfig) GetProtocol() string { return "vmess" }
func (v *VmessConfig) GetServer() string   { return v.Server }
func (v *VmessConfig) GetPort() int        { return v.Port }

// TrojanConfig holds parsed trojan:// URI parameters
type TrojanConfig struct {
	Name     string
	Password string
	Server   string
	Port     int
	Security string
	Type     string
	SNI      string
	Host     string
	Path     string
	Fp       string
}

func (t *TrojanConfig) GetName() string     { return t.Name }
func (t *TrojanConfig) GetProtocol() string { return "trojan" }
func (t *TrojanConfig) GetServer() string   { return t.Server }
func (t *TrojanConfig) GetPort() int        { return t.Port }

// ParseLine parses a single URI line into a ProxyConfig
func ParseLine(line string) (ProxyConfig, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, fmt.Errorf("empty or comment line")
	}

	switch {
	case strings.HasPrefix(line, "vless://"):
		return parseVless(line)
	case strings.HasPrefix(line, "ss://"):
		return parseSS(line)
	case strings.HasPrefix(line, "vmess://"):
		return parseVmess(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojan(line)
	default:
		return nil, fmt.Errorf("unsupported protocol in: %s", line)
	}
}

func parseVless(raw string) (*VlessConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("vless parse error: %w", err)
	}

	host := u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		portStr = "443"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}

	uuid := u.User.Username()
	q := u.Query()

	cfg := &VlessConfig{
		UUID:       uuid,
		Server:     host,
		Port:       port,
		Security:   q.Get("security"),
		Type:       q.Get("type"),
		SNI:        q.Get("sni"),
		Host:       q.Get("host"),
		Path:       q.Get("path"),
		Fp:         q.Get("fp"),
		Encryption: q.Get("encryption"),
		Flow:       q.Get("flow"),
		PublicKey:  q.Get("pbk"),
		ShortID:    q.Get("sid"),
		Name:       u.Fragment,
	}

	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("%s:%d", host, port)
	} else {
		if dec, err := url.QueryUnescape(cfg.Name); err == nil {
			cfg.Name = dec
		}
	}

	return cfg, nil
}

func parseSS(raw string) (*SSConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ss parse error: %w", err)
	}

	host := u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		portStr = "8388"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}

	// userinfo is base64-encoded "method:password"
	userinfo := u.User.String()
	decoded, err := base64DecodeUserinfo(userinfo)
	if err != nil {
		return nil, fmt.Errorf("ss userinfo decode error: %w", err)
	}

	parts := strings.SplitN(decoded, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("ss userinfo format invalid: %s", decoded)
	}

	name := u.Fragment
	if name == "" {
		name = fmt.Sprintf("%s:%d", host, port)
	} else {
		if dec, err := url.QueryUnescape(name); err == nil {
			name = dec
		}
	}

	return &SSConfig{
		Method:   parts[0],
		Password: parts[1],
		Server:   host,
		Port:     port,
		Name:     name,
	}, nil
}

// vmessJSON is the JSON payload embedded in a vmess:// URI
type vmessJSON struct {
	Add  string      `json:"add"`
	Aid  interface{} `json:"aid"` // can be string or int
	ID   string      `json:"id"`
	Net  string      `json:"net"`
	Path string      `json:"path"`
	Port interface{} `json:"port"` // can be string or int
	PS   string      `json:"ps"`
	Scy  string      `json:"scy"`
	SNI  string      `json:"sni"`
	TLS  string      `json:"tls"`
	Type string      `json:"type"`
	Host string      `json:"host"`
}

func parseVmess(raw string) (*VmessConfig, error) {
	b64 := strings.TrimPrefix(raw, "vmess://")
	// strip fragment if any
	if idx := strings.IndexByte(b64, '#'); idx >= 0 {
		b64 = b64[:idx]
	}

	data, err := base64DecodeUserinfo(b64)
	if err != nil {
		return nil, fmt.Errorf("vmess base64 decode: %w", err)
	}

	var v vmessJSON
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return nil, fmt.Errorf("vmess json: %w", err)
	}

	port, err := toInt(v.Port)
	if err != nil {
		return nil, fmt.Errorf("vmess port: %w", err)
	}
	aid, _ := toInt(v.Aid)

	name := v.PS
	if name == "" {
		name = fmt.Sprintf("%s:%d", v.Add, port)
	}

	sec := v.Scy
	if sec == "" {
		sec = "auto"
	}

	return &VmessConfig{
		Name:     name,
		UUID:     v.ID,
		Server:   v.Add,
		Port:     port,
		Aid:      aid,
		Security: sec,
		Network:  v.Net,
		TLS:      v.TLS,
		SNI:      v.SNI,
		Host:     v.Host,
		Path:     v.Path,
	}, nil
}

func parseTrojan(raw string) (*TrojanConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("trojan parse error: %w", err)
	}

	host := u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		portStr = "443"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}

	password := u.User.Username()
	q := u.Query()

	security := q.Get("security")
	if security == "" {
		security = "tls" // trojan default
	}

	name := u.Fragment
	if name == "" {
		name = fmt.Sprintf("%s:%d", host, port)
	} else {
		if dec, err := url.QueryUnescape(name); err == nil {
			name = dec
		}
	}

	return &TrojanConfig{
		Name:     name,
		Password: password,
		Server:   host,
		Port:     port,
		Security: security,
		Type:     q.Get("type"),
		SNI:      q.Get("sni"),
		Host:     q.Get("host"),
		Path:     q.Get("path"),
		Fp:       q.Get("fp"),
	}, nil
}

// base64DecodeUserinfo tries standard and URL-safe base64 decoding
func base64DecodeUserinfo(s string) (string, error) {
	s, _ = url.QueryUnescape(s)

	padded := s
	switch len(padded) % 4 {
	case 2:
		padded += "=="
	case 3:
		padded += "="
	}

	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(padded)
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(padded)
			if err != nil {
				return "", fmt.Errorf("base64 decode failed: %w", err)
			}
		}
	}
	return string(b), nil
}

// toInt coerces a json number/string to int
func toInt(v interface{}) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.Atoi(x)
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
