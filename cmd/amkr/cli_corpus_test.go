package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/service"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// 本文件回放 gen_amkr_cli_corpus.py（已随 Python 退役移除） 产出的语料。
//
// 语料来自**真实 Python**：生成脚本调用 ``main.main()``，只把全部副作用换成记录桩，
// 因此每条记录都是参照实现真实做出的**分支决策**（选了哪条 if/elif、拿到的配置、
// 要写进配置的键值、切换统一模型的实参、退出码）。
//
// Go 侧分两层验证：
//
//  1. **决策层**（TestCLICorpusDecisions）：parseOptions + selectCommand + 覆盖规则，
//     与语料逐条比对，不需要任何副作用。
//  2. **退出码/输出层**（TestCLICorpusExitCodes）：runCLI 配注入接缝跑完整流程。

// cliCorpus 是语料文件的顶层结构。
type cliCorpus struct {
	CorpusVersion int              `json:"corpus_version"`
	Version       string           `json:"version"`
	DroppedFlags  []string         `json:"dropped_flags"`
	Flags         []cliFlagCase    `json:"flags"`
	AddressText   []cliAddressCase `json:"address_text"`
	VersionCheck  []cliVersionCase `json:"version_check"`
	VersionOutput cliVersionOutput `json:"version_output"`
}

type cliFlagCase struct {
	ID           int                `json:"id"`
	Argv         []string           `json:"argv"`
	Exit         int                `json:"exit"`
	Actions      []string           `json:"actions"`
	LoadError    bool               `json:"load_error"`
	Config       *cliConfigSnapshot `json:"config"`
	Switch       *cliSwitchSnapshot `json:"switch"`
	SystemAction *string            `json:"system_action"`
	Updates      []map[string]any   `json:"updates"`
	Replacements []map[string]any   `json:"replacements"`
	Stdout       string             `json:"stdout"`
}

type cliConfigSnapshot struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	LocalAPIKey string `json:"local_api_key"`
}

type cliSwitchSnapshot struct {
	Target    string  `json:"target"`
	Model     *string `json:"model"`
	Key       *string `json:"key"`
	UpdateKey bool    `json:"update_key"`
}

type cliAddressCase struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Expected string `json:"expected"`
}

type cliSummaryCase struct {
	Name             string     `json:"name"`
	ConfigJSON       string     `json:"config_json"`
	Healthy          bool       `json:"healthy"`
	VisitorInstalled bool       `json:"visitor_installed"`
	SummaryLines     []string   `json:"summary_lines"`
	ModelRows        [][]string `json:"model_rows"`
	HasWarning       bool       `json:"has_warning"`
}

type cliVersionCase struct {
	Result struct {
		CurrentVersion  string  `json:"current_version"`
		LatestVersion   *string `json:"latest_version"`
		LatestTag       *string `json:"latest_tag"`
		ReleaseURL      *string `json:"release_url"`
		Source          *string `json:"source"`
		FallbackError   *string `json:"fallback_error"`
		Error           *string `json:"error"`
		UpdateAvailable bool    `json:"update_available"`
	} `json:"result"`
	Content       string  `json:"content"`
	ManualCommand *string `json:"manual_command"`
}

type cliVersionOutput struct {
	Stdout string `json:"stdout"`
	Exit   int    `json:"exit"`
}

// frozenSummaryCorpus 是冻结夹具的顶层结构。
//
// 这段期望值原先随语料一起由生成器产出，其来源是参照实现的
// `dashboard.config_renderables`。而 `dashboard.py` 已按决策 7 删除（终端仪表盘由
// WebUI 取代），因此**无法再重新生成**。期望值本身仍是「参照实现真实产出过的输出」，
// 对 Go 侧渲染依旧是有效的回归锁，故从删除前最后一次提交里取出、冻结成静态文件。
//
// 不要用生成器重写 testdata/cli_config_summary_frozen.json。
type frozenSummaryCorpus struct {
	Cases []cliSummaryCase `json:"cases"`
}

func loadFrozenSummary(t *testing.T) []cliSummaryCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cli_config_summary_frozen.json"))
	if err != nil {
		t.Fatalf("读取冻结夹具失败: %v", err)
	}
	var frozen frozenSummaryCorpus
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatalf("解析冻结夹具失败: %v", err)
	}
	if len(frozen.Cases) == 0 {
		t.Fatal("冻结夹具为空：--show-config 的渲染将失去回归锁")
	}
	return frozen.Cases
}

func loadCLICorpus(t *testing.T) *cliCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cli_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus cliCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	return &corpus
}

// writeCLITestConfig 写一份与语料同值的配置，返回其路径。
func writeCLITestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router-config.json")
	data, err := config.EmptyConfigDict()
	if err != nil {
		t.Fatalf("构造空配置失败: %v", err)
	}
	data.SetKey("local_api_key", canonical.NewString("amkr-cli-corpus-key"))
	data.SetKey("host", canonical.NewString("127.0.0.1"))
	data.SetKey("port", canonical.NewIntValue(8123))
	if err := config.SaveConfigData(path, data); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return path
}

// restoreArgs 把语料里的 $CONFIG 占位符换成测试配置路径。
func restoreArgs(argv []string, configPath string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		out = append(out, strings.ReplaceAll(arg, "$CONFIG", configPath))
	}
	return out
}

// containsDroppedFlag 报告参数里是否出现被砍掉的 flag（含 `--flag=value` 形态）。
func containsDroppedFlag(argv []string, dropped []string) (string, bool) {
	for _, arg := range argv {
		for _, flagName := range dropped {
			if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
				return flagName, true
			}
		}
	}
	return "", false
}

// branchAction 取参照实现最终的**分支动作**：--webui/--no-ops 的配置写入会先被记录
// （main.py:95-103），它不是分支，且可能出现在分支之前。
func branchAction(actions []string) string {
	for _, action := range actions {
		if action != "config-update" {
			return action
		}
	}
	return ""
}

// pythonToGoCommand 把参照实现的动作名映射成 Go 的 command；第二个返回值表示
// 「参照实现进 Terminal UI，而 Go 没有该模块」（默认动作的开放决策）。
func pythonToGoCommand(action string) (command, bool) {
	switch action {
	case "background-start":
		return commandBackground, false
	case "foreground":
		return commandForeground, false
	case "background-stop":
		return commandStop, false
	case "background-status":
		return commandStatus, false
	case "service-status":
		// main.py:186：--service status 走 service_status_panel（组合面板）。
		return commandManageService, false
	case "system-service":
		return commandManageService, false
	case "switch-unified":
		return commandSwitchUnified, false
	case "check-update":
		return commandCheckUpdate, false
	case "update":
		// 生成器当初把 main.update_latest_version 打桩记成 `update-dropped`（它记的是
		// 「这个动作被砍了」而不是动作本身）；决策 8 被推翻后改回动作名。
		return commandUpdate, false
	case "show-address":
		return commandShowAddress, false
	case "show-api-key":
		return commandShowAPIKey, false
	case "show-config":
		return commandShowConfig, false
	case "show-unified-model":
		return commandShowUnifiedModel, false
	case "terminal-ui":
		// dashboard.run_terminal_ui 已按决策 7 砍掉；Go 的默认动作是前台启动服务。
		return commandForeground, true
	}
	return commandForeground, false
}

// TestCLICorpusDecisions 逐条比对「选了哪条分支 + 配置被改成了什么」。
func TestCLICorpusDecisions(t *testing.T) {
	corpus := loadCLICorpus(t)
	configPath := writeCLITestConfig(t)
	fixture, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("载入配置失败: %v", err)
	}
	for _, entry := range corpus.Flags {
		argv := restoreArgs(entry.Argv, configPath)
		if dropped, present := containsDroppedFlag(argv, corpus.DroppedFlags); present {
			// 参照实现仍然接受这些 flag（它们只驱动被砍掉的模块），Go 侧刻意不定义；
			// 解析必须失败（TestDroppedFlagsAreNotDefined 断言退出码 2）。
			if _, err := parseOptions(argv, os.Stderr); err == nil {
				t.Errorf("用例 %d: %s 应当解析失败", entry.ID, dropped)
			}
			continue
		}
		opts, err := parseOptions(argv, os.Stderr)
		if err != nil {
			if entry.Exit != 2 {
				t.Errorf("用例 %d（%v）: 意外解析失败 %v（参照退出码 %d）", entry.ID, entry.Argv, err, entry.Exit)
			}
			continue
		}
		if entry.Exit == 2 {
			// --version 之外的解析错误：Go 必须同样拒绝。
			if !opts.showVersion {
				t.Errorf("用例 %d（%v）: 期望解析失败，实际成功", entry.ID, entry.Argv)
			}
			continue
		}
		if opts.showVersion {
			// --version 在 argparse 里于解析阶段打印并退出 0（见 TestCLICorpusVersionOutput）。
			continue
		}
		if len(entry.Actions) == 0 {
			// 只剩「配置加载失败」这类用例：解析成功但执行阶段退出 1。
			continue
		}
		got := selectCommand(opts)
		want, diverged := pythonToGoCommand(branchAction(entry.Actions))
		// --install-service 在参照实现里也走 manage_system_service("install")，但 Go 侧
		// 用独立分支表达（main.py:182-184）。
		if entry.SystemAction != nil && *entry.SystemAction == "install" && !opts.serviceSet {
			want = commandInstallService
		}
		if got != want {
			t.Errorf("用例 %d（%v）: 分支 = %s，期望 %s（参照 %s）",
				entry.ID, entry.Argv, got, want, branchAction(entry.Actions))
		}
		if diverged && got != defaultCommand() {
			t.Errorf("用例 %d: 默认动作应当落在 defaultCommand() 上", entry.ID)
		}
		// --install-service 走的是 system-service + install（main.py:183）。
		if entry.SystemAction != nil {
			if wantArg := *entry.SystemAction; opts.serviceArgument() != wantArg {
				t.Errorf("用例 %d（%v）: service 动作 = %q，期望 %q",
					entry.ID, entry.Argv, opts.serviceArgument(), wantArg)
			}
		}
		// host/port 覆盖（含「假值不覆盖」）。
		overridden := opts.configOverrides(fixture)
		if entry.Config != nil {
			wantHost, wantPort := entry.Config.Host, entry.Config.Port
			for _, replacement := range entry.Replacements {
				if value, present := replacement["host"]; present && value != nil {
					wantHost = value.(string)
				}
				if value, present := replacement["port"]; present && value != nil {
					wantPort = int(value.(float64))
				}
			}
			if overridden.Host != wantHost || overridden.Port != wantPort {
				t.Errorf("用例 %d（%v）: 覆盖后 = %s:%d，期望 %s:%d",
					entry.ID, entry.Argv, overridden.Host, overridden.Port, wantHost, wantPort)
			}
			if entry.Config.LocalAPIKey != "" && overridden.LocalAPIKey != entry.Config.LocalAPIKey {
				t.Errorf("用例 %d: 本地 key = %q", entry.ID, overridden.LocalAPIKey)
			}
		}
		// --webui / --no-webui / --no-ops 的写配置意图。
		for _, update := range entry.Updates {
			for key, value := range update {
				switch key {
				case "webui_enabled":
					if opts.webui == nil || *opts.webui != value.(bool) {
						t.Errorf("用例 %d（%v）: webui_enabled 意图 = %v，期望 %v",
							entry.ID, entry.Argv, opts.webui, value)
					}
				case "ops_enabled":
					if opts.ops == nil || *opts.ops != value.(bool) {
						t.Errorf("用例 %d（%v）: ops_enabled 意图 = %v，期望 %v",
							entry.ID, entry.Argv, opts.ops, value)
					}
				}
			}
		}
		// 统一模型切换的实参。
		if entry.Switch != nil {
			if opts.unifiedTarget != entry.Switch.Target {
				t.Errorf("用例 %d（%v）: target = %q，期望 %q", entry.ID, entry.Argv, opts.unifiedTarget, entry.Switch.Target)
			}
			if !equalOptional(opts.switchModel, entry.Switch.Model) {
				t.Errorf("用例 %d（%v）: model = %v，期望 %v", entry.ID, entry.Argv, opts.switchModel, entry.Switch.Model)
			}
			wantKey := entry.Switch.Key
			if opts.switchKey != nil && *opts.switchKey == "auto" {
				// main.py:140：auto 传 None（恢复自动路由）。
				if wantKey != nil {
					t.Errorf("用例 %d: auto 应映射为不指定 key", entry.ID)
				}
			} else if !equalOptional(opts.switchKey, wantKey) {
				t.Errorf("用例 %d（%v）: key = %v，期望 %v", entry.ID, entry.Argv, opts.switchKey, wantKey)
			}
		}
	}
}

func equalOptional(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// TestCLICorpusExitCodes 用注入接缝跑完整流程，比对退出码与标准输出。
func TestCLICorpusExitCodes(t *testing.T) {
	corpus := loadCLICorpus(t)
	configPath := writeCLITestConfig(t)
	for _, entry := range corpus.Flags {
		argv := restoreArgs(entry.Argv, configPath)
		if entry.LoadError {
			// 配置加载失败的用例：换成一个版本号非法的配置文件，两边都应当退出 1。
			argv = brokenConfigArgs(t, entry.Argv)
		}
		dropped, isDropped := containsDroppedFlag(argv, corpus.DroppedFlags)
		var stdout, stderr strings.Builder
		served := false
		env := &cliEnv{
			out:          &stdout,
			err:          &stderr,
			argv0:        "amkr",
			clearHistory: func() {},
			service:      newCLITestService(t),
			checkUpdate:  func() updatecheck.Result { return updatecheck.Result{CurrentVersion: version} },
			serveForeground: func(string, *config.RouterConfig) int {
				served = true
				return 0
			},
			// 不注入的话会走真实实现：轮询 /health 后**真的弹出浏览器**。测试必须替换它。
			launchWebUI: func(*config.RouterConfig, io.Writer) {},
			switchUnified: func(string, *options) (*config.RouterConfig, error) {
				loaded, err := config.Load(configPath)
				return loaded, err
			},
		}
		full := append([]string{"amkr"}, argv...)
		code := runCLI(full, &stdout, &stderr, env)

		if isDropped {
			if code != 2 {
				t.Errorf("用例 %d: 砍掉的 %s 应当解析失败（退出码 2），实际 %d", entry.ID, dropped, code)
			}
			continue
		}
		if code != entry.Exit {
			t.Errorf("用例 %d（%v）: 退出码 = %d，期望 %d（stderr=%q）",
				entry.ID, entry.Argv, code, entry.Exit, stderr.String())
			continue
		}
		// --show-api-key 直出本地 key（main.py:151）。
		if len(entry.Actions) > 0 && branchAction(entry.Actions) == "show-api-key" && entry.Stdout != "" {
			if stdout.String() != entry.Stdout {
				t.Errorf("用例 %d: stdout = %q，期望 %q", entry.ID, stdout.String(), entry.Stdout)
			}
		}
		// 默认无参数：参照实现进 dashboard TUI，Go 侧是前台启动服务（开放决策）。
		if len(entry.Actions) > 0 && branchAction(entry.Actions) == "terminal-ui" && !served {
			t.Errorf("用例 %d: 默认动作应当调用前台启动（当前开放决策）", entry.ID)
		}
	}
}

// brokenConfigArgs 把 --config 的取值换成一个版本号非法的配置文件，用于回放
// 「配置加载失败 → 退出码 1」（main.py:126-127）。
func brokenConfigArgs(t *testing.T, argv []string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broken-config.json")
	if err := os.WriteFile(path, []byte(`{"config_version": 99}`), 0o666); err != nil {
		t.Fatalf("写坏配置失败: %v", err)
	}
	out := append([]string(nil), argv...)
	for index, arg := range out {
		if arg == "$CONFIG" {
			out[index] = path
		}
	}
	return out
}

// newCLITestService 构造一个不会碰宿主机的服务 Env（命令与进程全部由假接缝承担）。
func newCLITestService(t *testing.T) *service.Env {
	t.Helper()
	env := &service.Env{
		GOOS:       "windows",
		Home:       t.TempDir(),
		Cwd:        t.TempDir(),
		Executable: filepath.Join(t.TempDir(), "amkr.exe"),
		User:       "amkr-test",
		Pid:        4242,
		Getenv:     func(string) string { return "" },
		LookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		Run:        func([]string) servicestatus.CommandResult { return servicestatus.CommandResult{Stdout: "OK"} },
		Spawn:      func(service.SpawnSpec) (service.SpawnResult, error) { return service.SpawnResult{Pid: 4242}, nil },
		Running:    func(int) bool { return false },
		Terminate:  func(int) error { return nil },
		IsAdmin:    func() bool { return true },
		Now:        func() time.Time { return time.Unix(0, 0) },
		Sleep:      func(time.Duration) {},
		ArchiveLog: func(string, time.Time) (string, bool, error) { return "", false, nil },
		Get:        func(string, time.Duration) (int, []byte, error) { return 0, nil, fmt.Errorf("no server") },
	}
	return env
}

// TestCLICorpusAddressText 对拍 router_address_text（main.py:20-29）。
func TestCLICorpusAddressText(t *testing.T) {
	corpus := loadCLICorpus(t)
	for index, entry := range corpus.AddressText {
		cfg := &config.RouterConfig{Host: entry.Host, Port: entry.Port}
		if got := routerAddressText(cfg); got != entry.Expected {
			t.Errorf("用例 %d: routerAddressText = %q，期望 %q", index, got, entry.Expected)
		}
	}
}

// TestCLICorpusConfigSummary 对拍 --show-config 的「运行概览」行与模型表行。
//
// 数据来自**冻结夹具**而非生成的语料：参照实现里的渲染函数已随 dashboard.py 一起按决策 7
// 删除，无法再重新生成（见 frozenSummaryCorpus 的说明）。
func TestCLICorpusConfigSummary(t *testing.T) {
	for _, entry := range loadFrozenSummary(t) {
		raw, err := canonical.ParseString(entry.ConfigJSON)
		if err != nil {
			t.Fatalf("用例 %s: 解析配置失败: %v", entry.Name, err)
		}
		cfg, err := config.FromDict(raw)
		if err != nil {
			t.Fatalf("用例 %s: 构造配置失败: %v", entry.Name, err)
		}
		line := configSummaryLine(cfg, entry.Healthy, entry.VisitorInstalled, 100)
		if len(entry.SummaryLines) == 0 {
			t.Fatalf("用例 %s: 语料缺 summary_lines", entry.Name)
		}
		if line != entry.SummaryLines[0] {
			t.Errorf("用例 %s: 概览行 = %q，期望 %q", entry.Name, line, entry.SummaryLines[0])
		}
		rows := configModelRows(cfg)
		if len(rows) != len(entry.ModelRows) {
			t.Fatalf("用例 %s: 模型行数 = %d，期望 %d", entry.Name, len(rows), len(entry.ModelRows))
		}
		for index := range rows {
			if len(rows[index]) != len(entry.ModelRows[index]) {
				t.Fatalf("用例 %s 行 %d: 列数 = %d，期望 %d", entry.Name, index, len(rows[index]), len(entry.ModelRows[index]))
			}
			for column := range rows[index] {
				if rows[index][column] != entry.ModelRows[index][column] {
					t.Errorf("用例 %s 行 %d 列 %d: = %q，期望 %q", entry.Name, index, column,
						rows[index][column], entry.ModelRows[index][column])
				}
			}
		}
		// 公网风险面板只在 0.0.0.0 时出现（dashboard.py:903-911）。
		if (cfg.Host == "0.0.0.0") != entry.HasWarning {
			t.Errorf("用例 %s: 公网风险面板 = %v，期望 %v", entry.Name, cfg.Host == "0.0.0.0", entry.HasWarning)
		}
	}
}

// TestCLICorpusVersionCheck 对拍版本检查面板的内容（update.py:557）。
//
// 手动更新命令是刻意差异（参照实现给 pip/uv，Go 给 go install），因此语料里的命令文本
// 作为参数喂进去，其余行逐字节比较。
func TestCLICorpusVersionCheck(t *testing.T) {
	corpus := loadCLICorpus(t)
	for index, entry := range corpus.VersionCheck {
		result := updatecheck.Result{
			CurrentVersion: entry.Result.CurrentVersion,
			LatestVersion:  entry.Result.LatestVersion,
			LatestTag:      entry.Result.LatestTag,
			ReleaseURL:     entry.Result.ReleaseURL,
			Source:         entry.Result.Source,
			Error:          entry.Result.Error,
		}
		if result.UpdateAvailable() != entry.Result.UpdateAvailable {
			t.Errorf("用例 %d: update_available = %v，期望 %v", index, result.UpdateAvailable(), entry.Result.UpdateAvailable)
		}
		manual := ""
		if entry.ManualCommand != nil {
			manual = *entry.ManualCommand
		}
		lines, _ := versionCheckLines(result, manual)
		if got := strings.Join(lines, "\n"); got != entry.Content {
			t.Errorf("用例 %d: 面板内容 = %q，期望 %q", index, got, entry.Content)
		}
	}
}

// TestCLICorpusVersionOutput 对拍 --version 的输出格式（main.py:52）。
func TestCLICorpusVersionOutput(t *testing.T) {
	corpus := loadCLICorpus(t)
	var stdout, stderr strings.Builder
	env := &cliEnv{out: &stdout, err: &stderr, argv0: "amkr", clearHistory: func() {}}
	if code := runCLI([]string{"amkr", "--version"}, &stdout, &stderr, env); code != corpus.VersionOutput.Exit {
		t.Fatalf("退出码 = %d，期望 %d", code, corpus.VersionOutput.Exit)
	}
	want := strings.ReplaceAll(corpus.VersionOutput.Stdout, "$VERSION", version)
	if stdout.String() != want {
		t.Errorf("stdout = %q，期望 %q", stdout.String(), want)
	}
}

// TestDroppedFlagsAreNotDefined 具名锁定两个被砍掉的 flag。
//
// `--show-logs` 只驱动 logs_tui（决策 7）；`--restart-service-after-update` 是 update.py
// 的自更新收尾开关（决策 8），Go 版改由 `--update-helper` 助手进程自动重启，不再需要它。
// 两者 Go 侧**不定义**：解析失败、退出码 2，而不是静默忽略或假装成功。
//
// `--update` **曾**在这一列里（决策 8「取消自更新」）。该决策已被推翻、功能已恢复，
// 因此它不再属于「被砍」——语料里对应条目的动作也从 `update-dropped` 改回 `update`。
func TestDroppedFlagsAreNotDefined(t *testing.T) {
	corpus := loadCLICorpus(t)
	want := []string{"--show-logs", "--restart-service-after-update"}
	if len(corpus.DroppedFlags) != len(want) {
		t.Fatalf("语料里的被砍 flag 数 = %d，期望 %d", len(corpus.DroppedFlags), len(want))
	}
	for index, name := range want {
		if corpus.DroppedFlags[index] != name {
			t.Errorf("被砍 flag[%d] = %q，期望 %q", index, corpus.DroppedFlags[index], name)
		}
		var stdout, stderr strings.Builder
		code := runCLI([]string{"amkr", name}, &stdout, &stderr,
			&cliEnv{out: &stdout, err: &stderr, argv0: "amkr", clearHistory: func() {}})
		if code != 2 {
			t.Errorf("%s: 退出码 = %d，期望 2", name, code)
		}
	}
}

// TestDefaultActionIsOpenDecision 具名锁定「无参数默认动作」这个开放决策。
//
// 参照实现进 dashboard.run_terminal_ui（决策 7 砍掉）；Go 侧由 defaultCommand 决定，
// 当前为前台启动服务。换默认动作只需要改那一个函数。
func TestDefaultActionIsOpenDecision(t *testing.T) {
	opts, err := parseOptions(nil, os.Stderr)
	if err != nil {
		t.Fatalf("解析空参数失败: %v", err)
	}
	if got := selectCommand(opts); got != commandForeground {
		t.Errorf("默认分支 = %s，期望 foreground（开放决策，见 cli.go 的 defaultCommand）", got)
	}
	if defaultCommand() != commandForeground {
		t.Errorf("defaultCommand() = %s", defaultCommand())
	}
}

// TestConfigPathPrecedenceDivergesFromPython 具名锁定 --config 的优先级差异。
//
// 参照实现的 --config 默认是具体路径，AMKR_CONFIG 永远走不到；Go 侧留空交给
// config.ResolveConfigPath（internal/config 的语料已锁定「显式 > 环境变量 > 默认」）。
func TestConfigPathPrecedenceDivergesFromPython(t *testing.T) {
	directory := t.TempDir()
	envPath := filepath.Join(directory, "env-config.json")
	t.Setenv("AMKR_CONFIG", envPath)

	opts, err := parseOptions(nil, os.Stderr)
	if err != nil {
		t.Fatalf("解析空参数失败: %v", err)
	}
	if opts.configPath != "" {
		t.Fatalf("--config 默认值 = %q，期望空串（交给 ResolveConfigPath）", opts.configPath)
	}
	resolved, err := config.ResolveConfigPath(opts.configPath)
	if err != nil {
		t.Fatalf("解析配置路径失败: %v", err)
	}
	if resolved != envPath {
		t.Errorf("解析结果 = %q，期望 $AMKR_CONFIG = %q", resolved, envPath)
	}
}
