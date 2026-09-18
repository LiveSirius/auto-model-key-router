package service

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/logfiles"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
)

// Runner 执行一条命令并捕获输出，对应 service.py:313 的 run_status_command 以及
// service.py:631 的 registration_result 里那次 subprocess.run。
//
// 类型直接复用 servicestatus.CommandRunner（同样的
// `Callable[[list[str]], CompletedProcess]` 形状），这样状态采集与动作执行共用同一个
// 注入点：语料里一次替换就能同时驱动两边。
type Runner = servicestatus.CommandRunner

// SpawnSpec 描述一次后台进程启动，对应 service.py:80 的 subprocess.Popen 参数。
//
// 注意 LogPath：Python 把 stdout/stderr 都指向同一个以追加方式打开的日志文件，
// 这里保持同样的语义（由 defaultSpawn 负责打开）。
type SpawnSpec struct {
	// Command 是完整命令行（第一个元素是程序）。
	Command []string
	// Cwd 是子进程工作目录（Python 传 Path.cwd()）。
	Cwd string
	// Env 是完整环境变量列表（Python 传 {**os.environ, "AMKR_LOG_ARCHIVED": "1"}）。
	Env []string
	// LogPath 是 stdout/stderr 追加写入的文件。
	LogPath string
	// Detached 为真表示子进程脱离父进程（Windows 的 DETACHED_PROCESS 组合，
	// POSIX 的 start_new_session=True）。
	Detached bool
	// CloseFiles 对应 Python 的 close_fds=os.name != "nt"。
	CloseFiles bool
}

// SpawnResult 是分离进程启动结果。
type SpawnResult struct {
	// Pid 是子进程号（写进 PID 文件）。
	Pid int
}

// SpawnFunc 启动一个分离进程。
type SpawnFunc func(spec SpawnSpec) (SpawnResult, error)

// HTTPGet 取回一个 URL，返回状态码与响应体；连接失败等一律通过 error 上报。
//
// 对应 service.py:650/665 的 `urlopen(...)`：Python 用异常区分成败，Go 用 error。
// 只有状态码 200 才算健康（`response.status == 200`）。
type HTTPGet func(url string, timeout time.Duration) (status int, body []byte, err error)

// healthCacheEntry 是一次健康检查的缓存记录（service.py:39 的
// `_service_status_cache[(host, port)] = (time.monotonic(), healthy)`）。
type healthCacheEntry struct {
	at      time.Time
	healthy bool
}

// Env 是本包全部 OS 交互的注入点（见包注释「OS 接缝」）。
//
// 除 Runner/Spawn/Get 这类函数字段外，其余字段都是环境事实（平台、主目录、PID）。
// 测试通过构造一个只填必要字段的 Env 来避免任何真实副作用；生产代码用 DefaultEnv()。
type Env struct {
	// GOOS 对应 platform.system().lower() 的归一化结果（"windows" / "linux" / ...）。
	GOOS string
	// Home 对应 Path.home()。
	Home string
	// Cwd 对应 Path.cwd()。
	Cwd string
	// Executable 对应 sys.executable，用于后台启动与系统服务注册的命令构造。
	Executable string
	// User 对应 os.getlogin()（systemd 的 enable-linger 用）。
	User string
	// Pid 对应 os.getpid()。
	Pid int
	// Getenv 对应 os.environ.get。
	Getenv func(string) string
	// LookPath 对应 shutil.which。
	LookPath func(string) (string, error)
	// Run 执行一条捕获输出的命令。
	Run Runner
	// Spawn 启动分离进程。
	Spawn SpawnFunc
	// Running 报告进程是否存活（Windows 由 tasklist 实现，POSIX 由 signal 0 实现）。
	Running func(pid int) bool
	// Terminate 终止进程（Windows taskkill /T /F，POSIX SIGTERM）。
	Terminate func(pid int) error
	// IsAdmin 对应 service.py:461 的 is_windows_admin。
	IsAdmin func() bool
	// Now 是缓存与日志归档用的时钟（Python 用 time.monotonic / datetime.now）。
	Now func() time.Time
	// Sleep 是等待（停止后台服务时轮询 20 次、每次 0.1 秒）。
	Sleep func(time.Duration)
	// ArchiveLog 对应 log_files.archive_current_log（已移植在 internal/logfiles）。
	ArchiveLog func(logFilePath string, now time.Time) (archivePath string, archived bool, err error)
	// Get 是 /health 的 HTTP 取回器。
	Get HTTPGet

	// cacheMu 保护 healthCache（Python 的模块级字典在本包里挂到 Env 上，
	// 这样每个测试实例互不影响）。
	cacheMu     sync.Mutex
	healthCache map[string]healthCacheEntry
}

// DefaultEnv 用真实操作系统事实构造一个 Env：os、os/exec、net/http 都在这里落地。
//
// 唯一不做真实工作的是 Spawn：它由平台文件提供（spawn_windows.go / spawn_posix.go）。
func DefaultEnv() *Env {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	env := &Env{
		GOOS:       runtime.GOOS,
		Home:       home,
		Cwd:        cwd,
		Executable: executable,
		User:       user,
		Pid:        os.Getpid(),
		Getenv:     os.Getenv,
		LookPath:   exec.LookPath,
		Now:        time.Now,
		Sleep:      time.Sleep,
		ArchiveLog: logfiles.ArchiveCurrentLog,
		Get:        defaultHTTPGet,
	}
	env.Run = defaultRunner
	env.Spawn = defaultSpawn()
	// 进程存活/终止与管理员判定是仅有的两处平台差异，交给平台文件提供默认实现
	// （windows: tasklist/taskkill + shell32；其余: signal + 恒假）。
	env.Running, env.Terminate, env.IsAdmin = platformProcessHooks(env)
	return env
}

// errEmptyCommand 表示命令行没有任何元素（Python 侧是不可达状态，Go 侧显式拒绝）。
var errEmptyCommand = errors.New("命令为空")

// defaultRunner 用 os/exec 执行命令，复刻 Python `subprocess.run(capture_output=True, text=True)`。
//
// 关键细节：Python 在命令不存在时抛 FileNotFoundError，而 run_status_command 会把它
// 折成 `CompletedProcess(command, 127, "", str(exc))`（service.py:316-317）——注意
// stdout 为空、stderr 是异常文本，**不**抛给调用方。这里逐字段对齐：Code=127、
// Stderr 为错误文本、Err 留空（异常已被 run_status_command 吞掉）。
func defaultRunner(command []string) servicestatus.CommandResult {
	if len(command) == 0 {
		return servicestatus.CommandResult{Code: 127, Stderr: errEmptyCommand.Error()}
	}
	cmd := exec.Command(command[0], command[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return servicestatus.CommandResult{Stdout: stdout.String(), Code: 0}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return servicestatus.CommandResult{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Code:   exitErr.ExitCode(),
		}
	}
	return servicestatus.CommandResult{Stdout: stdout.String(), Stderr: err.Error(), Code: 127}
}

// backgroundEnv 复刻 `{**os.environ, "AMKR_LOG_ARCHIVED": "1"}`。
//
// Python 覆盖同名变量；Go 的 os.Environ 里可能已有 AMKR_LOG_ARCHIVED，因此先剔除
// 再追加，保证只有一份且值为 1。
func backgroundEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if hasEnvPrefix(entry, "AMKR_LOG_ARCHIVED=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "AMKR_LOG_ARCHIVED=1")
}

// hasEnvPrefix 判断一条 "K=V" 是否以给定前缀开头（大小写敏感，与 Windows 上
// Go 的 os.Environ 输出一致）。
func hasEnvPrefix(entry, prefix string) bool {
	return len(entry) >= len(prefix) && entry[:len(prefix)] == prefix
}

// pidFilePathIn 是 PidFilePath 的实现体，单独抽出来便于语料直接喂路径。
func pidFilePathIn(logFilePath string) string {
	return filepath.Join(filepath.Dir(logFilePath), "server.pid")
}

// dirOf 返回路径的目录部分（Python 的 Path.parent 语义）。
func dirOf(path string) string { return filepath.Dir(path) }

// absPath 对应 Path.resolve()：在无法解析时退回原值（Python 会抛异常，但调用方
// 只把它用于展示与命令构造，回退比中断更贴近「尽力而为」的语义）。
func absPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}
