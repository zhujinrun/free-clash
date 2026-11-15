package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPreviousSnapshot(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"best.yaml":  "proxies:/n  - {name: a, type: ss, server: 1.1.1.1, port: 1}\n",
		"best.txt":   "YmFzZTY0",
		"best.links": "ss://x@1.1.1.1:1#SG-01\ntrojan://p@2.2.2.2:443#%F0%9F%87%AD%F0%9F%87%B0HK-02\n\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	snap, ok := loadPreviousSnapshot(dir)
	if !ok {
		t.Fatal("上次的产出没有被载入")
	}
	if !snap.Warm {
		t.Error("预载快照应标记为 warm")
	}
	if len(snap.Config) == 0 || len(snap.Subscription) == 0 {
		t.Error("配置或订阅内容缺失")
	}
	if snap.Available != 2 {
		t.Errorf("节点数 = %d，期望 2（空行不算）", snap.Available)
	}
	if snap.Updated.IsZero() {
		t.Error("更新时间缺失")
	}
	if len(snap.TopNodes) != 2 || snap.TopNodes[1] != "🇭🇰HK-02" {
		t.Errorf("节点名未解码: %v", snap.TopNodes)
	}
}

func TestLoadPreviousSnapshotMissing(t *testing.T) {
	if _, ok := loadPreviousSnapshot(t.TempDir()); ok {
		t.Error("空目录不该产生快照")
	}
}

func TestLoadPreviousSnapshotOnlyYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "best.yaml"), []byte("proxies: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadPreviousSnapshot(dir); ok {
		t.Error("只有 YAML、没有订阅内容时不该预载")
	}
}
