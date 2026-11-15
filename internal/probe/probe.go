// Package probe 对节点做并发可用性探测：先做 TCP 预筛淘汰不可达节点，
// 再通过协议隧道请求真实目标地址，测量端到端延迟。
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freeclash/internal/proxy"
	"freeclash/internal/sub"
)

// Options 控制探测行为。
type Options struct {
	ProbeURL       string        // 探测目标，默认 https://www.gstatic.com/generate_204
	PreTimeout     time.Duration // TCP 预筛超时
	ProbeTimeout   time.Duration // 单次完整探测超时
	PreConcurrency int           // 预筛并发
	Concurrency    int           // 探测并发
	Rounds         int           // 每个节点探测轮数
}

// Sample 是一次探测记录。
type Sample struct {
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Result 是单个节点的探测结论。
type Result struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Server     string   `json:"server"`
	Port       int      `json:"port"`
	Region     string   `json:"region"`
	Success    int      `json:"success"`
	Fail       int      `json:"fail"`
	AvgLatency int64    `json:"avg_latency_ms"`
	MinLatency int64    `json:"min_latency_ms"`
	Samples    []Sample `json:"samples,omitempty"`
	SkipReason string   `json:"skip_reason,omitempty"`

	Node    sub.Proxy    `json:"-"`
	Dialer  proxy.Dialer `json:"-"`
	healthy bool
}

// Available 表示该节点至少成功过一次。
func (r *Result) Available() bool { return r.Success > 0 }

type target struct {
	scheme string
	host   string
	addr   string
	path   string
}

func parseTarget(raw string) (target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, err
	}
	if u.Host == "" {
		return target{}, fmt.Errorf("invalid probe url: %s", raw)
	}
	addr := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			addr = net.JoinHostPort(u.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	p := u.RequestURI()
	if p == "" {
		p = "/"
	}
	return target{scheme: u.Scheme, host: u.Hostname(), addr: addr, path: p}, nil
}

// Run 对全部节点执行探测，返回与输入一一对应的结果。
func Run(ctx context.Context, nodes []sub.Proxy, opts Options, logger *slog.Logger) []Result {
	if opts.Rounds <= 0 {
		opts.Rounds = 1
	}
	if opts.PreConcurrency <= 0 {
		opts.PreConcurrency = 256
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 96
	}

	tgt, err := parseTarget(opts.ProbeURL)
	if err != nil {
		logger.Error("探测目标无效", "err", err)
		return nil
	}

	results := make([]Result, len(nodes))
	for i, n := range nodes {
		results[i] = Result{
			Name:   n.Name,
			Type:   n.Type,
			Server: n.Server,
			Port:   n.Port,
			Region: sub.Region(n.Name),
			Node:   n,
		}
		if d, derr := proxy.New(n.Raw); derr != nil {
			results[i].SkipReason = derr.Error()
		} else {
			results[i].Dialer = d
		}
	}

	total := len(results)
	var skipped, preFailed int64
	// 阶段一：TCP 预筛。绝大多数失效节点在这一步即被淘汰，成本极低。
	var preCount int64
	runPool(ctx, total, opts.PreConcurrency, func(i int) {
		r := &results[i]
		if r.Dialer == nil {
			atomic.AddInt64(&skipped, 1)
			return
		}
		if err := tcpCheck(ctx, r.Server, r.Port, opts.PreTimeout); err != nil {
			r.SkipReason = "端口不可达: " + shortErr(err)
			atomic.AddInt64(&preFailed, 1)
		}
		n := atomic.AddInt64(&preCount, 1)
		if n%1000 == 0 {
			logger.Info("预筛进度", "done", n, "total", total)
		}
	})

	logger.Info("TCP 预筛完成",
		"total", total,
		"不支持协议", skipped,
		"端口不可达", preFailed,
		"可继续探测", total-int(skipped)-int(preFailed))

	// 阶段二：真实探测。
	var probed, okCount int64
	runPool(ctx, total, opts.Concurrency, func(i int) {
		r := &results[i]
		if r.Dialer == nil || r.SkipReason != "" {
			return
		}
		for round := 0; round < opts.Rounds; round++ {
			if ctx.Err() != nil {
				return
			}
			lat, perr := probeOnce(ctx, r.Dialer, tgt, opts.ProbeTimeout)
			if perr != nil {
				r.Fail++
				r.Samples = append(r.Samples, Sample{Error: shortErr(perr)})
			} else {
				r.Success++
				r.Samples = append(r.Samples, Sample{LatencyMS: lat.Milliseconds()})
			}
		}
		finalize(r)
		if r.Available() {
			atomic.AddInt64(&okCount, 1)
		}
		n := atomic.AddInt64(&probed, 1)
		if n%200 == 0 {
			logger.Info("探测进度", "done", n, "可用", okCount)
		}
	})

	logger.Info("真实探测完成", "可用节点", okCount, "参与探测", total-int(skipped)-int(preFailed))
	return results
}

// Refine 对排名靠前的候选节点追加多轮探测，降低单次抖动带来的误判。
func Refine(ctx context.Context, results []Result, topK, rounds int, opts Options, logger *slog.Logger) {
	tgt, err := parseTarget(opts.ProbeURL)
	if err != nil {
		return
	}
	healthy := Rank(results, true)
	if topK > len(healthy) {
		topK = len(healthy)
	}
	if topK <= 0 {
		return
	}
	logger.Info("开始二次复测", "节点数", topK, "轮数", rounds)

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxInt(8, opts.Concurrency/4))
	for i := 0; i < topK; i++ {
		wg.Add(1)
		go func(r *Result) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			r.Samples = nil
			r.Success, r.Fail = 0, 0
			for round := 0; round < rounds; round++ {
				lat, perr := probeOnce(ctx, r.Dialer, tgt, opts.ProbeTimeout)
				if perr != nil {
					r.Fail++
					r.Samples = append(r.Samples, Sample{Error: shortErr(perr)})
				} else {
					r.Success++
					r.Samples = append(r.Samples, Sample{LatencyMS: lat.Milliseconds()})
				}
			}
			finalize(r)
		}(healthy[i])
	}
	wg.Wait()
}

func finalize(r *Result) {
	if r.Success == 0 {
		r.AvgLatency, r.MinLatency = 0, 0
		r.healthy = false
		return
	}
	var sum, min int64 = 0, 1 << 62
	for _, s := range r.Samples {
		if s.Error != "" {
			continue
		}
		sum += s.LatencyMS
		if s.LatencyMS < min {
			min = s.LatencyMS
		}
	}
	r.AvgLatency = sum / int64(r.Success)
	r.MinLatency = min
	r.healthy = true
}

// Rank 返回按质量排序的可用节点（指针，指向原切片元素，便于回写复测结果）。
// 排序原则：稳定性优先（成功率降序），其次平均延迟升序。
func Rank(results []Result, healthyOnly bool) []*Result {
	out := make([]*Result, 0, len(results))
	for i := range results {
		if results[i].SkipReason != "" {
			continue
		}
		if healthyOnly && !results[i].Available() {
			continue
		}
		out = append(out, &results[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		si := successRate(*out[i])
		sj := successRate(*out[j])
		if si != sj {
			return si > sj
		}
		return out[i].AvgLatency < out[j].AvgLatency
	})
	return out
}

func successRate(r Result) float64 {
	if r.Success+r.Fail == 0 {
		return 0
	}
	return float64(r.Success) / float64(r.Success+r.Fail)
}

// probeOnce 通过节点建立隧道，向真实目标发起一次 HTTPS 请求，测量端到端耗时。
func probeOnce(parent context.Context, d proxy.Dialer, tgt target, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	conn, err := d.Dial(ctx, tgt.addr)
	if err != nil {
		return 0, err
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
			return 0, fmt.Errorf("tls: %w", err)
		}
		stream = tlsConn
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: freeclash-probe/1.0\r\nAccept: */*\r\nConnection: close\r\n\r\n",
		tgt.path, tgt.host)
	if _, err := stream.Write([]byte(req)); err != nil {
		return 0, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	return time.Since(start), nil
}

func tcpCheck(ctx context.Context, server string, port int, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(server, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// runPool 以受限并发执行 fn(0..n-1)。
func runPool(ctx context.Context, n, concurrency int, fn func(i int)) {
	if n == 0 {
		return
	}
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 90 {
		s = s[:90]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
