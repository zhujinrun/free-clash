package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverPrefersExeDir(t *testing.T) {
	exeDir := t.TempDir()
	cwd := t.TempDir()
	write(t, filepath.Join(exeDir, DefaultConfigName))
	write(t, filepath.Join(cwd, DefaultConfigName))

	if got, want := Discover(exeDir, cwd), filepath.Join(exeDir, DefaultConfigName); got != want {
		t.Fatalf("优先取可执行文件目录，got %q want %q", got, want)
	}
}

func TestDiscoverFallsBackToCwd(t *testing.T) {
	exeDir := t.TempDir()
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, DefaultConfigName))

	if got, want := Discover(exeDir, cwd), filepath.Join(cwd, DefaultConfigName); got != want {
		t.Fatalf("回退到工作目录，got %q want %q", got, want)
	}
}

func TestDiscoverMissing(t *testing.T) {
	if got := Discover(t.TempDir(), t.TempDir()); got != "" {
		t.Fatalf("没有配置文件时应返回空，got %q", got)
	}
}

func TestResolvePathsUsesBaseDir(t *testing.T) {
	base := t.TempDir()
	absSub := filepath.Join(t.TempDir(), "s.yaml")
	cfg := Default()
	cfg.OutputDir = "out"
	cfg.LogFile = "logs/a.log"
	cfg.Subscriptions = []string{"sub.yaml", "https://example.com/s.yaml", absSub}

	ResolvePaths(&cfg, base)

	if want := filepath.Join(base, "out"); cfg.OutputDir != want {
		t.Errorf("OutputDir = %q want %q", cfg.OutputDir, want)
	}
	if want := filepath.Join(base, "logs", "a.log"); cfg.LogFile != want {
		t.Errorf("LogFile = %q want %q", cfg.LogFile, want)
	}
	if want := filepath.Join(base, "sub.yaml"); cfg.Subscriptions[0] != want {
		t.Errorf("本地订阅应按基准目录展开，got %q want %q", cfg.Subscriptions[0], want)
	}
	if cfg.Subscriptions[1] != "https://example.com/s.yaml" {
		t.Errorf("URL 不应被改写，got %q", cfg.Subscriptions[1])
	}
	if cfg.Subscriptions[2] != absSub {
		t.Errorf("绝对路径不应被改写，got %q want %q", cfg.Subscriptions[2], absSub)
	}
}

func TestResolvePathsKeepsAbsolute(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "out")
	cfg := Default()
	cfg.OutputDir = abs
	ResolvePaths(&cfg, t.TempDir())
	if cfg.OutputDir != abs {
		t.Fatalf("绝对路径应保持不变，got %q", cfg.OutputDir)
	}
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("site: https://example.com/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
