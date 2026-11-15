package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"freeclash/internal/proxy"
)

// SpeedOptions 控制下载测速。
type SpeedOptions struct {
	URL         string        // 测速地址，返回体越大越准
	Timeout     time.Duration // 单个节点的下载窗口，到点即按已下载量折算速率
	Concurrency int           // 并发节点数
}

// Speedtest 通过候选节点的隧道下载一段数据，测出实际吞吐（MB/s）。
//
// 为什么不能只靠延迟排序：探测测到的延迟是「建隧道 → TLS → 取回首包」的耗时，
// 其中包含 DNS 解析、多次握手与链路远近，和节点能跑多少带宽没有必然关系。实测同一批
// CDN 落地节点，客户端里 289ms / 8.2MB/s，本工具探到 1542ms，按延迟排序会把它们全部
// 压到截断线之外。吞吐才是用户真正能感知的指标，所以对候选节点直接拉一段数据来量。
//
// 用的是「固定时间窗口」而不是「固定体积」：慢节点不会拖住整轮，到点按已下载量折算，
// 快节点拉满预设体积就结束。
func Speedtest(ctx context.Context, results []Result, topK int, opts SpeedOptions, logger *slog.Logger) {
	if opts.URL == "" {
		return
	}
	tgt, err := parseTarget(opts.URL)
	if err != nil {
		logger.Warn("测速地址无效，跳过测速", "url", opts.URL, "err", err)
		return
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}

	cand := Rank(results, true)
	if topK > 0 && topK < len(cand) {
		cand = cand[:topK]
	}
	if len(cand) == 0 {
		return
	}
	logger.Info("开始下载测速", "节点数", len(cand), "窗口", opts.Timeout.String())

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	sem := make(chan struct{}, opts.Concurrency)
	for _, r := range cand {
		wg.Add(1)
		go func(r *Result) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			n, dur, err := speedOnce(ctx, r.Dialer, tgt, opts.Timeout)
			if dur <= 0 {
				dur = opts.Timeout
			}
			mbps := float64(n) / dur.Seconds() / (1024 * 1024)
			mu.Lock()
			r.SpeedBytes = n
			r.SpeedMBps = mbps
			mu.Unlock()
			if err != nil {
				logger.Debug("测速失败", "节点", r.Name, "已下载", n, "err", err)
				return
			}
			mu.Lock()
			okCount++
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	logger.Info("下载测速完成", "测得", okCount, "共", len(cand))
}

// speedOnce 在隧道内下载测速数据，返回下载字节数、耗时与错误。
// 到达时间窗口时按已下载量返回，不算失败——慢节点的速率正是要靠这个体现。
func speedOnce(parent context.Context, d proxy.Dialer, tgt target, timeout time.Duration) (int64, time.Duration, error) {
	if d == nil {
		return 0, 0, fmt.Errorf("节点不可用")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	conn, err := d.Dial(ctx, tgt.addr)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	var stream net.Conn = conn
	if tgt.scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: tgt.host,
			NextProtos: []string{"http/1.1"},
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, 0, fmt.Errorf("tls: %w", err)
		}
		stream = tlsConn
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: freeclash-speed/1.0\r\nAccept: */*\r\nConnection: close\r\n\r\n",
		tgt.path, tgt.host)

	start := time.Now()
	if _, err := stream.Write([]byte(req)); err != nil {
		return 0, 0, err
	}

	br := bufio.NewReaderSize(stream, 32*1024)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, time.Since(start), fmt.Errorf("http %d", resp.StatusCode)
	}

	var n int64
	buf := make([]byte, 32*1024)
	// 从收到第一个字节开始计时，把首包时延（DNS、握手、TTFB）排除在外。
	// 这里要的是吞吐，把 TTFB 摊进去会让高延迟节点的速率被系统性压低。
	start = time.Now()
	for {
		k, rerr := resp.Body.Read(buf)
		n += int64(k)
		if rerr != nil {
			if rerr == io.EOF {
				return n, time.Since(start), nil
			}
			// 窗口到点或对端提前断开：已下载的量仍然有效。
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				return n, time.Since(start), nil
			}
			if n > 0 {
				return n, time.Since(start), nil
			}
			return n, time.Since(start), rerr
		}
	}
}
