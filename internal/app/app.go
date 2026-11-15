// Package app 负责把运行参数组装成一次完整运行：配置日志、执行探测、对外提供订阅。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freeclash/internal/config"
	"freeclash/internal/logx"
	"freeclash/internal/proxy"
	"freeclash/internal/server"
	"freeclash/internal/site"
)

// Options 是一次运行的完整输入。Cfg 已完成默认值合并与工作目录解析。
type Options struct {
	Cfg config.Config

	// ConfigPath 是实际使用的配置文件路径，仅用于日志展示。
	ConfigPath string
	// BaseDir 是相对路径的解析基准，通常就是配置文件所在目录。
	BaseDir string

	Once        bool // 只探测一轮后退出
	ResolveOnly bool // 只解析订阅地址，不探测
	Verbose     bool // 输出调试日志
	Service     bool // 由系统服务管理器拉起，无终端可用
}

// Validate 检查参数是否足以完成一轮探测。
func Validate(o Options) error {
	if o.ResolveOnly {
		if o.Cfg.Site == "" {
			return errors.New("-resolve 需要配合 -site 或配置文件中的 site 使用")
		}
		return nil
	}
	if o.Cfg.Site == "" && len(o.Cfg.Subscriptions) == 0 {
		return errors.New("缺少订阅来源：请用 -url 指定订阅，或用 -site 指定分享站点首页，或在配置文件中设置 subscriptions / site")
	}
	// 服务模式下进程必须常驻，否则装完就立刻退出。
	if o.Service && o.Once {
		return errors.New("-once 只适用于前台单次运行，安装为系统服务后必须常驻；请改用 interval 或 serve_addr")
	}
	if o.Service && o.Cfg.Interval <= 0 && o.Cfg.ServeAddr == "" {
		return errors.New("服务模式需要 interval > 0（定时探测）或 serve_addr（常驻订阅服务），否则进程会立即退出")
	}
	return nil
}

// Run 执行一次完整运行，阻塞直到 ctx 结束或任务完成。
func Run(ctx context.Context, o Options) error {
	logger, closeLog, err := setupLogger(o)
	if err != nil {
		return err
	}
	defer closeLog()
	defer logger.Info("进程已退出")

	proxy.SetDefaultInsecure(o.Cfg.Insecure)

	if o.ResolveOnly {
		return resolveOnly(ctx, o, logger)
	}

	r := newRunner(o, logger)
	r.srv = startHTTPServer(ctx, o, r, logger)

	if o.Once {
		return r.refresh(ctx)
	}

	return runLoop(ctx, o, r, logger)
}

// runLoop 是定时探测主循环。
func runLoop(ctx context.Context, o Options, r *runner, logger *slog.Logger) error {
	cfg := o.Cfg

	if cfg.Interval <= 0 {
		// 不定时：跑一轮，然后（若开了订阅服务）常驻。
		if err := r.refresh(ctx); err != nil {
			if cfg.ServeAddr == "" {
				return err
			}
			logger.Error("首轮探测失败，订阅服务将返回 503 直至下次刷新成功", "err", err)
		}
		if cfg.ServeAddr == "" {
			return nil
		}
		<-ctx.Done()
		logger.Info("收到退出信号，正在停止")
		return nil
	}

	// 服务可能比网络先就绪，首轮失败时用短间隔补几次，避免干等一个完整周期。
	for attempt := 1; ; attempt++ {
		err := r.refresh(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= 3 {
			logger.Error("首轮探测连续失败，按正常周期重试", "err", err)
			break
		}
		wait := time.Duration(attempt) * 15 * time.Second
		logger.Warn("首轮探测失败，稍后重试", "第几次", attempt, "等待", wait.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	ticker := time.NewTicker(cfg.Interval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("收到退出信号，正在停止")
			return nil
		case <-ticker.C:
			if err := r.refresh(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logger.Error("本轮探测失败，将在下一轮重试", "err", err)
			}
		}
	}
}

// startHTTPServer 在配置了 serve_addr 时启动本地订阅服务。
func startHTTPServer(ctx context.Context, o Options, r *runner, logger *slog.Logger) *server.Server {
	if o.Cfg.ServeAddr == "" {
		return nil
	}
	srv := server.New(o.Cfg.ServeAddr, r.refresh)
	// 重启后到首轮跑完之间有几到十几分钟空窗，先用上次的产出顶上，别让客户端
	// 回源拿到 503。首轮完成时会被正常快照覆盖。
	if snap, ok := loadPreviousSnapshot(o.Cfg.OutputDir); ok {
		srv.Update(snap)
		logger.Info("已载入上次的产出，首轮完成前继续对外提供",
			"节点数", snap.Available, "上次更新", snap.Updated.Format("01-02 15:04"))
	}
	host := normalizeHost(o.Cfg.ServeAddr)
	go func() {
		logger.Info("订阅服务已启动",
			"addr", o.Cfg.ServeAddr,
			"Clash 订阅", "http://"+host+"/best.yaml",
			"v2ray 订阅", "http://"+host+"/best.txt",
			"状态", "http://"+host+"/status")
		if err := srv.Run(ctx); err != nil {
			logger.Error("订阅服务退出", "err", err)
		}
	}()
	return srv
}

func resolveOnly(ctx context.Context, o Options, logger *slog.Logger) error {
	res, err := site.New(logger).WithAnchor(o.Cfg.SiteButton).Resolve(ctx, o.Cfg.Site)
	if err != nil {
		logger.Error("解析订阅地址失败", "站点", o.Cfg.Site, "err", err)
		return err
	}
	// 结果走 stdout，日志仍走 stderr，便于脚本直接取用。
	fmt.Println(res.URL)
	logger.Info("解析完成", "节点数", len(res.Nodes))
	return nil
}

// setupLogger 决定日志去向：服务模式必须写文件（没有终端），
// 前台模式默认写终端，配置了 log_file 时同时写文件。
func setupLogger(o Options) (*slog.Logger, func(), error) {
	level := slog.LevelInfo
	if o.Verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	path := o.Cfg.LogFile
	toFile := path != "" || o.Service
	if !toFile {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), func() {}, nil
	}
	if path == "" {
		path = filepath.Join(o.BaseDir, "logs", "freeclash.log")
	}

	w, err := logx.NewRotatingWriter(path, 10<<20, 5)
	if err != nil {
		if !o.Service {
			// 前台模式下日志文件打不开不该阻断运行。
			fmt.Fprintf(os.Stderr, "日志文件不可用（%v），改为输出到终端\n", err)
			return slog.New(slog.NewTextHandler(os.Stderr, opts)), func() {}, nil
		}
		return nil, nil, fmt.Errorf("打开日志文件失败: %w", err)
	}

	var out io.Writer = w
	if !o.Service {
		out = io.MultiWriter(os.Stderr, w)
	}
	logger := slog.New(slog.NewTextHandler(out, opts))
	logger.Info("日志已启用", "文件", path)
	return logger, func() { w.Close() }, nil
}

func normalizeHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}
