// Package sub 负责拉取订阅、解析 Clash 配置中的节点，并生成裁剪后的订阅文件。
package sub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Proxy 是订阅中的单个节点，Raw 保留原始定义以便原样输出给 Clash。
type Proxy struct {
	Name   string
	Type   string
	Server string
	Port   int
	Raw    map[string]any

	// Source 记录节点来自哪一份订阅（取订阅地址的域名）。
	// 不写入 Raw，因此不会出现在输出给客户端的配置里；仅用于结果按来源配额挑选。
	Source string
}

// TagSource 给一批节点标注来源，已经标注过的节点保持原样。
func TagSource(nodes []Proxy, source string) []Proxy {
	if source == "" {
		return nodes
	}
	for i := range nodes {
		if nodes[i].Source == "" {
			nodes[i].Source = source
		}
	}
	return nodes
}

// SourceOf 从订阅地址提取来源标识（域名），解析失败时退回原字符串。
func SourceOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// Key 用于节点去重：同协议、同服务器、同端口且同凭证的重复节点只需保留一个。
func (p Proxy) Key() string {
	extra := ""
	for _, k := range []string{"uuid", "password", "cipher"} {
		if v, ok := p.Raw[k]; ok {
			extra += fmt.Sprint(v)
		}
	}
	if ws, ok := p.Raw["ws-opts"].(map[string]any); ok {
		extra += fmt.Sprint(ws["path"])
	}
	return fmt.Sprintf("%s|%s|%d|%s", p.Type, p.Server, p.Port, extra)
}

// RenderProxies 把节点列表渲染为仅含 proxies 段的 YAML，用作订阅缓存。
func RenderProxies(nodes []Proxy) ([]byte, error) {
	doc := struct {
		Proxies []map[string]any `yaml:"proxies"`
	}{Proxies: proxyMaps(nodes)}
	return yaml.Marshal(doc)
}

// Fetch 获取订阅内容。支持 http(s) URL 与本地文件路径。
func Fetch(ctx context.Context, source string, timeout time.Duration) ([]byte, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		// 部分订阅站点会依据 UA 返回不同格式。
		req.Header.Set("User-Agent", "clash-verge/v1.7.7")
		req.Header.Set("Accept", "*/*")

		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: http %d", source, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	}

	if strings.HasPrefix(source, "file://") {
		source = strings.TrimPrefix(source, "file://")
	}
	return os.ReadFile(source)
}

// Parse 解析 Clash 配置，返回其中的节点列表。
func Parse(data []byte) ([]Proxy, error) {
	proxies, err := parseClashYAML(data)
	if err == nil && len(proxies) > 0 {
		return proxies, nil
	}

	// 兼容部分订阅返回 base64 编码的 Clash 配置。
	if decoded, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data))); derr == nil {
		if proxies, yerr := parseClashYAML(decoded); yerr == nil && len(proxies) > 0 {
			return proxies, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("订阅中未找到 proxies 节点")
}

func parseClashYAML(data []byte) ([]Proxy, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %w", err)
	}

	out := make([]Proxy, 0, len(doc.Proxies))
	seen := make(map[string]struct{}, len(doc.Proxies))
	for _, raw := range doc.Proxies {
		raw = normalizeMap(raw)
		p := Proxy{
			Name:   toString(raw["name"]),
			Type:   strings.ToLower(toString(raw["type"])),
			Server: toString(raw["server"]),
			Port:   toInt(raw["port"]),
			Raw:    raw,
		}
		if p.Name == "" || p.Server == "" || p.Port == 0 {
			continue
		}
		if _, dup := seen[p.Key()]; dup {
			continue
		}
		seen[p.Key()] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

// Filter 应用关键字过滤。include 为空表示全部保留。
func Filter(in []Proxy, include, exclude []string) []Proxy {
	out := make([]Proxy, 0, len(in))
	for _, p := range in {
		if len(include) > 0 && !matchAny(p.Name, include) {
			continue
		}
		if matchAny(p.Name, exclude) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func matchAny(s string, patterns []string) bool {
	low := strings.ToLower(s)
	for _, p := range patterns {
		if p != "" && strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

var regionRe = regexp.MustCompile(`(?:^|[^A-Z])([A-Z]{2})-\d`)

// Region 从节点名中提取两位国家/地区代码，失败时返回 "OTHER"。
func Region(name string) string {
	if m := regionRe.FindStringSubmatch(name); len(m) == 2 {
		return m[1]
	}
	return "OTHER"
}

// Group 是输出配置中的一个策略组。
type Group struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	URL       string   `yaml:"url,omitempty"`
	Interval  int      `yaml:"interval,omitempty"`
	Tolerance int      `yaml:"tolerance,omitempty"`
	Proxies   []string `yaml:"proxies,omitempty"`
}

// OutConfig 是最终交给 Clash 使用的配置。
type OutConfig struct {
	MixedPort   int              `yaml:"mixed-port"`
	AllowLan    bool             `yaml:"allow-lan"`
	Mode        string           `yaml:"mode"`
	LogLevel    string           `yaml:"log-level"`
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []Group          `yaml:"proxy-groups"`
	Rules       []string         `yaml:"rules"`
}

// BuildConfig 生成只包含可用节点、并附带自动测速分组的 Clash 配置。
func BuildConfig(nodes []Proxy, probeURL string, perRegion int) OutConfig {
	names := make([]string, 0, len(nodes))
	regionNodes := make(map[string][]string)
	for _, n := range nodes {
		names = append(names, n.Name)
		r := Region(n.Name)
		regionNodes[r] = append(regionNodes[r], n.Name)
	}

	groups := make([]Group, 0, 8)
	mainGroup := Group{Name: "🚀 节点选择", Type: "select"}
	autoGroup := Group{
		Name:      "♻️ 自动选择",
		Type:      "url-test",
		URL:       probeURL,
		Interval:  300,
		Tolerance: 50,
		Proxies:   names,
	}

	mainGroup.Proxies = append(mainGroup.Proxies, autoGroup.Name)

	// 地区较多时按地区建测速组，方便按区域选路。
	regions := make([]string, 0, len(regionNodes))
	for r, list := range regionNodes {
		if r != "OTHER" && len(list) >= 3 {
			regions = append(regions, r)
		}
	}
	sort.Slice(regions, func(i, j int) bool {
		if len(regionNodes[regions[i]]) != len(regionNodes[regions[j]]) {
			return len(regionNodes[regions[i]]) > len(regionNodes[regions[j]])
		}
		return regions[i] < regions[j]
	})
	if perRegion > 0 && len(regions) > perRegion {
		regions = regions[:perRegion]
	}
	for _, r := range regions {
		g := Group{
			Name:      fmt.Sprintf("🌍 %s", r),
			Type:      "url-test",
			URL:       probeURL,
			Interval:  300,
			Tolerance: 50,
			Proxies:   regionNodes[r],
		}
		groups = append(groups, g)
		mainGroup.Proxies = append(mainGroup.Proxies, g.Name)
	}

	mainGroup.Proxies = append(mainGroup.Proxies, names...)
	groups = append([]Group{mainGroup}, append(groups, autoGroup)...)
	groups = append(groups, Group{Name: "🎯 全球直连", Type: "select", Proxies: []string{"DIRECT", "REJECT"}})

	return OutConfig{
		MixedPort:   7890,
		AllowLan:    false,
		Mode:        "rule",
		LogLevel:    "warning",
		Proxies:     proxyMaps(nodes),
		ProxyGroups: groups,
		Rules: []string{
			"DOMAIN-SUFFIX,local,🎯 全球直连",
			"IP-CIDR,127.0.0.0/8,🎯 全球直连,no-resolve",
			"IP-CIDR,192.168.0.0/16,🎯 全球直连,no-resolve",
			"IP-CIDR,10.0.0.0/8,🎯 全球直连,no-resolve",
			"GEOIP,CN,🎯 全球直连",
			"MATCH,🚀 节点选择",
		},
	}
}

func proxyMaps(nodes []Proxy) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Raw)
	}
	return out
}

// Marshal 输出 Clash 配置，并在头部附加生成说明。
func Marshal(cfg OutConfig) ([]byte, error) {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# freeclash 自动生成于 %s\n# 仅包含通过真实可用性探测的节点，共 %d 个\n\n",
		time.Now().Format("2006-01-02 15:04:05"), len(cfg.Proxies))
	return append([]byte(header), body...), nil
}

// WriteReport 输出机器可读的探测报告。
func WriteReport(path string, payload any) error {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ---- YAML 值归一化辅助 ----

func normalizeMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return normalizeMap(t)
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeValue(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = normalizeValue(t[i])
		}
		return t
	default:
		return v
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}
