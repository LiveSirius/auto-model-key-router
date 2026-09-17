package tui

// 本文件是**其他模块直接依赖的共享渲染助手**（tui.py:35-125、tui.py:234-273）。
//
// 待移植的 service.py / main.py / config_editor.py / dashboard.py / logs_tui.py /
// update.py 从这里导入的每一个符号，都在本文件（或 frame.go / scroll.go /
// options.go）里有同名 Go 对应物，映射表见 doc.go。

import (
	"fmt"
	"strings"
)

const (
	// MouseModeEnable / MouseModeDisable 是开启/关闭 SGR 鼠标上报的转义序列
	// （tui.py:42-43）。只在 Windows 上使用，与 Python 的门控一致。
	MouseModeEnable  = "\033[?1000h\033[?1006h"
	MouseModeDisable = "\033[?1000l\033[?1006l"
	// WheelEventIntervalSeconds 是滚轮事件去抖窗口（tui.py:44）。
	WheelEventIntervalSeconds = 0.16
	// EscSequenceTimeoutSeconds 是读取 ESC 后续字符的等待时间（tui.py:45）。
	EscSequenceTimeoutSeconds = 0.2
	// WheelContentStep 是滚轮一次滚动的行数（tui.py:46）。
	WheelContentStep = 1
	// MinRenderWidth 是渲染宽度的下限（tui.py:48）。
	MinRenderWidth = 4
	// FoldedContentMarker 是内容被折叠时顶部的提示行（tui.py:49）。
	FoldedContentMarker = "… 上方内容已折叠"
	// WindowTitle 是终端窗体面板的默认标题（tui.py:50）。
	WindowTitle = " Auto Model Key Router "
	// WindowBorderStyle 是窗体面板的边框色（tui.py:51）。
	WindowBorderStyle = "bright_magenta"
	// SelectedRowMarker 是「当前选中行」标记；窗体滚动时用它在正文里定位
	// （tui.py:52、tui.py:267）。
	SelectedRowMarker = "▶"
	// AppTitle 是 ASCII 旗标的默认标题行。
	AppTitle = "Auto Model Key Router"
)

// AppASCIIFlag 对应 tui.py:35 的四行 ASCII 旗标。
var AppASCIIFlag = [4]string{
	"   ___    __  ___  __ __  ___ ",
	"  / _ |  /  |/  / / //_/ / _ \\",
	" / __ | / /|_/ / / ,<   / , _/",
	"/_/ |_|/_/  /_/ /_/|_| /_/|_| ",
}

// AppASCIIFlagStyles 对应 tui.py:41 的四种样式名。
//
// Go 侧不保留颜色，只保留此表以便未来需要时对齐样式顺序（语料不涉及）。
var AppASCIIFlagStyles = [4]string{"bold bright_cyan", "bold cyan", "bold bright_blue", "bold bright_magenta"}

// WheelKeys 是「滚轮事件」的按键名（tui.py:47）。
var WheelKeys = []string{"scroll_up", "scroll_down"}

// Option 是菜单项：Value 是返回值/快捷键，Label 是展示文案。
//
// 对应 Python 的 `tuple[str, str]`，命名是为了让移植代码可读。
type Option struct {
	// Value 是选项值（也是数字快捷键）。
	Value string
	// Label 是展示给用户的中文说明。
	Label string
}

// AppFlagTitle 渲染启动横幅：ASCII 旗标 + 标题/副标题/版本，整体居中（tui.py:70-80）。
//
// 与 rich 一致：旗标的每一行后面补三个空格再接详情行，最后一行没有详情。
func AppFlagTitle(title, subtitle, version string) Renderable {
	details := [][2]string{
		{title, AppASCIIFlagStyles[0]},
		{subtitle, AppASCIIFlagStyles[1]},
		{"v" + version, AppASCIIFlagStyles[2]},
		{"", AppASCIIFlagStyles[3]},
	}
	var lines []string
	for index, flag := range AppASCIIFlag {
		lines = append(lines, flag+"   "+details[index][0])
	}
	return Panel{
		Content:     AlignCenter(NewText(strings.Join(lines, "\n"))),
		BorderStyle: WindowBorderStyle,
	}
}

// PageTitle 渲染居中标题面板（tui.py:83-87）。
func PageTitle(title string, subtitle ...string) Renderable {
	text := "[bold cyan]" + title + "[/bold cyan]"
	if len(subtitle) > 0 && subtitle[0] != "" {
		text = text + "\n[dim]" + subtitle[0] + "[/dim]"
	}
	return Panel{
		Content:     AlignCenter(NewMarkup(text)),
		BorderStyle: "cyan",
	}
}

// SectionPanel 渲染带标题（可选副标题）的圆角面板（tui.py:103-104）。
//
// 这是被外部模块引用最多的助手：service.py 的 30 处 `section_panel(...)`、
// main.py、config_editor.py、dashboard.py、update.py 全靠它。
//
// 参数用变参是为了照抄 Python 的可选参数：SectionPanel(c, "标题")、
// SectionPanel(c, "标题", "yellow")、SectionPanel(c, "标题", "cyan", "副标题")。
func SectionPanel(content any, title string, style ...string) Renderable {
	borderStyle := "cyan"
	subtitle := ""
	if len(style) > 0 {
		borderStyle = style[0]
	}
	if len(style) > 1 {
		subtitle = style[1]
	}
	return Panel{
		Content:     coerceRenderable(content),
		Title:       "[bold]" + title + "[/bold]",
		Subtitle:    subtitle,
		BorderStyle: borderStyle,
	}
}

// ShortcutText 渲染底部的快捷键提示（居中、单行、超宽省略号，tui.py:107-108）。
func ShortcutText(text string) Renderable {
	return AlignCenter(Text{Value: text, NoWrap: true, OverflowEllipsis: true})
}

// MenuTable 渲染单选菜单表格（tui.py:90-100）。
//
// 版式差异：rich 用 expand=True 的列宽分配算法，Go 侧用固定列宽
// （指示 1 格 + 编号 5 格 + 文案撑满），信息等价、间距不同，见文件头说明。
func MenuTable(options []Option, selected int) Renderable {
	return MenuTableStyled(options, selected, nil)
}

// MenuTableStyled 与 MenuTable 相同，但允许调用方给**选中行**的单元格套样式。
//
// 交互模型用 lipgloss 高亮当前项（迁移方案允许样式自选，只要信息等价）；
// style 为 nil 时与 rich 的纯文本版式一致，对拍语料走的就是这条路径。
func MenuTableStyled(options []Option, selected int, style func(string) string) Renderable {
	rows := make([][]string, 0, len(options))
	for index, option := range options {
		marker := ""
		value := option.Value
		label := option.Label
		if index == selected {
			marker = SelectedRowMarker
			if style != nil {
				marker, value, label = style(marker), style(value), style(label)
			}
		}
		rows = append(rows, []string{marker, value, label})
	}
	return Table{
		Columns: []TableColumn{
			{Width: 1, Align: "center"},
			{Width: 5, Align: "center"},
			{Align: "left"},
		},
		Rows: rows,
	}
}

// CheckboxMenuTable 渲染多选菜单表格（tui.py:736-753）。
func CheckboxMenuTable(options []Option, selected int, checked map[int]bool) Renderable {
	return CheckboxMenuTableStyled(options, selected, checked, nil)
}

// CheckboxMenuTableStyled 是多选版的高亮变体，语义同 MenuTableStyled。
func CheckboxMenuTableStyled(options []Option, selected int, checked map[int]bool, style func(string) string) Renderable {
	rows := make([][]string, 0, len(options))
	for index, option := range options {
		marker := ""
		value := option.Value
		label := option.Label
		mark := ""
		if checked[index] {
			mark = "✓"
		}
		if index == selected {
			marker = SelectedRowMarker
			if style != nil {
				marker, value, label = style(marker), style(value), style(label)
			}
		}
		rows = append(rows, []string{marker, mark, value, label})
	}
	return Table{
		Columns: []TableColumn{
			{Width: 1, Align: "center"},
			{Width: 1, Align: "center"},
			{Width: 5, Align: "center"},
			{Align: "left"},
		},
		Rows: rows,
	}
}

// ContentViewportHeight 返回正文视口高度（tui.py:111-112）：
// 终端高度减去菜单占用行数，再留 8 行给标题/边框/提示。
func ContentViewportHeight(optionCount int) int {
	height := Console.Height() - optionCount - 8
	if height < 1 {
		return 1
	}
	return height
}

// RenderableLineSegments 把任意值渲染成纯文本行（tui.py:115-116）。
//
// 宽度下限为 MinRenderWidth，与 rich 的 max(width, MIN_RENDER_WIDTH) 一致。
func RenderableLineSegments(content any, width int) []string {
	if width < MinRenderWidth {
		width = MinRenderWidth
	}
	return Console.RenderLines(content, width)
}

// SegmentLinesRenderable 把已渲染的行包装成可再嵌入的节点（tui.py:119-125）。
//
// Python 用 rich.segment.Segments 保存带样式的段；Go 只保留文本行，样式丢弃
// （迁移方案已把富文本降级为纯文本）。
func SegmentLinesRenderable(lines []string) Renderable {
	if len(lines) == 0 {
		lines = []string{""}
	}
	return Lines{Text: lines}
}

// FoldedMarkerLine 返回折叠提示行（tui.py:128-129）。
func FoldedMarkerLine() []string {
	return []string{FoldedContentMarker}
}

// FitTerminalLines 把 lines 适配到 height 行（tui.py:132-141）。
//
// preserveBottom 为真时保留**最后** height 行，并在需要时用折叠提示占据首行。
func FitTerminalLines(lines []string, height int, preserveBottom bool) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) > height {
		if !preserveBottom {
			return lines[:height]
		}
		if height == 1 {
			return FoldedMarkerLine()
		}
		out := FoldedMarkerLine()
		return append(out, lines[len(lines)-(height-1):]...)
	}
	out := make([]string, 0, height)
	out = append(out, lines...)
	for len(out) < height {
		out = append(out, "")
	}
	return out
}

// ContentScrollOffset 把一次滚动按键换算成新的偏移量（tui.py:296-309）。
//
// 滚轮每次一行（WHEEL_CONTENT_STEP），翻页键一次一屏；home/end 到两端。
func ContentScrollOffset(key string, offset, maxOffset, viewportHeight int) int {
	switch key {
	case "scroll_up":
		return maxInt(0, offset-WheelContentStep)
	case "scroll_down":
		return minInt(maxOffset, offset+WheelContentStep)
	case "page_up":
		return maxInt(0, offset-viewportHeight)
	case "page_down":
		return minInt(maxOffset, offset+viewportHeight)
	case "home":
		return 0
	case "end":
		return maxOffset
	}
	return offset
}

// ScrollableContentState 渲染带滚动条的正文面板（tui.py:234-244）。
//
// 返回值依次是：渲染节点、新偏移、最大偏移、视口高度。
// Windows 上的提示文案多一个「滚轮」（与 Python 的 sys.platform 分支一致）。
func ScrollableContentState(content any, offset, optionCount int) (Renderable, int, int, int) {
	viewportHeight := ContentViewportHeight(optionCount)
	width := Console.Width() - 4
	lines := RenderableLineSegments(content, width)
	maxOffset := len(lines) - viewportHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	offset = minInt(maxInt(offset, 0), maxOffset)
	if maxOffset == 0 {
		return coerceRenderable(content), offset, maxOffset, viewportHeight
	}
	end := minInt(offset+viewportHeight, len(lines))
	viewport := SegmentLinesRenderable(lines[offset:end])
	title := fmt.Sprintf("内容 第 %d-%d 行 / 共 %d 行", offset+1, end, len(lines))
	hint := "[dim]PgUp/PgDn 翻阅[/dim]"
	if isWindows() {
		hint = "[dim]滚轮或 PgUp/PgDn 翻阅[/dim]"
	}
	return SectionPanel(viewport, title, "blue", hint), offset, maxOffset, viewportHeight
}

// RenderOptionMenuState 渲染「标题 + 正文 + 操作菜单 + 快捷键」的整套窗体
// （tui.py:247-268），返回窗体状态。
func RenderOptionMenuState(title string, options []Option, selected int, content any, frameOffset int, ensureSelectedVisible bool) FrameState {
	return RenderOptionMenuStateWithTable(title, MenuTable(options, selected), content, frameOffset, ensureSelectedVisible)
}

// RenderOptionMenuStateWithTable 与 RenderOptionMenuState 相同，但菜单节点由调用方
// 给出——交互模型用它注入选中行高亮（lipgloss），对外行为一字不差。
func RenderOptionMenuStateWithTable(title string, menu Renderable, content any, frameOffset int, ensureSelectedVisible bool) FrameState {
	renderables := []Renderable{PageTitle(title)}
	if content != nil {
		renderables = append(renderables, coerceRenderable(content))
	}
	renderables = append(renderables, SectionPanel(menu, "操作菜单", "cyan", "[dim]选择下一步操作[/dim]"))
	shortcuts := "↑/↓ 选择  ·  Enter 确认  ·  PgUp/PgDn 翻阅窗体  ·  数字快捷键  ·  q/Ctrl+C 返回"
	if isWindows() && content != nil {
		shortcuts = "↑/↓ 选择  ·  Enter 确认  ·  PgUp/PgDn/滚轮 翻阅窗体  ·  数字快捷键  ·  q/Ctrl+C 返回"
	}
	focus := ""
	if ensureSelectedVisible {
		focus = SelectedRowMarker
	}
	return TerminalFrameState(renderables, ShortcutText(shortcuts), FrameOptions{
		Offset:    frameOffset,
		FocusText: focus,
	})
}

// RenderOptionMenu 只返回 RenderOptionMenuState 的渲染节点（tui.py:271-272）。
func RenderOptionMenu(title string, options []Option, selected int, content any, contentOffset int) Renderable {
	return RenderOptionMenuState(title, options, selected, content, contentOffset, true).Renderable
}

// RenderMultiSelectState 渲染多选菜单窗体（tui.py:756-778）。
func RenderMultiSelectState(title string, options []Option, selected int, checked map[int]bool, content any, frameOffset int, ensureSelectedVisible bool) FrameState {
	return RenderMultiSelectStateWithTable(title, CheckboxMenuTable(options, selected, checked), content, frameOffset, ensureSelectedVisible)
}

// RenderMultiSelectStateWithTable 是多选版的高亮变体，语义同
// RenderOptionMenuStateWithTable。
func RenderMultiSelectStateWithTable(title string, menu Renderable, content any, frameOffset int, ensureSelectedVisible bool) FrameState {
	renderables := []Renderable{PageTitle(title)}
	if content != nil {
		renderables = append(renderables, coerceRenderable(content))
	}
	renderables = append(renderables, SectionPanel(menu, "操作菜单", "cyan", "[dim]Space 切换 · A 全选/取消[/dim]"))
	shortcuts := "↑/↓ 选择  ·  Space 切换  ·  A 全选/取消  ·  Enter 确认  ·  PgUp/PgDn 翻阅  ·  q/Ctrl+C 返回"
	if isWindows() && content != nil {
		shortcuts = "↑/↓ 选择  ·  Space 切换  ·  A 全选/取消  ·  Enter 确认  ·  PgUp/PgDn/滚轮 翻阅  ·  q/Ctrl+C 返回"
	}
	focus := ""
	if ensureSelectedVisible {
		focus = SelectedRowMarker
	}
	return TerminalFrameState(renderables, ShortcutText(shortcuts), FrameOptions{
		Offset:    frameOffset,
		FocusText: focus,
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
