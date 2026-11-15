package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 生成的配置文件必须是人能读懂的：时长写成 "30m" 而不是纳秒整数。
func TestSaveWritesReadableDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interval: 30m", "pre_timeout: 2s", "probe_timeout: 6s"} {
		if !strings.Contains(raw, want) {
			t.Errorf("配置中缺少 %q，实际内容:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "1800000000000") {
		t.Errorf("时长不应写成纳秒整数:\n%s", raw)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Default()
	cfg.Site = "https://example.com/"
	cfg.Interval = Duration(45 * time.Minute)
	cfg.TopN = 7
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != cfg.Site || got.TopN != cfg.TopN {
		t.Fatalf("Site/TopN 不一致: %+v", got)
	}
	if got.Interval.Std() != 45*time.Minute {
		t.Fatalf("Interval = %v", got.Interval.Std())
	}
	if got.SiteButton != cfg.SiteButton {
		t.Fatalf("SiteButton = %q", got.SiteButton)
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
