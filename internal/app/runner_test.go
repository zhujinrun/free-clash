package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"freeclash/internal/config"
	"freeclash/internal/probe"
	"freeclash/internal/site"
	"freeclash/internal/sub"
)

func testRunner(t *testing.T) *runner {
	t.Helper()
	return &runner{
		cfg: config.Config{
			OutputDir:    t.TempDir(),
			RefineRounds: 3,
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastGood: make(map[string]int),
	}
}

func healthyResult(typ, server string, port int) probe.Result {
	node := sub.Proxy{Name: "n", Type: typ, Server: server, Port: port}
	return probe.Result{
		Name: node.Name, Type: typ, Server: server, Port: port,
		Success: 1, Node: node,
	}
}

func deadResult(typ, server string, port int) probe.Result {
	node := sub.Proxy{Name: "n", Type: typ, Server: server, Port: port}
	return probe.Result{
		Name: node.Name, Type: typ, Server: server, Port: port,
		Fail: 1, Node: node,
	}
}

func TestNodeKeyIgnoresName(t *testing.T) {
	a := sub.Proxy{Name: "改名前", Type: "ss", Server: "1.2.3.4", Port: 8388}
	b := sub.Proxy{Name: "改名后", Type: "ss", Server: "1.2.3.4", Port: 8388}
	if nodeKey(a) != nodeKey(b) {
		t.Errorf("同名节点改名后键变了：%s vs %s", nodeKey(a), nodeKey(b))
	}
	c := sub.Proxy{Name: "改名前", Type: "ss", Server: "1.2.3.4", Port: 8389}
	if nodeKey(a) == nodeKey(c) {
		t.Error("不同端口的节点键相同")
	}
}

func TestRememberGoodRefreshesAndExpires(t *testing.T) {
	r := testRunner(t)
	r.round = 1
	r.rememberGood([]probe.Result{
		healthyResult("ss", "1.1.1.1", 80),
		healthyResult("vmess", "2.2.2.2", 443),
		deadResult("trojan", "3.3.3.3", 443),
	})
	if len(r.lastGood) != 2 {
		t.Fatalf("记忆 = %d 个，期望 2", len(r.lastGood))
	}
	if _, ok := r.lastGood["trojan|3.3.3.3|443|"]; ok {
		t.Error("失败节点不该进入记忆")
	}

	// 4 轮后第一轮的记录应过期，只有本轮通过的那一个留下。
	r.round = 1 + lastGoodTTL + 1
	r.rememberGood([]probe.Result{healthyResult("ss", "1.1.1.1", 80)})
	if len(r.lastGood) != 1 {
		t.Fatalf("过期清理后 = %d 个，期望 1：%v", len(r.lastGood), r.lastGood)
	}
	if _, ok := r.lastGood["vmess|2.2.2.2|443|"]; ok {
		t.Error("超过 TTL 的记忆没有清掉")
	}

	// 连续通过会刷新轮次，不会被清。
	r.round++
	r.rememberGood([]probe.Result{healthyResult("ss", "1.1.1.1", 80)})
	if r.lastGood["ss|1.1.1.1|80|"] != r.round {
		t.Errorf("轮次没有刷新：%d", r.lastGood["ss|1.1.1.1|80|"])
	}
}

func TestLastGoodPersistsAcrossRounds(t *testing.T) {
	r := testRunner(t)
	r.round = 5
	r.rememberGood([]probe.Result{healthyResult("ss", "9.9.9.9", 443)})

	restored := &runner{
		cfg:      r.cfg,
		log:      r.log,
		lastGood: make(map[string]int),
	}
	restored.loadLastGood()
	if restored.round != 5 {
		t.Errorf("恢复的轮次 = %d，期望 5", restored.round)
	}
	if _, ok := restored.lastGood["ss|9.9.9.9|443|"]; !ok {
		t.Error("重启后没有恢复记忆，首轮无法补测")
	}
}

func TestLoadLastGoodDropsExpired(t *testing.T) {
	r := testRunner(t)
	// 手工造一份「最后一轮是 20，某节点只在第 1 轮通过」的记录：
	// 差了 19 轮，早该丢弃，不该再拿它去补测。
	saved := []byte(`{"round":20,"nodes":{"ss|9.9.9.9|443|":1,"ss|8.8.8.8|443|":19}}`)
	if err := os.WriteFile(r.lastGoodPath(), saved, 0o644); err != nil {
		t.Fatal(err)
	}
	r.loadLastGood()
	if r.round != 20 {
		t.Errorf("轮次 = %d，期望 20", r.round)
	}
	if _, ok := r.lastGood["ss|9.9.9.9|443|"]; ok {
		t.Error("过期 19 轮的记忆被恢复了")
	}
	if _, ok := r.lastGood["ss|8.8.8.8|443|"]; !ok {
		t.Error("上一轮的记忆被误删")
	}
}

func TestRetryDroppedWithoutMemory(t *testing.T) {
	r := testRunner(t)
	results := []probe.Result{deadResult("ss", "1.1.1.1", 80)}
	if n := r.retryDropped(context.Background(), results, probe.Options{}); n != 0 {
		t.Errorf("没有记忆时补测了 %d 个", n)
	}
}

func TestRetryDroppedSkipsHealthyAndUnknown(t *testing.T) {
	r := testRunner(t)
	r.lastGood["ss|1.1.1.1|80|"] = 1
	results := []probe.Result{
		healthyResult("ss", "1.1.1.1", 80), // 已可用，不该补测
		deadResult("ss", "8.8.8.8", 80),    // 不在记忆里，不该补测
	}
	// 空补测集合时直接返回，不会走到真实的网络探测。
	if n := r.retryDropped(context.Background(), results, probe.Options{}); n != 0 {
		t.Errorf("不该补测任何节点，却补测了 %d 个", n)
	}
}

func TestArticleSubsCacheRoundTrip(t *testing.T) {
	r := testRunner(t)
	src := &site.ArticleSources{
		Articles: []string{"https://site/posts/auto/2026-11-15-free-clash-nodes"},
		Subs:     []string{"https://a.example/sub", "https://b.example/sub"},
	}
	r.saveArticleSubs(src)

	subs, savedAt, ok := r.loadArticleSubs()
	if !ok {
		t.Fatal("刚写入的缓存读不回来")
	}
	if len(subs) != 2 || subs[0] != "https://a.example/sub" {
		t.Errorf("订阅列表 = %v", subs)
	}
	if time.Since(savedAt) > time.Minute {
		t.Errorf("缓存时间异常: %v", savedAt)
	}
}

func TestArticleSubsCacheExpires(t *testing.T) {
	r := testRunner(t)
	// 过期一天以上的地址列表不再可信，应丢弃让站点重新解析。
	old := []byte(`{"saved_at":"2020-01-01T00:00:00Z","articles":[],"subs":["https://x.example/sub"]}`)
	if err := os.WriteFile(r.articleSubsPath(), old, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.loadArticleSubs(); ok {
		t.Error("过期缓存被采用了")
	}
}

func TestArticleSubsCacheCorrupt(t *testing.T) {
	r := testRunner(t)
	if err := os.WriteFile(r.articleSubsPath(), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.loadArticleSubs(); ok {
		t.Error("损坏的缓存被采用了")
	}
}

func TestNodeKeyDistinguishesSameServerCredentials(t *testing.T) {
	// 同一台服务器上的多个 uuid 是不同节点，键不能合并。
	a := sub.Proxy{Type: "vless", Server: "1.2.3.4", Port: 443, Raw: map[string]any{"uuid": "aaa"}}
	b := sub.Proxy{Type: "vless", Server: "1.2.3.4", Port: 443, Raw: map[string]any{"uuid": "bbb"}}
	if nodeKey(a) == nodeKey(b) {
		t.Errorf("同地址不同 uuid 的节点键相同：%s", nodeKey(a))
	}
	c := sub.Proxy{Type: "vless", Server: "1.2.3.4", Port: 443, Raw: map[string]any{"uuid": "aaa"}}
	if nodeKey(a) != nodeKey(c) {
		t.Error("同一节点两次算出的键不同")
	}
}
