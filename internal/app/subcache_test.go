package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"freeclash/internal/config"
)

func cacheTestRunner(t *testing.T) *runner {
	t.Helper()
	r := testRunner(t)
	r.cfg.SubCacheTTL = config.Duration(12 * time.Hour)
	return r
}

const cacheTestBody = "trojan://pass@a.example.com:443?security=tls#A\n" +
	"trojan://pass@b.example.com:443?security=tls#B\n"

func TestFetchSubNodesFallsBackToCache(t *testing.T) {
	r := cacheTestRunner(t)
	src := filepath.Join(t.TempDir(), "sub.txt")
	if err := os.WriteFile(src, []byte(cacheTestBody), 0o644); err != nil {
		t.Fatal(err)
	}

	nodes, cached, err := r.fetchSubNodes(context.Background(), src, 5*time.Second)
	if err != nil || cached {
		t.Fatalf("首次拉取应成功且不走缓存: cached=%v err=%v", cached, err)
	}
	if len(nodes) != 2 {
		t.Fatalf("节点数 = %d，期望 2", len(nodes))
	}
	if _, err := os.Stat(r.subCacheFile(src)); err != nil {
		t.Fatalf("成功拉取后没写缓存: %v", err)
	}

	// 源被限流 / 宕掉：应当用缓存顶上，而不是整条源作废。
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	nodes, cached, err = r.fetchSubNodes(context.Background(), src, 5*time.Second)
	if err != nil {
		t.Fatalf("源不可用时应回退缓存，却报错: %v", err)
	}
	if !cached {
		t.Error("回退结果没有标记为来自缓存")
	}
	if len(nodes) != 2 {
		t.Errorf("缓存节点数 = %d，期望 2", len(nodes))
	}
}

func TestFetchSubNodesCacheExpires(t *testing.T) {
	r := cacheTestRunner(t)
	src := filepath.Join(t.TempDir(), "sub.txt")
	if err := os.WriteFile(src, []byte(cacheTestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.fetchSubNodes(context.Background(), src, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}

	// 超过有效期后，缓存里的节点多半已失效，不该再被采用。
	old := time.Now().Add(-13 * time.Hour)
	if err := os.Chtimes(r.subCacheFile(src), old, old); err != nil {
		t.Fatal(err)
	}
	if _, cached, err := r.fetchSubNodes(context.Background(), src, 5*time.Second); err == nil || cached {
		t.Errorf("过期缓存被采用了: cached=%v err=%v", cached, err)
	}
	if _, err := os.Stat(r.subCacheFile(src)); !os.IsNotExist(err) {
		t.Error("过期缓存文件没有被清掉")
	}
}

func TestFetchSubNodesDoesNotCacheEmpty(t *testing.T) {
	r := cacheTestRunner(t)
	src := filepath.Join(t.TempDir(), "sub.txt")
	// 订阅改版时可能返回 200 但内容为空或无法解析，这时写缓存会把上一次
	// 的好数据覆盖掉，下次故障就彻底没得兜底。
	if err := os.WriteFile(src, []byte("<html>not a subscription</html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cached, err := r.fetchSubNodes(context.Background(), src, 5*time.Second); err == nil || cached {
		t.Errorf("空内容应报错: cached=%v err=%v", cached, err)
	}
	if _, err := os.Stat(r.subCacheFile(src)); !os.IsNotExist(err) {
		t.Error("解析失败的响应被写进了缓存")
	}
}

func TestPruneSubCache(t *testing.T) {
	r := cacheTestRunner(t)
	dir := r.subCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.gz")
	stale := filepath.Join(dir, "stale.gz")
	partial := filepath.Join(dir, "partial.gz.tmp")
	for _, p := range []string{fresh, stale, partial} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-13 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}

	r.pruneSubCache()

	if _, err := os.Stat(fresh); err != nil {
		t.Error("未过期的缓存被误删")
	}
	for _, p := range []string{stale, partial} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s 应被清理", filepath.Base(p))
		}
	}
}

func TestSubCacheSummary(t *testing.T) {
	if got := subCacheSummary(0, 41); got != "全部 41 条实时拉取" {
		t.Errorf("全实时 = %q", got)
	}
	if got := subCacheSummary(3, 41); got != "3/41 条走缓存" {
		t.Errorf("部分缓存 = %q", got)
	}
}
