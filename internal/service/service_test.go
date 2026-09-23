package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/api"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
)

// 本文件覆盖语料无法表达的部分：与 internal/api 接缝的契约、平台常量、以及
// 「只有 fake 覆盖」的路径的决策部分。

// TestRunServiceActionCoversEveryOpsServiceTarget 锁定接缝覆盖了 api 的**全部**动作名。
//
// 分派表直接用 api.OpsServiceTargets，因此这里逐个动作跑一遍并断言「不是不支持」，
// 任何新增动作只要没被处理就会失败。
func TestRunServiceActionCoversEveryOpsServiceTarget(t *testing.T) {
	for action := range api.OpsServiceTargets {
		env, placeholders := newTestFixture(t)
		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}
		if _, err := env.RunServiceAction(action, placeholders.configPath(), loaded); err != nil {
			t.Errorf("动作 %s 未被处理: %v", action, err)
		}
	}
	if len(api.OpsServiceTargets) != 11 {
		t.Fatalf("参照实现的服务动作数 = %d，期望 11", len(api.OpsServiceTargets))
	}
}

// TestRunServiceActionUnknownActionMatchesAPIMessage 锁定未知动作的错误文案。
//
// HTTP 状态 422 由 api 的 handler 在调用接缝**之前**产生
// （inner/api/handlers_ops.go 的 `不支持的服务动作: %s`，对应 ops_api.py:187-188）；
// 本函数只保证直接调用时也拿到同一句话，避免两处文案漂移。
func TestRunServiceActionUnknownActionMatchesAPIMessage(t *testing.T) {
	env, placeholders := newTestFixture(t)
	loaded, err := config.Load(placeholders.configPath())
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	_, err = env.RunServiceAction("nope", placeholders.configPath(), loaded)
	if err == nil {
		t.Fatalf("未知动作应当报错")
	}
	const want = "不支持的服务动作: nope"
	if err.Error() != want {
		t.Errorf("错误文案 = %q，期望 %q", err.Error(), want)
	}
}

// TestRunServiceActionSystemKindUsesArgument 锁定 system 类动作按 Argument 分派。
func TestRunServiceActionSystemKindUsesArgument(t *testing.T) {
	cases := map[string]string{
		"status_amkr":           "schtasks",
		"start_system_amkr":     "schtasks",
		"install_user_amkr":     "schtasks",
		"uninstall_system_amkr": "schtasks",
	}
	for action, wantFirst := range cases {
		env, placeholders := newTestFixture(t)
		runner := newFakeRunner([]servicestatus.CommandResult{{Stdout: "OK"}})
		env.Run = runner.run
		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}
		if _, err := env.RunServiceAction(action, placeholders.configPath(), loaded); err != nil {
			t.Fatalf("动作 %s 失败: %v", action, err)
		}
		if len(runner.calls) == 0 || runner.calls[0][0] != wantFirst {
			t.Errorf("动作 %s 的首条命令 = %v，期望以 %s 开头", action, runner.calls, wantFirst)
		}
	}
}

// TestRunServiceActionRestartJoinsStopAndStart 锁定 restart 的「先停后启、空行连接」
// （ops_api.py:112-115）。
func TestRunServiceActionRestartJoinsStopAndStart(t *testing.T) {
	env, placeholders := newTestFixture(t)
	runner := newFakeRunner([]servicestatus.CommandResult{
		{Stdout: `"python.exe","4242","Console"`},
		{},
		{},
	})
	env.Run = runner.run
	env.Running = env.windowsRunning
	env.Terminate = env.windowsTerminate
	if err := os.WriteFile(placeholders.pidFilePath(), []byte("4242"), 0o666); err != nil {
		t.Fatalf("写 PID 文件失败: %v", err)
	}
	loaded, err := config.Load(placeholders.configPath())
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	text, err := env.RunServiceAction("restart_amkr", placeholders.configPath(), loaded)
	if err != nil {
		t.Fatalf("restart_amkr 失败: %v", err)
	}
	if !strings.Contains(text, "后台服务已停止。") || !strings.Contains(text, "后台服务已启动。") {
		t.Errorf("restart 文本缺少两段面板之一: %q", text)
	}
	if !strings.Contains(text, "\n\n") {
		t.Errorf("restart 文本的两段之间应当有空行: %q", text)
	}
}

// TestRunServiceActionPackageFunction 锁定包级入口存在且签名与 api 接缝一致。
//
// 这一行就是集成方需要写进 internal/server 的接线：
//
//	srv.RunServiceAction = service.RunServiceAction
func TestRunServiceActionPackageFunction(t *testing.T) {
	var seam func(action string, configPath string, cfg *config.RouterConfig) (string, error) = RunServiceAction
	if seam == nil {
		t.Fatalf("RunServiceAction 为空")
	}
	if _, err := seam("nope", "", &config.RouterConfig{}); err == nil {
		t.Errorf("未知动作应当报错")
	}
}

// TestBackgroundExecutableDivergence 具名锁定刻意的差异：Go 版没有 Python 解释器，
// 后台启动直接重入自身（service.py:524 会优先挑 pythonw.exe）。
func TestBackgroundExecutableDivergence(t *testing.T) {
	env, _ := newTestFixture(t)
	if got := env.BackgroundExecutable(); got != env.Executable {
		t.Errorf("BackgroundExecutable = %q，期望 %q", got, env.Executable)
	}
}

// TestWindowsTaskCommandLineDivergence 具名锁定 /TR 命令行的差异。
func TestWindowsTaskCommandLineDivergence(t *testing.T) {
	goLine := TaskActionCommandLine(`C:\amkr\amkr.exe`, `C:\cfg\router-config.json`)
	wantGo := `"C:\amkr\amkr.exe" --config "C:\cfg\router-config.json" --serve-foreground`
	if goLine != wantGo {
		t.Errorf("TaskActionCommandLine = %q，期望 %q", goLine, wantGo)
	}
	pythonLine := pythonTaskActionCommandLine(`C:\py\python.exe`, `C:\cfg\router-config.json`)
	wantPython := `"C:\py\python.exe" -m auto_model_key_router.main --config "C:\cfg\router-config.json" --serve-foreground`
	if pythonLine != wantPython {
		t.Errorf("pythonTaskActionCommandLine = %q，期望 %q", pythonLine, wantPython)
	}
}

// TestSystemdServiceCommandDivergence 具名锁定 systemd 回退命令的差异（service.py:549）。
func TestSystemdServiceCommandDivergence(t *testing.T) {
	env, placeholders := newTestFixture(t)
	got := env.SystemdServiceCommand(placeholders.configPath())
	want := []string{env.Executable, "--config", placeholders.configPath(), "--serve-foreground"}
	if !equalStrings(got, want) {
		t.Errorf("SystemdServiceCommand = %v，期望 %v", got, want)
	}
	pythonCommand := pythonSystemdServiceCommand(env.Executable, placeholders.configPath())
	if pythonCommand[1] != "-m" || pythonCommand[2] != "auto_model_key_router.main" {
		t.Errorf("参照实现形态 = %v", pythonCommand)
	}
}

// TestElevationArgumentsDivergence 具名锁定 UAC 参数形态（service.py:474-481）。
func TestElevationArgumentsDivergence(t *testing.T) {
	got := ElevationArguments("/cfg/router-config.json", "install")
	want := []string{"--config", "/cfg/router-config.json", "--service", "install-elevated"}
	if !equalStrings(got, want) {
		t.Errorf("ElevationArguments = %v，期望 %v", got, want)
	}
}

// TestForegroundPIDHookWritesAndCleans 锁定前台启动的 PID 文件生命周期
// （service.py:111-112）。
func TestForegroundPIDHookWritesAndCleans(t *testing.T) {
	env, placeholders := newTestFixture(t)
	loaded, err := config.Load(placeholders.configPath())
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	cleanup, err := env.ForegroundPIDHook(loaded)
	if err != nil {
		t.Fatalf("写 PID 失败: %v", err)
	}
	pid, ok := ReadPid(placeholders.pidFilePath())
	if !ok || pid != env.Pid {
		t.Errorf("PID 文件 = (%d,%v)，期望 (%d,true)", pid, ok, env.Pid)
	}
	cleanup()
	if _, ok := ReadPid(placeholders.pidFilePath()); ok {
		t.Errorf("清理后 PID 文件仍可读")
	}
}

// TestArchiveLogForForegroundHonoursEnv 锁定 AMKR_LOG_ARCHIVED=1 时跳过归档
// （service.py:108-109）。
func TestArchiveLogForForegroundHonoursEnv(t *testing.T) {
	env, placeholders := newTestFixture(t)
	calls := 0
	env.ArchiveLog = func(string, time.Time) (string, bool, error) {
		calls++
		return "archived", true, nil
	}
	loaded, err := config.Load(placeholders.configPath())
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	env.Getenv = func(string) string { return "1" }
	if _, archived, err := env.ArchiveLogForForeground(loaded); err != nil || archived || calls != 0 {
		t.Errorf("AMKR_LOG_ARCHIVED=1 时不应归档: calls=%d archived=%v err=%v", calls, archived, err)
	}
	env.Getenv = func(string) string { return "" }
	if _, archived, err := env.ArchiveLogForForeground(loaded); err != nil || !archived || calls != 1 {
		t.Errorf("普通情况下应归档: calls=%d archived=%v err=%v", calls, archived, err)
	}
}

// TestRestartServiceAfterConfigChange 锁定热重载提示的两种文案（service.py:150）。
func TestRestartServiceAfterConfigChange(t *testing.T) {
	oldConfig := &config.RouterConfig{Host: "127.0.0.1", Port: 8123}
	newConfig := &config.RouterConfig{Host: "127.0.0.1", Port: 8123}
	text := normalizePanel(RenderText(RestartServiceAfterConfigChange(oldConfig, newConfig)))
	if !strings.Contains(text, "热重载") {
		t.Errorf("同地址文案 = %q", text)
	}
	newConfig.Port = 9999
	text = normalizePanel(RenderText(RestartServiceAfterConfigChange(oldConfig, newConfig)))
	if !strings.Contains(text, "下次启动时生效") || !strings.Contains(text, "9999") {
		t.Errorf("改端口文案 = %q", text)
	}
}

// TestDefaultRunnerMissingCommandReturns127 锁定「命令不存在 → 返回码 127」
// （service.py:316-317 的 FileNotFoundError 分支）。
func TestDefaultRunnerMissingCommandReturns127(t *testing.T) {
	result := defaultRunner([]string{"amkr-definitely-missing-binary", "--version"})
	if result.Code != 127 {
		t.Errorf("返回码 = %d，期望 127", result.Code)
	}
	if result.Stdout != "" {
		t.Errorf("stdout = %q，期望空", result.Stdout)
	}
	if result.Stderr == "" {
		t.Errorf("stderr 应当带异常文本")
	}
}

// TestDefaultRunnerCapturesStdout 锁定真实命令的输出捕获（用 go 自身做被试）。
func TestDefaultRunnerCapturesStdout(t *testing.T) {
	executable := os.Getenv("COMSPEC")
	if executable == "" {
		t.Skip("没有 COMSPEC，跳过")
	}
	result := defaultRunner([]string{executable, "/C", "echo hello"})
	if result.Code != 0 || strings.TrimSpace(result.Stdout) != "hello" {
		t.Errorf("结果 = %+v，期望 hello", result)
	}
}

// TestDefaultRunnerEmptyCommand 锁定空命令行（Python 侧不可达，Go 侧显式 127）。
func TestDefaultRunnerEmptyCommand(t *testing.T) {
	if result := defaultRunner(nil); result.Code != 127 {
		t.Errorf("返回码 = %d，期望 127", result.Code)
	}
}

// TestPlatformProcessHooksAreInjected 锁定 DefaultEnv 装上了平台实现。
func TestPlatformProcessHooksAreInjected(t *testing.T) {
	env := DefaultEnv()
	if env.Running == nil || env.Terminate == nil || env.IsAdmin == nil {
		t.Fatalf("平台接缝未装上（Running/Terminate/IsAdmin 有空值）")
	}
	if env.Run == nil || env.Spawn == nil || env.Get == nil || env.ArchiveLog == nil {
		t.Fatalf("基础接缝未装上")
	}
	if env.GOOS != defaultGOOS {
		t.Errorf("GOOS = %q，期望 %q", env.GOOS, defaultGOOS)
	}
}

// TestConsoleScriptExecutablePrefersOwnPath 锁定「自身名字命中 console script 名单」
// 的第一条分支（service.py:536-541）。
func TestConsoleScriptExecutablePrefersOwnPath(t *testing.T) {
	directory := t.TempDir()
	selfPath := filepath.Join(directory, "amkr")
	if err := os.WriteFile(selfPath, nil, 0o666); err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	env := &Env{Executable: selfPath, LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	got, found := env.ConsoleScriptExecutable()
	if !found || normalizePath(got) != normalizePath(selfPath) {
		t.Errorf("结果 = (%q,%v)，期望 (%q,true)", got, found, selfPath)
	}
	// 名字不在名单里才走 PATH 查找。
	env.Executable = filepath.Join(directory, "other")
	foundPath := filepath.Join(directory, "bin", "amkr")
	env.LookPath = func(name string) (string, error) {
		if name == "amkr" {
			return foundPath, nil
		}
		return "", os.ErrNotExist
	}
	got, found = env.ConsoleScriptExecutable()
	if !found || normalizePath(got) != normalizePath(foundPath) {
		t.Errorf("PATH 查找结果 = (%q,%v)，期望 (%q,true)", got, found, foundPath)
	}
}

// TestSameFilePathWindowsCaseInsensitive 锁定路径比较对大小写不敏感
// （Python 的 PureWindowsPath 相等语义）。
func TestSameFilePathWindowsCaseInsensitive(t *testing.T) {
	if defaultGOOS != "windows" {
		t.Skip("该语义只在 Windows 上生效")
	}
	if !sameFilePath(`C:\Cfg\Router.json`, `c:/cfg/router.json`) {
		t.Errorf("大小写不同的同一路径应当相等")
	}
	if sameFilePath(`C:\Cfg\Router.json`, `C:\Cfg\Other.json`) {
		t.Errorf("不同路径不应相等")
	}
}

// TestEscapeStatusValue 锁定状态值转义（只有五种颜色前缀被放行，service.py:307）。
func TestEscapeStatusValue(t *testing.T) {
	cases := map[string]string{
		"[green]已注册[/green]": "[green]已注册[/green]",
		"[100]":              `\[100]`,
		"普通文本":               "普通文本",
	}
	for input, want := range cases {
		if got := escapeStatusValue(input); got != want {
			t.Errorf("escapeStatusValue(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// newTestFixture 构造一个指向临时目录的 Env 与对应路径。
func newTestFixture(t *testing.T) (*Env, *testPaths) {
	t.Helper()
	paths := newTestPaths(t)
	env := newTestEnv(t, paths, "windows")
	env.IsAdmin = func() bool { return true }
	env.Running = func(int) bool { return false }
	env.Terminate = func(int) error { return nil }
	return env, paths
}
