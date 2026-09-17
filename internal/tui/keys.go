package tui

// 本文件移植 tui.py:312-582 的按键读取与转义序列解析。
//
// 设计要点：Python 直接读 msvcrt / stdin 这些全局，无法在别的平台上回归测试。
// Go 侧把「原始输入」抽成两个小接口（RunReader 对应 msvcrt 的宽字符输入，
// ByteReader 对应 POSIX 的 os.read+select），解析逻辑不变。
// 于是 Windows 与 POSIX 两条解析分支都能用同一套脚本输入做对拍
// （见 scripts/gen_tui_corpus.py 的 key 段与 testdata/tui_corpus.json）。

import (
	"strings"
	"time"
)

// 特殊按键名，与 Python read_key 的返回值一一对应（tui.py:467-517）。
const (
	// KeyEnter 对应 \r 或 \n。
	KeyEnter = "enter"
	// KeyUp / KeyDown / KeyLeft / KeyRight 对应方向键。
	KeyUp    = "up"
	KeyDown  = "down"
	KeyLeft  = "left"
	KeyRight = "right"
	// KeyPageUp / KeyPageDown 对应翻页键。
	KeyPageUp   = "page_up"
	KeyPageDown = "page_down"
	// KeyHome / KeyEnd 对应首尾键。
	KeyHome = "home"
	KeyEnd  = "end"
	// KeyCancel 对应 Ctrl+C 与「孤立的 ESC」。
	KeyCancel = "cancel"
	// KeyIgnore 表示无法识别的序列，调用方应忽略。
	KeyIgnore = "ignore"
	// KeyScrollUp / KeyScrollDown 对应鼠标滚轮。
	KeyScrollUp   = "scroll_up"
	KeyScrollDown = "scroll_down"
	// KeyCtrlU / KeyCtrlW 对应行编辑快捷键（prompt_text 用）。
	KeyCtrlU = "\x15"
	KeyCtrlW = "\x17"
	// KeyBackspace 是退格键可能出现的两种字节。
	KeyBackspace = "\b"
	// KeyDelete 是 Delete 键在部分终端上的字节。
	KeyDelete = "\x7f"
)

// RunReader 是宽字符输入源，对应 Python 的 msvcrt 模块用法（tui.py:312-330）。
type RunReader interface {
	// Getwch 读一个宽字符，阻塞；ok=false 表示输入流已结束（Python 会阻塞，
	// 这里用于让脚本化对拍明确终止）。
	Getwch() (rune, bool)
	// Kbhit 报告是否还有待读字符（tui.py:325）。
	Kbhit() bool
}

// ByteReader 是字节输入源，对应 Python 的 os.read(fd, 1) + select（tui.py:333-364）。
type ByteReader interface {
	// NextByte 读一个字节，阻塞；ok=false 表示 EOF。
	NextByte() (byte, bool)
	// Readable 报告在 timeout 内是否有可读字节（对应 select.select）。
	Readable(timeout time.Duration) bool
}

// RunClock 提供 Poll 用的时间源，便于测试注入（Python 用 time.monotonic）。
type RunClock interface {
	// Monotonic 返回单调秒数。
	Monotonic() float64
	// Sleep 挂起一小段时间。
	Sleep(d time.Duration)
}

// realClock 是接真实时间的 RunClock。
type realClock struct{}

func (realClock) Monotonic() float64    { return MonotonicSeconds() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// ReadWindowsUntil 读取直到遇到 endChars 或达到 limit（tui.py:312-319）。
func ReadWindowsUntil(reader RunReader, endChars map[rune]bool, limit int) string {
	var b strings.Builder
	count := 0
	for count < limit {
		char, ok := reader.Getwch()
		if !ok {
			break
		}
		count++
		b.WriteRune(char)
		if endChars[char] {
			break
		}
	}
	return b.String()
}

// ReadWindowsCharIfAvailable 在超时内尝试读一个宽字符（tui.py:322-330）。
//
// Python 先轮询 kbhit（每次 sleep 5ms），超时后再补一次检查；Go 版保持一致，
// 因此脚本化输入下「有字符就立刻返回」的行为完全相同。
func ReadWindowsCharIfAvailable(reader RunReader, clock RunClock, timeout time.Duration) (rune, bool) {
	deadline := clock.Monotonic() + timeout.Seconds()
	for clock.Monotonic() < deadline {
		if reader.Kbhit() {
			return reader.Getwch()
		}
		clock.Sleep(5 * time.Millisecond)
	}
	if reader.Kbhit() {
		return reader.Getwch()
	}
	return 0, false
}

// ReadKeyFromRunReader 是 msvcrt 分支的 read_key（tui.py:460-518）。
func ReadKeyFromRunReader(reader RunReader, clock RunClock) string {
	char, ok := reader.Getwch()
	if !ok {
		return KeyIgnore
	}
	return classifyWindowsKey(reader, clock, char)
}

// classifyWindowsKey 处理首字符之后的解析；单独拆出来是为了让对拍直接喂
// 「首个字符 + 脚本化剩余输入」。
func classifyWindowsKey(reader RunReader, clock RunClock, char rune) string {
	switch char {
	case '\x03':
		return KeyCancel
	case '\r', '\n':
		return KeyEnter
	case 0x00, 0xE0:
		code, ok := reader.Getwch()
		if !ok {
			return KeyIgnore
		}
		switch code {
		case 'H':
			return KeyUp
		case 'P':
			return KeyDown
		case 'K':
			return KeyLeft
		case 'M':
			return KeyRight
		case 'I':
			return KeyPageUp
		case 'Q':
			return KeyPageDown
		case 'G':
			return KeyHome
		case 'O':
			return KeyEnd
		}
		// 未识别的扩展键：Python 不会 return，而是落到函数末尾的 `return key`，
		// 也就是把最初那个 \x00 / \xe0 原样返回（tui.py:471-488、tui.py:518）。
		return string(char)
	case 0x1b:
		second, ok := ReadWindowsCharIfAvailable(reader, clock, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
		if !ok {
			return KeyCancel
		}
		third := rune(0)
		if second == '[' || second == 'O' {
			third, _ = ReadWindowsCharIfAvailable(reader, clock, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
		}
		if third == '<' && second == '[' {
			sequence := ReadWindowsUntil(reader, map[rune]bool{'M': true, 'm': true}, 64)
			if mouseKey, ok := ParseSGRMouseSequence(sequence); ok {
				return mouseKey
			}
			return KeyIgnore
		}
		switch {
		case third == 'A':
			return KeyUp
		case third == 'B':
			return KeyDown
		case third == 'D':
			return KeyLeft
		case third == 'C':
			return KeyRight
		case third == '5' && second == '[':
			// 注意：Python 把 PageUp 的 `~` 丢掉，不再校验（tui.py:507-509）。
			_, _ = ReadWindowsCharIfAvailable(reader, clock, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
			return KeyPageUp
		case third == '6' && second == '[':
			_, _ = ReadWindowsCharIfAvailable(reader, clock, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
			return KeyPageDown
		case third == 'H':
			return KeyHome
		case third == 'F':
			return KeyEnd
		}
		return KeyCancel
	}
	return string(char)
}

// ReadPosixUntil 读取直到遇到 endChars 或达到 limit（tui.py:333-344）。
func ReadPosixUntil(reader ByteReader, endChars map[byte]bool, limit int) string {
	var b strings.Builder
	count := 0
	for count < limit && reader.Readable(time.Duration(EscSequenceTimeoutSeconds*float64(time.Second))) {
		value, ok := reader.NextByte()
		if !ok {
			break
		}
		count++
		char := decodeSingleByte(value)
		b.WriteString(char)
		if endChars[value] {
			break
		}
	}
	return b.String()
}

// ReadPosixCharIfAvailable 在超时内尝试读一个字节（tui.py:347-354）。
func ReadPosixCharIfAvailable(reader ByteReader, timeout time.Duration) (string, bool) {
	if !reader.Readable(timeout) {
		return "", false
	}
	value, ok := reader.NextByte()
	if !ok {
		return "", false
	}
	return decodeSingleByte(value), true
}

// ReadPosixCharsIfAvailable 连续读取至多 count 个字符（tui.py:357-364）。
func ReadPosixCharsIfAvailable(reader ByteReader, count int) string {
	var b strings.Builder
	for b.Len() < count {
		char, ok := ReadPosixCharIfAvailable(reader, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
		if !ok {
			break
		}
		b.WriteString(char)
	}
	return b.String()
}

// decodeSingleByte 复刻 `ch.decode("utf-8", errors="ignore")`。
//
// 关键：Python 是**逐字节**解码，多字节 UTF-8 的每个字节都独立解码失败被丢弃，
// 因此 CJK 按键在 POSIX 分支下返回空串（tui.py:340）。Go 侧必须同样处理，
// 否则行为对不上（语料里有该用例）。
func decodeSingleByte(value byte) string {
	if value < 0x80 {
		return string(rune(value))
	}
	return ""
}

// ReadKeyFromByteReader 是 POSIX 分支的 read_key（tui.py:521-564）。
func ReadKeyFromByteReader(reader ByteReader) string {
	value, ok := reader.NextByte()
	if !ok {
		return KeyIgnore
	}
	char := decodeSingleByte(value)
	switch char {
	case "\x03":
		return KeyCancel
	case "\r", "\n":
		return KeyEnter
	case "\x1b":
		return classifyPosixEscape(reader)
	}
	return char
}

// classifyPosixEscape 解析 ESC 之后的序列（tui.py:531-563）。
func classifyPosixEscape(reader ByteReader) string {
	second, ok := ReadPosixCharIfAvailable(reader, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
	if !ok {
		return KeyIgnore
	}
	if second != "[" && second != "O" {
		return KeyIgnore
	}
	third, ok := ReadPosixCharIfAvailable(reader, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second)))
	if !ok {
		return KeyIgnore
	}
	switch {
	case third == "<":
		sequence := ReadPosixUntil(reader, map[byte]bool{'M': true, 'm': true}, 64)
		if mouseKey, ok := ParseSGRMouseSequence(sequence); ok {
			return mouseKey
		}
		return KeyIgnore
	case third == "M" && second == "[":
		// X10 鼠标序列：吃掉三个字节后忽略（tui.py:544-546）。
		_ = ReadPosixCharsIfAvailable(reader, 3)
		return KeyIgnore
	case third == "A":
		return KeyUp
	case third == "B":
		return KeyDown
	case third == "D":
		return KeyLeft
	case third == "C":
		return KeyRight
	case third == "5":
		if next, ok := ReadPosixCharIfAvailable(reader, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second))); ok && next == "~" {
			return KeyPageUp
		}
		return KeyIgnore
	case third == "6":
		if next, ok := ReadPosixCharIfAvailable(reader, time.Duration(EscSequenceTimeoutSeconds*float64(time.Second))); ok && next == "~" {
			return KeyPageDown
		}
		return KeyIgnore
	case third == "H":
		return KeyHome
	case third == "F":
		return KeyEnd
	}
	return KeyIgnore
}

// ReadKey 读取一次按键，按平台分派（tui.py:460-462）。
func ReadKey() string {
	return readKeyFromNative()
}
