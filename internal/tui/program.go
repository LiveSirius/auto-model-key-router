package tui

// 本文件是交互模型与外部世界的两处接线：启动 Bubble Tea 程序、复制到剪贴板。
//
// 单独成文件是为了让 models.go 保持「纯状态机」，行为测试可以完全绕开
// runProgram（它需要真终端）。

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sparrived/auto-model-key-router/internal/clipboard"
)

// runProgram 启动 Bubble Tea 程序，等价于 Python 的
// `Live(..., screen=True)` 交互循环（tui.py:631、tui.py:696）。
//
// 用 AltScreen 是因为 Python 侧 Live 传了 screen=True。
func runProgram(model tea.Model) (tea.Model, error) {
	program := tea.NewProgram(model, tea.WithAltScreen())
	return program.Run()
}

// copyToClipboard 走 internal/clipboard 的注入式实现，等价于
// `from .clipboard import copy_to_clipboard`（tui.py:30、tui.py:893）。
func copyToClipboard(text string) (bool, string) {
	return clipboard.CopyToClipboard(clipboard.Native(), text)
}
