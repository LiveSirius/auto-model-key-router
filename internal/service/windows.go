package service

import (
	"fmt"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件移植 service.py:228-235、260-264、382-521：Windows 计划任务分支。

// WindowsTaskName 对应 service.py:40 的 WINDOWS_TASK_NAME。
const WindowsTaskName = "AutoModelKeyRouter"

// IsWindowsTaskRegistered 对应 service.py:228 的 is_windows_task_registered。
//
// 两条 schtasks 查询任一返回 0 即算已注册（`/V /FO LIST` 与 `/XML` 在部分系统上
// 一条可用另一条不可用）。
func (e *Env) IsWindowsTaskRegistered() bool {
	listResult := e.Run([]string{"schtasks", "/Query", "/TN", WindowsTaskName, "/V", "/FO", "LIST"})
	xmlResult := e.Run([]string{"schtasks", "/Query", "/TN", WindowsTaskName, "/XML"})
	return listResult.Code == 0 || xmlResult.Code == 0
}

// WindowsTaskStatusPanel 对应 service.py:260 的 windows_task_status_panel：
// 采集（internal/servicestatus）+ 渲染。
func (e *Env) WindowsTaskStatusPanel(executable, configPath string) tui.Renderable {
	status, err := servicestatus.CollectWindowsTaskStatus(executable, configPath, WindowsTaskName, e.Run)
	if err != nil {
		// Python 在这里不会出错：run_status_command 已经把 FileNotFoundError 折成
		// 返回码。只有 XML/属性解析这类内部错误会走到这里，此时给出一个红色面板
		// 而不是中断整个状态页（见 internal/servicestatus 的错误语义）。
		return tui.SectionPanel(fmt.Sprintf("[red]%s[/red]", err.Error()), "系统服务注册", "red")
	}
	return RenderSystemServiceStatus(status)
}

// PowershellQuote 对应 service.py:520 的 powershell_quote：单引号包裹并把内部单引号
// 翻倍。PowerShell 的单引号字符串不做转义，只认 `”`。
func PowershellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// WindowsTaskSettingsCommand 对应 service.py:504 的 windows_task_settings_command。
//
// 这段 PowerShell 把任务的电源/多实例/执行时限设置成「适合常驻服务」的取值；
// 它不含可执行文件信息，因此与参照实现逐字一致。
func WindowsTaskSettingsCommand() []string {
	script := "$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries" +
		" -DontStopIfGoingOnBatteries -StartWhenAvailable -MultipleInstances IgnoreNew" +
		" -ExecutionTimeLimit (New-TimeSpan -Seconds 0); Set-ScheduledTask -TaskName " +
		PowershellQuote(WindowsTaskName) + " -Settings $settings | Out-Null"
	return []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script}
}

// TaskActionCommandLine 是写进 `/TR` 的那条命令行。
//
// 参照实现是 `"{python}" -m auto_model_key_router.main --config "{config}" --serve-foreground`
// （service.py:397 与 431）；Go 版没有解释器与模块入口，因此是
// `"{exe}" --config "{config}" --serve-foreground`。差异由
// TestWindowsTaskCommandLineDivergence 具名锁定，任务命令列表的其余部分与语料一致。
func TaskActionCommandLine(executable, configPath string) string {
	return fmt.Sprintf(`"%s" --config "%s" --serve-foreground`, executable, configPath)
}

// pythonTaskActionCommandLine 是参照实现那一版命令行，只用于对拍断言
// （语料里记录的 /TR 元素应当等于它）。
func pythonTaskActionCommandLine(executable, configPath string) string {
	return fmt.Sprintf(`"%s" -m auto_model_key_router.main --config "%s" --serve-foreground`,
		executable, configPath)
}

// IsWindowsAdmin 对应 service.py:461 的 is_windows_admin：非 Windows 恒为假。
func (e *Env) IsWindowsAdmin() bool {
	if e.GOOS != "windows" || e.IsAdmin == nil {
		return false
	}
	return e.IsAdmin()
}

// ManageWindowsTask 对应 service.py:382 的 manage_windows_task。
//
// 与 Python 的差异只有 /TR 的命令行文本（见 TaskActionCommandLine）。未知动作在
// Python 里是 `commands[action]` 抛 KeyError（ops_api 折成 500），这里返回错误。
func (e *Env) ManageWindowsTask(executable, configPath, action string) (tui.Renderable, error) {
	taskName := WindowsTaskName
	if action == "install-user" {
		command := []string{
			"schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", taskName,
			"/RL", "LIMITED", "/IT",
			"/TR", TaskActionCommandLine(executable, configPath),
		}
		return tui.Group{Items: []tui.Renderable{
			e.RegistrationResult(command, "Windows 登录自启"),
			e.RegistrationResult(WindowsTaskSettingsCommand(), "Windows 自启设置"),
			e.RegistrationResult([]string{"schtasks", "/Run", "/TN", taskName}, "Windows 启动"),
			userServiceRegistrationNotePanel(),
		}}, nil
	}

	// `action.removesuffix("-elevated") if action.endswith("-elevated") else ""`
	elevated := ""
	if strings.HasSuffix(action, "-elevated") {
		elevated = strings.TrimSuffix(action, "-elevated")
	}
	if elevated != "" {
		// -elevated 后缀来自 UAC 二次启动：此时**必须**已经是管理员，否则直接报错，
		// 避免无限递归提权（service.py:405-411）。
		if !e.IsWindowsAdmin() {
			return tui.SectionPanel("未获得管理员权限，无法注册或管理开机启动任务。", "Windows UAC", "red"), nil
		}
		action = elevated
	} else if isWindowsServiceAction(action) && !e.IsWindowsAdmin() {
		return e.elevateWindowsServiceAction(configPath, action), nil
	}

	switch action {
	case "install":
		command := []string{
			"schtasks", "/Create", "/F", "/SC", "ONSTART", "/TN", taskName,
			"/RU", "SYSTEM", "/RL", "HIGHEST",
			"/TR", TaskActionCommandLine(executable, configPath),
		}
		return tui.Group{Items: []tui.Renderable{
			e.RegistrationResult(command, "Windows 开机自启"),
			e.RegistrationResult(WindowsTaskSettingsCommand(), "Windows 自启设置"),
			e.RegistrationResult([]string{"schtasks", "/Run", "/TN", taskName}, "Windows 启动"),
			serviceRegistrationNotePanel(e.GOOS),
		}}, nil
	case "uninstall":
		loaded, err := config.Load(configPath)
		if err != nil {
			return nil, err
		}
		return tui.Group{Items: []tui.Renderable{
			e.RegistrationResult([]string{"schtasks", "/End", "/TN", taskName}, "Windows 停止"),
			e.StopBackground(loaded),
			e.RegistrationResult([]string{"schtasks", "/Delete", "/F", "/TN", taskName}, "Windows 自启"),
		}}, nil
	case "restart":
		return tui.Group{Items: []tui.Renderable{
			e.RegistrationResult([]string{"schtasks", "/End", "/TN", taskName}, "Windows 停止"),
			e.RegistrationResult([]string{"schtasks", "/Run", "/TN", taskName}, "Windows 启动"),
		}}, nil
	case "start":
		return e.RegistrationResult([]string{"schtasks", "/Run", "/TN", taskName}, "Windows 自启"), nil
	case "stop":
		return e.RegistrationResult([]string{"schtasks", "/End", "/TN", taskName}, "Windows 自启"), nil
	case "status":
		return e.RegistrationResult(
			[]string{"schtasks", "/Query", "/TN", taskName, "/V", "/FO", "LIST"}, "Windows 自启"), nil
	default:
		// 对应 Python 的 KeyError（未列入 commands 的动作）。
		return nil, fmt.Errorf("不支持的系统服务动作: %s", action)
	}
}

// isWindowsServiceAction 对应 service.py:413 的动作集合。
func isWindowsServiceAction(action string) bool {
	switch action {
	case "install", "uninstall", "start", "stop", "restart":
		return true
	}
	return false
}

// ElevationArguments 是 UAC 二次启动时传给子进程的参数。
//
// 参照实现是 `-m auto_model_key_router.main --config <path> --service <action>-elevated`
// （service.py:474-481）；Go 版去掉解释器参数，直接以自身可执行文件重入。
func ElevationArguments(configPath, action string) []string {
	return []string{"--config", configPath, "--service", action + "-elevated"}
}

// elevationScript 是提权用的 PowerShell 脚本模板（service.py:482-488）。
//
// 抽成纯函数是为了让「模板 + powershell_quote」可以拿参照实现记录的脚本文本逐字对拍。
func elevationScript(executable string, arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, PowershellQuote(argument))
	}
	return "$process = Start-Process -FilePath " + PowershellQuote(executable) +
		" -ArgumentList @(" + strings.Join(quoted, ",") + ") -Verb RunAs -Wait -PassThru; exit $process.ExitCode"
}

// elevateWindowsServiceAction 对应 service.py:472 的 elevate_windows_service_action。
//
// 真机上会弹出 UAC 窗口，因此这条路径**只有 fake 覆盖**：测试注入 Env.Run 记录脚本。
func (e *Env) elevateWindowsServiceAction(configPath, action string) tui.Renderable {
	script := elevationScript(e.Executable, ElevationArguments(configPath, action))
	result := e.Run([]string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script})
	if result.Code == 0 {
		return tui.SectionPanel("已通过 UAC 管理员权限完成 Windows 开机自启任务管理。", "Windows UAC", "green")
	}
	// `(result.stderr or result.stdout or "管理员授权被取消或执行失败。").strip()`
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if message == "" {
		message = "管理员授权被取消或执行失败。"
	}
	return tui.SectionPanel(message, "Windows UAC", "red")
}
