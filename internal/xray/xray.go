package xray

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"vpn_checker/internal/parser"
)

// GenerateConfig creates an xray JSON config for the given proxy
func GenerateConfig(cfg parser.ProxyConfig, socksPort int) ([]byte, error) {
	switch c := cfg.(type) {
	case *parser.VlessConfig:
		return generateVlessConfig(c, socksPort)
	case *parser.SSConfig:
		return generateSSConfig(c, socksPort)
	case *parser.VmessConfig:
		return generateVmessConfig(c, socksPort)
	case *parser.TrojanConfig:
		return generateTrojanConfig(c, socksPort)
	default:
		return nil, fmt.Errorf("unsupported config type: %T", cfg)
	}
}

// inbound returns a standard SOCKS5 inbound block
func inbound(socksPort int) interface{} {
	return map[string]interface{}{
		"listen":   "127.0.0.1",
		"port":     socksPort,
		"protocol": "socks",
		"settings": map[string]interface{}{
			"auth": "noauth",
			"udp":  false,
		},
	}
}

// buildStreamSettings constructs streamSettings for transport-layer options
func buildStreamSettings(network, security, sni, host, path, fp string) map[string]interface{} {
	ss := map[string]interface{}{
		"network":  network,
		"security": security,
	}

	switch security {
	case "tls":
		tls := map[string]interface{}{"serverName": sni}
		if fp != "" {
			tls["fingerprint"] = fp
		}
		ss["tlsSettings"] = tls
	case "reality":
		ss["realitySettings"] = map[string]interface{}{
			"serverName":  sni,
			"fingerprint": fp,
		}
	}

	switch network {
	case "ws":
		ss["wsSettings"] = map[string]interface{}{
			"path":    path,
			"headers": map[string]string{"Host": host},
		}
	case "grpc":
		ss["grpcSettings"] = map[string]interface{}{
			"serviceName": path,
		}
	case "http", "h2":
		ss["httpSettings"] = map[string]interface{}{
			"path": path,
			"host": []string{host},
		}
	case "httpupgrade":
		ss["httpupgradeSettings"] = map[string]interface{}{
			"path": path,
			"host": host,
		}
	case "xhttp", "splithttp":
		ss["xhttpSettings"] = map[string]interface{}{
			"path": path,
			"host": host,
		}
	}

	return ss
}

func generateVlessConfig(c *parser.VlessConfig, socksPort int) ([]byte, error) {
	ss := buildStreamSettings(c.Type, c.Security, c.SNI, c.Host, c.Path, c.Fp)

	// Reality needs publicKey + shortId
	if c.Security == "reality" && c.PublicKey != "" {
		ss["realitySettings"] = map[string]interface{}{
			"serverName":  c.SNI,
			"fingerprint": c.Fp,
			"publicKey":   c.PublicKey,
			"shortId":     c.ShortID,
		}
	}

	enc := c.Encryption
	if enc == "" {
		enc = "none"
	}

	user := map[string]interface{}{
		"id":         c.UUID,
		"encryption": enc,
	}
	if c.Flow != "" {
		user["flow"] = c.Flow
	}

	config := xrayConfig(socksPort, "vless", map[string]interface{}{
		"vnext": []interface{}{
			map[string]interface{}{
				"address": c.Server,
				"port":    c.Port,
				"users":   []interface{}{user},
			},
		},
	}, ss)

	return json.MarshalIndent(config, "", "  ")
}

func generateSSConfig(c *parser.SSConfig, socksPort int) ([]byte, error) {
	config := xrayConfig(socksPort, "shadowsocks", map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{
				"address":  c.Server,
				"port":     c.Port,
				"method":   c.Method,
				"password": c.Password,
			},
		},
	}, nil)

	return json.MarshalIndent(config, "", "  ")
}

func generateVmessConfig(c *parser.VmessConfig, socksPort int) ([]byte, error) {
	security := c.Security
	if security == "" {
		security = "auto"
	}

	tlsSec := ""
	if c.TLS == "tls" {
		tlsSec = "tls"
	}
	ss := buildStreamSettings(c.Network, tlsSec, c.SNI, c.Host, c.Path, "")

	config := xrayConfig(socksPort, "vmess", map[string]interface{}{
		"vnext": []interface{}{
			map[string]interface{}{
				"address": c.Server,
				"port":    c.Port,
				"users": []interface{}{
					map[string]interface{}{
						"id":       c.UUID,
						"alterId":  c.Aid,
						"security": security,
					},
				},
			},
		},
	}, ss)

	return json.MarshalIndent(config, "", "  ")
}

func generateTrojanConfig(c *parser.TrojanConfig, socksPort int) ([]byte, error) {
	security := c.Security
	if security == "" {
		security = "tls"
	}
	ss := buildStreamSettings(c.Type, security, c.SNI, c.Host, c.Path, c.Fp)

	config := xrayConfig(socksPort, "trojan", map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{
				"address":  c.Server,
				"port":     c.Port,
				"password": c.Password,
			},
		},
	}, ss)

	return json.MarshalIndent(config, "", "  ")
}

// xrayConfig assembles the full xray JSON config document
func xrayConfig(socksPort int, protocol string, settings map[string]interface{}, streamSettings map[string]interface{}) map[string]interface{} {
	outbound := map[string]interface{}{
		"protocol": protocol,
		"settings": settings,
	}
	if streamSettings != nil {
		outbound["streamSettings"] = streamSettings
	}

	return map[string]interface{}{
		"log": map[string]interface{}{
			"loglevel": "none",
		},
		"inbounds":  []interface{}{inbound(socksPort)},
		"outbounds": []interface{}{outbound},
	}
}

// Start launches xray with config provided via stdin, returns the running Cmd
func Start(configJSON []byte) (*exec.Cmd, error) {
	cmd := exec.Command("xray", "run", "-config", "stdin:")
	cmd.Stdin = &bytesReader{data: configJSON}
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("xray start failed: %w", err)
	}
	return cmd, nil
}

// Stop kills the xray process
func Stop(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
