package service

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/formatting"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件移植 service.py:164-320 的展示层：注册结果面板、权限说明、系统服务状态表、
// 后台服务状态面板，以及 api 接缝需要的「渲染成纯文本」。
//
// 渲染策略：面板统一走 internal/tui 的富文本降级模型，宽度 100（与
// ops_api._render_text 同值）。唯一版式不同的是系统服务状态表（tui.Table 固定列宽
// vs rich 的 expand 列宽分配，已在 internal/tui 里声明为差异）。

// renderWidth 是面板渲染宽度，对应 ops_api.py:83 的 Console(width=100)。
const renderWidth = 100

// RenderText 把渲染对象转成纯文本，对应 ops_api.py:73 的 _render_text。
//
// 与参照实现的差异：rich 在 force_terminal=False 时会把 ROUNDED 边框换成 SQUARE，
// 而本包恒定用 ROUNDED（internal/tui 的既定差异）。文本内容与信息量一致，
// 边框字形不同——这正是 api 包把该文本定义为「不透明字符串」的原因。
func RenderText(value any) string {
	terminal := tui.NewTerminal()
	terminal.SetSize(renderWidth, 25)
	var buffer bytes.Buffer
	terminal.SetOutput(&buffer)
	terminal.Print(value)
	return strings.TrimSpace(buffer.String())
}

// RegistrationResult 对应 service.py:630 的 registration_result。
//
// 成功时展示 stdout（空则给一句成功文案），失败时展示 stderr→stdout→兜底文案。
func (e *Env) RegistrationResult(command []string, title string) tui.Renderable {
	result := e.Run(command)
	content := strings.TrimSpace(result.Stdout)
	if content == "" {
		content = "注册命令执行成功。"
	}
	if result.Code == 0 {
		return tui.SectionPanel(content, title, "green")
	}
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if message == "" {
		message = "注册命令执行失败。"
	}
	return tui.SectionPanel(message, title, "red")
}

// serviceRegistrationNotePanel 对应 service.py:363 的 service_registration_note_panel。
func serviceRegistrationNotePanel(goos string) tui.Renderable {
	var content string
	switch goos {
	case "windows":
		content = "当前使用 Windows 开机启动计划任务注册，权限级别为 SYSTEM/HIGHEST；非管理员运行时会自动弹出 UAC 授权窗口。"
	case "linux":
		content = "当前使用 systemd user service 注册，通常不需要 sudo；loginctl enable-linger 可能需要管理员授权，失败时服务仍可在用户登录后自启。"
	default:
		content = "当前系统暂不支持自动注册为用户级系统服务。"
	}
	return tui.SectionPanel(content, "权限说明", "blue")
}

// userServiceRegistrationNotePanel 对应 service.py:374 的
// user_service_registration_note_panel（只用于 Windows 用户级任务）。
func userServiceRegistrationNotePanel() tui.Renderable {
	return tui.SectionPanel(
		"当前使用 Windows 当前用户计划任务注册，权限级别为 LIMITED；任务会在该用户登录时启动。",
		"权限说明", "blue")
}

// StatusRows 返回「注册状态」行已按 registered 填好文案的行数据。
//
// 单独导出是为了让测试只比较**语义数据**：Go 的状态表用固定列宽，与 rich 的
// expand 列宽分配不同（internal/tui 的既定差异），因此只比较行数据、不比较版式。
func StatusRows(status servicestatus.SystemServiceStatus) []servicestatus.Row {
	registration := "[yellow]未注册[/yellow]"
	if status.Registered {
		registration = "[green]已注册[/green]"
	}
	rows := make([]servicestatus.Row, 0, len(status.Rows))
	for _, row := range status.Rows {
		if row.Label == "注册状态" {
			row.Value = registration
		}
		rows = append(rows, row)
	}
	return rows
}

// RenderSystemServiceStatus 对应 service.py:276 的 render_system_service_status。
//
// 「注册状态」这一行的值由这里填入（servicestatus 采集层刻意留空，与 Python 一致）。
func RenderSystemServiceStatus(status servicestatus.SystemServiceStatus) tui.Renderable {
	borderStyle := "yellow"
	if status.Registered {
		borderStyle = "green"
	}
	rows := StatusRows(status)
	items := []tui.Renderable{
		tui.SectionPanel(statusTable(rows), "系统服务注册", borderStyle),
	}
	for _, detail := range status.Details {
		items = append(items, tui.SectionPanel(detail.Content, detail.Title, detail.Style))
	}
	return tui.Group{Items: items}
}

// statusTable 对应 service.py:298 的 status_table。
//
// 列宽差异见文件头；单元格文本经过 escapeStatusValue 处理，避免值里的方括号被当成
// rich 标记吞掉（service.py:307 的同一考虑）。
func statusTable(rows []servicestatus.Row) tui.Renderable {
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells = append(cells, []string{row.Label, escapeStatusValue(row.Value)})
	}
	return tui.Table{Columns: []tui.TableColumn{{}, {}}, Rows: cells}
}

// statusValueStyles 是 service.py:308 里被「放行」的前缀：值本身就是标记时不转义。
var statusValueStyles = []string{"[green]", "[yellow]", "[red]", "[blue]", "[cyan]"}

// escapeStatusValue 对应 service.py:307 的 escape_status_value。
//
// 与 rich.markup.escape 的差异：这里只把 `[` 转义成 `\[`（internal/tui 的
// StripMarkup 只认这一种转义），不处理反斜杠——对「渲染出的可见文本」而言二者等价。
func escapeStatusValue(value string) string {
	for _, prefix := range statusValueStyles {
		if strings.HasPrefix(value, prefix) {
			return value
		}
	}
	return strings.ReplaceAll(value, "[", `\[`)
}

// BackgroundStatusPanel 对应 service.py:164 的 background_status_panel。
func (e *Env) BackgroundStatusPanel(cfg *config.RouterConfig, configPath string) tui.Renderable {
	health, healthy := e.ServiceHealth(cfg.Host, cfg.Port, false)
	address := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	lines := []string{
		fmt.Sprintf("地址: [bold]%s[/bold]", address),
		fmt.Sprintf("健康检查: [bold]%s/health[/bold]", address),
	}
	if !healthy {
		lines = append([]string{"状态: [yellow]未运行[/yellow]"}, lines...)
		return tui.SectionPanel(strings.Join(lines, "\n"), "后台服务", "yellow")
	}

	lines = append([]string{"状态: [green]运行中[/green]"}, lines...)
	expectedConfigPath := ""
	if configPath != "" {
		expectedConfigPath = absPath(configPath)
	}
	runningConfigPath := health.ConfigPath
	if runningConfigPath != "" {
		lines = append(lines, fmt.Sprintf("运行配置: [bold]%s[/bold]", runningConfigPath))
	}
	// Python 用 Path 相等比较；Windows 上 PureWindowsPath 的相等是**大小写不敏感**的，
	// 因此这里在 windows 平台也按大小写不敏感比较（见 sameFilePath）。
	if expectedConfigPath != "" && runningConfigPath != "" && !sameFilePath(runningConfigPath, expectedConfigPath) {
		lines = append(lines,
			fmt.Sprintf("[yellow]当前 TUI 配置: %s[/yellow]", expectedConfigPath),
			"[yellow]服务配置不同，Key 可能不一致。[/yellow]",
		)
	}

	runningFingerprint := health.LocalAPIKeyFingerprint
	expectedFingerprint := formatting.KeyFingerprint(cfg.LocalAPIKey)
	if runningFingerprint != "" {
		lines = append(lines, fmt.Sprintf("运行中本地 key 指纹: [bold]%s[/bold]", runningFingerprint))
	}
	if expectedFingerprint != "" {
		lines = append(lines, fmt.Sprintf("当前配置本地 key 指纹: [bold]%s[/bold]", expectedFingerprint))
	}
	return tui.SectionPanel(strings.Join(lines, "\n"), "后台服务", "green")
}

// sameFilePath 复刻 pathlib 的路径相等：先归一化，Windows 上再按大小写不敏感比较
// （PureWindowsPath.__eq__ 会把两侧都 casefold）。
func sameFilePath(left, right string) bool {
	normalizedLeft := normalizeSlashes(absPath(left))
	normalizedRight := normalizeSlashes(absPath(right))
	if isWindowsGOOS() {
		return strings.EqualFold(normalizedLeft, normalizedRight)
	}
	return normalizedLeft == normalizedRight
}

// normalizeSlashes 把反斜杠统一成正斜杠，便于跨平台比较（Windows 上 Python 与 Go
// 都可能拿到两种写法）。
func normalizeSlashes(path string) string { return strings.ReplaceAll(path, "\\", "/") }

// isWindowsGOOS 报告当前平台是否为 Windows。
//
// 单独一层是为了让测试在非 Windows 上也能覆盖 windows 分支（服务注册分支由
// Env.GOOS 决定，而这里只影响路径比较的大小写策略）。
func isWindowsGOOS() bool { return defaultGOOS == "windows" }

// defaultGOOS 是编译期平台（runtime.GOOS）。
const defaultGOOS = runtime.GOOS

// displaySystemName 把 GOOS 还原成 platform.system() 的展示形态（service.py:215）。
//
// 只覆盖三个主流平台：其余平台按首字母大写处理，与 platform.system() 的实际返回值
// 可能不同（例如 freebsd → Freebsd vs FreeBSD）。该文案只出现在「暂不支持」面板里，
// 实际只在这三个平台上验证过。
func displaySystemName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "Darwin"
	case "":
		return goos
	default:
		return strings.ToUpper(goos[:1]) + goos[1:]
	}
}

// ServiceStatusPanel 对应 service.py:199 的 service_status_panel：
// 后台状态 + 系统服务注册状态。
func (e *Env) ServiceStatusPanel(cfg *config.RouterConfig, configPath string) tui.Renderable {
	return tui.Group{Items: []tui.Renderable{
		e.BackgroundStatusPanel(cfg, configPath),
		e.SystemServiceStatusPanel(configPath),
	}}
}

// SystemServiceStatusPanel 对应 service.py:206 的 system_service_status_panel。
func (e *Env) SystemServiceStatusPanel(configPath string) tui.Renderable {
	absoluteConfig := absPath(configPath)
	executable := e.BackgroundExecutable()
	switch e.GOOS {
	case "windows":
		return e.WindowsTaskStatusPanel(executable, absoluteConfig)
	case "linux":
		return e.SystemdUserServiceStatusPanel(executable, absoluteConfig)
	default:
		return tui.SectionPanel(
			fmt.Sprintf("暂不支持当前系统自动注册: %s", displaySystemName(e.GOOS)),
			"系统服务注册", "yellow")
	}
}

// IsSystemServiceRegistered 对应 service.py:219 的 is_system_service_registered。
func (e *Env) IsSystemServiceRegistered(configPath string) bool {
	switch e.GOOS {
	case "windows":
		return e.IsWindowsTaskRegistered()
	case "linux":
		return e.IsSystemdUserServiceRegistered()
	default:
		return false
	}
}
