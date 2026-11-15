package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestXrayLoopback 用 Xray 作为独立对端，验证本包 VMess / Trojan 的握手实现。
// Xray 是这些协议的官方级参考实现，若本包实现有偏差，此测试会失败。
//
//	XRAY_BIN=/d/Program/go/bin/xray.exe go test ./internal/proxy -run TestXrayLoopback -v
func TestXrayLoopback(t *testing.T) {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN 未设置，跳过 Xray 回环测试")
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)

	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	const trojanPass = "trojan-test-pass"

	tlsSettings := map[string]any{
		"certificates": []any{map[string]any{
			"certificateFile": certFile,
			"keyFile":         keyFile,
		}},
	}

	cases := []struct {
		name string
		// inbound 为该用例所需的 Xray 入站配置
		inbound map[string]any
		// node 为喂给本包 New() 的 Clash 风格节点
		node func(port int) map[string]any
	}{
		{
			name: "vmess-tcp",
			inbound: map[string]any{
				"protocol": "vmess",
				"settings": map[string]any{
					"clients": []any{map[string]any{"id": uuid, "alterId": 0}},
				},
				"streamSettings": map[string]any{"network": "tcp"},
			},
			node: func(port int) map[string]any {
				return map[string]any{
					"type": "vmess", "server": "127.0.0.1", "port": port,
					"uuid": uuid, "alterId": 0, "cipher": "auto", "network": "tcp",
				}
			},
		},
		{
			name: "vmess-ws",
			inbound: map[string]any{
				"protocol": "vmess",
				"settings": map[string]any{
					"clients": []any{map[string]any{"id": uuid, "alterId": 0}},
				},
				"streamSettings": map[string]any{
					"network":    "ws",
					"wsSettings": map[string]any{"path": "/vmws"},
				},
			},
			node: func(port int) map[string]any {
				return map[string]any{
					"type": "vmess", "server": "127.0.0.1", "port": port,
					"uuid": uuid, "alterId": 0, "cipher": "auto", "network": "ws",
					"ws-opts": map[string]any{"path": "/vmws"},
				}
			},
		},
		{
			name: "vmess-wss",
			inbound: map[string]any{
				"protocol": "vmess",
				"settings": map[string]any{
					"clients": []any{map[string]any{"id": uuid, "alterId": 0}},
				},
				"streamSettings": map[string]any{
					"network":     "ws",
					"security":    "tls",
					"tlsSettings": tlsSettings,
					"wsSettings": map[string]any{
						"path":    "/vmwss",
						"headers": map[string]any{"Host": "vm.example.com"},
					},
				},
			},
			node: func(port int) map[string]any {
				return map[string]any{
					"type": "vmess", "server": "127.0.0.1", "port": port,
					"uuid": uuid, "alterId": 0, "cipher": "auto", "network": "ws",
					"tls": true, "servername": "vm.example.com", "skip-cert-verify": true,
					"ws-opts": map[string]any{
						"path":    "/vmwss",
						"headers": map[string]any{"Host": "vm.example.com"},
					},
				}
			},
		},
		{
			name: "trojan-tcp",
			inbound: map[string]any{
				"protocol": "trojan",
				"settings": map[string]any{
					"clients": []any{map[string]any{"password": trojanPass}},
				},
				"streamSettings": map[string]any{
					"network":     "tcp",
					"security":    "tls",
					"tlsSettings": tlsSettings,
				},
			},
			node: func(port int) map[string]any {
				return map[string]any{
					"type": "trojan", "server": "127.0.0.1", "port": port,
					"password": trojanPass, "sni": "tr.example.com", "skip-cert-verify": true,
				}
			},
		},
		{
			name: "trojan-ws",
			inbound: map[string]any{
				"protocol": "trojan",
				"settings": map[string]any{
					"clients": []any{map[string]any{"password": trojanPass}},
				},
				"streamSettings": map[string]any{
					"network":     "ws",
					"security":    "tls",
					"tlsSettings": tlsSettings,
					"wsSettings": map[string]any{
						"path":    "/trws",
						"headers": map[string]any{"Host": "tr.example.com"},
					},
				},
			},
			node: func(port int) map[string]any {
				return map[string]any{
					"type": "trojan", "server": "127.0.0.1", "port": port,
					"password": trojanPass, "sni": "tr.example.com", "skip-cert-verify": true,
					"network": "ws",
					"ws-opts": map[string]any{
						"path":    "/trws",
						"headers": map[string]any{"Host": "tr.example.com"},
					},
				}
			},
		},
	}

	// 所有入站一次性写入同一份配置，避免反复启动进程。
	var inbounds []any
	ports := make([]int, len(cases))
	for i, c := range cases {
		port := freePort(t)
		ports[i] = port
		inb := map[string]any{
			"port": port, "listen": "127.0.0.1",
			"protocol":       c.inbound["protocol"],
			"settings":       c.inbound["settings"],
			"streamSettings": c.inbound["streamSettings"],
		}
		inbounds = append(inbounds, inb)
	}

	cfg := map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"inbounds": inbounds,
		"outbounds": []any{
			map[string]any{"protocol": "freedom"},
		},
	}
	cfgPath := filepath.Join(dir, "server.json")
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatalf("写 Xray 配置失败: %v", err)
	}

	cmd := exec.Command(bin, "run", "-c", cfgPath)
	cmd.Dir = dir
	logFile, _ := os.Create(filepath.Join(dir, "xray.log"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Xray 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
	})

	// 等待全部入站端口就绪。
	for _, port := range ports {
		if err := waitPort("127.0.0.1:"+strconv.Itoa(port), 12*time.Second); err != nil {
			logs, _ := os.ReadFile(filepath.Join(dir, "xray.log"))
			t.Fatalf("Xray 入站 %d 未就绪: %v\n--- xray 日志 ---\n%s", port, err, tailText(string(logs), 1500))
		}
	}

	for i, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			d, err := New(c.node(ports[i]))
			if err != nil {
				t.Fatalf("构造客户端失败: %v", err)
			}
			elapsed, err := httpViaTunnel(d, targetAddr)
			if err != nil {
				logs, _ := os.ReadFile(filepath.Join(dir, "xray.log"))
				t.Fatalf("经隧道访问目标失败: %v\n--- xray 日志 ---\n%s", err, tailText(string(logs), 1200))
			}
			t.Logf("%s 通过，耗时 %v", c.name, elapsed)
		})
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配端口失败: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("超时未监听 %s", addr)
}

func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "freeclash-loopback"},
		DNSNames:     []string{"vm.example.com", "tr.example.com", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certPath)
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化私钥失败: %v", err)
	}
	keyOut, _ := os.Create(keyPath)
	_ = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_ = keyOut.Close()
	return certPath, keyPath
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
