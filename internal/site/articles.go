package site

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// 站点首页的「查看节点文章」会把读者带到一个每日汇总页，页面里挂着几十条
// 「通用订阅」。这些订阅才是节点数量的大头（单篇可达数万节点），而首页那个
// 「精选订阅」只是其中一份。
//
// 这里的做法是：首页找文章入口 → 跟进最近若干篇 → 从每篇里抽出订阅地址。
// 抽取出自固定的卡片结构（<div class="copy-box"><code>URL</code>），因此不会
// 把页面里的推广、教程、返佣链接带进来。

const (
	// defaultArticleLimit 是默认跟进的最近文章篇数。
	defaultArticleLimit = 2
	// maxArticleFetch 是文章页的抓取并发。
	articleFetchParallel = 3
	// maxArticleBytes 限制单篇文章体积。
	maxArticleBytes = 4 << 20
)

// ArticleSources 是文章路线的抓取结果。
type ArticleSources struct {
	// Articles 是实际打开的文章页地址。
	Articles []string
	// Subs 是抽到的订阅地址（按出现顺序去重）。
	Subs []string
}

// anchorRe 匹配 <a href="...">文本</a>，用于识别「查看节点文章」这类入口。
var anchorRe = regexp.MustCompile(`(?is)<a\b[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)

// tagRe 去掉标签，只留文本。
var tagRe = regexp.MustCompile(`(?s)<[^>]+>`)

// codeURLRe 匹配卡片里 <code> 包裹的地址，这是站点当前的订阅展示方式。
var codeURLRe = regexp.MustCompile(`(?is)<code[^>]*>\s*(https?://[^<\s]+)\s*</code>`)

// postsHrefRe 匹配站内文章路径。
var postsHrefRe = regexp.MustCompile(`(?i)href="([^"]*/(?:posts|post|article|articles)/[^"]*)"`)

// articleTextHints 是首页上「查看节点文章」一类入口的文案特征。
var articleTextHints = []string{"查看节点文章", "节点文章", "查看节点", "每日节点", "免费节点"}

// dateRe 用于按日期倒序排列文章。
var dateRe = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)

// subLabelRe 是卡片上的标签，抽取失败时用它做兜底定位。
var subLabelRe = regexp.MustCompile(`(?is)(?:通用订阅|clash\s*配置|订阅地址|clash\s*订阅)</span>\s*(?:<[^>]+>\s*)*<code[^>]*>\s*(https?://[^<\s]+)`)

// ArticleSubscriptions 抓取首页与最近 articleLimit 篇文章，收集其中的订阅地址。
// maxSubs 限制总数（0 表示不限）。
func (r *Resolver) ArticleSubscriptions(ctx context.Context, homepage string, articleLimit, maxSubs int) (*ArticleSources, error) {
	if articleLimit <= 0 {
		return &ArticleSources{}, nil
	}

	body, ctype, finalURL, err := r.get(ctx, homepage, maxPageBytes, true, pageTimeout)
	if err != nil {
		return nil, fmt.Errorf("抓取首页 %s 失败: %w", homepage, err)
	}
	if !isHTML(body, ctype) {
		return nil, fmt.Errorf("首页 %s 不是 HTML 页面（Content-Type: %s）", homepage, ctype)
	}

	base, perr := url.Parse(finalURL)
	if perr != nil {
		return nil, fmt.Errorf("首页地址无效: %w", perr)
	}

	articles := r.articleLinks(ctx, body, base)
	if len(articles) > articleLimit {
		articles = articles[:articleLimit]
	}
	if len(articles) == 0 {
		return nil, fmt.Errorf("首页与 /nodes 中都没有找到文章入口")
	}

	subs := r.collectArticleSubs(ctx, articles, base, maxSubs)
	return &ArticleSources{Articles: articles, Subs: subs}, nil
}

// articleLinks 从首页（必要时加上 /nodes 列表页）收集文章地址，按日期倒序。
func (r *Resolver) articleLinks(ctx context.Context, homeBody []byte, base *url.URL) []string {
	text := html.UnescapeString(string(homeBody))

	// 「查看节点文章」这类入口优先，其余 /posts/ 链接作为补充。
	var priority []string
	for _, m := range anchorRe.FindAllStringSubmatch(text, -1) {
		label := strings.TrimSpace(tagRe.ReplaceAllString(m[2], ""))
		if !containsAny(label, articleTextHints) {
			continue
		}
		if abs := resolveRef(base, m[1]); abs != "" {
			priority = append(priority, abs)
		}
	}
	for _, m := range postsHrefRe.FindAllStringSubmatch(text, -1) {
		if abs := resolveRef(base, m[1]); abs != "" {
			priority = append(priority, abs)
		}
	}

	// 列表页通常一次列出全部往期文章，能补齐首页只显示的入口。
	if listURL := resolveRef(base, "/nodes"); listURL != "" {
		if body, ctype, _, err := r.get(ctx, listURL, maxArticleBytes, true, pageTimeout); err == nil && isHTML(body, ctype) {
			listText := html.UnescapeString(string(body))
			for _, m := range postsHrefRe.FindAllStringSubmatch(listText, -1) {
				if abs := resolveRef(base, m[1]); abs != "" {
					priority = append(priority, abs)
				}
			}
		} else if err != nil {
			r.debug("列表页不可用，仅使用首页入口", "url", listURL, "err", err)
		}
	}

	return sortArticles(priority)
}

// collectArticleSubs 并行抓取文章页并抽取订阅地址。
func (r *Resolver) collectArticleSubs(ctx context.Context, articles []string, base *url.URL, maxSubs int) []string {
	bodies := make([][]byte, len(articles))

	sem := make(chan struct{}, articleFetchParallel)
	var wg sync.WaitGroup
	for i, u := range articles {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			body, ctype, _, err := r.get(ctx, u, maxArticleBytes, true, pageTimeout)
			if err != nil {
				r.debug("文章抓取失败", "url", u, "err", err)
				return
			}
			if !isHTML(body, ctype) {
				r.debug("文章不是 HTML", "url", u, "ctype", ctype)
				return
			}
			bodies[i] = body
		}(i, u)
	}
	wg.Wait()

	var out []string
	seen := make(map[string]struct{}, 64)
	for i, body := range bodies {
		if len(body) == 0 {
			continue
		}
		found := extractSubURLs(body, base)
		r.debug("文章订阅抽取完成", "url", articles[i], "条数", len(found))
		for _, u := range found {
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			out = append(out, u)
			if maxSubs > 0 && len(out) >= maxSubs {
				return out
			}
		}
	}
	return out
}

// extractSubURLs 从文章页抽取订阅地址。优先取卡片结构里的 <code>，
// 抽不到时再用「标签 + 链接」兜底，最后才退化为扫描全部外链。
func extractSubURLs(body []byte, base *url.URL) []string {
	text := html.UnescapeString(string(body))
	text = strings.ReplaceAll(text, `\/`, "/")

	var cands []string
	for _, m := range codeURLRe.FindAllStringSubmatch(text, -1) {
		cands = append(cands, m[1])
	}
	if len(cands) == 0 {
		for _, m := range subLabelRe.FindAllStringSubmatch(text, -1) {
			cands = append(cands, m[1])
		}
	}
	if len(cands) == 0 {
		// 结构变化时的兜底：扫描全部外链，后续拉取校验会淘汰非订阅。
		for _, u := range urlRe.FindAllString(text, -1) {
			cands = append(cands, strings.TrimRight(u, ".,;:"))
		}
	}

	out := make([]string, 0, len(cands))
	seen := make(map[string]struct{}, len(cands))
	for _, raw := range cands {
		u := strings.TrimSpace(html.UnescapeString(raw))
		if u == "" || strings.ContainsAny(u, "${}{} ") {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		if !isPlausibleSub(parsed, base) {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// isPlausibleSub 过滤掉站内链接、静态资源、推广与社交链接。
func isPlausibleSub(u *url.URL, base *url.URL) bool {
	host := strings.ToLower(u.Host)
	if base != nil && strings.EqualFold(host, base.Host) {
		return false
	}
	if isNoiseHost(host) {
		return false
	}
	if assetExtRe.MatchString(strings.ToLower(u.Path)) {
		return false
	}
	low := strings.ToLower(u.String())
	if promoRe.MatchString(u.Path) || promoRe.MatchString(low) {
		return false
	}
	for _, bad := range skipSubHosts {
		if matchHost(host, []string{bad}) {
			return false
		}
	}
	return true
}

// skipSubHosts 是明确不可能是订阅的站点（社交、视频、文档）。
var skipSubHosts = []string{
	"t.me", "telegram.me", "youtube.com", "youtu.be", "twitter.com", "x.com",
	"github.com", "docs.github.com", "google.com", "wikipedia.org",
}

// sortArticles 去重并按路径里的日期倒序排列，无日期的保持原顺序靠后。
func sortArticles(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, u := range in {
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, oki := articleDate(out[i])
		dj, okj := articleDate(out[j])
		if oki && okj && di != dj {
			return di > dj
		}
		if oki != okj {
			return oki
		}
		return false
	})
	return out
}

func articleDate(u string) (string, bool) {
	if m := dateRe.FindStringSubmatch(u); len(m) == 4 {
		return m[1] + m[2] + m[3], true
	}
	return "", false
}

func resolveRef(base *url.URL, ref string) string {
	ref = strings.TrimSpace(html.UnescapeString(ref))
	if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "javascript:") ||
		strings.HasPrefix(ref, "mailto:") || strings.HasPrefix(ref, "data:") {
		return ""
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(parsed)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
