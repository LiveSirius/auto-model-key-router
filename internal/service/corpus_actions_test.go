package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
)

// 本文件回放语料里「会执行命令 / 会写文件 / 会拉进程」的段：Windows 计划任务、
// UAC 提权、systemd、后台启停、健康检查与运维面服务动作分派。
//
// 所有子进程、HTTP、时钟、日志归档、进程创建都由假接缝承担；唯一的真实文件操作是
// 写临时目录里的 systemd unit 与 PID 文件（Env.Home / Env.Cwd / 配置路径都指向
// t.TempDir()）。

// —— 与参照实现刻意不同的两处命令文本 ——
//
// 1. 计划任务的 /TR：Python 是 `"{python}" -m auto_model_key_router.main --config ...`，
//    Go 是 `"{amkr}" --config ...`（没有解释器与模块入口）。
// 2. systemd 的 ExecStart 回退：同上。
//
// 下面两个比较函数把「哪些元素应当不同」变成显式断言，而不是把差异藏进宽松比较。

// compareTaskCommands 比较计划任务命令表：除「刻意不同的文本」外必须逐字节相同。
//
// 两处刻意不同，各自显式断言而不是宽松跳过：
//  1. /TR 命令行（Python 经 python -m 重入，Go 直接重入自身）；
//  2. 非管理员时的 UAC 脚本（同样是解释器参数差异）。
//
// 其余任何差异都按真失败上报。
func compareTaskCommands(t *testing.T, got, want [][]string, placeholders *corpusPlaceholders, action string) {
	t.Helper()
	executable := placeholders.get("$EXE")
	configPath := placeholders.configPath()
	if len(got) != len(want) {
		t.Fatalf("命令条数 = %d，期望 %d（got=%v want=%v）", len(got), len(want), got, want)
	}
	for index := range got {
		if len(got[index]) != len(want[index]) {
			t.Fatalf("第 %d 条命令长度 = %d，期望 %d", index, len(got[index]), len(want[index]))
		}
		for position := range got[index] {
			gotElement, wantElement := got[index][position], want[index][position]
			if gotElement == wantElement {
				continue
			}
			switch {
			case wantElement == placeholders.restore(pythonTaskActionCommandLine(executable, configPath)):
				// 语料里的 /TR 必须是参照实现那一版，Go 侧必须是重入自身那一版。
				if gotElement != TaskActionCommandLine(executable, configPath) {
					t.Errorf("第 %d 条命令的 /TR = %q，期望 %q", index, gotElement,
						TaskActionCommandLine(executable, configPath))
				}
			case strings.Contains(wantElement, "Start-Process"):
				pythonArgs := []string{"-m", "auto_model_key_router.main", "--config", configPath,
					"--service", action + "-elevated"}
				if wantElement != placeholders.restore(elevationScript(executable, pythonArgs)) {
					t.Errorf("语料里的 UAC 脚本 = %q", wantElement)
				}
				if gotElement != elevationScript(executable, ElevationArguments(configPath, action)) {
					t.Errorf("第 %d 条命令的 UAC 脚本 = %q", index, gotElement)
				}
			default:
				t.Errorf("第 %d 条命令第 %d 个参数 = %q，期望 %q", index, position, gotElement, wantElement)
			}
		}
	}
}

// compareUnitText 比较 unit 文本：ExecStart 行按两个实现各自的命令形态断言，
// 其余行在路径占位符还原后必须逐字节相同。
func compareUnitText(t *testing.T, got, want string, placeholders *corpusPlaceholders, expectedGoCommand []string) {
	t.Helper()
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("unit 行数 = %d，期望 %d\ngot=%q\nwant=%q", len(gotLines), len(wantLines), got, want)
	}
	goCommand := "ExecStart=" + ShlexJoin(expectedGoCommand)
	for index := range gotLines {
		if placeholders.restore(gotLines[index]) == wantLines[index] {
			continue
		}
		if !strings.HasPrefix(wantLines[index], "ExecStart=") {
			t.Errorf("unit 第 %d 行 = %q，期望 %q", index+1,
				placeholders.restore(gotLines[index]), wantLines[index])
			continue
		}
		if gotLines[index] != goCommand {
			t.Errorf("ExecStart = %q，期望 %q", gotLines[index], goCommand)
		}
	}
}

// TestCorpusManageWindowsTask 回放 Windows 计划任务的全部动作
// （service.py:382-458，含 -elevated 与 UAC 分支）。
func TestCorpusManageWindowsTask(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "windows_manage") {
		action := fieldString(t, entry, "action")
		if action == "bogus" {
			env := newTestEnv(t, placeholders, "windows")
			if _, err := env.ManageWindowsTask(placeholders.get("$EXE"), placeholders.configPath(), action); err == nil {
				t.Errorf("未支持的动作应当报错")
			} else if !strings.Contains(err.Error(), "不支持的系统服务动作: bogus") {
				t.Errorf("错误文案 = %q", err.Error())
			}
			continue
		}
		env := newTestEnv(t, placeholders, "windows")
		isAdmin := fieldBool(t, entry, "is_admin")
		env.IsAdmin = func() bool { return isAdmin }
		executable := placeholders.denormalize(fieldString(t, entry, "executable"))
		configPath := placeholders.denormalize(fieldString(t, entry, "config_path"))
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		_ = os.Remove(placeholders.pidFilePath())

		panel, err := env.ManageWindowsTask(executable, configPath, action)
		wantError, hasError := optionalString(entry, "error")
		if hasError {
			if err == nil {
				t.Errorf("用例 %d（%s/admin=%v）: 期望错误 %s", index, action, isAdmin, wantError)
			}
			continue
		}
		if err != nil {
			t.Errorf("用例 %d（%s/admin=%v）: 意外错误 %v", index, action, isAdmin, err)
			continue
		}
		compareTaskCommands(t, runner.calls, commandList(t, entry, "commands"), placeholders, action)
		got := normalizePanel(placeholders.restore(RenderText(panel)))
		if want := normalizePanel(fieldString(t, entry, "panel")); got != want {
			t.Errorf("用例 %d（%s/admin=%v）面板:\n got=%q\nwant=%q", index, action, isAdmin, got, want)
		}
	}
}

// TestCorpusElevation 回放 UAC 提权路径（service.py:472-501）。
//
// 真机上会弹 UAC 窗口，这里只回放决策：脚本模板、参数形态与结果面板。
func TestCorpusElevation(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "elevation") {
		action := fieldString(t, entry, "action")
		env := newTestEnv(t, placeholders, "windows")
		env.IsAdmin = func() bool { return false }
		executable := placeholders.get("$EXE")
		configPath := placeholders.configPath()
		runner := newFakeRunner([]servicestatus.CommandResult{{
			Code:   fieldInt(t, entry, "result_code"),
			Stdout: fieldString(t, entry, "result_stdout"),
			Stderr: fieldString(t, entry, "result_stderr"),
		}})
		env.Run = runner.run

		panel, err := env.ManageWindowsTask(executable, configPath, action)
		if err != nil {
			t.Fatalf("用例 %d（%s）: 意外错误 %v", index, action, err)
		}
		want := commandList(t, entry, "commands")
		if len(runner.calls) != 1 || len(want) != 1 {
			t.Fatalf("用例 %d（%s）: 命令条数 = %d，期望 1", index, action, len(runner.calls))
		}
		prefix := []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command"}
		if !equalStrings(runner.calls[0][:5], prefix) || !equalStrings(want[0][:5], prefix) {
			t.Fatalf("用例 %d（%s）: 命令前缀 = %v / %v", index, action, runner.calls[0], want[0])
		}
		// 语料里的脚本必须等于「模板 + 参照实现的参数」，Go 侧必须等于「模板 + 重入自身的参数」。
		pythonArgs := []string{"-m", "auto_model_key_router.main", "--config", configPath, "--service", action + "-elevated"}
		if want[0][5] != placeholders.restore(elevationScript(executable, pythonArgs)) {
			t.Errorf("用例 %d（%s）: 语料脚本 = %q，期望 %q", index, action, want[0][5],
				placeholders.restore(elevationScript(executable, pythonArgs)))
		}
		if runner.calls[0][5] != elevationScript(executable, ElevationArguments(configPath, action)) {
			t.Errorf("用例 %d（%s）: 脚本 = %q", index, action, runner.calls[0][5])
		}
		got := normalizePanel(placeholders.restore(RenderText(panel)))
		if wantPanel := normalizePanel(fieldString(t, entry, "panel")); got != wantPanel {
			t.Errorf("用例 %d（%s）面板:\n got=%q\nwant=%q", index, action, got, wantPanel)
		}
	}
}

// TestCorpusSystemdServiceCommand 锁定 systemd 命令构造的两个分支（service.py:549）。
func TestCorpusSystemdServiceCommand(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "systemd_service_command") {
		env := newTestEnv(t, placeholders, "linux")
		executable := placeholders.denormalize(fieldString(t, entry, "executable"))
		configPath := placeholders.denormalize(fieldString(t, entry, "config_path"))
		consoleScript, hasConsole := optionalString(entry, "console_script")
		if hasConsole {
			env.Executable = placeholders.denormalize(consoleScript)
			which := map[string]string{"amkr": placeholders.denormalize(consoleScript)}
			env.LookPath = func(name string) (string, error) {
				if found, present := which[name]; present {
					return found, nil
				}
				return "", os.ErrNotExist
			}
		}
		got := env.SystemdServiceCommand(configPath)
		want := stringList(t, entry["expected"])
		if hasConsole {
			restoredGot := make([]string, 0, len(got))
			for _, part := range got {
				restoredGot = append(restoredGot, placeholders.restore(part))
			}
			if !equalStrings(restoredGot, want) {
				t.Errorf("用例 %d: 命令 = %v，期望 %v", index, got, want)
			}
			continue
		}
		// console script 缺失时是刻意差异：参照实现回退到 python -m，Go 回退到自身。
		restoredWant := make([]string, 0, len(want))
		for _, part := range want {
			restoredWant = append(restoredWant, placeholders.denormalize(part))
		}
		if !equalStrings(restoredWant, pythonSystemdServiceCommand(executable, configPath)) {
			t.Errorf("用例 %d: 语料命令 = %v，期望参照实现形态", index, want)
		}
		expected := []string{env.Executable, "--config", configPath, "--serve-foreground"}
		if !equalStrings(got, expected) {
			t.Errorf("用例 %d: 命令 = %v，期望 %v", index, got, expected)
		}
	}
}

// TestCorpusSystemdRegistered 锁定 systemd 注册判定（service.py:238）。
func TestCorpusSystemdRegistered(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "systemd_registered") {
		env := newTestEnv(t, placeholders, "linux")
		unitPath := placeholders.systemdUnitPath()
		if fieldBool(t, entry, "file_exists") {
			if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
				t.Fatalf("创建 unit 目录失败: %v", err)
			}
			if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o666); err != nil {
				t.Fatalf("写 unit 失败: %v", err)
			}
		} else {
			_ = os.Remove(unitPath)
		}
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		if got := env.IsSystemdUserServiceRegistered(); got != fieldBool(t, entry, "expected") {
			t.Errorf("用例 %d（file=%v show=%q）: = %v，期望 %v", index, entry["file_exists"],
				entry["show_stdout"], got, entry["expected"])
		}
		if !equalCommands(runner.calls, commandList(t, entry, "commands")) {
			t.Errorf("用例 %d: 命令 = %v，期望 %v", index, runner.calls, entry["commands"])
		}
	}
}

// TestCorpusManageSystemdUserService 回放 systemd 的全部动作（service.py:563-627）。
//
// install 会真的写 unit 文件——写进 t.TempDir() 下的 HOME。
func TestCorpusManageSystemdUserService(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "systemd_manage") {
		action := fieldString(t, entry, "action")
		executable := placeholders.denormalize(fieldString(t, entry, "executable"))
		configPath := placeholders.denormalize(fieldString(t, entry, "config_path"))
		env := newTestEnv(t, placeholders, "linux")
		env.Executable = executable
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		_ = os.Remove(placeholders.pidFilePath())
		unitPath := placeholders.systemdUnitPath()
		_ = os.Remove(unitPath)
		consoleScript, hasConsole := optionalString(entry, "console_script")
		if hasConsole {
			env.Executable = placeholders.denormalize(consoleScript)
			which := map[string]string{"amkr": placeholders.denormalize(consoleScript)}
			env.LookPath = func(name string) (string, error) {
				if found, present := which[name]; present {
					return found, nil
				}
				return "", os.ErrNotExist
			}
		}

		panel, err := env.ManageSystemdUserService(executable, configPath, action)
		if err != nil {
			t.Fatalf("用例 %d（%s）: 意外错误 %v", index, action, err)
		}
		if wantText, present := optionalString(entry, "unit_text"); present {
			raw, readErr := os.ReadFile(unitPath)
			if readErr != nil {
				t.Fatalf("用例 %d（%s）: unit 文件未写出: %v", index, action, readErr)
			}
			expectedCommand := []string{executable, "--config", configPath, "--serve-foreground"}
			if hasConsole {
				expectedCommand = []string{placeholders.denormalize(consoleScript), "--config", configPath, "--serve-foreground"}
			}
			compareUnitText(t, string(raw), wantText, placeholders, expectedCommand)
		} else if _, statErr := os.Stat(unitPath); statErr == nil {
			t.Errorf("用例 %d（%s）: 不应写出 unit 文件", index, action)
		}
		if wantCommands, present := entry["commands"]; present && wantCommands != nil {
			want := commandList(t, entry, "commands")
			got := runner.calls
			if action != "uninstall" {
				if !equalCommands(got, want) {
					t.Errorf("用例 %d（%s）: 命令 = %v，期望 %v", index, action, got, want)
				}
			} else if len(got) != len(want) {
				t.Errorf("用例 %d（%s）: 命令条数 = %d，期望 %d（%v / %v）", index, action, len(got), len(want), got, want)
			}
		}
		if wantPanel, present := optionalString(entry, "panel"); present {
			if action == "install" {
				// install 面板里嵌着 unit 路径，而该路径会被按宽度折行：占位符与真实
				// 临时路径的长度不同，折行位置必然不同。含路径的行整体丢掉再比较，
				// 其余内容仍逐行对拍（unit 文本本身已在上面逐行对拍）。
				compareDropPathLines(t, RenderText(panel), wantPanel, placeholders, "systemd 用户服务文件已写入:")
			} else {
				got := normalizePanel(placeholders.restore(RenderText(panel)))
				if got != normalizePanel(wantPanel) {
					t.Errorf("用例 %d（%s）面板:\n got=%q\nwant=%q", index, action, got, wantPanel)
				}
			}
		}
		_ = os.Remove(unitPath)
	}
}

// TestCorpusBackgroundStart 回放后台启动（service.py:44-101）。
func TestCorpusBackgroundStart(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "background_start") {
		env := newTestEnv(t, placeholders, "windows")
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		env.Running = env.windowsRunning
		http := newFakeHTTP(httpEntriesFromCorpus(t, entry["responses"]))
		env.Get = http.get
		spawner := &fakeSpawn{pid: 4242}
		env.Spawn = spawner.spawn
		archiveCalls := []string{}
		archiveResult, hasArchive := optionalString(entry, "archive_result")
		env.ArchiveLog = func(logPath string, _ time.Time) (string, bool, error) {
			archiveCalls = append(archiveCalls, logPath)
			if hasArchive {
				return placeholders.denormalize(archiveResult), true, nil
			}
			return "", false, nil
		}
		_ = os.Remove(placeholders.pidFilePath())
		if pid, present := optionalInt(entry, "existing_pid"); present {
			if err := os.WriteFile(placeholders.pidFilePath(), []byte(itoa(pid)), 0o666); err != nil {
				t.Fatalf("写 PID 文件失败: %v", err)
			}
		}

		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}
		panel, err := env.StartBackground(placeholders.configPath(), loaded)
		if err != nil {
			t.Fatalf("用例 %d: 启动后台失败: %v", index, err)
		}

		if !equalCommands(runner.calls, commandList(t, entry, "commands")) {
			t.Errorf("用例 %d: 命令 = %v，期望 %v", index, runner.calls, entry["commands"])
		}
		wantURLs := stringList(t, entry["url_calls"])
		gotURLs := make([]string, 0, len(http.calls))
		for _, call := range http.calls {
			gotURLs = append(gotURLs, call.url)
		}
		if !equalStrings(gotURLs, wantURLs) {
			t.Errorf("用例 %d: /health 调用 = %v，期望 %v", index, gotURLs, wantURLs)
		}
		wantArchiveCalls := stringList(t, entry["archive_calls"])
		gotArchiveCalls := make([]string, 0, len(archiveCalls))
		for _, call := range archiveCalls {
			gotArchiveCalls = append(gotArchiveCalls, placeholders.restore(call))
		}
		if !equalStrings(gotArchiveCalls, wantArchiveCalls) {
			t.Errorf("用例 %d: 归档调用 = %v，期望 %v", index, gotArchiveCalls, wantArchiveCalls)
		}

		spawnRecords, _ := entry["spawn"].([]any)
		if len(spawner.specs) != len(spawnRecords) {
			t.Fatalf("用例 %d: 进程创建次数 = %d，期望 %d", index, len(spawner.specs), len(spawnRecords))
		}
		for spawnIndex, raw := range spawnRecords {
			record := raw.(map[string]any)
			spec := spawner.specs[spawnIndex]
			wantCommand := stringList(t, record["command"])
			// 命令文本是刻意的差异：参照实现经 python -m 重入，Go 直接重入自身。
			if len(wantCommand) < 3 || wantCommand[1] != "-m" {
				t.Errorf("用例 %d: 语料命令 = %v，期望参照实现形态", index, wantCommand)
			}
			expected := []string{placeholders.get("$EXE"), "--config", placeholders.configPath(), "--serve-foreground"}
			if !equalStrings(spec.Command, expected) {
				t.Errorf("用例 %d: 启动命令 = %v，期望 %v", index, spec.Command, expected)
			}
			if normalizePath(spec.Cwd) != normalizePath(placeholders.get("$CWD")) {
				t.Errorf("用例 %d: 工作目录 = %q", index, spec.Cwd)
			}
			if normalizePath(spec.LogPath) != normalizePath(placeholders.logPath()) {
				t.Errorf("用例 %d: 日志路径 = %q", index, spec.LogPath)
			}
			if !spec.Detached {
				t.Errorf("用例 %d: Detached 应为真", index)
			}
			if spec.CloseFiles != (env.GOOS != "windows") {
				t.Errorf("用例 %d: CloseFiles = %v", index, spec.CloseFiles)
			}
			if !hasEnvValue(spec.Env, "AMKR_LOG_ARCHIVED=1") {
				t.Errorf("用例 %d: 环境缺少 AMKR_LOG_ARCHIVED=1", index)
			}
			if fieldBool(t, record, "has_stdout") != true || fieldBool(t, record, "has_stderr") != true {
				t.Errorf("用例 %d: 语料记录 stdout/stderr 应指向日志文件", index)
			}
		}

		if wantPID, present := optionalString(entry, "pid_file"); present {
			pidRaw, readErr := os.ReadFile(placeholders.pidFilePath())
			if readErr != nil {
				t.Fatalf("用例 %d: PID 文件未写出: %v", index, readErr)
			}
			if strings.TrimSpace(string(pidRaw)) != wantPID {
				t.Errorf("用例 %d: PID 文件 = %q，期望 %q", index, string(pidRaw), wantPID)
			}
		} else if _, statErr := os.Stat(placeholders.pidFilePath()); statErr == nil {
			t.Errorf("用例 %d: 不应写出 PID 文件（服务已在运行）", index)
		}
		if want, present := optionalString(entry, "panel"); present {
			compareDropPathLines(t, RenderText(panel), want, placeholders, "日志:", "旧日志已归档:")
			// 路径内容本身不能逐字节比较，这里改成定向断言：面板必须报出配置里的
			// 日志路径，并且「旧日志已归档」只在真的归档了时才出现。
			gotText := placeholders.restore(RenderText(panel))
			if !fieldBool(t, entry, "healthy") &&
				!strings.Contains(normalizePanel(gotText), placeholders.restore(placeholders.logPath())) {
				t.Errorf("用例 %d: 面板未报出日志路径", index)
			}
			_, archived := optionalString(entry, "archive_result")
			if strings.Contains(gotText, "旧日志已归档") != archived {
				t.Errorf("用例 %d: 归档提示出现 = %v，期望 %v", index,
					strings.Contains(gotText, "旧日志已归档"), archived)
			}
		}
		_ = os.Remove(placeholders.pidFilePath())
	}
}

// TestCorpusBackgroundStop 回放后台停止的轮询语义（service.py:122-147）。
func TestCorpusBackgroundStop(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "background_stop") {
		env := newTestEnv(t, placeholders, "windows")
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		env.Running = env.windowsRunning
		env.Terminate = env.windowsTerminate
		clock := &fakeClock{}
		env.Sleep = clock.sleep
		_ = os.Remove(placeholders.pidFilePath())
		if pid, present := optionalInt(entry, "pid_file"); present {
			if err := os.WriteFile(placeholders.pidFilePath(), []byte(itoa(pid)), 0o666); err != nil {
				t.Fatalf("写 PID 文件失败: %v", err)
			}
		}
		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}

		panel := env.StopBackground(loaded)

		if !equalCommands(runner.calls, commandList(t, entry, "commands")) {
			t.Errorf("用例 %d: 命令 = %v，期望 %v", index, runner.calls, entry["commands"])
		}
		wantSleeps, _ := entry["sleeps"].([]any)
		if len(clock.slept) != len(wantSleeps) {
			t.Errorf("用例 %d: 轮询次数 = %d，期望 %d", index, len(clock.slept), len(wantSleeps))
		}
		for _, duration := range clock.slept {
			if duration != backgroundStopPollInterval {
				t.Errorf("用例 %d: 轮询间隔 = %v", index, duration)
			}
		}
		_, exists := os.Stat(placeholders.pidFilePath())
		if (exists == nil) != fieldBool(t, entry, "pid_file_exists") {
			t.Errorf("用例 %d: PID 文件存在 = %v，期望 %v", index, exists == nil, entry["pid_file_exists"])
		}
		got := normalizePanel(RenderText(panel))
		if want := normalizePanel(fieldString(t, entry, "panel")); got != want {
			t.Errorf("用例 %d 面板:\n got=%q\nwant=%q", index, got, want)
		}
		_ = os.Remove(placeholders.pidFilePath())
	}
}

// TestCorpusHealth 回放健康检查（service.py:640-669）。
func TestCorpusHealth(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "health") {
		env := newTestEnv(t, placeholders, "windows")
		http := newFakeHTTP(httpEntriesFromCorpus(t, entry["responses"]))
		env.Get = http.get
		host := fieldString(t, entry, "host")
		port := fieldInt(t, entry, "port")

		healthy := env.IsServiceHealthy(host, port, false)
		if healthy != fieldBool(t, entry, "healthy") {
			t.Errorf("用例 %d: IsServiceHealthy = %v，期望 %v", index, healthy, entry["healthy"])
		}
		info, ok := env.ServiceHealth(host, port, false)
		if ok != fieldBool(t, entry, "service_health") {
			t.Errorf("用例 %d: ServiceHealth 可用 = %v，期望 %v", index, ok, entry["service_health"])
		}
		wantConfig, _ := optionalString(entry, "config_path")
		wantFingerprint, _ := optionalString(entry, "fingerprint")
		if healthy {
			if placeholders.restore(info.ConfigPath) != wantConfig {
				t.Errorf("用例 %d: config_path = %q，期望 %q", index, placeholders.restore(info.ConfigPath), wantConfig)
			}
			if info.LocalAPIKeyFingerprint != wantFingerprint {
				t.Errorf("用例 %d: fingerprint = %q，期望 %q", index, info.LocalAPIKeyFingerprint, wantFingerprint)
			}
		}
		wantCalls, _ := entry["url_calls"].([]any)
		if len(http.calls) != len(wantCalls) {
			t.Fatalf("用例 %d: HTTP 调用 = %d，期望 %d", index, len(http.calls), len(wantCalls))
		}
		for callIndex, raw := range wantCalls {
			record := raw.(map[string]any)
			wantURL := placeholders.denormalize(fieldString(t, record, "url"))
			if http.calls[callIndex].url != wantURL {
				t.Errorf("用例 %d 第 %d 次调用: url = %q，期望 %q", index, callIndex, http.calls[callIndex].url, wantURL)
			}
			timeoutSeconds := record["timeout"].(float64)
			wantTimeout := time.Duration(timeoutSeconds * float64(time.Second))
			if http.calls[callIndex].timeout != wantTimeout {
				t.Errorf("用例 %d 第 %d 次调用: 超时 = %v，期望 %v", index, callIndex, http.calls[callIndex].timeout, wantTimeout)
			}
		}
	}
}

// TestCorpusHealthCache 锁定 2 秒 TTL 的缓存语义（service.py:640-655）。
func TestCorpusHealthCache(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "health_cache") {
		env := newTestEnv(t, placeholders, "linux")
		clockValues, _ := entry["clock"].([]any)
		values := make([]float64, 0, len(clockValues))
		for _, value := range clockValues {
			values = append(values, value.(float64))
		}
		clock := &fakeClock{values: values}
		env.Now = clock.now
		http := newFakeHTTP(httpEntriesFromCorpus(t, entry["responses"]))
		env.Get = http.get

		host := fieldString(t, entry, "host")
		port := fieldInt(t, entry, "port")
		wantRaw, _ := entry["expected"].([]any)
		for callIndex, raw := range wantRaw {
			got := env.IsServiceHealthy(host, port, true)
			if got != raw.(bool) {
				t.Errorf("用例 %d 第 %d 次调用: = %v，期望 %v", index, callIndex, got, raw.(bool))
			}
		}
		wantCalls, _ := entry["url_calls"].([]any)
		if len(http.calls) != len(wantCalls) {
			t.Errorf("用例 %d: HTTP 调用 = %d，期望 %d（缓存未生效或过早过期）", index, len(http.calls), len(wantCalls))
		}
	}
}

// TestCorpusBackgroundStatusPanel 回放后台状态面板（service.py:164-196）。
func TestCorpusBackgroundStatusPanel(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "background_status") {
		env := newTestEnv(t, placeholders, "windows")
		http := newFakeHTTP(httpEntriesFromCorpus(t, entry["responses"]))
		env.Get = http.get
		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}
		got := normalizePanel(RenderText(env.BackgroundStatusPanel(loaded, placeholders.configPath())))
		if want := normalizePanel(fieldString(t, entry, "panel")); got != want {
			t.Errorf("用例 %d 面板:\n got=%q\nwant=%q", index, got, want)
		}
		wantCalls, _ := entry["url_calls"].([]any)
		if len(http.calls) != len(wantCalls) {
			t.Errorf("用例 %d: HTTP 调用 = %d，期望 %d", index, len(http.calls), len(wantCalls))
		}
	}
}

// TestCorpusRunServiceAction 回放运维面服务动作分派（ops_api.py:103 的
// _run_service_action + api.OpsServiceTargets 的 11 个动作名）。
func TestCorpusRunServiceAction(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "run_service_action") {
		action := fieldString(t, entry, "action")
		platform := strings.ToLower(fieldString(t, entry, "platform"))
		env := newTestEnv(t, placeholders, platform)
		runner := newFakeRunner(scriptFromCorpus(t, entry["run_script"]))
		env.Run = runner.run
		env.Running = env.windowsRunning
		env.Terminate = env.windowsTerminate
		env.IsAdmin = func() bool { return true }
		http := newFakeHTTP(httpEntriesFromCorpus(t, entry["responses"]))
		env.Get = http.get
		env.Sleep = func(time.Duration) {}
		env.ArchiveLog = func(string, time.Time) (string, bool, error) { return "", false, nil }
		spawner := &fakeSpawn{pid: 4242}
		env.Spawn = spawner.spawn
		_ = os.Remove(placeholders.pidFilePath())
		_ = os.Remove(placeholders.systemdUnitPath())
		if pid, present := optionalInt(entry, "pid"); present {
			if err := os.WriteFile(placeholders.pidFilePath(), []byte(itoa(pid)), 0o666); err != nil {
				t.Fatalf("写 PID 文件失败: %v", err)
			}
		}

		loaded, err := config.Load(placeholders.configPath())
		if err != nil {
			t.Fatalf("载入配置失败: %v", err)
		}
		text, err := env.RunServiceAction(action, placeholders.configPath(), loaded)
		if wantError, hasError := optionalString(entry, "error"); hasError {
			if err == nil {
				t.Errorf("用例 %d（%s）: 期望错误 %s", index, action, wantError)
			}
			continue
		}
		if err != nil {
			t.Errorf("用例 %d（%s）: 意外错误 %v", index, action, err)
			continue
		}
		wantCommands := commandList(t, entry, "commands")
		if platform == "windows" {
			compareTaskCommands(t, runner.calls, wantCommands, placeholders, action)
		} else if !equalCommands(runner.calls, wantCommands) {
			t.Errorf("用例 %d（%s）: 命令 = %v，期望 %v", index, action, runner.calls, wantCommands)
		}
		got := normalizePanel(placeholders.restore(text))
		want := fieldString(t, entry, "text")
		if strings.Contains(want, "$HOME") {
			// systemd install 的面板里嵌着会被折行的 unit 路径（见
			// compareDropPathLines 的说明）。
			compareDropPathLines(t, text, want, placeholders, "systemd 用户服务文件已写入:")
		} else if got != normalizePanel(want) {
			t.Errorf("用例 %d（%s）文本:\n got=%q\nwant=%q", index, action, got, want)
		}
		_ = os.Remove(placeholders.pidFilePath())
		_ = os.Remove(placeholders.systemdUnitPath())
	}
}

// compareDropPathLines 用于「面板里嵌着会被按宽度折行的路径」的用例。
//
// 占位符与 Go 测试的真实临时路径长度不同，折行位置必然不同，含路径的行因此无法
// 逐字节比较。这里把以给定标签开头的行连同其后到面板下边框为止的所有行一起丢掉
// （两侧同样处理），其余行仍逐行归一化比较；同时断言两侧丢掉的区段数一致。
func compareDropPathLines(t *testing.T, got, want string, placeholders *corpusPlaceholders, labels ...string) {
	t.Helper()
	gotKept, gotRegions := dropPathRegions(placeholders.restore(got), labels)
	wantKept, wantRegions := dropPathRegions(want, labels)
	if gotRegions != wantRegions {
		t.Errorf("路径区段数 = %d，期望 %d", gotRegions, wantRegions)
	}
	if normalizePanel(gotKept) != normalizePanel(wantKept) {
		t.Errorf("丢路径区段后仍不一致:\n got=%q\nwant=%q", normalizePanel(gotKept), normalizePanel(wantKept))
	}
}

// dropPathRegions 丢掉「以 label 开头、直到面板下边框为止」的整段内容。
//
// 只对含路径的面板用：路径长度不同 ⇒ 折行位置不同 ⇒ 只能比较其余内容。
func dropPathRegions(text string, labels []string) (string, int) {
	kept := []string{}
	regions := 0
	skipping := false
	for _, line := range strings.Split(text, "\n") {
		if skipping {
			if strings.HasPrefix(line, "╰") {
				skipping = false
				kept = append(kept, line)
			}
			continue
		}
		matched := false
		for _, label := range labels {
			if strings.Contains(line, label) {
				matched = true
				break
			}
		}
		if matched {
			regions++
			skipping = true
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), regions
}

// itoa 是小工具（避免为几处调用引入 strconv）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// hasEnvValue 判断环境列表里是否有给定条目。
func hasEnvValue(environ []string, entry string) bool {
	for _, item := range environ {
		if item == entry {
			return true
		}
	}
	return false
}

var _ = json.Marshal
var _ = servicestatus.CommandResult{}
