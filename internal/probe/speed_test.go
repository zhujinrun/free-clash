package probe

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// directDialer 直连目标，用于在没有真实节点的情况下测试测速逻辑。
type directDialer struct{}

func (directDialer) Dial(ctx context.Context, target string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", target)
}

// slowDialer 永远失败，模拟连不上的节点。
type slowDialer struct{}

func (slowDialer) Dial(ctx context.Context, target string) (net.Conn, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestSpeedOnceMeasuresThroughput(t *testing.T) {
	const size = 512 * 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, size))
	}))
	defer srv.Close()

	tgt, err := parseTarget(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	n, dur, err := speedOnce(context.Background(), directDialer{}, tgt, 5*time.Second)
	if err != nil {
		t.Fatalf("测速失败: %v", err)
	}
	if n != size {
		t.Errorf("下载 %d 字节，期望 %d", n, size)
	}
	if dur <= 0 {
		t.Errorf("耗时 = %v", dur)
	}
	if mbps := float64(n) / dur.Seconds() / (1024 * 1024); mbps <= 0 {
		t.Errorf("速率 = %v", mbps)
	}
}

func TestSpeedOnceHonoursWindow(t *testing.T) {
	// 服务端迟迟不结束响应：到点按已下载量折算，不能一直等。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 100; i++ {
			if _, err := w.Write(make([]byte, 16*1024)); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	tgt, _ := parseTarget(srv.URL)
	start := time.Now()
	n, _, err := speedOnce(context.Background(), directDialer{}, tgt, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("窗口到点应算成功: %v", err)
	}
	if n <= 0 {
		t.Fatalf("应已下载部分数据，实际 %d", n)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("耗时 %v，未按窗口截断", elapsed)
	}
}

func TestSpeedOnceRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	tgt, _ := parseTarget(srv.URL)
	if _, _, err := speedOnce(context.Background(), directDialer{}, tgt, time.Second); err == nil {
		t.Error("403 应视为测速失败")
	}
}

func TestSpeedtestFillsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 256*1024))
	}))
	defer srv.Close()

	// 只有可用节点会被测速，不可用的应保持 0。
	good := Result{Name: "good", Success: 3, Dialer: directDialer{}}
	bad := Result{Name: "bad", Success: 3, Dialer: slowDialer{}}
	dead := Result{Name: "dead", Success: 0, Dialer: directDialer{}}
	results := []Result{good, bad, dead}

	Speedtest(context.Background(), results, 10, SpeedOptions{
		URL:         srv.URL,
		Timeout:     3 * time.Second,
		Concurrency: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if results[0].SpeedMBps <= 0 {
		t.Errorf("可用节点应测出速率，得到 %v", results[0].SpeedMBps)
	}
	if results[1].SpeedMBps != 0 {
		t.Errorf("连不上的节点速率应为 0，得到 %v", results[1].SpeedMBps)
	}
	if results[2].SpeedMBps != 0 {
		t.Errorf("不可用节点不该参与测速，得到 %v", results[2].SpeedMBps)
	}
}

func TestRankPrefersFasterNode(t *testing.T) {
	// 延迟低的慢节点应排在延迟高但带宽好的节点之后。
	slow := Result{Name: "slow", Success: 3, AvgLatency: 300, SpeedMBps: 0.3}
	fast := Result{Name: "fast", Success: 3, AvgLatency: 1800, SpeedMBps: 8}
	ranked := Rank([]Result{slow, fast}, true)
	if len(ranked) != 2 {
		t.Fatalf("可用节点 = %d，期望 2", len(ranked))
	}
	if ranked[0].Name != "fast" {
		t.Errorf("应按吞吐降序，首位是 %s（%.2fMB/s）", ranked[0].Name, ranked[0].SpeedMBps)
	}
}
