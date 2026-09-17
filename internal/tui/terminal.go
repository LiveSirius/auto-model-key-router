package tui

// 本文件移植 tui.py:367-451、856-912 的终端模式控制与少量系统集成：
// 鼠标模式、POSIX 输入模式、按键轮询、清屏、用系统程序打开配置文件。

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// MouseWheelMode 打开鼠标滚轮上报，返回恢复函数（tui.py:393-409 的
// contextmanager）。用法：
//
//	restore := tui.MouseWheelMode(true)
//	defer restore()
//
// 与 Python 一致：非 Windows 或 stdout 不可用时什么都不做。Go 侧没有
// 「sys.stdout is None」这种状态，因此只保留平台门控。
func MouseWheelMode(enabled bool) func() {
	if !enabled || !isWindows() {
		return func() {}
	}
	restoreInputMode := EnableWindowsVirtualTerminalInput()
	Console.Write(MouseModeEnable)
	return func() {
		Console.Write(MouseModeDisable)
		restoreInputMode()
	}
}

// PosixInputMode 把终端切到 cbreak 模式，返回恢复函数（tui.py:415-429）。
//
// 与 Python 一致：Windows 上是 no-op；重复进入时不重复切换（模块级标志），
// 因此嵌套调用安全。
func PosixInputMode() func() {
	if isWindows() || posixInputModeActive {
		return func() {}
	}
	restore := setCBreak()
	if restore == nil {
		return func() {}
	}
	posixInputModeActive = true
	return func() {
		posixInputModeActive = false
		restore()
		// 按键轮询会把 fd 0 设成非阻塞（见 native_posix.go），退出时还原，
		// 免得影响之后接管终端的 Bubble Tea 事件循环。
		setBlockingInput()
	}
}

// KeyPressed 非阻塞探测一次按键（tui.py:432-437），返回 (按键名, 是否有输入)。
func KeyPressed() (string, bool) {
	return keyPressedNative()
}

// ReadKeyResponsive 等待一次按键，期间按 onResize 回调处理终端尺寸变化
// （tui.py:440-451）。
//
// 差异说明：Python 每轮重新查询终端尺寸；Go 侧读取 Console.Size()，尺寸变化
// 由 Bubble Tea 的 tea.WindowSizeMsg（或调用方 SetSize）驱动，因此这里比较的是
// Console 里记录的尺寸。
func ReadKeyResponsive(onResize func()) string {
	width, height := Console.Size()
	for {
		if key, ok := KeyPressed(); ok {
			return key
		}
		currentWidth, currentHeight := Console.Size()
		if currentWidth != width || currentHeight != height {
			width, height = currentWidth, currentHeight
			if onResize != nil {
				onResize()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ClearTerminalHistory 清屏并清掉回滚缓冲（tui.py:856-864）。
//
// Python 先写 ANSI 序列（`\033[2J\033[3J\033[H`）再调 console.clear()。Go 侧的
// Console 没有需要清空的内部缓冲，所以只写一次序列——重复写两次是同一个效果，
// 但会让「写到 stdout 的字节」与参照不一致（语料之外的具名测试
// TestClearTerminalHistoryWritesSequence 钉住了这里只写一次）。
func ClearTerminalHistory() {
	Console.Write("\033[2J\033[3J\033[H")
}

// OpenConfigFile 用系统默认程序打开配置文件（tui.py:899-912）。
//
// 返回给用户看的中文提示，与 Python 的三种分支文案一致。
func OpenConfigFile(configPath string) string {
	info, err := os.Stat(configPath)
	if err != nil || info.IsDir() {
		return "配置文件不存在: " + configPath
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// os.startfile 的等价物：cmd /c start 会立刻返回。
		command = exec.Command("cmd", "/c", "start", "", configPath)
	case "darwin":
		command = exec.Command("open", "-t", configPath)
	default:
		command = exec.Command("xdg-open", configPath)
	}
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return "无法打开配置文件: " + err.Error()
	}
	return "已使用默认文本编辑器打开: " + configPath
}

// ErrCancelled 对应 Python 的 KeyboardInterrupt：用户在子流程里按了取消键。
//
// Go 里没有异常，RunSubmodule 用这个哨兵错误区分「取消」与「出错」。
var ErrCancelled = errors.New("操作已取消")

// posixInputModeActive 对应 tui.py:412 的模块级标志：嵌套调用时不再切换模式。
var posixInputModeActive bool
