// Package config 定义运行参数，支持 YAML 配置文件与命令行覆盖。
package config

import (
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 YAML 中可以写 "30m" 这类可读时长。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML 保证写出的配置是 "30m" 这样的可读形式，而不是纳秒整数。
func (d Duration) MarshalYAML() (any, error) {
	v := time.Duration(d)
	if v == 0 {
		return "0s", nil
	}
	// Go 的 String() 会把整分钟写成 "30m0s"、整小时写成 "1h0m0s"，去掉多余的零。
	s := v.String()
	switch {
	case strings.HasSuffix(s, "h0m0s"):
		s = s[:len(s)-4]
	case strings.HasSuffix(s, "m0s"):
		s = s[:len(s)-2]
	}
	return s, nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config 是完整的运行配置。
type Config struct {
	// Site 是分享站点首页地址。配置后程序会自动从页面解析出订阅地址，
	// 无需再手动「复制精选订阅」。Subscriptions 可与它并用，结果会合并去重。
	Site string `yaml:"site"`

	// SiteButton 是页面上「复制精选订阅」按钮的文案。解析器用它定位按钮
	// 所在的 JS 模块，再在模块内提取订阅地址；站点改文案时调整此项即可。
	SiteButton string `yaml:"site_button"`

	// ArticleLimit 是跟进「查看节点文章」的篇数。每篇文章通常挂着几十条
	// 「通用订阅」，节点数量远多于首页那份精选订阅。0 表示只用精选订阅。
	ArticleLimit int `yaml:"article_limit"`
	// ArticleMaxSubs 限制每轮从文章中收集的订阅总数，0 表示不限。
	ArticleMaxSubs int `yaml:"article_max_subs"`
	// ArticleConcurrency 是拉取这些订阅的并发数。
	ArticleConcurrency int `yaml:"article_concurrency"`
	// MaxNodes 是合并去重后的节点上限，0 表示不限。节点过多时可用来缩短单轮耗时。
	MaxNodes int `yaml:"max_nodes"`

	// 订阅来源，支持 http(s) 与本地文件，可填多个。
	Subscriptions []string `yaml:"subscriptions"`

	// 探测目标。建议保留默认的 gstatic 204，它能同时验证 DNS、TLS 与出境链路。
	ProbeURL string `yaml:"probe_url"`

	PreTimeout     Duration `yaml:"pre_timeout"`
	ProbeTimeout   Duration `yaml:"probe_timeout"`
	PreConcurrency int      `yaml:"pre_concurrency"`
	Concurrency    int      `yaml:"concurrency"`

	// Rounds 为全部节点的初测轮数；RefineRounds 为入选候选的复测轮数。
	Rounds       int `yaml:"rounds"`
	RefineRounds int `yaml:"refine_rounds"`
	TopN         int `yaml:"top_n"`

	// SourceCapRatio 限制单一订阅来源最多占 top_n 的比例（0.5 表示不超过一半），
	// 避免规模大的订阅把名额占满、其它来源的可用节点被截断。填 1 表示不做限制。
	SourceCapRatio float64 `yaml:"source_cap_ratio"`

	// 下载测速：对候选节点在隧道内实拉一段数据，按吞吐排序。
	// 延迟只反映握手开销，和带宽没有必然关系，免费节点里常见「延迟高但速度好」。
	Speedtest        bool     `yaml:"speedtest"`
	SpeedtestURL     string   `yaml:"speedtest_url"`
	SpeedtestTimeout Duration `yaml:"speedtest_timeout"`
	// SpeedtestConcurrency 是同时测速的节点数，太大会互相抢带宽、把结果压低。
	SpeedtestConcurrency int `yaml:"speedtest_concurrency"`

	// Include / Exclude 按节点名关键字过滤。
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`

	// 输出。
	OutputDir       string   `yaml:"output_dir"`
	ServeAddr       string   `yaml:"serve_addr"`
	Interval        Duration `yaml:"interval"`
	PerRegionGroups int      `yaml:"per_region_groups"`

	// LogFile 日志文件路径。留空时：服务模式写 <配置目录>/logs/freeclash.log，
	// 前台模式输出到终端。服务模式必须落盘，因为系统服务没有终端可看。
	LogFile string `yaml:"log_file"`

	// Insecure 忽略节点侧 TLS 证书错误（免费节点常见自签证书）。
	Insecure bool `yaml:"insecure"`
}

// Default 返回一份可直接运行的默认配置。
func Default() Config {
	return Config{
		Subscriptions:        nil,
		SiteButton:           "复制精选订阅",
		ArticleLimit:         1,
		ArticleMaxSubs:       100,
		ArticleConcurrency:   8,
		MaxNodes:             0,
		ProbeURL:             "https://www.gstatic.com/generate_204",
		PreTimeout:           Duration(2 * time.Second),
		ProbeTimeout:         Duration(6 * time.Second),
		PreConcurrency:       256,
		Concurrency:          96,
		Rounds:               1,
		RefineRounds:         3,
		TopN:                 30,
		SourceCapRatio:       0.5,
		Speedtest:            true,
		SpeedtestURL:         "https://speed.cloudflare.com/__down?bytes=2000000",
		SpeedtestTimeout:     Duration(5 * time.Second),
		SpeedtestConcurrency: 6,
		OutputDir:            "out",
		Interval:             Duration(30 * time.Minute),
		PerRegionGroups:      6,
		Insecure:             true,
	}
}

// Load 读取配置文件，未出现的字段沿用默认值。
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save 写出示例配置。
func Save(path string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
