// Package clipboard 移植 auto_model_key_router/clipboard.py：跨平台剪贴板读写。
//
// Python 版直接读模块级全局（platform.system / shutil.which / os.environ /
// sys.stdout / subprocess.run），因此在 Linux 上无法覆盖 Windows/macOS 分支。
// Go 侧把这些全局收进一个 Env 结构体**注入**进来：环境是唯一的外部输入，
// 语料就能在任何平台上把三个系统分支、命令回退顺序、失败文案全部跑一遍
// （见 testdata/clipboard_corpus.json）。真实调用方用 Native() 拿到接系统的 Env。
package clipboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// remoteTerminalEnvVars 与 clipboard.py:11 一致：任一变量非空即认为处在远程终端。
var remoteTerminalEnvVars = []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"}

// commandTimeout 对应 Python 里写死的 timeout=5 秒。
const commandTimeout = 5 * time.Second

// CommandResult 是一次外部命令的执行结果。
//
// 对应 Python 的 subprocess.CompletedProcess：Code 是退出码，Err 对应
// subprocess.run 抛出的异常（OSError / SubprocessError）。退出码非 0 **不算**
// 错误——Python 用 check=False，把非零退出码当作「命令失败但可继续尝试下一条」。
type CommandResult struct {
	Stdout string
	Stderr string
	Code   int
	Err    error
}

// Runner 执行一条命令并把 stdin 交给它，对应 subprocess.run(command, input=...)。
//
// stdin 为空串表示「没有要写入的输入」。Python 在读取剪贴板时不重定向 stdin
// （子进程继承父进程的 stdin）；剪贴板读取命令都不读 stdin，故不区分两者。
type Runner func(command []string, stdin string) CommandResult

// Env 是剪贴板操作依赖的全部外部环境。
//
// 四个函数字段都必须是有效的非 nil 值（Native() 提供接系统的实现）；
// 只有 Stdout 允许为 nil，用来表达 Python 里 sys.stdout is None 的情形。
type Env struct {
	// System 对应 platform.system()：只区分 "Windows"、"Darwin" 与「其他」。
	System string
	// Which 对应 shutil.which：返回可执行文件路径与是否找到。
	Which func(name string) (string, bool)
	// LookupEnv 对应 os.environ.get：第二个返回值表示变量是否存在。
	LookupEnv func(key string) (string, bool)
	// Stdout 对应 sys.stdout：nil 表示不可用。
	Stdout io.Writer
	// Run 对应 subprocess.run。
	Run Runner
}

// ClipboardCommands 返回按优先级排列的「写入剪贴板」命令（clipboard.py:14）。
//
// Windows 固定用 clip；macOS 只在找到 pbcopy 时给出命令；其余系统按
// wl-copy → xclip → xsel 的顺序收集**所有**可用的命令，由调用方逐个尝试。
func ClipboardCommands(env Env) [][]string {
	switch env.System {
	case "Windows":
		return [][]string{{"clip"}}
	case "Darwin":
		if _, found := env.Which("pbcopy"); found {
			return [][]string{{"pbcopy"}}
		}
		return nil
	}
	var commands [][]string
	if _, found := env.Which("wl-copy"); found {
		commands = append(commands, []string{"wl-copy"})
	}
	if _, found := env.Which("xclip"); found {
		commands = append(commands, []string{"xclip", "-selection", "clipboard"})
	}
	if _, found := env.Which("xsel"); found {
		commands = append(commands, []string{"xsel", "--clipboard", "--input"})
	}
	return commands
}

// PasteCommands 返回按优先级排列的「读取剪贴板」命令（clipboard.py:30）。
//
// Windows 会为**每个**能找到的 PowerShell 可执行文件各生成一条命令
// （clipboard.py:34 的循环），一个都找不到时仍返回默认的 powershell 命令：
// Python 刻意让后续执行去报错，把真实错误回给用户，而不是直接说「没有可用命令」。
func PasteCommands(env Env) [][]string {
	switch env.System {
	case "Windows":
		var commands [][]string
		for _, executable := range []string{"powershell", "powershell.exe", "pwsh"} {
			if _, found := env.Which(executable); found {
				commands = append(commands, []string{
					executable, "-NoProfile", "-Command", "Get-Clipboard -Raw",
				})
			}
		}
		if len(commands) == 0 {
			return [][]string{
				{"powershell", "-NoProfile", "-Command", "Get-Clipboard -Raw"},
			}
		}
		return commands
	case "Darwin":
		if _, found := env.Which("pbpaste"); found {
			return [][]string{{"pbpaste"}}
		}
		return nil
	}
	var commands [][]string
	if _, found := env.Which("wl-paste"); found {
		commands = append(commands, []string{"wl-paste", "--no-newline"})
	}
	if _, found := env.Which("xclip"); found {
		commands = append(commands, []string{"xclip", "-selection", "clipboard", "-o"})
	}
	if _, found := env.Which("xsel"); found {
		commands = append(commands, []string{"xsel", "--clipboard", "--output"})
	}
	return commands
}

// IsRemoteTerminal 判断是否处在 SSH 会话中（clipboard.py:50）。
//
// 判据是「变量存在且非空」：Python 用 os.environ.get(name) 的真值，
// 因此 SSH_TTY="" 不算远程会话。
func IsRemoteTerminal(env Env) bool {
	for _, name := range remoteTerminalEnvVars {
		if value, found := env.LookupEnv(name); found && value != "" {
			return true
		}
	}
	return false
}

// CopyToTerminalClipboard 通过 OSC 52 转义序列把文本交给终端剪贴板
// （clipboard.py:54），成功返回 true。
//
// stdout 为 nil（Python 里 sys.stdout is None）或写入失败都返回 false，
// 让调用方回退到外部命令。base64 编的是 UTF-8 字节，与 Python 一致。
//
// 注意：Python 写完会 flush()。Go 的 os.Stdout 本身无缓冲，但若传入
// bufio.Writer 之类的缓冲写入器，需要调用方自行 Flush。
func CopyToTerminalClipboard(stdout io.Writer, text string) bool {
	if stdout == nil {
		return false
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	if _, err := io.WriteString(stdout, "\033]52;c;"+encoded+"\a"); err != nil {
		return false
	}
	return true
}

// CopyToClipboard 依次尝试可用手段把文本写入剪贴板（clipboard.py:66）。
//
// 顺序与 Python 一致：空文本直接拒绝 → 远程终端优先用 OSC 52 → 逐个尝试
// ClipboardCommands。每条命令的失败原因都会累积，最终只把**最后一条**报给
// 用户（Python 取 errors[-1]），因为最后一条通常最接近真实原因。
func CopyToClipboard(env Env, text string) (bool, string) {
	if text == "" {
		return false, "没有可复制的内容。"
	}
	if IsRemoteTerminal(env) && CopyToTerminalClipboard(env.Stdout, text) {
		return true, "已发送复制请求到终端剪贴板。"
	}
	commands := ClipboardCommands(env)
	if len(commands) == 0 {
		return false, "当前系统未找到可用的剪贴板命令。"
	}
	var failures []string
	for _, command := range commands {
		result := env.Run(command, text)
		if result.Err != nil {
			failures = append(failures, result.Err.Error())
			continue
		}
		if result.Code == 0 {
			return true, "已复制到剪贴板。"
		}
		failures = append(failures, commandFailureDetail(result))
	}
	return false, "复制失败: " + lastFailure(failures)
}

// PasteFromClipboard 依次尝试可用手段读取剪贴板（clipboard.py:88）。
//
// 成功时返回**原样**的 stdout（含末尾换行，Python 不 strip），只在判断
// 「剪贴板是否为空」时才 strip：全是空白同样算没有内容。
func PasteFromClipboard(env Env) (bool, string) {
	commands := PasteCommands(env)
	if len(commands) == 0 {
		return false, "当前系统未找到可用的剪贴板读取命令。"
	}
	var failures []string
	for _, command := range commands {
		result := env.Run(command, "")
		if result.Err != nil {
			failures = append(failures, result.Err.Error())
			continue
		}
		if result.Code == 0 {
			if strings.TrimSpace(result.Stdout) == "" {
				return false, "剪贴板没有可粘贴内容。"
			}
			return true, result.Stdout
		}
		failures = append(failures, commandFailureDetail(result))
	}
	return false, "读取剪贴板失败: " + lastFailure(failures)
}

// commandFailureDetail 复刻 `(stderr or stdout or f"退出码 {code}").strip()`
// （clipboard.py:83）。
//
// 两步不能合并：先用**原始**字符串判空（因此 stderr 全是空格时也算「有内容」，
// 不会被 stdout 顶替），再 strip。全空白与全空串都会得到空字符串。
func commandFailureDetail(result CommandResult) string {
	raw := result.Stderr
	if raw == "" {
		raw = result.Stdout
	}
	if raw == "" {
		raw = fmt.Sprintf("退出码 %d", result.Code)
	}
	return strings.TrimSpace(raw)
}

// lastFailure 取最后一条失败原因；没有任何原因时退化成 "未知错误"
// （Python 的 `errors[-1] if errors else "未知错误"`）。
func lastFailure(failures []string) string {
	if len(failures) == 0 {
		return "未知错误"
	}
	return failures[len(failures)-1]
}

// Native 返回接真实系统环境的 Env，供生产代码直接使用。
//
// Python 侧这些全局是隐式的，Go 侧显式提供一处接线，其余逻辑都走注入的 Env。
func Native() Env {
	return Env{
		System: nativeSystemName(),
		Which: func(name string) (string, bool) {
			path, err := exec.LookPath(name)
			return path, err == nil
		},
		LookupEnv: os.LookupEnv,
		Stdout:    os.Stdout,
		Run:       runNative,
	}
}

// nativeSystemName 近似 platform.system()。
//
// 本包只区分 "Windows" / "Darwin" / 其他，因此除了这两者以外，把 runtime.GOOS
// 首字母大写即可（FreeBSD 之类会得到 "Freebsd"，不影响分支，故不额外维护映射表）。
func nativeSystemName() string {
	if runtime.GOOS == "" {
		return ""
	}
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}

// runNative 用与 subprocess.run 等价的参数执行命令：stdin 喂入 stdin 字符串、
// 分别捕获 stdout/stderr、5 秒超时、非零退出码不算错误。
//
// 错误文案与 Python 不逐字对齐：Python 抛 OSError 时带的是平台错误文本
// （Windows 上是 WinError 文案），无法在 Go 里复现；语料里的失败分支全部
// 由注入的 Runner 提供两侧一致的合成消息，因此不受影响。
func runNative(command []string, stdin string) CommandResult {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr

	err := process.Run()
	result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case err == nil:
		return result
	case ctx.Err() != nil:
		result.Err = fmt.Errorf("命令 %v 超过 %s 未结束", command, commandTimeout)
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.Code = exitErr.ExitCode()
		return result
	}
	result.Err = err
	return result
}
