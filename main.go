package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"freeclash/internal/app"
	"freeclash/internal/config"
	"freeclash/internal/svc"
)

// 由构建时注入：go build -ldflags "-X main.version=1.0.0 -X main.buildTime=..."
var (
	version   = "dev"
	buildTime = ""
)

func main() {
	args := os.Args[1:]

	// 不带前导连字符的第一个参数视为子命令（install/start/...）；
	// 其余情况走参数解析，兼容原有的 freeclash -site ... 用法。
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if err := runCommand(args[0], args[1:]); err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(os.Stderr, "错误: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	if err := runService(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	}
}

/*==========  子命令  ==========*/

func runCommand(cmd string, rest []string) error {
	switch cmd {
	case "run":
		return runService(rest)
	case "install":
		return cmdInstall(rest)
	case "uninstall":
		return cmdUninstall(rest)
	case "start", "stop", "restart":
		return cmdControl(cmd, rest)
	case "status":
		return cmdStatus(rest)
	case "init-config":
		return cmdInitConfig(rest)
	case "version":
		printVersion()
		return nil
	case "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("未知命令 %q", cmd)
	}
}

func cmdInstall(rest []string) error {
	f, err := parseFlags(rest)
	if err != nil {
		return err
	}
	o, err := buildOptions(f)
	if err != nil {
		return err
	}
	o.Service = true

	if o.ConfigPath == "" && o.Cfg.Site == "" && len(o.Cfg.Subscriptions) == 0 {
		return fmt.Errorf("未找到配置文件，也没有指定订阅来源\n" +
			"请任选其一：\n" +
			"  1) 在可执行文件旁放置 config.yaml（参考 config.example.yaml）\n" +
			"  2) 用 freeclash install -site https://分享站点/ 直接生成配置\n" +
			"  3) 用 freeclash install -url https://订阅地址 直接生成配置")
	}
	// 先校验参数，再落盘配置，避免校验失败却在磁盘上留一份半成品配置。
	if err := app.Validate(o); err != nil {
		return err
	}

	// 没有配置文件时，若已通过 -site/-url 指定了订阅来源，就顺手生成一份，
	// 避免用户装完服务才发现缺配置。
	if o.ConfigPath == "" {
		if _, err = writeDefaultConfig(o); err != nil {
			return fmt.Errorf("生成配置文件失败: %w", err)
		}
		if o, err = buildOptions(f); err != nil {
			return err
		}
		o.Service = true
	}
	cfgPath := o.ConfigPath

	id := svc.DefaultIdentity()
	id.Arguments = []string{"-config", cfgPath}
	id.WorkingDir = filepath.Dir(cfgPath)

	s, err := svc.NewForControl(id)
	if err != nil {
		return err
	}

	// 重复安装时先卸载旧定义，避免「服务已存在」导致流程中断。
	if installed, _, _ := svc.Status(s); installed {
		fmt.Println("检测到已安装的服务，先卸载以便更新服务定义")
		if err := removeService(s); err != nil {
			return fmt.Errorf("卸载旧服务失败: %w", err)
		}
	}

	if err := svc.Control(s, "install"); err != nil {
		return err
	}
	fmt.Printf("服务安装成功: %s\n", id.Name)
	fmt.Printf("  可执行文件: %s\n", selfPath())
	fmt.Printf("  配置文件  : %s\n", cfgPath)
	fmt.Printf("  输出目录  : %s\n", o.Cfg.OutputDir)

	if err := svc.Control(s, "start"); err != nil {
		fmt.Fprintf(os.Stderr, "服务已安装但启动失败: %v\n用 freeclash start 重试，或查看系统日志\n", err)
		return nil
	}
	fmt.Println("服务已启动")

	installed, running, _ := svc.Status(s)
	fmt.Printf("当前状态: 已安装=%v 运行中=%v\n", installed, running)
	if o.Cfg.ServeAddr != "" {
		fmt.Printf("订阅地址: http://%s/best.yaml\n", displayHost(o.Cfg.ServeAddr))
	}
	fmt.Printf("日志文件: %s\n", logPathOf(o))
	return nil
}

func cmdUninstall(_ []string) error {
	s, err := controlService()
	if err != nil {
		return err
	}
	installed, _, _ := svc.Status(s)
	if !installed {
		fmt.Println("服务尚未安装，无需卸载")
		return nil
	}
	if err := removeService(s); err != nil {
		return err
	}
	fmt.Println("服务卸载成功")
	return nil
}

// removeService 停止并删除服务。
//
// Windows 的 DeleteService 只是「标记删除」，服务仍在运行时不会真正消失，
// 紧接着再安装就会报「服务已存在」，所以这里必须等到服务确实停下再删、删完再确认。
func removeService(s svc.Service) error {
	installed, running, _ := svc.Status(s)
	if !installed {
		return nil
	}
	if running {
		if err := svc.Control(s, "stop"); err != nil {
			return err
		}
		if !waitService(s, false, 15*time.Second) {
			return fmt.Errorf("服务在 15 秒内未能停止")
		}
		fmt.Println("服务已停止")
	}
	if err := svc.Control(s, "uninstall"); err != nil {
		// Windows 上「标记删除但服务已停」的情况下，卸载会报 not installed，
		// 只要服务管理器已经查不到，就视为卸载成功。
		if installed, _, _ := svc.Status(s); !installed {
			return nil
		}
		return err
	}
	// 删除同样是异步的，等到服务管理器确实查不到再返回。
	for i := 0; i < 20; i++ {
		if installed, _, _ := svc.Status(s); !installed {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("服务定义未能删除，请重启后再试")
}

// waitService 轮询等待服务达到期望的运行状态。
func waitService(s svc.Service, wantRunning bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if installed, running, _ := svc.Status(s); installed && running == wantRunning {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func cmdControl(action string, _ []string) error {
	s, err := controlService()
	if err != nil {
		return err
	}
	installed, _, _ := svc.Status(s)
	if !installed {
		return fmt.Errorf("服务尚未安装，请先执行 freeclash install")
	}
	if err := svc.Control(s, action); err != nil {
		return err
	}
	_, running, _ := svc.Status(s)
	fmt.Printf("已执行 %s，当前运行中=%v\n", action, running)
	return nil
}

func cmdStatus(rest []string) error {
	s, err := controlService()
	if err != nil {
		return err
	}
	installed, running, _ := svc.Status(s)
	if !installed {
		fmt.Println("服务状态: 未安装")
		return nil
	}
	state := "已停止"
	if running {
		state = "运行中"
	}
	fmt.Printf("服务状态: %s\n", state)

	f, ferr := parseFlags(rest)
	if ferr != nil {
		return ferr
	}
	if o, oerr := buildOptions(f); oerr == nil {
		fmt.Printf("配置文件: %s\n", o.ConfigPath)
		fmt.Printf("输出目录: %s\n", o.Cfg.OutputDir)
		fmt.Printf("日志文件: %s\n", logPathOf(o))
		if o.Cfg.ServeAddr != "" {
			fmt.Printf("订阅地址: http://%s/best.yaml\n", displayHost(o.Cfg.ServeAddr))
		}
	}
	return nil
}

func cmdInitConfig(rest []string) error {
	fs := flag.NewFlagSet("init-config", flag.ContinueOnError)
	out := fs.String("out", "", "输出路径（默认为可执行文件旁的 config.yaml）")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	path := *out
	if path == "" {
		exeDir, cwd := dirs()
		dir := exeDir
		if dir == "" {
			dir = cwd
		}
		path = filepath.Join(dir, config.DefaultConfigName)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在，如需覆盖请先删除", path)
	}
	if err := config.Save(path, config.Default()); err != nil {
		return err
	}
	fmt.Printf("已生成默认配置: %s\n", path)
	fmt.Println("请按需修改 site 或 subscriptions 后执行 freeclash install")
	return nil
}

// controlService 构造一个只用于服务控制的对象，不加载订阅配置。
func controlService() (svc.Service, error) {
	return svc.NewForControl(svc.DefaultIdentity())
}

/*==========  前台/服务运行  ==========*/

// runService 是主运行路径：
//   - 前台启动时直接跑业务，Ctrl+C 退出；
//   - 由系统服务管理器拉起时交给 service 包接管生命周期。
func runService(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	o, err := buildOptions(f)
	if err != nil {
		return err
	}

	if svc.Interactive() {
		o.Service = false
		if err := app.Validate(o); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return app.Run(ctx, o)
	}

	// 服务模式：Windows SCM / systemd 已把 -config 作为参数传入，这里直接沿用。
	o.Service = true
	if err := app.Validate(o); err != nil {
		return err
	}
	id := svc.DefaultIdentity()
	id.Arguments = args
	id.WorkingDir = o.BaseDir

	s, _, err := svc.New(id, func(ctx context.Context) error {
		return app.Run(ctx, o)
	})
	if err != nil {
		return err
	}
	return svc.Run(s)
}

/*==========  参数解析  ==========*/

type cliFlags struct {
	configPath  *string
	urls        *string
	siteURL     *string
	siteButton  *string
	resolveOnly *bool
	once        *bool
	interval    *time.Duration
	topN        *int
	serveAddr   *string
	outDir      *string
	logFile     *string
	concurrency *int
	verbose     *bool
	showVersion *bool
}

func parseFlags(args []string) (*cliFlags, error) {
	fs := flag.NewFlagSet("freeclash", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = printUsage

	f := &cliFlags{
		configPath:  fs.String("config", "", "配置文件路径（YAML），默认查找可执行文件旁或当前目录的 config.yaml"),
		urls:        fs.String("url", "", "订阅地址，多个用逗号分隔（覆盖配置）"),
		siteURL:     fs.String("site", "", "分享站点首页地址，自动从页面解析精选订阅（覆盖配置）"),
		siteButton:  fs.String("button", "", "页面上「复制精选订阅」按钮文案，用于定位订阅地址（覆盖配置）"),
		resolveOnly: fs.Bool("resolve", false, "只解析并打印订阅地址，不执行探测"),
		once:        fs.Bool("once", false, "只探测一次后退出"),
		interval:    fs.Duration("interval", 0, "定时探测间隔，如 30m（覆盖配置）"),
		topN:        fs.Int("top", 0, "输出的节点数量（覆盖配置）"),
		serveAddr:   fs.String("serve", "", "启动本地订阅服务，如 :9898（为空则不起服务）"),
		outDir:      fs.String("out", "", "输出目录（覆盖配置）"),
		logFile:     fs.String("log", "", "日志文件路径（覆盖配置）"),
		concurrency: fs.Int("concurrency", 0, "真实探测并发数（覆盖配置）"),
		verbose:     fs.Bool("v", false, "输出详细日志"),
		showVersion: fs.Bool("version", false, "打印版本号"),
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

// buildOptions 合并配置文件与命令行覆盖项，并解析相对路径。
func buildOptions(f *cliFlags) (app.Options, error) {
	if *f.showVersion {
		printVersion()
		os.Exit(0)
	}

	exeDir, cwd := dirs()

	cfgPath := *f.configPath
	if cfgPath == "" {
		cfgPath = config.Discover(exeDir, cwd)
	}
	if cfgPath != "" {
		abs, err := filepath.Abs(cfgPath)
		if err != nil {
			return app.Options{}, err
		}
		cfgPath = abs
	}

	cfg := config.Default()
	baseDir := cwd
	if cfgPath != "" {
		loaded, err := config.Load(cfgPath)
		if err != nil {
			return app.Options{}, fmt.Errorf("读取配置文件失败: %w", err)
		}
		cfg = loaded
		baseDir = filepath.Dir(cfgPath)
	} else if !svc.Interactive() {
		// 服务模式下工作目录不可信（Windows 是 system32），统一用可执行文件所在目录。
		baseDir = exeDir
	}

	if *f.urls != "" {
		var list []string
		for _, u := range strings.Split(*f.urls, ",") {
			if u = strings.TrimSpace(u); u != "" {
				list = append(list, u)
			}
		}
		cfg.Subscriptions = list
	}
	if *f.siteURL != "" {
		cfg.Site = *f.siteURL
	}
	if *f.siteButton != "" {
		cfg.SiteButton = *f.siteButton
	}
	if *f.interval > 0 {
		cfg.Interval = config.Duration(*f.interval)
	}
	if *f.topN > 0 {
		cfg.TopN = *f.topN
	}
	if *f.serveAddr != "" {
		cfg.ServeAddr = *f.serveAddr
	}
	if *f.outDir != "" {
		cfg.OutputDir = *f.outDir
	}
	if *f.logFile != "" {
		cfg.LogFile = *f.logFile
	}
	if *f.concurrency > 0 {
		cfg.Concurrency = *f.concurrency
	}

	config.ResolvePaths(&cfg, baseDir)

	return app.Options{
		Cfg:         cfg,
		ConfigPath:  cfgPath,
		BaseDir:     baseDir,
		Once:        *f.once,
		ResolveOnly: *f.resolveOnly,
		Verbose:     *f.verbose,
	}, nil
}

// writeDefaultConfig 在可执行文件旁生成一份配置，写入当前命令行指定的订阅来源。
func writeDefaultConfig(o app.Options) (string, error) {
	exeDir, cwd := dirs()
	dir := exeDir
	if dir == "" || !writable(dir) {
		dir = cwd
	}
	path := filepath.Join(dir, config.DefaultConfigName)

	cfg := config.Default()
	cfg.Site = o.Cfg.Site
	cfg.SiteButton = o.Cfg.SiteButton
	cfg.Subscriptions = o.Cfg.Subscriptions
	if err := config.Save(path, cfg); err != nil {
		return "", err
	}
	fmt.Printf("已生成配置文件: %s\n", path)
	return path, nil
}

func writable(dir string) bool {
	tmp, err := os.CreateTemp(dir, ".freeclash-*")
	if err != nil {
		return false
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	return true
}

/*==========  辅助  ==========*/

func dirs() (exeDir, cwd string) {
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	cwd, _ = os.Getwd()
	return exeDir, cwd
}

func selfPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return os.Args[0]
}

func displayHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func logPathOf(o app.Options) string {
	if o.Cfg.LogFile != "" {
		return o.Cfg.LogFile
	}
	return filepath.Join(o.BaseDir, "logs", "freeclash.log")
}

func printVersion() {
	if buildTime != "" {
		fmt.Printf("freeclash %s (build %s)\n", version, buildTime)
		return
	}
	fmt.Printf("freeclash %s\n", version)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `freeclash %s —— 免费机场节点探测与最优订阅生成

用法:
  freeclash [参数]                前台运行（Ctrl+C 退出）
  freeclash <命令> [参数]         管理后台服务

服务命令:
  install     安装为系统服务并启动（需管理员 / root）
  uninstall   停止并卸载服务
  start       启动已安装的服务
  stop        停止服务
  restart     重启服务
  status      查看服务状态
  run         前台运行（等价于不带命令）
  init-config 在可执行文件旁生成默认配置文件
  version     打印版本号

常用参数:
  -config <路径>   配置文件，默认查找 exe 旁或当前目录的 config.yaml
  -site <地址>     分享站点首页，自动解析「复制精选订阅」的地址
  -url <地址>      订阅地址，多个用逗号分隔
  -serve <监听>    本地订阅服务，如 :9898
  -interval <时长> 定时探测间隔，如 30m
  -top <数量>      输出的节点数量
  -out <目录>      输出目录
  -log <路径>      日志文件
  -once            只探测一轮后退出
  -resolve         只解析订阅地址
  -v               输出调试日志

示例:
  # 开发调试
  freeclash -site https://freeclash.zhaotizi.best/ -serve :9898 -v

  # 安装为 Windows 服务 / systemd 服务
  freeclash install -site https://freeclash.zhaotizi.best/
  freeclash status
`, version)
}
