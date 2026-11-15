// Package svc 把 freeclash 的常驻任务包装成系统服务。
//
// 覆盖 Windows 服务管理器（SCM）、Linux systemd / OpenRC / SysV 与 macOS launchd：
// 这些平台差异全部由 github.com/kardianos/service 处理，本包只负责
// 「把长驻任务接到服务生命周期上」以及「安装 / 卸载 / 启动 / 停止」这些命令。
package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kardianos/service"
)

// Identity 描述服务在系统服务管理器里注册的身份信息。
type Identity struct {
	// Name 是服务内部名（systemd unit 名、Windows 服务名），不含空格。
	Name string
	// DisplayName 是服务管理器界面上显示的名字。
	DisplayName string
	// Description 是服务说明。
	Description string
	// Arguments 是服务启动时附加给可执行文件的命令行参数。
	Arguments []string
	// WorkingDir 是 Linux/macOS 下的初始工作目录（Windows 由系统决定）。
	WorkingDir string
}

// DefaultIdentity 返回本项目默认的服务标识。
func DefaultIdentity() Identity {
	return Identity{
		Name:        "freeclash",
		DisplayName: "FreeClash Subscription Updater",
		Description: "定时探测免费机场节点，筛选可用节点并生成本地 Clash 订阅",
	}
}

// Program 实现 service.Interface。Start 必须立刻返回，
// 否则 Windows SCM 会报「服务没有及时响应启动或控制请求」。
type Program struct {
	work    func(ctx context.Context) error
	log     service.Logger
	cancel  context.CancelFunc
	done    chan struct{}
	stopTmo time.Duration
}

// Service 是服务句柄的别名，避免调用方直接依赖 kardianos 包。
type Service = service.Service

// New 构造服务对象。work 是真正的长驻任务，返回即表示任务结束。
func New(id Identity, work func(ctx context.Context) error) (service.Service, *Program, error) {
	if id.Name == "" {
		return nil, nil, errors.New("服务名不能为空")
	}
	prg := &Program{work: work, stopTmo: 10 * time.Second}

	cfg := &service.Config{
		Name:             id.Name,
		DisplayName:      id.DisplayName,
		Description:      id.Description,
		Arguments:        id.Arguments,
		WorkingDirectory: id.WorkingDir,
		Option: service.KeyValue{
			// Windows：开机自动启动；延迟启动可避开开机瞬间网络尚未就绪。
			"StartType":        "automatic",
			"DelayedAutoStart": true,
			// Windows：进程异常退出后由 SCM 拉起，间隔 30s，避免配置错误时疯狂重启。
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "30s",
			"OnFailureResetPeriod":   "10",
			// Linux systemd：任何情况下都重启（进程崩溃可自愈）。
			"Restart": "always",
		},
	}

	s, err := service.New(prg, cfg)
	if err != nil {
		return nil, nil, err
	}
	if lg, lerr := s.Logger(nil); lerr == nil {
		prg.log = lg
	}
	return s, prg, nil
}

// NewForControl 构造一个只用于 install/uninstall/start/stop/status 的句柄，
// 不承载任何业务逻辑。
func NewForControl(id Identity) (Service, error) {
	s, _, err := New(id, nil)
	return s, err
}

// Start 由服务管理器调用；这里只拉起协程，不阻塞。
func (p *Program) Start(s service.Service) error {
	if p.work == nil {
		return errors.New("未设置任务函数，该服务句柄仅用于控制操作")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	go func() {
		defer close(p.done)
		if err := p.work(ctx); err != nil && !errors.Is(err, context.Canceled) {
			p.report("任务异常结束: %v", err)
			// 任务已经无法继续，若进程继续挂着，服务管理器会以为它还在正常工作。
			// 主动以非零码退出，交给 SCM / systemd 的失败重启策略处理。
			go func() {
				time.Sleep(time.Second)
				os.Exit(1)
			}()
		}
	}()
	return nil
}

// Stop 由服务管理器调用，负责通知任务退出并等待其收尾。
func (p *Program) Stop(s service.Service) error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(p.stopTmo):
		p.report("任务在 %s 内未退出，强制结束", p.stopTmo)
	}
	return nil
}

func (p *Program) report(format string, args ...any) {
	if p.log != nil {
		_ = p.log.Errorf(format, args...)
	}
}

// Interactive 报告当前进程是否由用户在前台启动。
// 返回 false 说明是被服务管理器拉起的，此时不能往 stdout 写东西。
func Interactive() bool { return service.Interactive() }

// Run 以服务方式阻塞运行，直到服务被停止。
func Run(s service.Service) error {
	if err := s.Run(); err != nil {
		return fmt.Errorf("服务运行失败: %w", err)
	}
	return nil
}

// Status 返回服务的安装与运行状态。
func Status(s service.Service) (installed bool, running bool, err error) {
	st, err := s.Status()
	if err != nil {
		return false, false, nil // 未安装时各平台返回的错误不统一，统一视为未安装
	}
	switch st {
	case service.StatusRunning:
		return true, true, nil
	case service.StatusStopped:
		return true, false, nil
	default:
		return false, false, nil
	}
}

// Control 执行 start / stop / restart / install / uninstall 之一。
func Control(s service.Service, action string) error {
	if err := service.Control(s, action); err != nil {
		return friendly(action, err)
	}
	return nil
}

// friendly 把各平台原始错误翻译成可操作的提示。
func friendly(action string, err error) error {
	msg := err.Error()
	low := strings.ToLower(msg)

	switch {
	case strings.Contains(low, "access is denied"), strings.Contains(low, "请求的操作需要提升"):
		return fmt.Errorf("%s 失败：需要管理员权限，请用管理员身份打开终端后重试（%v）", action, err)
	case strings.Contains(low, "permission denied"), strings.Contains(low, "operation not permitted"):
		return fmt.Errorf("%s 失败：需要 root 权限，请用 sudo 重试（%v）", action, err)
	case action == "install" && strings.Contains(low, "already exists"),
		action == "install" && strings.Contains(low, "已存在"):
		return fmt.Errorf("服务已安装；如需重装请先执行 freeclash uninstall（%v）", err)
	case action == "uninstall" && strings.Contains(low, "does not exist"):
		return fmt.Errorf("服务尚未安装，无需卸载（%v）", err)
	}
	return err
}
