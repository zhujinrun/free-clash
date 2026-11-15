package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"freeclash/internal/config"
	"freeclash/internal/probe"
	"freeclash/internal/proxy"
	"freeclash/internal/server"
	"freeclash/internal/site"
	"freeclash/internal/sub"
)

// version 由构建时注入：go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	var (
		cfgPath     = flag.String("config", "", "配置文件路径（YAML）")
		urls        = flag.String("url", "", "订阅地址，多个用逗号分隔（覆盖配置）")
		siteURL     = flag.String("site", "", "分享站点首页地址，自动从页面解析精选订阅（覆盖配置）")
		siteButton  = flag.String("button", "", "页面上「复制精选订阅」按钮文案，用于定位订阅地址（覆盖配置）")
		resolveOnly = flag.Bool("resolve", false, "只解析并打印订阅地址，不执行探测")
		once        = flag.Bool("once", false, "只探测一次后退出")
		interval    = flag.Duration("interval", 0, "定时探测间隔，如 30m（覆盖配置）")
		topN        = flag.Int("top", 0, "输出的节点数量（覆盖配置）")
		serveAddr   = flag.String("serve", "", "启动本地订阅服务，如 :9898（为空则不起服务）")
		outDir      = flag.String("out", "", "输出目录（覆盖配置）")
		concurrency = flag.Int("concurrency", 0, "真实探测并发数（覆盖配置）")
		verbose     = flag.Bool("v", false, "输出详细日志")
		showVersion = flag.Bool("version", false, "打印版本号")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("freeclash %s\n", version)
		return
	}

	cfg := config.Default()
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取配置文件失败: %v\n", err)
			os.Exit(1)
		}
		cfg = loaded
	}
	if *urls != "" {
		var list []string
		for _, u := range strings.Split(*urls, ",") {
			if u = strings.TrimSpace(u); u != "" {
				list = append(list, u)
			}
		}
		cfg.Subscriptions = list
	}
	if *siteURL != "" {
		cfg.Site = *siteURL
	}
	if *siteButton != "" {
		cfg.SiteButton = *siteButton
	}
	if *interval > 0 {
		cfg.Interval = config.Duration(*interval)
	}
	if *topN > 0 {
		cfg.TopN = *topN
	}
	if *serveAddr != "" {
		cfg.ServeAddr = *serveAddr
	}
	if *outDir != "" {
		cfg.OutputDir = *outDir
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	if len(cfg.Subscriptions) == 0 && cfg.Site == "" {
		fmt.Fprintln(os.Stderr, "缺少订阅来源：请用 -url 指定订阅，或用 -site 指定分享站点首页，或在配置文件中设置 subscriptions / site")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	proxy.SetDefaultInsecure(cfg.Insecure)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolver := site.New(logger).WithAnchor(cfg.SiteButton)

	// 只解析地址，供脚本或人工查看；结果输出到 stdout，日志仍走 stderr。
	if *resolveOnly {
		if cfg.Site == "" {
			fmt.Fprintln(os.Stderr, "-resolve 需要配合 -site 或配置文件中的 site 使用")
			os.Exit(2)
		}
		res, err := resolver.Resolve(ctx, cfg.Site)
		if err != nil {
			logger.Error("解析订阅地址失败", "站点", cfg.Site, "err", err)
			os.Exit(1)
		}
		fmt.Println(res.URL)
		logger.Info("解析完成", "节点数", len(res.Nodes))
		return
	}

	r := &runner{
		cfg:      cfg,
		log:      logger,
		resolver: resolver,
		cache:    site.Cache{Path: filepath.Join(cfg.OutputDir, "resolved.json")},
	}
	if cfg.ServeAddr != "" {
		r.srv = server.New(cfg.ServeAddr, r.refresh)
		go func() {
			logger.Info("订阅服务已启动", "addr", cfg.ServeAddr,
				"订阅地址", "http://"+normalizeHost(cfg.ServeAddr)+"/best.yaml")
			if err := r.srv.Run(ctx); err != nil {
				logger.Error("订阅服务退出", "err", err)
			}
		}()
	}

	if *once || cfg.Interval <= 0 {
		if err := r.refresh(ctx); err != nil {
			logger.Error("探测失败", "err", err)
			os.Exit(1)
		}
		if cfg.ServeAddr != "" && !*once {
			<-ctx.Done()
		}
		return
	}

	ticker := time.NewTicker(cfg.Interval.Std())
	defer ticker.Stop()
	for {
		if err := r.refresh(ctx); err != nil {
			logger.Error("本轮探测失败，将在下一轮重试", "err", err)
		}
		select {
		case <-ctx.Done():
			logger.Info("收到退出信号，正在停止")
			return
		case <-ticker.C:
		}
	}
}

type runner struct {
	cfg      config.Config
	log      *slog.Logger
	srv      *server.Server
	resolver *site.Resolver
	cache    site.Cache

	mu      sync.Mutex
	running bool
}

// refresh 执行一轮完整流程：拉取订阅 → 探测 → 排序 → 输出。
func (r *runner) refresh(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("上一轮探测尚未结束")
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	start := time.Now()
	nodes, err := r.loadNodes(ctx)
	if err != nil {
		return err
	}
	nodes = sub.Filter(nodes, r.cfg.Include, r.cfg.Exclude)
	if len(nodes) == 0 {
		return fmt.Errorf("过滤后没有可探测的节点")
	}
	r.log.Info("订阅就绪", "节点数", len(nodes))

	opts := probe.Options{
		ProbeURL:       r.cfg.ProbeURL,
		PreTimeout:     r.cfg.PreTimeout.Std(),
		ProbeTimeout:   r.cfg.ProbeTimeout.Std(),
		PreConcurrency: r.cfg.PreConcurrency,
		Concurrency:    r.cfg.Concurrency,
		Rounds:         r.cfg.Rounds,
	}
	results := probe.Run(ctx, nodes, opts, r.log)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if r.cfg.RefineRounds > 0 && r.cfg.TopN > 0 {
		probe.Refine(ctx, results, r.cfg.TopN*2, r.cfg.RefineRounds, opts, r.log)
	}

	ranked := probe.Rank(results, true)
	available := len(ranked)

	// 无论本轮是否成功，都先落盘完整报告，便于排查失效原因。
	if err := os.MkdirAll(r.cfg.OutputDir, 0o755); err == nil {
		report := map[string]any{
			"generated_at": time.Now().Format(time.RFC3339),
			"elapsed_ms":   time.Since(start).Milliseconds(),
			"total":        len(nodes),
			"available":    available,
			"probe_url":    r.cfg.ProbeURL,
			"nodes":        results,
		}
		if err := sub.WriteReport(filepath.Join(r.cfg.OutputDir, "report.json"), report); err != nil {
			r.log.Warn("写报告失败", "err", err)
		}
	}

	if available == 0 {
		r.logFailureSummary(results)
		return fmt.Errorf("本轮未发现任何可用节点（共探测 %d 个）", len(nodes))
	}

	top := ranked
	if r.cfg.TopN > 0 && len(top) > r.cfg.TopN {
		top = top[:r.cfg.TopN]
	}

	selected := make([]sub.Proxy, 0, len(top))
	topNames := make([]string, 0, len(top))
	for _, item := range top {
		selected = append(selected, item.Node)
		topNames = append(topNames, item.Name)
	}

	outCfg := sub.BuildConfig(selected, r.cfg.ProbeURL, r.cfg.PerRegionGroups)
	data, err := sub.Marshal(outCfg)
	if err != nil {
		return fmt.Errorf("生成配置失败: %w", err)
	}

	if err := os.MkdirAll(r.cfg.OutputDir, 0o755); err != nil {
		return err
	}
	bestPath := filepath.Join(r.cfg.OutputDir, "best.yaml")
	if err := os.WriteFile(bestPath, data, 0o644); err != nil {
		return err
	}

	if r.srv != nil {
		r.srv.Update(server.Snapshot{
			Config:    data,
			Updated:   time.Now(),
			Total:     len(nodes),
			Available: available,
			TopNodes:  topNames,
		})
	}

	r.log.Info("本轮完成",
		"耗时", time.Since(start).Round(time.Second).String(),
		"探测总数", len(nodes),
		"可用", available,
		"已输出", len(selected),
		"配置", bestPath)

	for i, item := range top {
		if i >= 10 {
			break
		}
		r.log.Info("优选节点",
			"排名", i+1,
			"名称", item.Name,
			"协议", item.Type,
			"平均延迟", fmt.Sprintf("%dms", item.AvgLatency),
			"成功率", fmt.Sprintf("%d/%d", item.Success, item.Success+item.Fail))
	}
	return nil
}

// loadNodes 拉取全部订阅。来源优先级：分享站点解析 → 静态订阅地址；
// 全部失败时回退到上一次成功拉取的缓存。
func (r *runner) loadNodes(ctx context.Context) ([]sub.Proxy, error) {
	cachePath := filepath.Join(r.cfg.OutputDir, "last-sub.yaml")

	var (
		all  []sub.Proxy
		errs []string
	)

	// 站点解析：直接拿页面上的「精选订阅」，无需手工复制链接。
	if r.cfg.Site != "" {
		nodes, err := r.loadFromSite(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.cfg.Site, err))
			r.log.Warn("站点订阅不可用", "站点", r.cfg.Site, "err", err)
		}
		all = append(all, nodes...)
	}

	for _, src := range r.cfg.Subscriptions {
		data, err := sub.Fetch(ctx, src, 60*time.Second)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", src, err))
			r.log.Warn("订阅拉取失败", "源", src, "err", err)
			continue
		}
		nodes, err := sub.Parse(data)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", src, err))
			r.log.Warn("订阅解析失败", "源", src, "err", err)
			continue
		}
		r.log.Info("订阅拉取成功", "源", src, "节点数", len(nodes))
		all = append(all, nodes...)
	}

	if len(all) == 0 {
		// 全部失败时使用缓存，保证工具在订阅站不可用时仍能工作。
		if data, err := os.ReadFile(cachePath); err == nil {
			if nodes, perr := sub.Parse(data); perr == nil && len(nodes) > 0 {
				r.log.Warn("全部订阅不可用，使用本地缓存", "节点数", len(nodes))
				return nodes, nil
			}
		}
		return nil, fmt.Errorf("所有订阅均拉取失败: %s", strings.Join(errs, "; "))
	}

	// 保存缓存，供下次网络异常时兜底。
	if err := os.MkdirAll(r.cfg.OutputDir, 0o755); err == nil {
		if raw, err := sub.RenderProxies(all); err == nil {
			os.WriteFile(cachePath, raw, 0o644)
		}
	}
	return dedupe(all), nil
}

// loadFromSite 从分享站点首页解析出订阅地址并拉取节点。
// 解析失败时回退到上次成功的地址，保证站点改版或临时故障时不中断。
func (r *runner) loadFromSite(ctx context.Context) ([]sub.Proxy, error) {
	res, err := r.resolver.Resolve(ctx, r.cfg.Site)
	if err == nil {
		r.log.Info("已从站点解析订阅地址", "地址", res.URL, "节点数", len(res.Nodes))
		if serr := r.cache.Save(r.cfg.Site, res.URL); serr != nil {
			r.log.Debug("写解析缓存失败", "err", serr)
		}
		return res.Nodes, nil
	}
	r.log.Warn("站点解析失败", "站点", r.cfg.Site, "err", err)

	cached, ok := r.cache.Load()
	if !ok {
		return nil, err
	}
	data, ferr := sub.Fetch(ctx, cached, 60*time.Second)
	if ferr != nil {
		return nil, fmt.Errorf("%w；缓存的订阅地址也不可用: %v", err, ferr)
	}
	nodes, perr := sub.Parse(data)
	if perr != nil || len(nodes) == 0 {
		return nil, fmt.Errorf("%w；缓存的订阅地址解析失败: %v", err, perr)
	}
	r.log.Warn("已回退到上次解析出的订阅地址", "地址", cached, "节点数", len(nodes))
	return nodes, nil
}

func dedupe(in []sub.Proxy) []sub.Proxy {
	seen := make(map[string]struct{}, len(in))
	out := make([]sub.Proxy, 0, len(in))
	for _, p := range in {
		k := p.Key()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p)
	}
	return out
}

func normalizeHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// logFailureSummary 在整轮零可用时汇总失败原因，便于判断是订阅质量问题还是程序缺陷。
func (r *runner) logFailureSummary(results []probe.Result) {
	counts := make(map[string]int)
	for _, res := range results {
		if res.SkipReason != "" {
			counts["跳过: "+clip(res.SkipReason, 60)]++
			continue
		}
		if len(res.Samples) == 0 {
			counts["TCP 预筛未通过"]++
			continue
		}
		for _, s := range res.Samples {
			if s.Error != "" {
				counts[clip(s.Error, 60)]++
			}
		}
	}

	type kv struct {
		k string
		v int
	}
	list := make([]kv, 0, len(counts))
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	for i, item := range list {
		if i >= 5 {
			break
		}
		r.log.Warn("失败原因", "次数", item.v, "原因", item.k)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
