package tui

// 本文件对应 rich.console.Console 在 tui.py 里的用法：持有输出流与终端尺寸，
// 把 Renderable 渲染成纯文本行并写出。

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// defaultTerminalWidth / defaultTerminalHeight 是探测不到终端尺寸时的兜底值。
//
// rich 在非终端环境同样退化成 80x25（rich/console.py 的 `_detect_size`），
// 这里保持同值，让语料在任何环境下都能复现。
const (
	defaultTerminalWidth  = 80
	defaultTerminalHeight = 25
)

// Terminal 对应 rich.console.Console：渲染与写出终端文本。
//
// 与 rich 的差异：Go 侧不做 TTY 探测与 ioctl 取尺寸，只读 COLUMNS/LINES
// 环境变量再退化成 80x25。交互式界面（Bubble Tea 模型）会在收到
// tea.WindowSizeMsg 时调用 SetSize 修正，所以真实运行时的尺寸来自事件循环。
type Terminal struct {
	mu     sync.Mutex
	out    io.Writer
	width  int
	height int
}

// NewTerminal 创建终端，并做一次尺寸探测。
func NewTerminal() *Terminal {
	terminal := &Terminal{out: os.Stdout}
	terminal.detectSize()
	return terminal
}

// Console 是模块级共享终端，对应 tui.py:33 的 `console = Console()`。
//
// 其他模块（service.py / main.py / config_editor.py / dashboard.py）都通过它
// 打印，因此这里必须导出同名变量。
var Console = NewTerminal()

// SetOutput 替换输出目标（nil 表示丢弃）。
func (t *Terminal) SetOutput(w io.Writer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.out = w
}

// detectSize 读取 COLUMNS/LINES，缺失时退化成 80x25。
func (t *Terminal) detectSize() {
	width, height := defaultTerminalWidth, defaultTerminalHeight
	if value, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && value > 0 {
		width = value
	}
	if value, err := strconv.Atoi(os.Getenv("LINES")); err == nil && value > 0 {
		height = value
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.width, t.height = width, height
}

// SetSize 覆盖终端尺寸（交互模型收到窗口尺寸消息时调用，测试也用它注入）。
func (t *Terminal) SetSize(width, height int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if width > 0 {
		t.width = width
	}
	if height > 0 {
		t.height = height
	}
}

// Size 返回终端宽高（单元格）。
func (t *Terminal) Size() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.width, t.height
}

// Width 返回终端宽度。
func (t *Terminal) Width() int {
	width, _ := t.Size()
	return width
}

// Height 返回终端高度。
func (t *Terminal) Height() int {
	_, height := t.Size()
	return height
}

// RenderLines 按给定宽度渲染任意受支持的值，返回纯文本行。
//
// 对应 rich 的 `console.render_lines(content, options.update(width=...), pad=False)`：
// 行数用于版面计算（tui.py:115-116、tui.py:162、tui.py:178）。
func (t *Terminal) RenderLines(value any, width int) []string {
	renderable := coerceRenderable(value)
	if renderable == nil {
		return nil
	}
	return renderable.RenderLines(width)
}

// Print 渲染并写出若干值，等价于 `console.print(*objects)`。
//
// 每个值渲染成多行后再补一个换行，与 rich 的 `end="\n"` 一致。
func (t *Terminal) Print(values ...any) {
	t.mu.Lock()
	out := t.out
	width := t.width
	t.mu.Unlock()
	if out == nil {
		return
	}
	var b strings.Builder
	for _, value := range values {
		for _, line := range t.RenderLines(value, width) {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	_, _ = io.WriteString(out, b.String())
}

// Printf 用 fmt 语法渲染一行纯文本，便于过渡期代码。
func (t *Terminal) Printf(format string, args ...any) {
	t.Print(fmt.Sprintf(format, args...))
}

// Write 直接写出一个字符串（用于鼠标模式等转义序列）。
//
// 对应 Python 里对 sys.stdout.write 的调用；写出失败返回 false，
// 让调用方沿用 Python 的 `except (OSError, ValueError)` 回退路径。
func (t *Terminal) Write(text string) bool {
	t.mu.Lock()
	out := t.out
	t.mu.Unlock()
	if out == nil {
		return false
	}
	if _, err := io.WriteString(out, text); err != nil {
		return false
	}
	return true
}

// Clear 清屏，对应 `console.clear()`（tui.py:862）。
//
// 与 ClearTerminalHistory 的区别：rich 的 console.clear() 会同时清掉 Console 内部
// 的缓冲状态，Go 侧没有这类状态，因此这里只写清屏序列；ClearTerminalHistory 已经
// 自己写过一次，所以不再调它（避免向 stdout 重复写序列）。
func (t *Terminal) Clear() {
	t.Write("\033[2J\033[3J\033[H")
}
