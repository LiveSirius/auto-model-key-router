package service

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件移植 service.py:44-147 的后台服务启停与 104-119 的前台启动准备。
//
// 全部真实 OS 副作用都在 Env 的注入点后面：进程创建走 Spawn（只有 fake 覆盖）、
// 存活判定走 Running、终止走 Terminate、日志归档走 ArchiveLog。语料里这些接缝被
// 换成脚本化桩，因此「启动了哪条命令、写了什么 PID 文件、轮询了几次」都能对拍。

// backgroundStopPollCount / backgroundStopPollInterval 对应 service.py:138-144 的
// `for _ in range(20)` 与 `time.sleep(0.1)`。
const (
	backgroundStopPollCount    = 20
	backgroundStopPollInterval = 100 * time.Millisecond
)

// StartBackground 对应 service.py:44 的 start_service_background。
//
// 返回面板与错误：Python 侧 Popen 失败会抛异常，由调用方（ops_api）折成 500。
func (e *Env) StartBackground(configPath string, cfg *config.RouterConfig) (tui.Renderable, error) {
	if e.IsServiceHealthy(cfg.Host, cfg.Port, false) {
		return tui.SectionPanel(
			fmt.Sprintf("后台服务已在运行。\n地址: [bold]http://%s:%d[/bold]", cfg.Host, cfg.Port),
			"后台服务", "yellow"), nil
	}

	pidFile := PidFilePath(cfg)
	// 注意这里刻意保留参照实现的反直觉分支：**进程还活着**时才删 PID 文件
	// （service.py:54-55）。它是遗留写法，但改掉会让行为偏离 oracle。
	if pid, ok := ReadPid(pidFile); ok && e.IsProcessRunning(pid) {
		_ = os.Remove(pidFile)
	}

	archivedLogPath, archived, err := e.ArchiveLog(cfg.LogFilePath, e.Now())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dirOf(pidFile), 0o755); err != nil {
		return nil, err
	}

	spec := SpawnSpec{
		// 与参照实现的差异：没有解释器与模块入口，直接重入自身（见 doc.go）。
		Command:    []string{e.BackgroundExecutable(), "--config", configPath, "--serve-foreground"},
		Cwd:        e.Cwd,
		Env:        backgroundEnv(os.Environ()),
		LogPath:    cfg.LogFilePath,
		Detached:   true,
		CloseFiles: e.GOOS != "windows",
	}
	result, err := e.Spawn(spec)
	if err != nil {
		return nil, err
	}
	if err := writePid(pidFile, result.Pid); err != nil {
		return nil, err
	}

	lines := []string{
		"后台服务已启动。",
		fmt.Sprintf("PID: [bold]%d[/bold]", result.Pid),
		fmt.Sprintf("地址: [bold]http://%s:%d[/bold]", cfg.Host, cfg.Port),
		fmt.Sprintf("日志: [bold]%s[/bold]", cfg.LogFilePath),
	}
	if archived {
		lines = append(lines, fmt.Sprintf("旧日志已归档: [bold]%s[/bold]", archivedLogPath))
	}
	return tui.SectionPanel(strings.Join(lines, "\n"), "后台服务", "green"), nil
}

// StopBackground 对应 service.py:122 的 stop_background_service。
//
// 与参照实现一致的三段：没有 PID 文件 → 黄面板；进程已不存在 → 清理 PID 文件并报
// 「已不存在」；否则发终止信号后最多轮询 20×0.1 秒。
func (e *Env) StopBackground(cfg *config.RouterConfig) tui.Renderable {
	pidFile := PidFilePath(cfg)
	pid, ok := ReadPid(pidFile)
	if !ok {
		return tui.SectionPanel("没有找到后台服务 PID 文件。", "后台服务", "yellow")
	}
	if !e.IsProcessRunning(pid) {
		_ = os.Remove(pidFile)
		return tui.SectionPanel(
			fmt.Sprintf("PID %d 已不存在，已清理 PID 文件。", pid), "后台服务", "yellow")
	}
	if err := e.Terminate(pid); err != nil {
		// Python 的 taskkill 返回码被忽略、POSIX 的 os.kill 可能抛 OSError 并向上
		// 传播；Go 侧统一继续走轮询（下方 20 次循环会给出结论）。
		_ = err
	}
	for i := 0; i < backgroundStopPollCount; i++ {
		if !e.IsProcessRunning(pid) {
			_ = os.Remove(pidFile)
			return tui.SectionPanel(
				fmt.Sprintf("后台服务已停止。\nPID: [bold]%d[/bold]", pid), "后台服务", "green")
		}
		e.Sleep(backgroundStopPollInterval)
	}
	return tui.SectionPanel(
		fmt.Sprintf("已发送停止信号，进程退出中。\nPID: [bold]%d[/bold]", pid), "后台服务", "yellow")
}

// ForegroundPIDHook 写前台进程的 PID 文件，返回清理函数。
//
// 对应 service.py:111-112（`pid_file_path(config).parent.mkdir(...)` +
// `write_text(str(os.getpid()))`）。前台服务真正监听端口由 cmd/amkr 负责——本包不
// 依赖 internal/server，避免 service → server → api → service 的环。
func (e *Env) ForegroundPIDHook(cfg *config.RouterConfig) (func(), error) {
	pidFile := PidFilePath(cfg)
	if err := os.MkdirAll(dirOf(pidFile), 0o755); err != nil {
		return nil, err
	}
	if err := writePid(pidFile, e.Pid); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(pidFile) }, nil
}

// ArchiveLogForForeground 对应 service.py:108-109：前台启动时归档旧日志，
// 但被后台启动拉起时（AMKR_LOG_ARCHIVED=1）跳过，避免二次归档。
func (e *Env) ArchiveLogForForeground(cfg *config.RouterConfig) (string, bool, error) {
	if e.Getenv != nil && e.Getenv("AMKR_LOG_ARCHIVED") == "1" {
		return "", false, nil
	}
	return e.ArchiveLog(cfg.LogFilePath, e.Now())
}

// RestartServiceAfterConfigChange 对应 service.py:150 的
// restart_service_after_config_change：只产出提示面板，不做重启。
//
// 监听地址变化时提示「下次启动生效」，否则提示热重载。
func RestartServiceAfterConfigChange(oldConfig, newConfig *config.RouterConfig) tui.Renderable {
	if oldConfig.Host != newConfig.Host || oldConfig.Port != newConfig.Port {
		return tui.SectionPanel(
			fmt.Sprintf("配置已保存。当前进程将继续监听旧地址；新监听地址 [bold]http://%s:%d[/bold] 会在下次启动时生效。",
				newConfig.Host, newConfig.Port),
			"配置热重载", "yellow")
	}
	return tui.SectionPanel("配置已保存，运行中的服务会在下一次请求时自动热重载。", "配置热重载", "green")
}
