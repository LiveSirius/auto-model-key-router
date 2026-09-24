package tui

// 本文件移植 tui.py:275-309 的滚轮/鼠标/滚动换算——都是**纯函数**，
// 因此可以在不碰真终端的前提下直接测试：滚轮去抖由
// TestSelectModelWheelThrottledByInterval 覆盖，鼠标模式开关由
// TestMouseWheelModeNoopWhenDisabled 覆盖。

import (
	"runtime"
	"strconv"
	"strings"
	"time"
)

// isWindows 对应 Python 的 `sys.platform == "win32"` 判断。
//
// tui.py 在多处按平台切换文案与行为（tui.py:244、tui.py:261、tui.py:395 等）。
func isWindows() bool { return runtime.GOOS == "windows" }

// ParseSGRMouseSequence 解析 SGR 鼠标序列，只关心滚轮（tui.py:275-284）。
//
// sequence 形如 "64;10;5M"（前导 `ESC [ <` 已被 read_key 消费）。
// 返回 (按键名, 是否为滚轮)；按键名取 "scroll_up"/"scroll_down"。
// button 的低位为 1 表示向下滚（tui.py:284）。
func ParseSGRMouseSequence(sequence string) (string, bool) {
	if sequence == "" {
		return "", false
	}
	last := sequence[len(sequence)-1]
	if last != 'M' && last != 'm' {
		return "", false
	}
	head := sequence[:len(sequence)-1]
	if index := strings.IndexByte(head, ';'); index >= 0 {
		head = head[:index]
	}
	button, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil {
		return "", false
	}
	if button < 64 {
		return "", false
	}
	if button&1 != 0 {
		return "scroll_down", true
	}
	return "scroll_up", true
}

// ShouldHandleWheel 复刻滚轮去抖（tui.py:287-293）：同一个滚轮方向在
// WHEEL_EVENT_INTERVAL_SECONDS 内只处理一次，返回
// (是否处理, 新的 lastKey, 新的 lastAt)。
//
// 与 Python 的差异：`now` 由调用方给出——Python 内部直接调 time.monotonic()，
// 注入时间才能让单测稳定。真实调用方用 ShouldHandleWheelNow。
func ShouldHandleWheel(key, lastKey string, lastAt, now float64) (bool, string, float64) {
	if !isWheelKey(key) {
		return true, lastKey, lastAt
	}
	if key == lastKey && now-lastAt < WheelEventIntervalSeconds {
		return false, lastKey, lastAt
	}
	return true, key, now
}

// ShouldHandleWheelNow 用当前单调时钟调用 ShouldHandleWheel。
func ShouldHandleWheelNow(key, lastKey string, lastAt float64) (bool, string, float64) {
	return ShouldHandleWheel(key, lastKey, lastAt, MonotonicSeconds())
}

// MonotonicSeconds 返回进程内单调递增的秒数，等价于 time.monotonic()。
func MonotonicSeconds() float64 {
	return float64(time.Since(processStart).Nanoseconds()) / 1e9
}

// processStart 是包初始化时刻，作为单调时钟的起点。
var processStart = time.Now()

// isWheelKey 判断按键名是否为滚轮事件。
func isWheelKey(key string) bool {
	for _, wheelKey := range WheelKeys {
		if key == wheelKey {
			return true
		}
	}
	return false
}
