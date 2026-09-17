package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// 本文件只覆盖一个决策函数：进程退出码。
//
// 为什么单独抽出来测：130 这条路径需要真的收到 Ctrl+C，而 Go 在 Windows 上不支持
// `os.Process.Signal(os.Interrupt)`，测试进程里发不出这个信号。把「中断通道被关闭」
// 作为输入，三个退出码就都能钉住（130 / 0 / 1）。

// TestWaitForStopReturns130OnInterrupt 锁定「Ctrl+C → 130」。
func TestWaitForStopReturns130OnInterrupt(t *testing.T) {
	interrupted := make(chan struct{})
	close(interrupted)
	server := &http.Server{Addr: "127.0.0.1:0"}
	var output bytes.Buffer

	code := waitForStop(server, make(chan error, 1), interrupted, &output)

	if code != 130 {
		t.Errorf("退出码 = %d，期望 130", code)
	}
	if !strings.Contains(output.String(), "正在关停") {
		t.Errorf("应输出关停提示，实际 %q", output.String())
	}
}

// TestWaitForStopReturns0OnGracefulStop 锁定「服务器自己停了（ErrServerClosed）→ 0」。
func TestWaitForStopReturns0OnGracefulStop(t *testing.T) {
	failures := make(chan error, 1)
	failures <- http.ErrServerClosed
	server := &http.Server{Addr: "127.0.0.1:0"}
	var output bytes.Buffer

	if code := waitForStop(server, failures, make(chan struct{}), &output); code != 0 {
		t.Errorf("退出码 = %d，期望 0", code)
	}
}

// TestWaitForStopReturns1OnListenFailure 锁定「监听失败 → 1」。
func TestWaitForStopReturns1OnListenFailure(t *testing.T) {
	failures := make(chan error, 1)
	failures <- errors.New("listen tcp 127.0.0.1:8000: bind: address already in use")
	server := &http.Server{Addr: "127.0.0.1:8000"}
	var output bytes.Buffer

	if code := waitForStop(server, failures, make(chan struct{}), &output); code != 1 {
		t.Errorf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(output.String(), "监听") {
		t.Errorf("应输出监听失败提示，实际 %q", output.String())
	}
}

// TestVersionIsSemverLike 锁定版本号形状：它同时进入 /health、/api/tool 与更新检查，
// 空串或非 semver 会让「当前版本」的展示与比较都失去意义。
func TestVersionIsSemverLike(t *testing.T) {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		t.Fatalf("version = %q，期望至少三段（如 4.1.0）", version)
	}
	for _, part := range parts[:3] {
		if part == "" {
			t.Fatalf("version = %q 含有空段", version)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				t.Fatalf("version = %q 的前三段必须是数字", version)
			}
		}
	}
}
