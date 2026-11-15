package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSSLoopbackCiphers 用独立实现（go-shadowsocks2）作为服务端，逐个算法验证 SS 客户端。
//
//	SS_SERVER_BIN=/d/Program/go/bin/go-shadowsocks2.exe go test ./internal/proxy -run TestSSLoopbackCiphers -v
func TestSSLoopbackCiphers(t *testing.T) {
	bin := os.Getenv("SS_SERVER_BIN")
	if bin == "" {
		t.Skip("SS_SERVER_BIN 未设置，跳过 SS 多算法回环测试")
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	const password = "loop-pass-123"

	cases := []struct{ clash, peer string }{
		{"aes-128-gcm", "AEAD_AES_128_GCM"},
		{"aes-256-gcm", "AEAD_AES_256_GCM"},
		{"chacha20-ietf-poly1305", "AEAD_CHACHA20_POLY1305"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.clash, func(t *testing.T) {
			port := freePort(t)
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

			var logs bytes.Buffer
			cmd := exec.Command(bin, "-s", addr, "-cipher", c.peer, "-password", password)
			cmd.Stdout, cmd.Stderr = &logs, &logs
			if err := cmd.Start(); err != nil {
				t.Fatalf("启动服务端失败: %v", err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			if err := waitPort(addr, 10*time.Second); err != nil {
				t.Fatalf("服务端未就绪: %v\n%s", err, logs.String())
			}

			d, err := New(map[string]any{
				"type": "ss", "server": "127.0.0.1", "port": port,
				"cipher": c.clash, "password": password,
			})
			if err != nil {
				t.Fatalf("构造客户端失败: %v", err)
			}
			elapsed, err := httpViaTunnel(d, targetAddr)
			if err != nil {
				t.Fatalf("经隧道访问失败: %v\n--- 服务端日志 ---\n%s", err, logs.String())
			}
			t.Logf("%s 通过，耗时 %v", c.clash, elapsed)
		})
	}
}
