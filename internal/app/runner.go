package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"freeclash/internal/config"
	"freeclash/internal/probe"
	"freeclash/internal/server"
	"freeclash/internal/site"
	"freeclash/internal/sub"
)

// articleSubTimeout 是拉取文章里单条订阅的超时。这类订阅普遍只有几十 KB，
// 用不到 Clash 大配置那样的 90 秒预算；超时短一些才能在几十条并发里及时发现死链。
const articleSubTimeout = 30 * time.Second

// runner 持有一轮轮探测所需的全部依赖，并在多次刷新之间共享状态。
type runner struct {
	cfg      config.Config
	log      *slog.Logger
	srv      *server.Server
	resolver *site.Resolver
	cache    site.Cache

	mu      sync.Mutex
	running bool
}

func newRunner(o Options, logger *slog.Logger) *runner {
	return &runner{
		cfg:      o.Cfg,
		log:      logger,
		resolver: site.New(logger).WithAnchor(o.Cfg.SiteButton),
		cache:    site.Cache{Path: filepath.Join(o.Cfg.OutputDir, "resolved.json")},
	}
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

	top := probe.Select(ranked, r.cfg.TopN, r.cfg.SourceCapRatio)
	if len(top) < len(ranked) {
		r.log.Info("按来源配额挑选",
			"可用", len(ranked),
			"输出", len(top),
			"来源分布", sourceSummary(top))
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

	// 同一批节点再输出一份 v2ray 系客户端能读的订阅（base64 分享链接）。
	// Clash 的 YAML 只有 Clash 系客户端认，v2rayN / v2rayA 都不支持。
	subData := sub.RenderSubscription(selected)
	linksData := sub.PlainLinks(selected)
	subPath := filepath.Join(r.cfg.OutputDir, "best.txt")
	if err := os.WriteFile(subPath, subData, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.cfg.OutputDir, "best.links"), linksData, 0o644); err != nil {
		return err
	}

	if r.srv != nil {
		r.srv.Update(server.Snapshot{
			Config:       data,
			Subscription: subData,
			Links:        linksData,
			Updated:      time.Now(),
			Total:        len(nodes),
			Available:    available,
			TopNodes:     topNames,
		})
	}

	r.log.Info("本轮完成",
		"耗时", time.Since(start).Round(time.Second).String(),
		"探测总数", len(nodes),
		"可用", available,
		"已输出", len(selected),
		"配置", bestPath,
		"v2ray 订阅", subPath)

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

	// 文章路线：首页「查看节点文章」→ 每日汇总页 → 几十条通用订阅。
	// 这条线才是节点数量的大头，精选订阅只是其中一份。
	if r.cfg.Site != "" && r.cfg.ArticleLimit > 0 {
		nodes, err := r.loadFromArticles(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("文章订阅: %v", err))
			r.log.Warn("文章订阅不可用", "err", err)
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
		all = append(all, sub.TagSource(nodes, sub.SourceOf(src))...)
	}

	if len(all) == 0 {
		// 全部失败时使用缓存，保证工具在订阅站不可用时仍能工作。
		if data, err := os.ReadFile(cachePath); err == nil {
			if nodes, perr := sub.Parse(data); perr == nil && len(nodes) > 0 {
				r.log.Warn("全部订阅不可用，使用本地缓存", "节点数", len(nodes))
				return sub.TagSource(nodes, "cache"), nil
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
	out := dedupe(all)
	if r.cfg.MaxNodes > 0 && len(out) > r.cfg.MaxNodes {
		r.log.Info("节点数超过上限，按来源顺序截断", "上限", r.cfg.MaxNodes, "实际", len(out))
		out = out[:r.cfg.MaxNodes]
	}
	return out, nil
}

// loadFromArticles 跟进「查看节点文章」，把文章里列出的订阅逐个拉一遍。
func (r *runner) loadFromArticles(ctx context.Context) ([]sub.Proxy, error) {
	src, err := r.resolver.ArticleSubscriptions(ctx, r.cfg.Site, r.cfg.ArticleLimit, r.cfg.ArticleMaxSubs)
	if err != nil {
		return nil, err
	}
	if len(src.Subs) == 0 {
		return nil, fmt.Errorf("最近 %d 篇文章中没有解析到订阅地址", len(src.Articles))
	}
	r.log.Info("已从节点文章收集订阅",
		"文章数", len(src.Articles),
		"订阅数", len(src.Subs),
		"首篇", src.Articles[0])

	concurrency := r.cfg.ArticleConcurrency
	if concurrency <= 0 {
		concurrency = 8
	}

	type fetched struct {
		URL   string
		Nodes []sub.Proxy
		Err   error
	}
	results := make([]fetched, len(src.Subs))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, u := range src.Subs {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, ferr := sub.Fetch(ctx, u, articleSubTimeout)
			if ferr != nil {
				results[i] = fetched{URL: u, Err: ferr}
				return
			}
			nodes, perr := sub.ParseAny(data)
			if perr != nil || len(nodes) == 0 {
				if perr == nil {
					perr = fmt.Errorf("解析出 0 个节点")
				}
				results[i] = fetched{URL: u, Err: perr}
				return
			}
			results[i] = fetched{URL: u, Nodes: nodes}
		}(i, u)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var (
		all      []sub.Proxy
		okCount  int
		failURLs []string
	)
	for _, res := range results {
		if res.Err != nil {
			failURLs = append(failURLs, res.URL)
			r.log.Debug("文章订阅不可用", "url", res.URL, "err", res.Err)
			continue
		}
		okCount++
		all = append(all, sub.TagSource(res.Nodes, sub.SourceOf(res.URL))...)
	}
	r.log.Info("文章订阅拉取完成",
		"成功", okCount,
		"失败", len(failURLs),
		"节点数", len(all))

	if len(all) == 0 {
		return nil, fmt.Errorf("%d 条文章订阅全部拉取失败", len(src.Subs))
	}
	return all, nil
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
		return sub.TagSource(res.Nodes, sub.SourceOf(res.URL)), nil
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
	return sub.TagSource(nodes, sub.SourceOf(cached)), nil
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

// sourceSummary 汇总本轮输出的来源构成，便于确认没有单一来源占满名额。
func sourceSummary(top []*probe.Result) string {
	counts := make(map[string]int, len(top))
	for _, item := range top {
		k := item.Source
		if k == "" {
			k = "unknown"
		}
		counts[k]++
	}
	list := make([]sourceCount, 0, len(counts))
	for k, v := range counts {
		list = append(list, sourceCount{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	var sb strings.Builder
	for i, item := range list {
		if i >= 6 {
			sb.WriteString(" …")
			break
		}
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "%s=%d", item.k, item.v)
	}
	return sb.String()
}

type sourceCount struct {
	k string
	v int
}
