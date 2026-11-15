// Package site 从分享站点页面中解析出真实订阅地址。
//
// 站点的订阅地址不写在 HTML 里，而是编译进带内容哈希的 JS chunk
// （如 /assets/chunks/theme.BdP30UdW.js），站点重新构建后文件名就会变，
// 所以不能硬编码路径。这里采用「动态发现 + 打分排序 + 实测验证」：
//
//  1. 抓页面，若本身就是 Clash 订阅则直接返回；
//  2. 找出页面引用的全部 JS，与页面正文一起作为文本集合；
//  3. 从文本中提取候选 URL，按「订阅特征」打分排序；
//  4. 逐个真实拉取，用 Clash 解析器验证，第一个能解析出节点的即目标。
//
// 第 4 步是关键：页面里通常混有推广、返佣、文档等大量链接，只有真实
// 拉取才能确定哪个是订阅地址。
package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"freeclash/internal/sub"
)

const (
	defaultUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	maxPageBytes     = 8 << 20
	maxAssetBytes    = 8 << 20
	maxSubBytes      = 32 << 20
	defaultMaxAssets = 16
	defaultMaxCands  = 4
)

// 单次请求的时间预算。订阅体积可达数 MB（本场景约 1.4MB），
// 与首页、脚本不能用同一个上限，否则慢网下必然读超时。
const (
	pageTimeout  = 30 * time.Second
	assetTimeout = 30 * time.Second
	subTimeout   = 90 * time.Second
)

// 打分权重。订阅特征的得分远高于普通链接，避免被页面里的推广链接挤掉。
const (
	scoreAnchorModule    = 200 // 出现在「复制精选订阅」按钮所在模块内的订阅赋值
	scoreSubscriptionKey = 120 // 出现 subscription: "..." 这类赋值
	scoreSubPath         = 60  // 路径含 /sub/ 或 /subscribe
	scoreSubExt          = 40  // 路径以 .yml/.yaml 结尾或含 clash
	scoreCrossHost       = 20  // 与页面不同域（订阅通常托管在别处）
	scoreTokenQuery      = 15  // 带 token/key 查询参数

	penaltyPromo = 60 // 推广/返佣链接
	penaltyHTTP  = 25 // 非 HTTPS
	penaltyCDN   = 30 // 公共 CDN，通常只放静态资源
)

// defaultAnchors 是「复制精选订阅」按钮的常见文案。
//
// 按钮本身不含地址（HTML 里就是 <button>复制精选订阅</button>，没有
// data-clipboard-text 之类属性），点击时才由 JS 从数据对象取值。但按钮文案
// 与地址同处一个 JS 模块，用文案锚定模块，再只在模块内找订阅赋值，
// 比全局扫描所有链接精确得多，也不受页面新增推广链接的干扰。
var defaultAnchors = []string{
	"复制精选订阅", "复制订阅", "复制订阅链接", "精选订阅",
	"copy subscription", "copy-subscription",
}

// Result 是一次成功解析的结果。Body 与 Nodes 一并返回，调用方可直接复用，
// 避免为了验证候选地址而把订阅下载两遍。
type Result struct {
	URL   string
	Body  []byte
	Nodes []sub.Proxy
	// Direct 表示传入地址本身就是订阅，未经页面解析。
	Direct bool
}

// Resolver 负责从页面解析订阅地址。
type Resolver struct {
	Client    *http.Client
	UserAgent string
	// MaxAssets 限制跟踪的脚本数量，默认 16。
	MaxAssets int
	// MaxCands 限制实测验证的候选数量，默认 4。
	MaxCands int
	// Anchors 是用于定位「复制精选订阅」按钮模块的文案，为空时用默认集合。
	Anchors []string
	Log     *slog.Logger
}

// New 构造一个使用默认参数的解析器。HTTP 客户端默认遵循代理环境变量。
func New(log *slog.Logger) *Resolver {
	return &Resolver{
		// 不设全局超时，改为每次请求按用途设置预算（见 pageTimeout 等），
		// 否则小页面与大订阅会被同一个上限卡住。
		Client:    &http.Client{},
		UserAgent: defaultUA,
		MaxAssets: defaultMaxAssets,
		MaxCands:  defaultMaxCands,
		Anchors:   defaultAnchors,
		Log:       log,
	}
}

// WithAnchor 追加一个按钮文案（置于优先位置），默认文案仍作为兜底。
// 站点改文案时可通过它适配。
func (r *Resolver) WithAnchor(text string) *Resolver {
	text = strings.TrimSpace(text)
	if text == "" {
		return r
	}
	out := make([]string, 0, len(defaultAnchors)+1)
	out = append(out, text)
	for _, a := range defaultAnchors {
		if a != text {
			out = append(out, a)
		}
	}
	r.Anchors = out
	return r
}

func (r *Resolver) anchorList() []string {
	if len(r.Anchors) == 0 {
		return defaultAnchors
	}
	return r.Anchors
}

// Resolve 解析页面，返回真实订阅地址及其内容。
func (r *Resolver) Resolve(ctx context.Context, pageURL string) (*Result, error) {
	body, ctype, finalURL, err := r.get(ctx, pageURL, maxPageBytes, true, pageTimeout)
	if err != nil {
		return nil, fmt.Errorf("抓取页面 %s 失败: %w", pageURL, err)
	}

	// 传入的地址本身就是订阅，无需解析。
	if nodes, perr := sub.Parse(body); perr == nil && len(nodes) > 0 {
		return &Result{URL: finalURL, Body: body, Nodes: nodes, Direct: true}, nil
	}

	if !isHTML(body, ctype) {
		return nil, fmt.Errorf("%s 既不是 Clash 订阅，也不是 HTML 页面（Content-Type: %s）", pageURL, ctype)
	}

	texts := []string{string(body)}
	assets := r.assetURLs(body, finalURL)
	texts = append(texts, r.fetchAssets(ctx, assets)...)

	anchors := r.anchorList()
	r.debug("页面文本集合就绪",
		"页面", finalURL, "脚本数", len(assets),
		"按钮模块数", len(filterByAnchors(texts, anchors)))

	cands := rankCandidates(texts, finalURL, anchors)
	if len(cands) == 0 {
		return nil, fmt.Errorf("在 %s 及其 %d 个脚本中未找到任何候选订阅地址", pageURL, len(assets))
	}

	// 已有明确的正分候选时，0 分及以下的链接（推广、示例域名、资源占位）
	// 无需再验证，避免浪费请求并淹没错误信息。
	pool := cands
	if cands[0].score > 0 {
		filtered := make([]candidate, 0, len(cands))
		for _, c := range cands {
			if c.score > 0 {
				filtered = append(filtered, c)
			}
		}
		pool = filtered
	}

	var (
		tryErrs []string
		tried   int
	)
	for _, c := range pool {
		if tried >= r.MaxCands {
			break
		}
		tried++
		data, _, _, gerr := r.get(ctx, c.url, maxSubBytes, false, subTimeout)
		if gerr != nil {
			tryErrs = append(tryErrs, fmt.Sprintf("%s: %v", c.url, gerr))
			continue
		}
		nodes, perr := sub.Parse(data)
		if perr != nil || len(nodes) == 0 {
			reason := "不是有效的 Clash 订阅"
			if perr != nil {
				reason = perr.Error()
			}
			tryErrs = append(tryErrs, fmt.Sprintf("%s: %s", c.url, reason))
			r.debug("候选未通过校验", "url", c.url, "分数", c.score, "原因", reason)
			continue
		}
		r.debug("候选通过校验", "url", c.url, "分数", c.score, "依据", strings.Join(c.why, "+"), "节点数", len(nodes))
		return &Result{URL: c.url, Body: data, Nodes: nodes}, nil
	}

	return nil, fmt.Errorf("候选地址均未通过订阅校验: %s", strings.Join(tryErrs, "; "))
}

// fetchAssets 并行拉取脚本内容。单个脚本失败或超时不影响整体，
// 因为订阅地址通常只出现在其中一个 chunk 里。
func (r *Resolver) fetchAssets(ctx context.Context, urls []string) []string {
	const maxParallel = 6
	sem := make(chan struct{}, maxParallel)
	out := make([]string, len(urls))

	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, _, _, err := r.get(ctx, u, maxAssetBytes, true, assetTimeout)
			if err != nil {
				r.debug("跳过无法获取的脚本", "url", u, "err", err)
				return
			}
			out[i] = string(data)
		}(i, u)
	}
	wg.Wait()

	texts := make([]string, 0, len(urls))
	for _, s := range out {
		if s != "" {
			texts = append(texts, s)
		}
	}
	return texts
}

// statusError 表示服务端返回了非 200 状态码。
// 5xx 多为网关瞬时故障，值得重试；4xx 是确定性结果，重试无意义。
type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("http %d", e.code) }

func (e statusError) retryable() bool { return e.code >= 500 }

// get 拉取内容，对网络类错误与 5xx 自动重试。每次尝试各自计时，
// 避免多次重试共享同一个超时预算。
// allowPartial 为真时，即使读取中断也返回已收到的部分——
// 扫描脚本只关心文件中的目标字符串，截断内容通常仍然可用。
func (r *Resolver) get(ctx context.Context, rawURL string, limit int64, allowPartial bool, timeout time.Duration) ([]byte, string, string, error) {
	const attempts = 3

	var lastErr error
	for i := 0; i < attempts; i++ {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		data, ctype, final, err := r.getOnce(attemptCtx, rawURL, limit, allowPartial)
		cancel()

		if err == nil {
			return data, ctype, final, nil
		}
		lastErr = err

		var se statusError
		if errors.As(err, &se) && !se.retryable() {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if i+1 < attempts {
			r.debug("请求失败，准备重试", "url", rawURL, "次数", i+1, "err", err)
			select {
			case <-ctx.Done():
				return nil, "", "", ctx.Err()
			case <-time.After(time.Duration(700*(i+1)) * time.Millisecond):
			}
		}
	}
	return nil, "", "", lastErr
}

func (r *Resolver) getOnce(ctx context.Context, rawURL string, limit int64, allowPartial bool) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", r.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", statusError{code: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		if allowPartial && len(data) >= 256 {
			return data, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
		}
		return nil, "", "", err
	}
	return data, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
}

func (r *Resolver) debug(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Debug(msg, args...)
	}
}

// jsRefRe 匹配页面中被引用的脚本路径，覆盖 src/href 与 ESM 的 import/from。
var jsRefRe = regexp.MustCompile(`["'(]([^"'()\s]+\.js)["')]`)

// assetURLs 从页面中提取脚本绝对地址，用于后续文本扫描。
func (r *Resolver) assetURLs(body []byte, base string) []string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil
	}
	limit := r.MaxAssets
	if limit <= 0 {
		limit = defaultMaxAssets
	}

	seen := make(map[string]struct{})
	out := make([]string, 0, limit)
	for _, m := range jsRefRe.FindAllStringSubmatch(string(body), -1) {
		ref := strings.TrimSpace(m[1])
		if ref == "" || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "javascript:") {
			continue
		}
		refURL, perr := url.Parse(ref)
		if perr != nil {
			continue
		}
		abs := baseURL.ResolveReference(refURL).String()
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// urlRe 提取文本中的 http(s) 地址。排除反引号与括号，避免截到 JS 模板字面量。
var urlRe = regexp.MustCompile("https?://[^\\s\"'<>`)\\]}]+")

// assetExtRe 命中静态资源后缀。后缀可能出现在路径中段
// （如 /beacon.min.js/abc123），因此不要求结尾匹配。
var assetExtRe = regexp.MustCompile(`\.(js|mjs|css|png|jpe?g|svg|ico|woff2?|ttf|eot|webp|gif|map|mp4|mp3|webm|pdf|zip|gz)(?:/|$)`)

// subKeyLitRe 匹配「订阅地址」直接写字面量的形式，如 subscription:"https://..."。
var subKeyLitRe = regexp.MustCompile(`(?i)(?:subscription|subscribe_url|sub_url|suburl|clash_url)["']?\s*[:=]\s*["'](https?://[^"']+)["']`)

// subKeyRefRe 匹配 subscription:Bo 这类引用变量的形式。
// 打包器常把长字符串提取成变量，站点实测就是 Bo="https://..." 加 subscription:Bo。
var subKeyRefRe = regexp.MustCompile(`(?i)(?:subscription|subscribe_url|sub_url|suburl|clash_url)["']?\s*[:=]\s*([A-Za-z_$][\w$]*)`)

// varAssignRe 收集变量到字符串的赋值，用于还原上面的间接引用。
var varAssignRe = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*=\s*["'](https?://[^"']+)["']`)

// subscriptionURLs 收集被明确标注为订阅的地址，这类地址权重最高。
// 同时支持字面量与变量引用两种写法。
func subscriptionURLs(texts []string) map[string]bool {
	vars := make(map[string]string)
	for _, text := range texts {
		norm := strings.ReplaceAll(text, `\/`, "/")
		for _, m := range varAssignRe.FindAllStringSubmatch(norm, -1) {
			vars[m[1]] = m[2]
		}
	}

	out := make(map[string]bool)
	for _, text := range texts {
		norm := strings.ReplaceAll(text, `\/`, "/")
		for _, m := range subKeyLitRe.FindAllStringSubmatch(norm, -1) {
			if v := strings.TrimSpace(m[1]); v != "" {
				out[v] = true
			}
		}
		for _, m := range subKeyRefRe.FindAllStringSubmatch(norm, -1) {
			if u, ok := vars[m[1]]; ok {
				out[u] = true
			}
		}
	}
	return out
}

// promoRe 命中推广、返佣、注册类链接。
var promoRe = regexp.MustCompile(`(?i)(register|/aff|aff=|invite|signup|referral|/r/)`)

// tokenRe 命中订阅常见的令牌参数。
var tokenRe = regexp.MustCompile(`(?i)(token|key|sub|uuid)=`)

// noiseHosts 是与订阅无关的第三方域名，直接跳过。
var noiseHosts = []string{
	"googletagmanager.com", "google-analytics.com", "doubleclick.net",
	"facebook.net", "twitter.com", "w3.org", "schema.org",
	"iconify.design", "fonts.googleapis.com", "fonts.gstatic.com",
	"cloudflareinsights.com", "clarity.ms", "hotjar.com", "sentry.io",
}

// cdnHosts 通常只托管静态资源，给一定惩罚而非直接排除。
var cdnHosts = []string{
	"jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com", "bootcdn.net",
}

type candidate struct {
	url   string
	score int
	why   []string
}

// rankCandidates 提取候选订阅地址并按可信度降序返回。
// anchors 非空时，出现在按钮模块内的订阅赋值会获得最高权重。
func rankCandidates(texts []string, pageURL string, anchors []string) []candidate {
	page, _ := url.Parse(pageURL)
	claimed := subscriptionURLs(texts)
	claimedInButton := subscriptionURLs(filterByAnchors(texts, anchors))

	agg := make(map[string]*candidate)
	order := make([]string, 0, 16)

	for _, text := range texts {
		// VitePress 等打包工具会把 / 转义成 \/，先还原再匹配。
		norm := strings.ReplaceAll(text, `\/`, "/")
		for _, raw := range urlRe.FindAllString(norm, -1) {
			u := strings.TrimRight(raw, ".,;:")
			if strings.Contains(u, "${") || strings.Contains(u, "{{") {
				continue // JS 模板字面量或占位符，不是真实地址
			}
			parsed, err := url.Parse(u)
			if err != nil || parsed.Host == "" {
				continue
			}
			if isNoiseHost(parsed.Host) {
				continue
			}
			if assetExtRe.MatchString(strings.ToLower(parsed.Path)) {
				continue
			}
			if samePage(parsed, page) {
				continue // 页面自身
			}

			c, ok := agg[u]
			if !ok {
				c = &candidate{url: u}
				agg[u] = c
				order = append(order, u)
			}
			pageHost := ""
			if page != nil {
				pageHost = page.Host
			}
			scoreURL(c, parsed, claimed[u], claimedInButton[u], pageHost)
		}
	}

	out := make([]candidate, 0, len(order))
	for _, u := range order {
		out = append(out, *agg[u])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].url < out[j].url
	})
	return out
}

func scoreURL(c *candidate, u *url.URL, claimed, claimedInButton bool, pageHost string) {
	add := func(score int, reason string) {
		c.score += score
		c.why = append(c.why, reason)
	}
	lowPath := strings.ToLower(u.Path)
	lowQuery := u.Query().Encode()

	switch {
	case claimedInButton:
		add(scoreAnchorModule, "按钮模块内订阅赋值")
	case claimed:
		add(scoreSubscriptionKey, "subscription赋值")
	case strings.Contains(lowPath, "/sub/") || strings.Contains(lowPath, "/subscribe") || strings.Contains(lowPath, "sub?"):
		add(scoreSubPath, "订阅路径")
	}
	if strings.HasSuffix(lowPath, ".yml") || strings.HasSuffix(lowPath, ".yaml") || strings.Contains(lowPath, "clash") {
		add(scoreSubExt, "订阅后缀")
	}
	if u.Scheme == "http" {
		add(-penaltyHTTP, "明文HTTP")
	}
	if promoRe.MatchString(u.Path) || promoRe.MatchString(u.Fragment) || promoRe.MatchString(lowQuery) {
		add(-penaltyPromo, "推广链接")
	}
	// 返佣/推广站常把 aff 写进域名（如 link.xxx.com、vipaff.cc）。
	// 订阅域名极少含该片段，因此按域名判断误伤概率低。
	if strings.Contains(strings.ToLower(u.Host), "aff") {
		add(-penaltyPromo, "推广域名")
	}
	if tokenRe.MatchString(lowQuery) {
		add(scoreTokenQuery, "带令牌参数")
	}
	// 订阅一般托管在独立域名上，与页面同域的链接多为站内文档。
	if pageHost != "" && !strings.EqualFold(u.Host, pageHost) {
		add(scoreCrossHost, "跨域")
	}
	if isCDNHost(u.Host) {
		add(-penaltyCDN, "公共CDN")
	}
}

// filterByAnchors 挑出包含按钮文案的文本，即「复制精选订阅」按钮所在的模块。
func filterByAnchors(texts []string, anchors []string) []string {
	if len(anchors) == 0 {
		return nil
	}
	out := make([]string, 0, 2)
	for _, t := range texts {
		for _, a := range anchors {
			if a != "" && strings.Contains(t, a) {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

func isHTML(body []byte, ctype string) bool {
	if strings.Contains(strings.ToLower(ctype), "html") {
		return true
	}
	head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

func samePage(a, b *url.URL) bool {
	if b == nil || a == nil {
		return false
	}
	return strings.EqualFold(a.Host, b.Host) &&
		strings.TrimSuffix(a.Path, "/") == strings.TrimSuffix(b.Path, "/")
}

func isNoiseHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return matchHost(h, noiseHosts)
}

func isCDNHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return matchHost(h, cdnHosts)
}

func matchHost(host string, list []string) bool {
	for _, d := range list {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ---- 解析结果缓存 ----

// Cache 记录上次成功解析出的订阅地址，供站点改版或临时不可用时兜底。
type Cache struct {
	Path string
}

type cacheEntry struct {
	URL        string `json:"url"`
	Page       string `json:"page"`
	ResolvedAt string `json:"resolved_at"`
}

// Load 读取上次缓存的订阅地址。
func (c Cache) Load() (string, bool) {
	if c.Path == "" {
		return "", false
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return "", false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.URL == "" {
		return "", false
	}
	return entry.URL, true
}

// Save 写入本次解析结果。
func (c Cache) Save(page, subURL string) error {
	if c.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(cacheEntry{
		URL:        subURL,
		Page:       page,
		ResolvedAt: time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.Path, data, 0o644)
}
