package site

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testSubYAML = `proxies:
  - {name: "HK-01", type: ss, server: hk1.example.com, port: 8388, cipher: aes-256-gcm, password: pass1}
  - {name: "JP-01", type: vmess, server: jp1.example.com, port: 443, uuid: 11111111-2222-3333-4444-555555555555, alterId: 0}
`

// newResolver 关掉日志噪音，返回指向测试服务器的解析器。
func newResolver() *Resolver {
	r := New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	return r
}

// TestResolveFromHashedChunk 复刻真实站点形态：订阅地址藏在带内容哈希的
// JS chunk 里，页面中同时存在推广链接与静态资源，解析结果必须唯一命中。
func TestResolveFromHashedChunk(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><head>
<link rel="modulepreload" href="/assets/chunks/theme.BdP30UdW.js">
<script type="module" src="/assets/app.C8K7CoJB.js"></script>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-TEST"></script>
</head><body>
<button type="button">复制精选订阅</button>
<a href="https://99ba.net/#/register?code=Mirp7pOQ">推广</a>
<a href="/nodes">节点页</a>
</body></html>`))
	})

	mux.HandleFunc("/assets/app.C8K7CoJB.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import{R as p}from"./chunks/theme.BdP30UdW.js";const a=1;`))
	})

	// 关键：订阅地址在 chunk 中，另外混入多个诱饵链接。
	mux.HandleFunc("/assets/chunks/theme.BdP30UdW.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`Bo="` + srv.URL + `/sub/clash.yml",Eo=742,Oo=4125,` +
			`j={updated:"2026-11-14",subscription:Bo,node_count:Oo};` +
			`const x=["https://bb.example-aff.com/#/register?code=abc",` +
			`"http://a.com","https://cdn.jsdelivr.net/gh/x/clash.yaml",` +
			`"https://api.iconify.design/simple-icons/${t.icon}.svg"];`))
	})

	mux.HandleFunc("/sub/clash.yml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Write([]byte(testSubYAML))
	})

	srv = httptest.NewServer(mux)
	defer srv.Close()

	res, err := newResolver().Resolve(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := srv.URL + "/sub/clash.yml"
	if res.URL != want {
		t.Fatalf("订阅地址不符\n期望: %s\n实际: %s", want, res.URL)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("节点数期望 2，实际 %d", len(res.Nodes))
	}
	if res.Direct {
		t.Fatal("页面解析结果不应标记为 Direct")
	}
	// 返回的 Body 应可直接复用，避免调用方二次下载。
	if !strings.Contains(string(res.Body), "hk1.example.com") {
		t.Fatal("返回的 Body 未包含订阅内容")
	}
}

// TestResolveDirectSubscription 传入的已是订阅地址时应直接返回。
func TestResolveDirectSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.Write([]byte(testSubYAML))
	}))
	defer srv.Close()

	res, err := newResolver().Resolve(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !res.Direct || len(res.Nodes) != 2 {
		t.Fatalf("期望 Direct 且 2 个节点，实际 Direct=%v nodes=%d", res.Direct, len(res.Nodes))
	}
}

// TestResolveRejectsNonSubscription 页面里的候选地址都不可用时必须报错，
// 不能随意返回一个链接。
func TestResolveRejectsNonSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><a href="https://example.com/sub/clash.yml">订阅</a>` +
			`<a href="https://example.com/api/user/token">其它</a></body></html>`))
	}))
	defer srv.Close()

	if _, err := newResolver().Resolve(context.Background(), srv.URL); err == nil {
		t.Fatal("候选地址均为 404 时应返回错误")
	}
}

// TestResolveNotHTML 既不是订阅也不是 HTML 时应给出明确错误。
func TestResolveNotHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if _, err := newResolver().Resolve(context.Background(), srv.URL); err == nil {
		t.Fatal("非 HTML 非订阅应返回错误")
	}
}

// TestRankCandidatesPrefersSubscriptionKey 打分应让 subscription 赋值胜出。
func TestRankCandidatesPrefersSubscriptionKey(t *testing.T) {
	text := `var Bo="https://sub.example.com/sub/clash.yml";` +
		`j={subscription:Bo};` +
		`var ads=["https://promo.example.com/#/register?code=x","http://a.com"];`
	cands := rankCandidates([]string{text}, "https://page.example.com/", defaultAnchors)
	if len(cands) == 0 {
		t.Fatal("未提取到候选")
	}
	// 断言赋值语句中的地址得分最高。
	claimed := subscriptionURLs([]string{text})
	if !claimed["https://sub.example.com/sub/clash.yml"] {
		t.Fatal("未识别出 subscription 赋值中的地址")
	}
	for _, c := range cands {
		if c.url == "https://promo.example.com/#/register?code=x" && c.score >= cands[0].score {
			t.Fatalf("推广链接得分过高: %d", c.score)
		}
	}
}

// TestRankCandidatesAnchorBeatsGlobal 按钮模块内的订阅赋值应优先于
// 其它模块里的 subscription 赋值，即使后者带更强的路径特征。
func TestRankCandidatesAnchorBeatsGlobal(t *testing.T) {
	buttonModule := `n("button",{type:"button",onClick:p},g(s.value?"已复制":"复制精选订阅"),1),` +
		`Bo="https://real.example.com/api/v1/sub?token=abc",j={subscription:Bo};`
	otherModule := `var x={subscription:"https://decoy.example.com/sub/clash.yml"};`

	cands := rankCandidates([]string{buttonModule, otherModule}, "https://page.example.com/", defaultAnchors)
	if len(cands) < 2 {
		t.Fatalf("候选数量不足: %d", len(cands))
	}
	if cands[0].url != "https://real.example.com/api/v1/sub?token=abc" {
		t.Fatalf("未优先选中按钮模块内的地址，实际首位: %s", cands[0].url)
	}
	if !strings.Contains(strings.Join(cands[0].why, "+"), "按钮模块") {
		t.Fatalf("打分依据未标注按钮模块: %v", cands[0].why)
	}
}

// TestResolveViaButtonAnchor 端到端复刻真实站点：按钮文案与地址同模块。
func TestResolveViaButtonAnchor(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><body>
<button type="button">复制精选订阅</button>
<script type="module" src="/assets/app.aaa.js"></script>
</body></html>`))
	})
	mux.HandleFunc("/assets/app.aaa.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		// 同一模块内既有按钮渲染代码，也有订阅赋值；另有诱饵模块。
		w.Write([]byte(`Bo="` + srv.URL + `/sub/clash.yml",j={subscription:Bo};` +
			`n("button",{type:"button",onClick:p},"复制精选订阅");`))
	})
	mux.HandleFunc("/sub/clash.yml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.Write([]byte(testSubYAML))
	})

	srv = httptest.NewServer(mux)
	defer srv.Close()

	res, err := newResolver().Resolve(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if res.URL != srv.URL+"/sub/clash.yml" {
		t.Fatalf("订阅地址不符，实际: %s", res.URL)
	}
}

// TestGetRetriesOnNetworkError 连接被中途断开时应自动重试。
func TestGetRetriesOnNetworkError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// 直接断开连接，模拟网络抖动。
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	data, _, _, err := newResolver().get(context.Background(), srv.URL, 1<<20, false, pageTimeout)
	if err != nil {
		t.Fatalf("重试后应成功，实际错误: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("内容不符: %q", data)
	}
	if hits.Load() < 2 {
		t.Fatalf("未发生重试，请求次数: %d", hits.Load())
	}
}

// TestGetNoRetryOnStatusError 4xx 属于确定性结果，不应重试。
func TestGetNoRetryOnStatusError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, _, err := newResolver().get(context.Background(), srv.URL, 1<<20, false, pageTimeout); err == nil {
		t.Fatal("404 应返回错误")
	}
	if hits.Load() != 1 {
		t.Fatalf("404 不应重试，请求次数: %d", hits.Load())
	}
}

// TestCacheRoundTrip 缓存用于站点改版时兜底。
func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Cache{Path: filepath.Join(dir, "nested", "resolved.json")}

	if _, ok := c.Load(); ok {
		t.Fatal("空缓存不应命中")
	}
	if err := c.Save("https://page.example.com/", "https://sub.example.com/sub/clash.yml"); err != nil {
		t.Fatalf("写入缓存失败: %v", err)
	}
	got, ok := c.Load()
	if !ok || got != "https://sub.example.com/sub/clash.yml" {
		t.Fatalf("缓存读取不符: %q ok=%v", got, ok)
	}
}
