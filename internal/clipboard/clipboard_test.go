package clipboard

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

// stubEnv 构造一个「什么都不支持」的最小环境，供具名测试按需覆盖字段。
func stubEnv() Env {
	return Env{
		System:    "Linux",
		Which:     func(string) (string, bool) { return "", false },
		LookupEnv: func(string) (string, bool) { return "", false },
		Stdout:    nil,
		Run: func([]string, string) CommandResult {
			return CommandResult{Err: errors.New("不该执行命令")}
		},
	}
}

// TestCommandTimeoutMatchesPython 锁定 Python 里写死的 timeout=5（clipboard.py:77）。
func TestCommandTimeoutMatchesPython(t *testing.T) {
	if commandTimeout != 5*time.Second {
		t.Fatalf("命令超时 = %s，期望 5s", commandTimeout)
	}
}

// TestCopyToTerminalClipboardWritesOSC52 锁定 OSC 52 序列的字节形态。
//
// 序列是 ESC ] 52 ; c ; <base64(UTF-8)> BEL，终端据此把内容放进剪贴板；
// 编码用 UTF-8 而不是 UTF-16，与 Python 的 text.encode("utf-8") 一致。
func TestCopyToTerminalClipboardWritesOSC52(t *testing.T) {
	var buffer bytes.Buffer
	if !CopyToTerminalClipboard(&buffer, "hello") {
		t.Fatal("写入成功的流应返回 true")
	}
	want := "\033]52;c;aGVsbG8=\a"
	if buffer.String() != want {
		t.Fatalf("序列 = %q，期望 %q", buffer.String(), want)
	}

	// 空文本同样返回 true，只是 base64 为空。
	buffer.Reset()
	if !CopyToTerminalClipboard(&buffer, "") {
		t.Fatal("空文本也应返回 true")
	}
	if buffer.String() != "\033]52;c;\a" {
		t.Fatalf("空文本序列 = %q", buffer.String())
	}

	// 多字节按 UTF-8 编码。
	buffer.Reset()
	CopyToTerminalClipboard(&buffer, "中")
	if buffer.String() != "\033]52;c;5Lit\a" {
		t.Fatalf("中文序列 = %q", buffer.String())
	}
}

// TestCopyToTerminalClipboardWithoutStdout 锁定 sys.stdout is None 与写入失败
// 都返回 false（clipboard.py:55），让调用方回退到外部命令。
func TestCopyToTerminalClipboardWithoutStdout(t *testing.T) {
	if CopyToTerminalClipboard(nil, "hello") {
		t.Fatal("stdout 为 nil 时应返回 false")
	}
	if CopyToTerminalClipboard(failingWriter{err: errors.New("boom")}, "hello") {
		t.Fatal("写入失败时应返回 false")
	}
}

// TestRequestedOSC52DoesNotRunCommands 锁定「远程终端优先用 OSC 52」：
// 成功时不应该执行任何外部命令。
func TestRequestedOSC52DoesNotRunCommands(t *testing.T) {
	env := stubEnv()
	env.System = "Linux"
	env.Which = func(name string) (string, bool) { return "/usr/bin/" + name, true }
	env.LookupEnv = func(key string) (string, bool) {
		return "10.0.0.1 1 10.0.0.2 22", key == "SSH_CONNECTION"
	}
	var buffer bytes.Buffer
	env.Stdout = &buffer
	called := false
	env.Run = func([]string, string) CommandResult {
		called = true
		return CommandResult{Code: 0}
	}

	ok, detail := CopyToClipboard(env, "hello")
	if !ok || detail != "已发送复制请求到终端剪贴板。" {
		t.Fatalf("CopyToClipboard = (%v, %q)", ok, detail)
	}
	if called {
		t.Fatal("OSC 52 成功时不应执行外部命令")
	}
	if buffer.String() != "\033]52;c;aGVsbG8=\a" {
		t.Fatalf("stdout = %q", buffer.String())
	}
}

// TestIsRemoteTerminalTreatsEmptyValueAsAbsent 锁定「变量存在但为空不算远程」
// （clipboard.py:51 用 os.environ.get 的真值）。
func TestIsRemoteTerminalTreatsEmptyValueAsAbsent(t *testing.T) {
	env := stubEnv()
	env.LookupEnv = func(key string) (string, bool) { return "", true }
	if IsRemoteTerminal(env) {
		t.Fatal("空串不应算作远程终端")
	}
	env.LookupEnv = func(key string) (string, bool) {
		return "/dev/pts/0", key == "SSH_TTY"
	}
	if !IsRemoteTerminal(env) {
		t.Fatal("SSH_TTY 非空应算作远程终端")
	}
	// 名字不在名单里的变量不影响判定。
	env.LookupEnv = func(key string) (string, bool) { return "/tmp/agent", key == "SSH_AUTH_SOCK" }
	if IsRemoteTerminal(env) {
		t.Fatal("SSH_AUTH_SOCK 不在名单里，不应算作远程终端")
	}
}

// TestCopyToClipboardEmptyTextSkipsEverything 锁定空文本的短路（clipboard.py:67）。
//
// 空文本既不查环境也不执行命令，直接返回提示。
func TestCopyToClipboardEmptyTextSkipsEverything(t *testing.T) {
	env := stubEnv()
	env.Which = func(string) (string, bool) {
		t.Fatal("空文本不应查询可执行文件")
		return "", false
	}
	env.Run = func([]string, string) CommandResult {
		t.Fatal("空文本不应执行命令")
		return CommandResult{}
	}
	ok, detail := CopyToClipboard(env, "")
	if ok || detail != "没有可复制的内容。" {
		t.Fatalf("CopyToClipboard(\"\") = (%v, %q)", ok, detail)
	}
}

// TestCopyToClipboardReportsLastFailure 锁定「只报最后一条失败原因」
// （clipboard.py:84 取 errors[-1]）。
func TestCopyToClipboardReportsLastFailure(t *testing.T) {
	env := stubEnv()
	env.Which = func(name string) (string, bool) { return "/usr/bin/" + name, true }
	step := 0
	env.Run = func([]string, string) CommandResult {
		step++
		if step == 1 {
			return CommandResult{Err: errors.New("第一条错误")}
		}
		if step == 2 {
			return CommandResult{Code: 1, Stderr: "第二条错误"}
		}
		return CommandResult{Code: 2, Stdout: "第三条错误"}
	}
	ok, detail := CopyToClipboard(env, "hello")
	if ok || detail != "复制失败: 第三条错误" {
		t.Fatalf("CopyToClipboard = (%v, %q)", ok, detail)
	}
	if step != 3 {
		t.Fatalf("执行了 %d 条命令，期望 3 条（wl-copy/xclip/xsel 全试一遍）", step)
	}
}

// TestCommandFailureDetailKeepsPythonPrecedence 锁定失败原因的取值顺序
// （clipboard.py:83 的 `stderr or stdout or f"退出码 {code}"`）。
func TestCommandFailureDetailKeepsPythonPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		result CommandResult
		want   string
	}{
		{"stderr 优先", CommandResult{Stderr: "e", Stdout: "o", Code: 1}, "e"},
		{"stderr 为空用 stdout", CommandResult{Stdout: "o", Code: 1}, "o"},
		{"都为空用退出码", CommandResult{Code: 7}, "退出码 7"},
		{"两端空白被 strip", CommandResult{Stderr: "\n  boom  \n"}, "boom"},
		// stderr 全是空白时仍然「有内容」，不会被 stdout 顶替；strip 后是空串。
		{"空白 stderr 不退回 stdout", CommandResult{Stderr: "  ", Stdout: "o", Code: 1}, ""},
	}
	for _, testCase := range cases {
		if got := commandFailureDetail(testCase.result); got != testCase.want {
			t.Errorf("%s: commandFailureDetail = %q，期望 %q", testCase.name, got, testCase.want)
		}
	}
}

// TestPasteCommandsWindowsKeepsFallback 锁定两处 Windows 细节（clipboard.py:32）：
//
//   - 每个能找到的 PowerShell 可执行文件各生成一条命令（不是只取第一个）；
//   - 一个都没有时仍返回默认的 powershell 命令，让执行阶段报出真实错误。
func TestPasteCommandsWindowsKeepsFallback(t *testing.T) {
	env := stubEnv()
	env.System = "Windows"
	env.Which = func(name string) (string, bool) {
		return "/x/" + name, name == "powershell" || name == "pwsh"
	}
	got := PasteCommands(env)
	want := [][]string{
		{"powershell", "-NoProfile", "-Command", "Get-Clipboard -Raw"},
		{"pwsh", "-NoProfile", "-Command", "Get-Clipboard -Raw"},
	}
	if !equalCommands(got, want) {
		t.Fatalf("命令 = %v，期望 %v", got, want)
	}

	env.Which = func(string) (string, bool) { return "", false }
	got = PasteCommands(env)
	want = [][]string{{"powershell", "-NoProfile", "-Command", "Get-Clipboard -Raw"}}
	if !equalCommands(got, want) {
		t.Fatalf("缺少 PowerShell 时的命令 = %v，期望 %v", got, want)
	}
}

// TestPasteFromClipboardKeepsRawStdout 锁定成功时返回**原样** stdout
// （clipboard.py:103 不 strip），只在判空时 strip。
func TestPasteFromClipboardKeepsRawStdout(t *testing.T) {
	env := stubEnv()
	env.Which = func(string) (string, bool) { return "/usr/bin/pbpaste", true }
	env.System = "Darwin"
	env.Run = func([]string, string) CommandResult {
		return CommandResult{Code: 0, Stdout: "  hello\n"}
	}
	ok, detail := PasteFromClipboard(env)
	if !ok || detail != "  hello\n" {
		t.Fatalf("PasteFromClipboard = (%v, %q)", ok, detail)
	}

	env.Run = func([]string, string) CommandResult {
		return CommandResult{Code: 0, Stdout: "\n\t "}
	}
	ok, detail = PasteFromClipboard(env)
	if ok || detail != "剪贴板没有可粘贴内容。" {
		t.Fatalf("全空白 stdout = (%v, %q)", ok, detail)
	}
}

// TestNativeEnvWiresHostPlatform 锁定 Native() 的接线：系统名由 runtime.GOOS 得来，
// which/lookup/env/runner 都指向真实实现。
//
// 这是 Go 侧相对 Python 新增的一处 API（Python 直接用模块级全局），
// 因此单独立一个测试盯住它，避免「注入版」与「真实版」接错。
func TestNativeEnvWiresHostPlatform(t *testing.T) {
	env := Native()
	if env.System == "" {
		t.Fatal("Native().System 不应为空")
	}
	// 只有 Windows / Darwin 影响分支，其余平台名不进分支，故只校验这两者。
	switch runtime.GOOS {
	case "windows":
		if env.System != "Windows" {
			t.Fatalf("Windows 上 System = %q", env.System)
		}
	case "darwin":
		if env.System != "Darwin" {
			t.Fatalf("macOS 上 System = %q", env.System)
		}
	}
	if env.Which == nil || env.LookupEnv == nil || env.Run == nil {
		t.Fatal("Native() 的函数字段不能为空")
	}
	if env.Stdout == nil {
		t.Fatal("Native().Stdout 应为 os.Stdout")
	}
}

// TestNativeRunReportsMissingExecutable 覆盖 runNative 的「找不到可执行文件」
// 分支：对应 Python 的 OSError（clipboard.py:78），必须作为错误上报而不是
// 退出码。
func TestNativeRunReportsMissingExecutable(t *testing.T) {
	result := runNative([]string{"amkr-definitely-missing-binary-xyz"}, "")
	if result.Err == nil {
		t.Fatalf("缺失的可执行文件应报错，实际 code=%d", result.Code)
	}
	if result.Code != 0 {
		t.Fatalf("缺失的可执行文件不该有退出码，实际 %d", result.Code)
	}
}

// TestFailingWriterSatisfiesIOWriter 保证测试替身与 io.Writer 契约一致。
func TestFailingWriterSatisfiesIOWriter(t *testing.T) {
	var writer io.Writer = failingWriter{err: errors.New("boom")}
	if _, err := writer.Write([]byte("x")); err == nil {
		t.Fatal("failingWriter 必须返回错误")
	}
}
