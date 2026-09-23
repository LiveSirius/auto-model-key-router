package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
)

// 本文件是服务动作测试的夹具：一个完全指向临时目录的 Env，以及若干小工具。
//
// 所有测试都在临时目录里构造 Env，绝不触碰宿主机的计划任务 / systemd / 进程。

// testPaths 持有本次测试用到的真实路径。
type testPaths struct {
	root string
	home string
	cwd  string
	exe  string
	log  string
}

// configPath 返回本次测试的配置文件路径。
func (p *testPaths) configPath() string { return filepath.Join(p.root, "router-config.json") }

// pidFilePath 返回本次测试的 PID 文件路径（与日志同目录）。
func (p *testPaths) pidFilePath() string { return filepath.Join(filepath.Dir(p.log), "server.pid") }

// systemdUnitPath 返回本次测试的 systemd unit 路径。
func (p *testPaths) systemdUnitPath() string {
	return filepath.Join(p.home, ".config", "systemd", "user", SystemdUserServiceName)
}

// newTestPaths 建夹具目录与文件，并写出一份最小合法配置。
func newTestPaths(t *testing.T) *testPaths {
	t.Helper()
	root := t.TempDir()
	paths := &testPaths{
		root: root,
		home: filepath.Join(root, "home"),
		cwd:  filepath.Join(root, "cwd"),
		exe:  filepath.Join(root, "python", "python.exe"),
		log:  filepath.Join(root, "logs", "server.log"),
	}
	for _, directory := range []string{
		paths.home, paths.cwd, filepath.Dir(paths.exe), filepath.Dir(paths.log),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("创建夹具目录 %s 失败: %v", directory, err)
		}
	}
	if err := os.WriteFile(paths.exe, nil, 0o666); err != nil {
		t.Fatalf("创建夹具文件 %s 失败: %v", paths.exe, err)
	}
	writeTestConfig(t, paths.configPath(), paths.log)
	return paths
}

// writeTestConfig 写出一份固定配置（本地 key 与监听地址影响面板里的指纹与地址）。
func writeTestConfig(t *testing.T, path, logPath string) {
	t.Helper()
	data, err := config.EmptyConfigDict()
	if err != nil {
		t.Fatalf("构造空配置失败: %v", err)
	}
	data.SetKey("log_file_path", canonical.NewString(logPath))
	data.SetKey("local_api_key", canonical.NewString("amkr-test-key"))
	data.SetKey("host", canonical.NewString("127.0.0.1"))
	data.SetKey("port", canonical.NewIntValue(8123))
	if err := config.SaveConfigData(path, data); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
}

// newTestEnv 构造一个只指向临时目录的 Env。
func newTestEnv(t *testing.T, paths *testPaths, goos string) *Env {
	t.Helper()
	env := &Env{
		GOOS:       goos,
		Home:       paths.home,
		Cwd:        paths.cwd,
		Executable: paths.exe,
		User:       "amkr-user",
		Pid:        4242,
		Getenv:     func(string) string { return "" },
		LookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		Now:        time.Now,
		Sleep:      func(time.Duration) {},
		ArchiveLog: func(string, time.Time) (string, bool, error) { return "", false, nil },
	}
	env.Run = func([]string) servicestatus.CommandResult { return servicestatus.CommandResult{} }
	env.Spawn = func(SpawnSpec) (SpawnResult, error) { return SpawnResult{Pid: 4242}, nil }
	env.Running = func(int) bool { return false }
	env.Terminate = func(int) error { return nil }
	env.IsAdmin = func() bool { return true }
	env.Get = func(string, time.Duration) (int, []byte, error) { return 0, nil, fmt.Errorf("no server") }
	return env
}

// fakeRunner 记录被调用的命令并按脚本返回结果（空脚本返回零值）。
type fakeRunner struct {
	script []servicestatus.CommandResult
	calls  [][]string
}

func newFakeRunner(script []servicestatus.CommandResult) *fakeRunner {
	return &fakeRunner{script: append([]servicestatus.CommandResult(nil), script...)}
}

func (f *fakeRunner) run(command []string) servicestatus.CommandResult {
	f.calls = append(f.calls, append([]string(nil), command...))
	switch len(f.script) {
	case 0:
		return servicestatus.CommandResult{}
	case 1:
		return f.script[0]
	default:
		entry := f.script[0]
		f.script = f.script[1:]
		return entry
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// normalizePanel 按行比较时忽略行内空白。
func normalizePanel(text string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Join(lines, "\n")
}

// normalizePath 统一路径分隔符，让断言在 Windows/Linux 上都能通过。
func normalizePath(value string) string { return strings.ReplaceAll(value, "\\", "/") }
