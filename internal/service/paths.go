package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件移植 service.py:324-348 与 524-546：PID 文件、进程存活判定、可执行文件选择。

// PidFilePath 对应 service.py:324 的 pid_file_path：日志文件同目录下的 server.pid。
func PidFilePath(cfg *config.RouterConfig) string {
	return pidFilePathIn(cfg.LogFilePath)
}

// ReadPid 对应 service.py:328 的 read_pid：读不出整数（含文件不存在）即视为没有 PID。
//
// Python 的 `int(text.strip())` 接受正负号与前后的空白，Go 的 strconv.Atoi 在
// TrimSpace 之后与之等价。
func ReadPid(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// writePid 写出 PID 文件（Python 的 `pid_file.write_text(str(pid))`）。
func writePid(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o666)
}

// IsProcessRunning 对应 service.py:335 的 is_process_running。
//
// Windows 走 tasklist 的 CSV 匹配（可用语料逐条对拍），其余平台走 signal 0。
func (e *Env) IsProcessRunning(pid int) bool {
	if e.Running == nil {
		return false
	}
	return e.Running(pid)
}

// windowsRunning 是 Windows 分支的实现：`tasklist /FI "PID eq N" /FO CSV /NH`
// 的输出里含有 `"N"` 或 `,N,` 就算存活（service.py:337-342）。
//
// 这个匹配规则很松（例如 PID 12 会被 `"123"` 误判），但它是参照实现的行为，
// 语料逐条锁定，不做「修正」。
func (e *Env) windowsRunning(pid int) bool {
	result := e.Run([]string{"tasklist", "/FI", "PID eq " + strconv.Itoa(pid), "/FO", "CSV", "/NH"})
	text := result.Stdout
	return strings.Contains(text, `"`+strconv.Itoa(pid)+`"`) ||
		strings.Contains(text, ","+strconv.Itoa(pid)+",")
}

// windowsTerminate 是 Windows 分支的终止实现：taskkill /PID N /T /F
// （service.py:132-135，Python 忽略其返回码）。
func (e *Env) windowsTerminate(pid int) error {
	e.Run([]string{"taskkill", "/PID", strconv.Itoa(pid), "/T", "/F"})
	return nil
}

// BackgroundExecutable 对应 service.py:524 的 background_python_executable。
//
// 刻意差异：参照实现会优先选同目录的 pythonw.exe（避免后台进程弹控制台窗口），
// Go 版没有解释器，直接返回自身可执行文件；「不弹窗」由启动标志
// （detachedProcessFlags 里的 CREATE_NO_WINDOW）承担。
// TestBackgroundExecutableDivergence 具名锁定该差异。
func (e *Env) BackgroundExecutable() string { return e.Executable }

// consoleScriptNames 对应 service.py:534 的 names 集合（注意 Python 的集合无序，
// 这里用切片把「argv0 命中」与「PATH 查找」两段的顺序显式化）。
var consoleScriptNames = []string{"auto-model-key-router", "amkr", "auto-model-key-router.exe", "amkr.exe"}

// ConsoleScriptExecutable 对应 service.py:533 的 console_script_executable。
//
// 语义（两步，顺序与 Python 一致）：
//  1. 若当前可执行文件的名字就在 names 里（即用户直接调用了 console script），
//     用它自己；不是绝对路径或不存在时退回 PATH 查找同名命令。
//  2. 否则依次在 PATH 里找 auto-model-key-router、amkr。
func (e *Env) ConsoleScriptExecutable() (string, bool) {
	base := filepath.Base(e.Executable)
	if containsString(consoleScriptNames, base) {
		if filepath.IsAbs(e.Executable) && fileExists(e.Executable) {
			return e.Executable, true
		}
		if found, err := e.LookPath(base); err == nil {
			return absPath(found), true
		}
	}
	for _, name := range []string{"auto-model-key-router", "amkr"} {
		if found, err := e.LookPath(name); err == nil {
			return absPath(found), true
		}
	}
	return "", false
}

// fileExists 对应 Path.exists()（只关心存在性，不区分目录与文件）。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// containsString 是小工具（Go 1.21 的 slices.Contains 也可，这里避免额外导入）。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// shlexUnsafeChars 对应 CPython shlex 的 `_find_unsafe = re.compile(r'[^\w@%+=:,./-]',
// re.ASCII).search`：命中即需要加引号。
func shlexUnsafe(value string) bool {
	for _, char := range value {
		if isShlexSafe(char) {
			continue
		}
		return true
	}
	return false
}

// isShlexSafe 报告一个字符是否属于「不需要引号」的字符集。
func isShlexSafe(char rune) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		return true
	}
	switch char {
	case '_', '@', '%', '+', '=', ':', ',', '.', '/', '-':
		return true
	}
	return false
}

// ShlexQuote 是 CPython shlex.quote 的移植（shlex.py）。
//
// 空串返回两个单引号；含不安全字符时用单引号包裹，并把内部的单引号写成
// `'"'"'`。systemd unit 的 ExecStart 用 shlex.join 生成，因此这个函数必须与
// Python 逐字一致（语料覆盖 Windows 路径、空格、中文与引号）。
func ShlexQuote(value string) string {
	if value == "" {
		return "''"
	}
	if !shlexUnsafe(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// ShlexJoin 是 shlex.join 的移植：对每个参数做 ShlexQuote 再用空格连接。
func ShlexJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, ShlexQuote(arg))
	}
	return strings.Join(quoted, " ")
}
