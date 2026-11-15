package site

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// 构造一个与真实站点结构一致的小站：首页有「查看节点文章」入口，
// /nodes 列出往期文章，每篇文章用 copy-box + code 展示订阅地址。
func newArticleSite(t *testing.T) *httptest.Server {
	t.Helper()

	page := func(date string, subs []string) string {
		body := fmt.Sprintf(`<!doctype html><html><body><h1>%s 免费节点</h1>`, date)
		for i, s := range subs {
			body += fmt.Sprintf(`<div class="copy-box"><div><span>%02d. 通用订阅</span><code>%s</code></div><button>复制</button></div>`, i+1, s)
		}
		// 干扰项：推广与站内链接不应被当成订阅。
		body += `<a href="https://promo.example.com/#/register?code=abc">注册</a>`
		body += `<a href="/posts/auto/2026-11-14-free-clash-nodes">查看上一期检测结果 →</a>`
		return body + `</body></html>`
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<html><body>
<div class="hero-card-actions"><button>复制精选订阅</button>
<a href="/posts/auto/2026-11-15-free-clash-nodes">查看节点文章 →</a></div>
<a href="/nodes">免费节点</a>
</body></html>`)
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
<a href="/posts/auto/2026-11-15-free-clash-nodes">11-15</a>
<a href="/posts/auto/2026-11-14-free-clash-nodes">11-14</a>
<a href="/posts/auto/2026-11-03-free-clash-nodes">11-03</a>
</body></html>`)
	})
	mux.HandleFunc("/posts/auto/2026-11-15-free-clash-nodes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page("11-15", []string{
			"https://sub1.example.com/v2ray.txt",
			"https://sub2.example.com/api/v1/client/subscribe?token=xyz",
		}))
	})
	mux.HandleFunc("/posts/auto/2026-11-14-free-clash-nodes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page("11-14", []string{
			"https://sub2.example.com/api/v1/client/subscribe?token=xyz", // 与 11-15 重复
			"https://sub3.example.com/base64.txt",
		}))
	})
	mux.HandleFunc("/posts/auto/2026-11-03-free-clash-nodes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page("11-03", []string{"https://old.example.com/v2ray.txt"}))
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("地址无法解析: %v", err)
	}
	return u
}

func newTestResolver() *Resolver {
	r := New(nil)
	r.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return r
}

func TestArticleSubscriptions(t *testing.T) {
	srv := newArticleSite(t)
	r := newTestResolver()

	src, err := r.ArticleSubscriptions(context.Background(), srv.URL, 2, 0)
	if err != nil {
		t.Fatal(err)
	}

	// 应按日期倒序只取最近两篇。
	wantArticles := []string{
		srv.URL + "/posts/auto/2026-11-15-free-clash-nodes",
		srv.URL + "/posts/auto/2026-11-14-free-clash-nodes",
	}
	if len(src.Articles) != 2 || src.Articles[0] != wantArticles[0] || src.Articles[1] != wantArticles[1] {
		t.Fatalf("文章 = %v want %v", src.Articles, wantArticles)
	}

	// 去重后 3 条，且不含推广与站内链接。
	want := []string{
		"https://sub1.example.com/v2ray.txt",
		"https://sub2.example.com/api/v1/client/subscribe?token=xyz",
		"https://sub3.example.com/base64.txt",
	}
	if len(src.Subs) != len(want) {
		t.Fatalf("订阅 = %v want %v", src.Subs, want)
	}
	for i := range want {
		if src.Subs[i] != want[i] {
			t.Errorf("第 %d 条 = %q want %q", i+1, src.Subs[i], want[i])
		}
	}
}

func TestArticleSubscriptionsLimit(t *testing.T) {
	srv := newArticleSite(t)
	r := newTestResolver()

	src, err := r.ArticleSubscriptions(context.Background(), srv.URL, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Articles) != 1 {
		t.Errorf("文章数 = %d want 1", len(src.Articles))
	}
	if len(src.Subs) != 1 {
		t.Errorf("订阅数 = %d want 1", len(src.Subs))
	}

	// articleLimit=0 表示不跟进文章，不应发起任何抓取。
	src, err = r.ArticleSubscriptions(context.Background(), srv.URL, 0, 0)
	if err != nil || len(src.Articles) != 0 || len(src.Subs) != 0 {
		t.Errorf("articleLimit=0 时应为空: %v %+v", err, src)
	}
}

// 站点结构变化、没有 <code> 卡片时，仍应能通过标签兜底抽到地址。
func TestExtractSubURLsFallback(t *testing.T) {
	body := []byte(`<html><body>
<div><span>通用订阅</span><span><a href="https://fallback.example.com/sub.txt">点此</a></span></div>
</body></html>`)
	got := extractSubURLs(body, nil)
	if len(got) != 1 || got[0] != "https://fallback.example.com/sub.txt" {
		t.Fatalf("兜底抽取 = %v", got)
	}
}

func TestSortArticles(t *testing.T) {
	in := []string{
		"https://x.test/posts/manual/clashbox-complete-guide",
		"https://x.test/posts/auto/2026-11-03-free-clash-nodes",
		"https://x.test/posts/auto/2026-11-15-free-clash-nodes",
		"https://x.test/posts/auto/2026-11-15-free-clash-nodes",
	}
	got := sortArticles(in)
	want := []string{
		"https://x.test/posts/auto/2026-11-15-free-clash-nodes",
		"https://x.test/posts/auto/2026-11-03-free-clash-nodes",
		"https://x.test/posts/manual/clashbox-complete-guide",
	}
	if len(got) != len(want) {
		t.Fatalf("排序结果 = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 位 = %q want %q", i+1, got[i], want[i])
		}
	}
}

func TestIsPlausibleSub(t *testing.T) {
	base := mustParse(t, "https://site.test/")
	bad := []string{
		"https://site.test/posts/auto/x",              // 站内
		"https://promo.example.com/#/register?code=1", // 推广
		"https://t.me/somechannel",                    // 社交
		"https://cdn.example.com/app.js",              // 静态资源
	}
	for _, u := range bad {
		if isPlausibleSub(mustParse(t, u), base) {
			t.Errorf("%s 不应被当作订阅", u)
		}
	}
	good := []string{
		"https://raw.githubusercontent.com/x/y/main/sub",
		"https://sub.example.com/api/v1/client/subscribe?token=a",
	}
	for _, u := range good {
		if !isPlausibleSub(mustParse(t, u), base) {
			t.Errorf("%s 应被保留", u)
		}
	}
}

// 首页除了节点文章，还挂着客户端教程（/posts/manual/...）。这些页面里的
// 链接是示例和推广，混进候选只会白拉一遍——尤其当 /nodes 列表页超时、
// 只能靠首页兜底的时候。
func TestArticleLinksSkipTutorials(t *testing.T) {
	var listHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>
<a href="/posts/auto/2026-11-15-free-clash-nodes">查看节点文章 →</a>
<a href="/nodes">免费节点</a>
<a href="/posts/manual/clashbox-complete-guide">ClashBox 教程</a>
<a href="/posts/manual/v2rayng-complete-guide">v2rayNG 教程</a>
</body></html>`)
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&listHits, 1)
		fmt.Fprint(w, `<html><body>
<a href="/posts/auto/2026-11-14-free-clash-nodes">11-14</a>
<a href="/posts/auto/2026-11-13-free-clash-nodes">11-13</a>
<a href="/posts/manual/streisand-complete-guide">教程</a>
</body></html>`)
	})
	for _, p := range []string{"2026-11-15", "2026-11-14", "2026-11-13"} {
		date := p
		mux.HandleFunc("/posts/auto/"+date+"-free-clash-nodes", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `<html><body><div class="copy-box"><code>https://sub.example.com/%s.txt</code></div></body></html>`, date)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := newTestResolver()
	src, err := r.ArticleSubscriptions(context.Background(), srv.URL, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Articles) != 3 {
		t.Fatalf("文章数 = %d want 3: %v", len(src.Articles), src.Articles)
	}
	for _, u := range src.Articles {
		if strings.Contains(u, "/posts/manual/") {
			t.Errorf("教程文章不应进入候选: %s", u)
		}
	}
	// 入口命中的那篇必须排第一。
	if !strings.HasSuffix(src.Articles[0], "/posts/auto/2026-11-15-free-clash-nodes") {
		t.Errorf("首篇应是入口指向的当天文章，实际 %v", src.Articles)
	}

	// 只跟一篇时不应再请求列表页。
	atomic.StoreInt32(&listHits, 0)
	if _, err := r.ArticleSubscriptions(context.Background(), srv.URL, 1, 0); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&listHits); n != 0 {
		t.Errorf("article_limit=1 时不应抓取 /nodes，实际请求 %d 次", n)
	}
}

// 入口指向的文章路径里可能没有日期，此时按日期排序会把它压到后面。
func TestArticleLinksAnchorFirst(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>
<a href="/posts/latest">查看节点文章 →</a>
<a href="/nodes">免费节点</a>
</body></html>`)
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>
<a href="/posts/auto/2026-11-14-free-clash-nodes">11-14</a>
</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := newTestResolver()
	src, err := r.ArticleSubscriptions(context.Background(), srv.URL, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Articles) != 2 {
		t.Fatalf("文章数 = %d want 2: %v", len(src.Articles), src.Articles)
	}
	if src.Articles[0] != srv.URL+"/posts/latest" {
		t.Errorf("无日期的入口应排第一，实际 %v", src.Articles)
	}
}
