package service

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件移植 service.py:238-273、549-627：Linux systemd user 分支。

// SystemdUserServiceName 对应 service.py:41 的 SYSTEMD_USER_SERVICE_NAME。
const SystemdUserServiceName = "auto-model-key-router.service"

// systemdUserServicePath 是 `Path.home()/.config/systemd/user/<name>`。
func (e *Env) systemdUserServicePath() string {
	return filepath.Join(e.Home, ".config", "systemd", "user", SystemdUserServiceName)
}

// IsSystemdUserServiceRegistered 对应 service.py:238 的 is_systemd_user_service_registered。
//
// 已注册 = unit 文件存在，或 systemctl show 的 LoadState 不是「空 / not-found」。
func (e *Env) IsSystemdUserServiceRegistered() bool {
	servicePath := e.systemdUserServicePath()
	showResult := e.Run([]string{
		"systemctl", "--user", "show", SystemdUserServiceName,
		"--property=LoadState", "--no-pager",
	})
	showValues := servicestatus.ParseSystemctlProperties(showResult.Stdout)
	loadState, present := showValues["LoadState"]
	if fileExists(servicePath) {
		return true
	}
	return present && loadState != "" && loadState != "not-found"
}

// SystemdUserServiceStatusPanel 对应 service.py:267 的 systemd_user_service_status_panel。
func (e *Env) SystemdUserServiceStatusPanel(executable, configPath string) tui.Renderable {
	status, err := servicestatus.CollectSystemdUserStatus(
		executable, configPath, e.systemdUserServicePath(), SystemdUserServiceName, e.Run)
	if err != nil {
		return tui.SectionPanel(fmt.Sprintf("[red]%s[/red]", err.Error()), "系统服务注册", "red")
	}
	return RenderSystemServiceStatus(status)
}

// SystemdServiceCommand 对应 service.py:549 的 systemd_service_command。
//
// 优先用 PATH 里的 console script（安装过的 amkr），否则退回自身可执行文件。
// 参照实现的回退是 `python -m auto_model_key_router.main`，Go 侧没有解释器——
// TestSystemdServiceCommandDivergence 具名锁定该差异。
func (e *Env) SystemdServiceCommand(configPath string) []string {
	if executable, found := e.ConsoleScriptExecutable(); found {
		return []string{executable, "--config", configPath, "--serve-foreground"}
	}
	return []string{e.Executable, "--config", configPath, "--serve-foreground"}
}

// pythonSystemdServiceCommand 是参照实现的回退形态，生产路径不会调用它：
// TestSystemdServiceCommandDivergence 用它对照 Go 侧去掉解释器后的命令。
func pythonSystemdServiceCommand(executable, configPath string) []string {
	return []string{executable, "-m", "auto_model_key_router.main", "--config", configPath, "--serve-foreground"}
}

// SystemdUnitText 生成 unit 文件内容（service.py:570-590）。
//
// `shlex.join(command)` 必须与 Python 逐字一致：含空格、中文与单引号的路径
// 都要生成同样的引号形态。
func (e *Env) SystemdUnitText(configPath string) string {
	return UnitText(e.Cwd, e.SystemdServiceCommand(configPath))
}

// UnitText 是 unit 文本的纯函数形式（工作目录与命令都由参数给出），便于测试。
func UnitText(workingDirectory string, command []string) string {
	lines := []string{
		"[Unit]",
		"Description=Auto Model Key Router",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"WorkingDirectory=" + workingDirectory,
		"ExecStart=" + ShlexJoin(command),
		"Restart=always",
		"RestartSec=3",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}
	return joinLines(lines)
}

// joinLines 复刻 Python 的 `"\n".join(lines)`（不做任何换行归一化）。
func joinLines(lines []string) string {
	out := ""
	for index, line := range lines {
		if index > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

// ManageSystemdUserService 对应 service.py:563 的 manage_systemd_user_service。
//
// install 会真的写 unit 文件——测试通过把 Env.Home 指到临时目录来隔离，
// 命令（systemctl/loginctl）则全部由注入的 Runner 吞掉。
func (e *Env) ManageSystemdUserService(executable, configPath, action string) (tui.Renderable, error) {
	serviceName := SystemdUserServiceName
	servicePath := e.systemdUserServicePath()
	if action == "install" {
		if err := os.MkdirAll(dirOf(servicePath), 0o755); err != nil {
			return nil, err
		}
		unitText := UnitText(e.Cwd, e.SystemdServiceCommand(configPath))
		if err := os.WriteFile(servicePath, []byte(unitText), 0o666); err != nil {
			return nil, err
		}
		return tui.Group{Items: []tui.Renderable{
			e.RegistrationResult([]string{"systemctl", "--user", "daemon-reload"}, "systemd user"),
			e.RegistrationResult([]string{"systemctl", "--user", "enable", "--now", serviceName}, "systemd user"),
			e.RegistrationResult([]string{"loginctl", "enable-linger", e.User}, "systemd linger"),
			tui.SectionPanel(
				fmt.Sprintf("systemd 用户服务文件已写入:\n[bold]%s[/bold]", servicePath),
				"系统服务", "green"),
			serviceRegistrationNotePanel(e.GOOS),
		}}, nil
	}

	switch action {
	case "start":
		return e.RegistrationResult([]string{"systemctl", "--user", "start", serviceName}, "systemd user"), nil
	case "stop":
		return e.RegistrationResult([]string{"systemctl", "--user", "stop", serviceName}, "systemd user"), nil
	case "restart":
		return e.RegistrationResult([]string{"systemctl", "--user", "restart", serviceName}, "systemd user"), nil
	case "status":
		return e.RegistrationResult(
			[]string{"systemctl", "--user", "status", serviceName, "--no-pager"}, "systemd user"), nil
	case "uninstall":
		loaded, err := config.Load(configPath)
		if err != nil {
			return nil, err
		}
		result := e.RegistrationResult(
			[]string{"systemctl", "--user", "disable", "--now", serviceName}, "systemd user")
		_ = os.Remove(servicePath)
		return tui.Group{Items: []tui.Renderable{
			result,
			e.StopBackground(loaded),
			e.RegistrationResult([]string{"systemctl", "--user", "daemon-reload"}, "systemd user"),
		}}, nil
	default:
		// 对应 Python 的 KeyError。
		return nil, fmt.Errorf("不支持的系统服务动作: %s", action)
	}
}
