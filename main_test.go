package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"freeclash/internal/config"
)

// newFlags 解析参数并把错误转换为测试失败。
func newFlags(t *testing.T, args ...string) *cliFlags {
	t.Helper()
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	return f
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, config.DefaultConfigName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildOptionsDiscoversConfigInCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, dir, "site: https://example.com/\noutput_dir: out\n")

	o, err := buildOptions(newFlags(t))
	if err != nil {
		t.Fatal(err)
	}
	if o.ConfigPath != filepath.Join(dir, "config.yaml") {
		t.Fatalf("ConfigPath = %q", o.ConfigPath)
	}
	if o.Cfg.Site != "https://example.com/" {
		t.Fatalf("Site = %q", o.Cfg.Site)
	}
	// 相对路径按配置文件所在目录展开，服务模式与前台模式行为一致。
	if want := filepath.Join(dir, "out"); o.Cfg.OutputDir != want {
		t.Fatalf("OutputDir = %q want %q", o.Cfg.OutputDir, want)
	}
}

func TestBuildOptionsFlagOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, dir, "site: https://old.example/\ntop_n: 5\ninterval: 1h\nserve_addr: \":1111\"\n")

	o, err := buildOptions(newFlags(t,
		"-site", "https://new.example/",
		"-top", "12",
		"-interval", "45m",
		"-serve", ":9898",
		"-out", "custom",
		"-log", "logs/run.log",
	))
	if err != nil {
		t.Fatal(err)
	}
	if o.Cfg.Site != "https://new.example/" {
		t.Errorf("Site = %q", o.Cfg.Site)
	}
	if o.Cfg.TopN != 12 {
		t.Errorf("TopN = %d", o.Cfg.TopN)
	}
	if got := o.Cfg.Interval.Std(); got != 45*time.Minute {
		t.Errorf("Interval = %v", got)
	}
	if o.Cfg.ServeAddr != ":9898" {
		t.Errorf("ServeAddr = %q", o.Cfg.ServeAddr)
	}
	if want := filepath.Join(dir, "custom"); o.Cfg.OutputDir != want {
		t.Errorf("OutputDir = %q want %q", o.Cfg.OutputDir, want)
	}
	if want := filepath.Join(dir, "logs", "run.log"); o.Cfg.LogFile != want {
		t.Errorf("LogFile = %q want %q", o.Cfg.LogFile, want)
	}
}

func TestBuildOptionsExplicitConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	path := writeConfig(t, dir, "site: https://explicit.example/\n")

	o, err := buildOptions(newFlags(t, "-config", path))
	if err != nil {
		t.Fatal(err)
	}
	if o.ConfigPath != path {
		t.Fatalf("ConfigPath = %q want %q", o.ConfigPath, path)
	}
	if o.Cfg.Site != "https://explicit.example/" {
		t.Fatalf("Site = %q", o.Cfg.Site)
	}
}

// 服务定义里写入的是 -config <绝对路径>，必须能被同一套参数解析还原。
func TestServiceArgumentsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "site: https://svc.example/\n")

	o, err := buildOptions(newFlags(t, "-config", path))
	if err != nil {
		t.Fatal(err)
	}
	if o.ConfigPath != path || o.Cfg.Site != "https://svc.example/" {
		t.Fatalf("参数还原失败: path=%q site=%q", o.ConfigPath, o.Cfg.Site)
	}
}

func TestBuildOptionsDefaultsWithoutConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	o, err := buildOptions(newFlags(t))
	if err != nil {
		t.Fatal(err)
	}
	if o.ConfigPath != "" {
		t.Fatalf("没有配置文件时 ConfigPath 应为空，got %q", o.ConfigPath)
	}
	if o.Cfg.Interval.Std() != 30*time.Minute {
		t.Fatalf("应使用默认间隔，got %v", o.Cfg.Interval.Std())
	}
}

func TestParseFlagsURLList(t *testing.T) {
	o, err := buildOptions(newFlags(t, "-url", "https://a/sub, https://b/sub"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a/sub", "https://b/sub"}
	if len(o.Cfg.Subscriptions) != len(want) {
		t.Fatalf("Subscriptions = %v", o.Cfg.Subscriptions)
	}
	for i := range want {
		if o.Cfg.Subscriptions[i] != want[i] {
			t.Fatalf("Subscriptions[%d] = %q want %q", i, o.Cfg.Subscriptions[i], want[i])
		}
	}
}
