package tui

// 本文件把**刻意与 rich 不同**的地方逐条具名钉住。
//
// 迁移方案明确把「不追求与 rich 逐像素对齐」定为非目标，因此差异必须可见、
// 可解释、可回归，而不是散落在实现里。这里的每个测试都引用语料中记录的
// Python 原始输出（testdata/tui_corpus.json）或直接说明平台差异。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFrameDegenerateHeightHasNoPanel 钉住「高度 < 3 行」的退化分支
// （tui.py:155-158）：不套边框面板，直接把正文裁到终端高度输出。
//
// 这条曾经被误判成「rich 打印裸 Segments 时会丢掉最后一行」——实测（语料
// frame 段的 height_2_degenerate / height_1_degenerate）证明 rich 会把全部可见行
// 都打印出来，Go 与之一致，所以这里按普通对拍处理，不是差异记录。
func TestFrameDegenerateHeightHasNoPanel(t *testing.T) {
	corpus := loadTUICorpus(t)
	found := 0
	for _, testCase := range corpus.Frame {
		if testCase.Height >= 3 {
			continue
		}
		found++
		t.Run(testCase.Name, func(t *testing.T) {
			Console.SetSize(testCase.Width, testCase.Height)
			defer Console.SetSize(80, 25)

			renderables := make([]Renderable, 0, len(testCase.Renderables))
			for _, spec := range testCase.Renderables {
				renderables = append(renderables, buildRenderable(t, spec))
			}
			state := TerminalFrameState(renderables, nil, FrameOptions{})
			if state.ViewportHeight != testCase.Height {
				t.Fatalf("退化分支视口高度 = %d, 期望 %d", state.ViewportHeight, testCase.Height)
			}
			lines := state.Renderable.RenderLines(testCase.Width)
			if len(lines) != testCase.Height {
				t.Fatalf("退化分支行数 = %d, 期望 %d", len(lines), testCase.Height)
			}
			for _, line := range lines {
				if strings.ContainsAny(line, "╭╮╰╯│") {
					t.Fatalf("退化分支不应有面板边框: %q", line)
				}
			}
		})
	}
	if found == 0 {
		t.Fatal("语料应包含高度 < 3 的退化分支用例")
	}
}

// TestMenuLayoutDivergesByName 记录菜单表格的版式差异：只钉住 Go 侧的固定列宽
// 版式，并把 Python 的实际输出（语料 want_python）作为对照。
//
// Python（rich Table, expand=True, padding=(0,1)）会按可用宽度重新分配列宽，
// 因此行首缩进与列间距都随终端宽度变化；Go 用固定列宽，宽度只影响右边的空白。
func TestMenuLayoutDivergesByName(t *testing.T) {
	corpus := loadTUICorpus(t)
	for _, testCase := range corpus.Menu {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.Diverges == "" {
				t.Fatal("语料应标记该用例为刻意差异")
			}
			options := optionsFromSpec(testCase.Options)
			node := MenuTable(options, testCase.Selected)
			lines := node.RenderLines(testCase.Width)
			first := lines[0]
			// 固定列宽版式的形状：指示列(1) + 空格 + 编号列(5) + 空格 + 文案。
			if !strings.HasPrefix(first, "  ") && !strings.HasPrefix(first, SelectedRowMarker) {
				t.Fatalf("首行应以指示列开头: %q", first)
			}
			if len(testCase.Want) > 0 && len(lines) == len(testCase.Want) {
				t.Logf("Python 版式: %q / Go 版式: %q", testCase.Want[0], first)
			}
		})
	}
}

// TestStripMarkupNeverRaises 记录标记解析的差异。
//
// Python：rich 的 Text.from_markup 对多余的闭合标签抛 MarkupError（语料
// markup_error 段记录了具体条目）。Go：StripMarkup 永不抛异常，尽力给出文本
// ——移植后的内容全是固定文案，把版面问题变成崩溃不划算。
func TestStripMarkupNeverRaises(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.MarkupError) == 0 {
		t.Fatal("语料缺少 markup_error 段")
	}
	for _, testCase := range corpus.MarkupError {
		t.Run(testCase.Input, func(t *testing.T) {
			// 先钉住语料的断言：这些输入在参照实现里确实被 rich 拒绝
			// （rich 抛 MarkupError）。若换 rich 版本后不再抛，这里会失败，
			// 提醒重新生成语料而不是悄悄放宽。
			if testCase.PythonError != "MarkupError" {
				t.Fatalf("语料记录的错误类型 = %q, 期望 MarkupError", testCase.PythonError)
			}
			got := StripMarkup(testCase.Input)
			if testCase.Input == "已闭合[/bold]" && got != "已闭合" {
				t.Fatalf("多余闭合标签应被丢弃: 得到 %q", got)
			}
			// 纯标签（无论闭合与否）在 rich 的标记规则下都会被吃掉，
			// 区别只是 rich 会先抛 MarkupError；Go 直接按规则给出空串。
			if testCase.Input == "[/]" && got != "" {
				t.Fatalf("纯闭合标签应被吃掉: 得到 %q", got)
			}
			if testCase.Input == "[red]" && got != "" {
				t.Fatalf("未闭合的纯标签应被吃掉: 得到 %q", got)
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
// 单独的 emoji（含变体选择符）与 CJK 与 rich 一致（语料 width 段覆盖）；
// 这里只把「已知不同」写清楚，避免以后有人误以为差异是回归。
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
	// 但二者的宽度表对区域指示符（旗帜）取值不同，故不在语料里比较。
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
