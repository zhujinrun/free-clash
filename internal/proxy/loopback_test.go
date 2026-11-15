package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这些是回环集成测试：用一个独立实现的代理服务端作为对端，
// 验证本包实现的握手是否符合协议规范。默认跳过，需显式提供环境变量。
//
//	SS_SERVER_ADDR=127.0.0.1:18488 SS_CIPHER=AEAD_AES_256_GCM SS_PASSWORD=testpass go test ./internal/proxy -run Loopback
func TestSSLoopback(t *testing.T) {
	addr := os.Getenv("SS_SERVER_ADDR")
	if addr == "" {
		t.Skip("SS_SERVER_ADDR 未设置，跳过 SS 回环测试")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("解析 SS_SERVER_ADDR 失败: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	// 被访问的真实目标：一个本地 HTTP 服务。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	cipherName := os.Getenv("SS_CIPHER")
	if cipherName == "" {
		cipherName = "AEAD_AES_256_GCM"
	}
	// go-shadowsocks2 的命名带 AEAD_ 前缀，本程序使用 clash 命名。
	name := strings.ToLower(strings.TrimPrefix(cipherName, "AEAD_"))
	name = strings.ReplaceAll(name, "_", "-")

	d, err := New(map[string]any{
		"type":     "ss",
		"server":   host,
		"port":     port,
		"cipher":   name,
		"password": os.Getenv("SS_PASSWORD"),
	})
	if err != nil {
		t.Fatalf("构造 SS 客户端失败: %v", err)
	}

	elapsed, err := httpViaTunnel(d, strings.TrimPrefix(target.URL, "http://"))
	if err != nil {
		t.Fatalf("经 SS 隧道访问目标失败: %v", err)
	}
	t.Logf("SS 回环成功，耗时 %v", elapsed)
}

func httpViaTunnel(d Dialer, target string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := d.Dial(ctx, target)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))

	host, _, _ := net.SplitHostPort(target)
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return time.Since(start), nil
}
