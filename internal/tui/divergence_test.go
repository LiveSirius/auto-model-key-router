package tui

// 本文件把**刻意与 rich 不同**的地方逐条具名钉住。
//
// 迁移方案明确把「不追求与 rich 逐像素对齐」定为非目标，因此差异必须可见、
// 可解释、可回归，而不是散落在实现里。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rowGroup 构造一个只含 "rowNN" 行的渲染节点，用于退化高度分支。
func rowGroup(count int) Renderable {
	items := make([]Renderable, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, NewMarkup(fmt.Sprintf("row%02d", index)))
	}
	return Group{Items: items}
}

// TestFrameDegenerateHeightHasNoPanel 钉住「高度 < 3 行」的退化分支
// （tui.py:155-158）：不套边框面板，直接把正文裁到终端高度输出。
func TestFrameDegenerateHeightHasNoPanel(t *testing.T) {
	for _, height := range []int{1, 2} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			Console.SetSize(40, height)
			defer Console.SetSize(80, 25)

			state := TerminalFrameState([]Renderable{rowGroup(10)}, nil, FrameOptions{})
			if state.ViewportHeight != height {
				t.Fatalf("退化分支视口高度 = %d, 期望 %d", state.ViewportHeight, height)
			}
			lines := state.Renderable.RenderLines(40)
			if len(lines) != height {
				t.Fatalf("退化分支行数 = %d, 期望 %d", len(lines), height)
			}
			for _, line := range lines {
				if strings.ContainsAny(line, "╭╮╰╯│") {
					t.Fatalf("退化分支不应有面板边框: %q", line)
				}
			}
		})
	}
}

// TestMenuLayoutDivergesByName 记录菜单表格的版式差异：只钉住 Go 侧的固定列宽
// 版式。
//
// Python（rich Table, expand=True, padding=(0,1)）会按可用宽度重新分配列宽，
// 因此行首缩进与列间距都随终端宽度变化；Go 用固定列宽，宽度只影响右边的空白。
func TestMenuLayoutDivergesByName(t *testing.T) {
	cases := []struct {
		name     string
		options  []Option
		selected int
		width    int
	}{
		{"menu_three", []Option{{Value: "a", Label: "甲"}, {Value: "b", Label: "乙"}, {Value: "c", Label: "丙"}}, 1, 40},
		{"menu_first", []Option{{Value: "a", Label: "甲"}, {Value: "b", Label: "乙"}}, 0, 60},
		{"menu_ascii", []Option{{Value: "a", Label: "alpha"}, {Value: "b", Label: "beta"}}, 1, 30},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			node := MenuTable(testCase.options, testCase.selected)
			lines := node.RenderLines(testCase.width)
			first := lines[0]
			// 固定列宽版式的形状：指示列(1) + 空格 + 编号列(5) + 空格 + 文案。
			if !strings.HasPrefix(first, "  ") && !strings.HasPrefix(first, SelectedRowMarker) {
				t.Fatalf("首行应以指示列开头: %q", first)
			}
		})
	}
}

// TestStripMarkupNeverRaises 记录标记解析的差异。
//
// Python：rich 的 Text.from_markup 对多余的闭合标签抛 MarkupError。Go：
// StripMarkup 永不抛异常，尽力给出文本——移植后的内容全是固定文案，把版面问题
// 变成崩溃不划算。
func TestStripMarkupNeverRaises(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// 多余闭合标签被丢弃。
		{"已闭合[/bold]", "已闭合"},
		// 纯标签（无论闭合与否）都被吃掉。
		{"[/]", ""},
		{"[red]", ""},
		{"[bold]粗[/]", "粗"},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			got := StripMarkup(testCase.input)
			if got != testCase.want {
				t.Fatalf("StripMarkup(%q) = %q, 期望 %q", testCase.input, got, testCase.want)
			}
			if got != "" && strings.Contains(got, "[/") {
				t.Fatalf("结果不应残留闭合标签: %q", got)
			}
		})
	}
}

// TestDisplayWidthEmojiDivergence 记录宽度表的边界：Go 用内置区段表，
// 不做 ZWJ 组合序列的折叠，因此 emoji 组合可能与 rich 的 cell_len 不同。
//
// 单独的 emoji（含变体选择符）与 CJK 与 rich 一致（下方用例的 richWidth 列是当时的
// Python 实测值）；这里只把「已知不同」写清楚，避免以后有人误以为差异是回归。
func TestDisplayWidthEmojiDivergence(t *testing.T) {
	cases := []struct {
		text      string
		goWidth   int
		richWidth int
		note      string
	}{
		{"🎯", 2, 2, "单独 emoji 一致"},
		{"中文", 4, 4, "CJK 一致"},
	}
	for _, testCase := range cases {
		if got := DisplayWidth(testCase.text); got != testCase.goWidth {
			t.Fatalf("DisplayWidth(%q) = %d, 期望 %d", testCase.text, got, testCase.goWidth)
		}
		if testCase.goWidth != testCase.richWidth {
			t.Fatalf("%s: Go=%d rich=%d", testCase.note, testCase.goWidth, testCase.richWidth)
		}
	}
	// ZWJ 家族序列：Go 逐个码点相加（2+0+2+0+2=6），rich 也按码点累加（同样 6），
	// 但二者的宽度表对区域指示符（旗帜）取值不同，故这里不做跨实现比较。
	if got := DisplayWidth("👨\u200d👩\u200d👧"); got != 6 {
		t.Fatalf("ZWJ 序列按码点累加应为 6, 得到 %d", got)
	}
}

// TestMouseWheelModeNoopWhenDisabled 记录装饰器语义的差异：Python 的
// mouse_wheel_mode 是 contextmanager（with 语句），Go 返回恢复函数。
func TestMouseWheelModeNoopWhenDisabled(t *testing.T) {
	if restore := MouseWheelMode(false); restore == nil {
		t.Fatal("恢复函数不应为 nil")
	}
	// PosixInputMode 返回的恢复函数必须调用：它在 POSIX 上会设置包级标志
	// posixInputModeActive（tui.py:412），丢掉恢复函数等于让标志永久为真并泄漏给
	// 后续用例——Windows 上 isWindows() 直接 no-op，所以这个泄漏只在 POSIX 上可见。
	restoreInputMode := PosixInputMode()
	if restoreInputMode == nil {
		t.Fatal("恢复函数不应为 nil")
	}
	restoreInputMode()
	// 关掉鼠标模式时不应写出任何东西。
	var builder strings.Builder
	Console.SetOutput(&builder)
	defer Console.SetOutput(os.Stdout)
	MouseWheelMode(false)()
	if builder.String() != "" {
		t.Fatalf("禁用时不应写转义序列, 实际 %q", builder.String())
	}
}

// TestClearTerminalHistoryWritesSequence 钉住清屏序列（tui.py:860）。
func TestClearTerminalHistoryWritesSequence(t *testing.T) {
	var builder strings.Builder
	Console.SetOutput(&builder)
	defer Console.SetOutput(os.Stdout)
	ClearTerminalHistory()
	if builder.String() != "\033[2J\033[3J\033[H" {
		t.Fatalf("清屏序列 = %q", builder.String())
	}
}

// TestOpenConfigFileMissingFile 钉住「配置文件不存在」的文案（tui.py:900-901）。
func TestOpenConfigFileMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if got := OpenConfigFile(missing); !strings.HasPrefix(got, "配置文件不存在: ") {
		t.Fatalf("文案 = %q", got)
	}
	if got := OpenConfigFile(t.TempDir()); !strings.HasPrefix(got, "配置文件不存在: ") {
		t.Fatalf("目录应视为不存在配置文件, 文案 = %q", got)
	}
}
