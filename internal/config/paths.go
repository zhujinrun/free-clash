package config

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfigName 是自动查找配置时使用的文件名。
const DefaultConfigName = "config.yaml"

// Discover 按优先级查找配置文件：
//
//  1. exeDir/config.yaml —— 安装形态的常规布局，exe 与配置放在同一目录
//  2. cwd/config.yaml    —— 源码目录直接 go run . 的习惯布局
//
// 返回空字符串表示没有找到。服务模式下 Windows 会把工作目录设为 system32，
// 所以必须优先按可执行文件所在目录查找，否则装完服务就找不到配置。
func Discover(exeDir, cwd string) string {
	for _, dir := range []string{exeDir, cwd} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, DefaultConfigName)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// ResolvePaths 把配置中的相对路径按 baseDir 展开为绝对路径。
//
// 基准目录统一取配置文件所在目录，这样无论服务模式（工作目录是 system32）
// 还是前台模式（工作目录是终端当前目录），输出与日志的位置都保持一致。
func ResolvePaths(cfg *Config, baseDir string) {
	if baseDir == "" {
		return
	}
	if cfg.OutputDir != "" {
		cfg.OutputDir = resolveOne(baseDir, cfg.OutputDir)
	}
	if cfg.LogFile != "" {
		cfg.LogFile = resolveOne(baseDir, cfg.LogFile)
	}
	for i, s := range cfg.Subscriptions {
		// 只处理本地文件形态；URL 保持原样。
		if strings.Contains(s, "://") {
			continue
		}
		cfg.Subscriptions[i] = resolveOne(baseDir, s)
	}
}

func resolveOne(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}
