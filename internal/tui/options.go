package tui

// 本文件移植 tui.py:55-60、584-668、867-896 的对外交互 API。
//
// 与其他模块的关系（决定函数签名）：config_editor.py / dashboard.py 直接调用
// select_option / select_multiple / prompt_text / confirm_choice / show_result_page /
// run_submodule；它们的返回值语义（返回值、取消、复制回执）必须保持不变。
// 交互本体由 models.go 里的 Bubble Tea 模型承担，本文件只负责参数换算与结果收敛。

import (
	"errors"
	"fmt"
	"strings"
)

// ResultPage 对应 tui.py:55-60：既能展示、又能复制的操作结果。
type ResultPage struct {
	// Content 是要展示的内容（Renderable 或字符串等可渲染值）。
	Content any
	// CopyText 非空时提供「复制」选项。
	CopyText string
	// CopyLabel 是复制选项的文案。
	CopyLabel string
}

// CopyResultPage 构造一个带复制内容的 ResultPage。
func CopyResultPage(content any, copyText, copyLabel string) ResultPage {
	if copyLabel == "" {
		copyLabel = "复制 key"
	}
	return ResultPage{Content: content, CopyText: copyText, CopyLabel: copyLabel}
}

// VisiblePromptValue 复刻 prompt_text 输入框的可见值计算（tui.py:608-610）。
//
// 注意 Python 用 len() 与切片，都是**码点**而非显示宽度，Go 侧必须同样按
// rune 处理，否则中文输入的长度截断会与参照不一致（语料 prompt_visible 段覆盖）。
func VisiblePromptValue(value string, password bool, maxVisible int) string {
	if maxVisible < 8 {
		maxVisible = 8
	}
	runes := []rune(value)
	if password {
		return truncateRunes(strings.Repeat("*", len(runes)), maxVisible)
	}
	return truncateRunes(value, maxVisible)
}

// truncateRunes 复刻 `"…" + value[-(max_visible-1):]`。
func truncateRunes(value string, maxVisible int) string {
	runes := []rune(value)
	if len(runes) <= maxVisible {
		return value
	}
	if maxVisible <= 1 {
		return "…"
	}
	return "…" + string(runes[len(runes)-(maxVisible-1):])
}

// SelectOptions 对应 select_option 的可选参数（tui.py:670-677）。
type SelectOptions struct {
	// Selected 是初始选中项下标。
	Selected int
	// Content 是菜单上方的正文（nil 表示没有）。
	Content any
	// OnKey 是按键钩子：返回 (值, true) 表示立即结束并返回该值。
	OnKey KeyHandler
}

// PromptOptions 对应 prompt_text 的关键字参数（tui.py:584-591）。
type PromptOptions struct {
	// Default 是默认值（nil 表示没有默认值）。
	Default *string
	// Password 为真时输入回显成星号。
	Password bool
	// Choices 非空时退化成单选菜单（Python 的同一分支）。
	Choices []string
}

// RunSubmodule 执行一个子流程并兜住错误（tui.py:867-880）。
//
// Python 用异常区分两种结局：KeyboardInterrupt → 黄色「取消」面板；
// 其他异常 → 红色「操作出错」面板并进入结果页。Go 侧用错误值：包装了
// ErrCancelled 的错误算取消，其余算出错。
func RunSubmodule(action func() (any, error)) any {
	ClearTerminalHistory()
	value, err := action()
	if err == nil {
		return value
	}
	if errors.Is(err, ErrCancelled) {
		return SectionPanel("已取消，返回上一级。", "取消", "yellow")
	}
	content := SectionPanel(
		fmt.Sprintf("[red]%s[/red]\n\n[dim]已捕获错误，按返回键回到主页。[/dim]", errorText(err)),
		"操作出错", "red",
	)
	ShowResultPage("操作出错", content)
	return content
}

// errorText 复刻 Python 的 `f"{exc.__class__.__name__}: {exc}"`。
//
// 差异说明：Go 没有异常类名，这里用错误的类型名（如 `*errors.errorString`）+
// 错误文本，信息量与 Python 的「类名: 文本」等价。
func errorText(err error) string {
	return fmt.Sprintf("%T: %v", err, err)
}
