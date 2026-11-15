// Package config 定义运行参数，支持 YAML 配置文件与命令行覆盖。
package config

import (
	"os"
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

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config 是完整的运行配置。
type Config struct {
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

	// Include / Exclude 按节点名关键字过滤。
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`

	// 输出。
	OutputDir       string   `yaml:"output_dir"`
	ServeAddr       string   `yaml:"serve_addr"`
	Interval        Duration `yaml:"interval"`
	PerRegionGroups int      `yaml:"per_region_groups"`

	// Insecure 忽略节点侧 TLS 证书错误（免费节点常见自签证书）。
	Insecure bool `yaml:"insecure"`
}

// Default 返回一份可直接运行的默认配置。
func Default() Config {
	return Config{
		Subscriptions:   nil,
		ProbeURL:        "https://www.gstatic.com/generate_204",
		PreTimeout:      Duration(2 * time.Second),
		ProbeTimeout:    Duration(6 * time.Second),
		PreConcurrency:  256,
		Concurrency:     96,
		Rounds:          1,
		RefineRounds:    3,
		TopN:            30,
		OutputDir:       "out",
		Interval:        Duration(30 * time.Minute),
		PerRegionGroups: 6,
		Insecure:        true,
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
