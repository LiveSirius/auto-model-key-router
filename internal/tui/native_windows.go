//go:build windows

package tui

// Windows 原生按键/控制台支持。
//
// 对应 tui.py:312-330（msvcrt.getwch / msvcrt.kbhit）与 tui.py:367-390
// （GetConsoleMode/SetConsoleMode）。这里用 syscall.NewLazyDLL 直接调
// msvcrt.dll 与 kernel32.dll，避免为两个调用引入额外依赖。

import (
	"syscall"
	"unsafe"
)

var (
	msvcrtDLL          = syscall.NewLazyDLL("msvcrt.dll")
	procGetwch         = msvcrtDLL.NewProc("_getwch")
	procKbhit          = msvcrtDLL.NewProc("_kbhit")
	kernel32DLL        = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandle   = kernel32DLL.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32DLL.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32DLL.NewProc("SetConsoleMode")
)

// stdInputHandle 对应 GetStdHandle(STD_INPUT_HANDLE)。
const stdInputHandle = ^uintptr(9) // -10

// windowsRunReader 是 Windows 上的宽字符输入源。
type windowsRunReader struct{}

// Getwch 调 msvcrt._getwch；返回 0xFFFF（WEOF）表示读取失败。
func (windowsRunReader) Getwch() (rune, bool) {
	value, _, _ := procGetwch.Call()
	char := uint16(value)
	if char == 0xFFFF {
		return 0, false
	}
	return rune(char), true
}

// Kbhit 调 msvcrt._kbhit。
func (windowsRunReader) Kbhit() bool {
	value, _, _ := procKbhit.Call()
	return value != 0
}

// readKeyFromNative 用 Windows 控制台读一次按键（tui.py:460-518）。
func readKeyFromNative() string {
	return ReadKeyFromRunReader(windowsRunReader{}, realClock{})
}

// keyPressedNative 非阻塞探测按键（tui.py:432-437）。
func keyPressedNative() (string, bool) {
	reader := windowsRunReader{}
	if !reader.Kbhit() {
		return "", false
	}
	return ReadKeyFromRunReader(reader, realClock{}), true
}

// EnableWindowsVirtualTerminalInput 打开虚拟终端输入并返回恢复函数
// （tui.py:367-390）。
//
// 失败时返回 no-op，与 Python 的 lambda: None 一致。
func EnableWindowsVirtualTerminalInput() func() {
	handle, _, _ := procGetStdHandle.Call(stdInputHandle)
	if handle == 0 || handle == ^uintptr(0) {
		return func() {}
	}
	var mode uint32
	if ok, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode))); ok == 0 {
		return func() {}
	}
	original := mode
	if ok, _, _ := procSetConsoleMode.Call(handle, uintptr(mode|0x0200)); ok == 0 {
		return func() {}
	}
	return func() {
		_, _, _ = procSetConsoleMode.Call(handle, uintptr(original))
	}
}

// setCBreak / setRawMode / setBlockingInput 在 Windows 上是 no-op：
// PosixInputMode 会提前返回，这些符号只是为了让跨平台共用代码能编译
// （对应 Python 里 win32 直接 yield）。
func setCBreak() func()  { return nil }
func setRawMode() func() { return nil }
func setBlockingInput()  {}
