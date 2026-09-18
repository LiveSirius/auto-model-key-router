package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/servicestatus"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// 本文件回放 gen_service_corpus.py（已随 Python 退役移除） 产出的语料。
//
// 语料来自**真实 Python**：service.py 的全部外部依赖（subprocess / urlopen / time /
// platform / Path.home / Path.cwd / os.getlogin / archive_current_log）在生成时被换成
// 脚本化桩，因此「执行了哪些命令、命令参数是什么、面板文本是什么」都是参照实现的
// 真实输出。
//
// 两条比较规则：
//
//  1. **命令与文本字段逐字节比较**（路径经占位符还原后）。
//  2. **面板文本按行比较、忽略行内空白**。原因：面板内边距按显示宽度补齐，而语料里的
//     路径被替换成占位符（长度与 Go 测试的临时目录不同），内边距因此必然不同。版式
//     本身已由 internal/tui 的对拍语料逐字节锁定，这里只对拍内容与行数。
//
// 所有测试都在临时目录里构造 Env，绝不触碰宿主机的计划任务/systemd/进程。

// —— 语料装载 ——

// corpusPlaceholders 同时持有「占位符 → 本次测试的真实路径」与
// 「占位符 → 生成语料时 Python 的真实路径」，用于在两个方向做替换。
type corpusPlaceholders struct {
	goValues     map[string]string
	pythonValues map[string]string
	keys         []string
}

// newPlaceholders 建夹具目录，并从语料的 placeholders 段读出 Python 侧的真实路径。
func newPlaceholders(t *testing.T, sections map[string]json.RawMessage) *corpusPlaceholders {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "cwd")
	python := filepath.Join(root, "python", "python.exe")
	logs := filepath.Join(root, "logs", "server.log")
	bin := filepath.Join(root, "bin", "amkr")
	for _, directory := range []string{home, cwd, filepath.Dir(python), filepath.Dir(logs), filepath.Dir(bin)} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("创建夹具目录 %s 失败: %v", directory, err)
		}
	}
	for _, file := range []string{python, bin} {
		if err := os.WriteFile(file, nil, 0o666); err != nil {
			t.Fatalf("创建夹具文件 %s 失败: %v", file, err)
		}
	}
	configPath := filepath.Join(root, "router-config.json")
	writeCorpusConfig(t, configPath, logs)

	raw, present := sections["placeholders"]
	if !present {
		t.Fatalf("语料缺少 placeholders 段")
	}
	pythonValues := map[string]string{}
	if err := json.Unmarshal(raw, &pythonValues); err != nil {
		t.Fatalf("解析 placeholders 失败: %v", err)
	}

	placeholders := &corpusPlaceholders{
		goValues: map[string]string{
			"$HOME":    home,
			"$CWD":     cwd,
			"$CONFIG":  configPath,
			"$EXE":     python,
			"$LOG":     logs,
			"$BIN":     bin,
			"$ARCHIVE": filepath.Join(filepath.Dir(logs), "server.20260102-030405.log"),
		},
		pythonValues: pythonValues,
	}
	for key := range placeholders.goValues {
		placeholders.keys = append(placeholders.keys, key)
	}
	// 长路径优先，避免 $LOG 与 $HOME 之类互为前缀时错配。
	sort.Slice(placeholders.keys, func(i, j int) bool {
		return len(placeholders.goValues[placeholders.keys[i]]) > len(placeholders.goValues[placeholders.keys[j]])
	})
	return placeholders
}

// restore 把真实路径换回占位符，便于与语料比较。
func (p *corpusPlaceholders) restore(value string) string {
	text := value
	for _, key := range p.keys {
		text = strings.ReplaceAll(text, p.goValues[key], key)
	}
	return text
}

// denormalize 把语料里的 Python 真实路径与占位符换成本次测试的真实路径（用于输入字段）。
func (p *corpusPlaceholders) denormalize(value string) string {
	text := value
	for key, pythonPath := range p.pythonValues {
		if _, present := p.goValues[key]; present {
			text = strings.ReplaceAll(text, pythonPath, key)
		}
	}
	for _, key := range p.keys {
		text = strings.ReplaceAll(text, key, p.goValues[key])
	}
	return text
}

// get 返回本次测试里某个占位符对应的真实路径。
func (p *corpusPlaceholders) get(key string) string { return p.goValues[key] }

// configPath 返回本次测试的配置文件路径。
func (p *corpusPlaceholders) configPath() string { return p.goValues["$CONFIG"] }

// home 返回本次测试的 HOME。
func (p *corpusPlaceholders) home() string { return p.goValues["$HOME"] }

// logPath 返回本次测试的日志路径。
func (p *corpusPlaceholders) logPath() string { return p.goValues["$LOG"] }

// pidFilePath 返回本次测试的 PID 文件路径（与语料中的 $LOG 同目录）。
func (p *corpusPlaceholders) pidFilePath() string {
	return filepath.Join(filepath.Dir(p.goValues["$LOG"]), "server.pid")
}

// systemdUnitPath 返回本次测试的 systemd unit 路径。
func (p *corpusPlaceholders) systemdUnitPath() string {
	return filepath.Join(p.home(), ".config", "systemd", "user", SystemdUserServiceName)
}

// writeCorpusConfig 写出一份与语料同值的配置（本地 key 与监听地址必须一致，
// 否则面板里的 key 指纹与地址会对不上）。
func writeCorpusConfig(t *testing.T, path, logPath string) {
	t.Helper()
	data, err := config.EmptyConfigDict()
	if err != nil {
		t.Fatalf("构造空配置失败: %v", err)
	}
	data.SetKey("log_file_path", canonical.NewString(logPath))
	data.SetKey("local_api_key", canonical.NewString("amkr-corpus-key"))
	data.SetKey("host", canonical.NewString("127.0.0.1"))
	data.SetKey("port", canonical.NewIntValue(8123))
	if err := config.SaveConfigData(path, data); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
}

// corpusTable 是按段装载的语料：每个元素是原始 JSON 对象。
type corpusTable []map[string]any

// loadServiceCorpus 读取语料文件。
func loadServiceCorpus(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "service_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	return sections
}

// section 解出一段语料为表；对象形式的段会被包成单元素表。
func section(t *testing.T, sections map[string]json.RawMessage, name string) corpusTable {
	t.Helper()
	raw, present := sections[name]
	if !present {
		t.Fatalf("语料缺少分段 %q", name)
	}
	var table corpusTable
	if err := json.Unmarshal(raw, &table); err != nil {
		var single map[string]any
		if err2 := json.Unmarshal(raw, &single); err2 != nil {
			t.Fatalf("解析分段 %q 失败: %v", name, err)
		}
		table = corpusTable{single}
	}
	if len(table) == 0 {
		t.Fatalf("分段 %q 为空", name)
	}
	return table
}

// —— 比较助手 ——

// normalizePanel 按行比较时忽略行内空白（见文件头第 2 条规则）。
func normalizePanel(text string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Join(lines, "\n")
}

// normalizePath 统一路径分隔符，让语料在 Windows/Linux 上都能重放。
func normalizePath(value string) string { return strings.ReplaceAll(value, "\\", "/") }

func fieldString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	value, present := entry[key]
	if !present || value == nil {
		t.Fatalf("语料字段 %q 缺失", key)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("语料字段 %q 类型不是字符串: %T", key, value)
	}
	return text
}

func optionalString(entry map[string]any, key string) (string, bool) {
	value, present := entry[key]
	if !present || value == nil {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return text, true
}

func fieldBool(t *testing.T, entry map[string]any, key string) bool {
	t.Helper()
	value, present := entry[key]
	if !present {
		t.Fatalf("语料字段 %q 缺失", key)
	}
	flag, ok := value.(bool)
	if !ok {
		t.Fatalf("语料字段 %q 类型不是布尔: %T", key, value)
	}
	return flag
}

func fieldInt(t *testing.T, entry map[string]any, key string) int {
	t.Helper()
	value, present := entry[key]
	if !present || value == nil {
		t.Fatalf("语料字段 %q 缺失", key)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("语料字段 %q 类型不是数字: %T", key, value)
	}
	return int(number)
}

func optionalInt(entry map[string]any, key string) (int, bool) {
	value, present := entry[key]
	if !present || value == nil {
		return 0, false
	}
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	return int(number), true
}

// stringList 把语料里的字符串数组转成 []string。
func stringList(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("期望字符串数组，实际 %T", value)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("数组元素不是字符串: %T", item)
		}
		out = append(out, text)
	}
	return out
}

// commandList 解出「命令数组的数组」。
func commandList(t *testing.T, entry map[string]any, key string) [][]string {
	t.Helper()
	value, present := entry[key]
	if !present || value == nil {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("语料字段 %q 不是数组: %T", key, value)
	}
	out := make([][]string, 0, len(items))
	for _, item := range items {
		out = append(out, stringList(t, item))
	}
	return out
}

// —— 假接缝 ——

// fakeRunner 复刻生成脚本里的 FakeRun：按脚本返回，脚本只剩一条时重复使用，
// 空脚本返回零值结果。
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

// scriptFromCorpus 把语料里的 {code,stdout,stderr} 数组转成假执行脚本。
func scriptFromCorpus(t *testing.T, value any) []servicestatus.CommandResult {
	t.Helper()
	if value == nil {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("脚本不是数组: %T", value)
	}
	out := make([]servicestatus.CommandResult, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("脚本元素不是对象: %T", item)
		}
		result := servicestatus.CommandResult{}
		if code, present := entry["code"]; present {
			result.Code = int(code.(float64))
		}
		if stdout, present := entry["stdout"]; present && stdout != nil {
			result.Stdout = stdout.(string)
		}
		if stderr, present := entry["stderr"]; present && stderr != nil {
			result.Stderr = stderr.(string)
		}
		out = append(out, result)
	}
	return out
}

// fakeSpawn 记录 SpawnSpec 并返回固定 PID。
type fakeSpawn struct {
	pid   int
	specs []SpawnSpec
}

func (f *fakeSpawn) spawn(spec SpawnSpec) (SpawnResult, error) {
	f.specs = append(f.specs, spec)
	return SpawnResult{Pid: f.pid}, nil
}

// httpEntry 是一条脚本化 HTTP 响应。
type httpEntry struct {
	status int
	body   string
	// fail 为真时返回错误（对应 Python 侧抛 OSError）。
	fail bool
}

type httpCall struct {
	url     string
	timeout time.Duration
}

// fakeHTTP 复刻生成脚本里的 FakeURL。
type fakeHTTP struct {
	script []httpEntry
	calls  []httpCall
}

func newFakeHTTP(entries []httpEntry) *fakeHTTP { return &fakeHTTP{script: entries} }

func (f *fakeHTTP) get(url string, timeout time.Duration) (int, []byte, error) {
	f.calls = append(f.calls, httpCall{url: url, timeout: timeout})
	var entry httpEntry
	switch len(f.script) {
	case 0:
		entry = httpEntry{status: 200, body: "{}"}
	case 1:
		entry = f.script[0]
	default:
		entry = f.script[0]
		f.script = f.script[1:]
	}
	if entry.fail {
		return 0, nil, fmt.Errorf("connection refused")
	}
	return entry.status, []byte(entry.body), nil
}

// httpEntriesFromCorpus 解出 {status,body} 或 {error} 形式的 HTTP 脚本。
func httpEntriesFromCorpus(t *testing.T, value any) []httpEntry {
	t.Helper()
	if value == nil {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("HTTP 脚本不是数组: %T", value)
	}
	out := make([]httpEntry, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("HTTP 脚本元素不是对象: %T", item)
		}
		record := httpEntry{}
		if _, failed := entry["error"]; failed {
			record.fail = true
		}
		if status, present := entry["status"]; present {
			record.status = int(status.(float64))
		}
		if body, present := entry["body"]; present && body != nil {
			record.body = body.(string)
		}
		out = append(out, record)
	}
	return out
}

// fakeClock 复刻生成脚本里的 FakeClock。
type fakeClock struct {
	values []float64
	index  int
	slept  []time.Duration
}

func (f *fakeClock) now() time.Time {
	value := 0.0
	if len(f.values) > 0 {
		if f.index < len(f.values) {
			value = f.values[f.index]
			f.index++
		} else {
			value = f.values[len(f.values)-1]
		}
	}
	// 用纳秒承载「单调秒」，Go 侧的 TTL 比较按秒换算。
	return time.Unix(0, int64(value*float64(time.Second)))
}

func (f *fakeClock) sleep(duration time.Duration) { f.slept = append(f.slept, duration) }

// newTestEnv 构造一个只指向临时目录的 Env。
func newTestEnv(t *testing.T, placeholders *corpusPlaceholders, goos string) *Env {
	t.Helper()
	env := &Env{
		GOOS:       goos,
		Home:       placeholders.home(),
		Cwd:        placeholders.get("$CWD"),
		Executable: placeholders.get("$EXE"),
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

// equalCommands 逐元素比较命令表。
func equalCommands(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if !equalStrings(got[index], want[index]) {
			return false
		}
	}
	return true
}

// —— 各段用例 ——

// TestCorpusPidFilePath 锁定 PID 文件路径推导（service.py:324）。
func TestCorpusPidFilePath(t *testing.T) {
	sections := loadServiceCorpus(t)
	for index, entry := range section(t, sections, "pid_file_path") {
		logPath := fieldString(t, entry, "log_file_path")
		want := fieldString(t, entry, "expected")
		got := PidFilePath(&config.RouterConfig{LogFilePath: logPath})
		if normalizePath(got) != normalizePath(want) {
			t.Errorf("用例 %d: PidFilePath(%q) = %q，期望 %q", index, logPath, got, want)
		}
	}
}

// TestCorpusReadPid 锁定 PID 文件解析（service.py:328）。
func TestCorpusReadPid(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	target := filepath.Join(placeholders.home(), "read-pid.txt")
	for index, entry := range section(t, sections, "read_pid") {
		content, present := entry["content"].(string)
		if present {
			if err := os.WriteFile(target, []byte(content), 0o666); err != nil {
				t.Fatalf("写 PID 文件失败: %v", err)
			}
		} else {
			_ = os.Remove(target)
		}
		got, ok := ReadPid(target)
		want, wantPresent := optionalInt(entry, "expected")
		if ok != wantPresent || got != want {
			t.Errorf("用例 %d（content=%v）: ReadPid = (%d,%v)，期望 (%d,%v)",
				index, entry["content"], got, ok, want, wantPresent)
		}
	}
}

// TestCorpusProcessRunningWindows 锁定 tasklist 的 CSV 匹配规则（service.py:337-342）。
func TestCorpusProcessRunningWindows(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "process_running_windows") {
		env := newTestEnv(t, placeholders, "windows")
		runner := newFakeRunner([]servicestatus.CommandResult{
			{Stdout: fieldString(t, entry, "tasklist_stdout")},
		})
		env.Run = runner.run
		env.Running = env.windowsRunning
		pid := fieldInt(t, entry, "pid")
		if got := env.IsProcessRunning(pid); got != fieldBool(t, entry, "expected") {
			t.Errorf("用例 %d（pid=%d）: IsProcessRunning = %v，期望 %v", index, pid, got, entry["expected"])
		}
		wantCommand := stringList(t, entry["command"])
		if len(runner.calls) != 1 || !equalStrings(runner.calls[0], wantCommand) {
			t.Errorf("用例 %d: tasklist 命令 = %v，期望 %v", index, runner.calls, wantCommand)
		}
	}
}

// TestCorpusPowershellQuote 锁定 powershell_quote（service.py:520）。
func TestCorpusPowershellQuote(t *testing.T) {
	sections := loadServiceCorpus(t)
	for index, entry := range section(t, sections, "powershell_quote") {
		got := PowershellQuote(fieldString(t, entry, "value"))
		if want := fieldString(t, entry, "expected"); got != want {
			t.Errorf("用例 %d: PowershellQuote = %q，期望 %q", index, got, want)
		}
	}
}

// TestCorpusShlexJoin 锁定 shlex.join 的引号规则（systemd ExecStart 依赖它）。
func TestCorpusShlexJoin(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "shlex_join") {
		args := stringList(t, entry["args"])
		for position, arg := range args {
			args[position] = placeholders.denormalize(arg)
		}
		got := placeholders.restore(ShlexJoin(args))
		if want := fieldString(t, entry, "expected"); got != want {
			t.Errorf("用例 %d: ShlexJoin = %q，期望 %q", index, got, want)
		}
	}
}

// TestCorpusWindowsTaskRegistered 锁定「两条查询任一为 0 即算注册」（service.py:228）。
func TestCorpusWindowsTaskRegistered(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "windows_task_registered") {
		env := newTestEnv(t, placeholders, "windows")
		runner := newFakeRunner([]servicestatus.CommandResult{
			{Code: fieldInt(t, entry, "list_code")},
			{Code: fieldInt(t, entry, "xml_code")},
		})
		env.Run = runner.run
		if got := env.IsWindowsTaskRegistered(); got != fieldBool(t, entry, "expected") {
			t.Errorf("用例 %d: IsWindowsTaskRegistered = %v，期望 %v", index, got, entry["expected"])
		}
		if !equalCommands(runner.calls, commandList(t, entry, "commands")) {
			t.Errorf("用例 %d: 命令 = %v，期望 %v", index, runner.calls, entry["commands"])
		}
	}
}

// TestCorpusConsoleScript 锁定 console script 查找规则（service.py:533）。
func TestCorpusConsoleScript(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for index, entry := range section(t, sections, "console_script") {
		env := newTestEnv(t, placeholders, "linux")
		env.Executable = placeholders.denormalize(fieldString(t, entry, "argv0"))
		which := map[string]string{}
		if raw, present := entry["which"].(map[string]any); present {
			for key, value := range raw {
				which[key] = placeholders.denormalize(value.(string))
			}
		}
		env.LookPath = func(name string) (string, error) {
			if found, present := which[name]; present {
				return found, nil
			}
			return "", os.ErrNotExist
		}
		got, found := env.ConsoleScriptExecutable()
		want, wantPresent := optionalString(entry, "expected")
		if found != wantPresent {
			t.Errorf("用例 %d: 命中 = %v，期望 %v", index, found, wantPresent)
			continue
		}
		if found && normalizePath(placeholders.restore(got)) != normalizePath(want) {
			t.Errorf("用例 %d: 路径 = %q，期望 %q", index, placeholders.restore(got), want)
		}
	}
}

// TestCorpusNotes 锁定权限说明文案（service.py:363/374）。
func TestCorpusNotes(t *testing.T) {
	sections := loadServiceCorpus(t)
	for index, entry := range section(t, sections, "notes") {
		goos := strings.ToLower(fieldString(t, entry, "platform"))
		got := normalizePanel(RenderText(serviceRegistrationNotePanel(goos)))
		if want := normalizePanel(fieldString(t, entry, "expected")); got != want {
			t.Errorf("用例 %d: 面板 = %q，期望 %q", index, got, want)
		}
	}
}

// TestCorpusConstants 锁定与参照实现同值的常量。
func TestCorpusConstants(t *testing.T) {
	if renderWidth != 100 {
		t.Fatalf("renderWidth = %d，期望 100", renderWidth)
	}
	if serviceStatusCacheTTL != 2*time.Second {
		t.Fatalf("serviceStatusCacheTTL = %v，期望 2s", serviceStatusCacheTTL)
	}
	if healthyTimeout != 100*time.Millisecond || healthDetailTimeout != 500*time.Millisecond {
		t.Fatalf("健康检查超时 = %v/%v，期望 100ms/500ms", healthyTimeout, healthDetailTimeout)
	}
	if backgroundStopPollCount != 20 || backgroundStopPollInterval != 100*time.Millisecond {
		t.Fatalf("停止轮询 = %d x %v，期望 20 x 100ms", backgroundStopPollCount, backgroundStopPollInterval)
	}
	if WindowsTaskName != "AutoModelKeyRouter" {
		t.Fatalf("WindowsTaskName = %q", WindowsTaskName)
	}
	if SystemdUserServiceName != "auto-model-key-router.service" {
		t.Fatalf("SystemdUserServiceName = %q", SystemdUserServiceName)
	}
}

// TestCorpusStatusRows 锁定状态表的行数据与详情面板（service.py:276-311）。
func TestCorpusStatusRows(t *testing.T) {
	sections := loadServiceCorpus(t)
	for index, entry := range section(t, sections, "status_render") {
		registered := fieldBool(t, entry, "registered")
		rawRows, ok := entry["rows"].([]any)
		if !ok {
			t.Fatalf("用例 %d: rows 不是数组", index)
		}
		rows := make([]servicestatus.Row, 0, len(rawRows))
		for _, item := range rawRows {
			pair := stringList(t, item)
			rows = append(rows, servicestatus.Row{Label: pair[0], Value: pair[1]})
		}
		gotRows := StatusRows(servicestatus.SystemServiceStatus{Registered: registered, Rows: rows})
		wantRaw, ok := entry["expected_rows"].([]any)
		if !ok {
			t.Fatalf("用例 %d: expected_rows 不是数组", index)
		}
		if len(gotRows) != len(wantRaw) {
			t.Fatalf("用例 %d: 行数 = %d，期望 %d", index, len(gotRows), len(wantRaw))
		}
		for rowIndex, item := range wantRaw {
			pair := stringList(t, item)
			if gotRows[rowIndex].Label != pair[0] || gotRows[rowIndex].Value != pair[1] {
				t.Errorf("用例 %d 行 %d: = (%q,%q)，期望 (%q,%q)", index, rowIndex,
					gotRows[rowIndex].Label, gotRows[rowIndex].Value, pair[0], pair[1])
			}
		}
	}
}

// TestCorpusStatusDetailPanels 对拍状态详情面板（内容为纯文本，可逐字节比较）。
func TestCorpusStatusDetailPanels(t *testing.T) {
	sections := loadServiceCorpus(t)
	contents := []string{"RAW TEXT", "未找到计划任务，或 schtasks 不可用。"}
	titles := []string{"Windows 原始状态", "其他"}
	styles := []string{"blue", "yellow"}
	for index, entry := range section(t, sections, "status_render") {
		detailRaw, ok := entry["detail_panels"].([]any)
		if !ok {
			t.Fatalf("用例 %d: detail_panels 不是数组", index)
		}
		if len(detailRaw) != len(contents) {
			t.Fatalf("用例 %d: 详情数 = %d，期望 %d", index, len(detailRaw), len(contents))
		}
		for detailIndex, item := range detailRaw {
			got := normalizePanel(RenderText(tui.SectionPanel(contents[detailIndex], titles[detailIndex], styles[detailIndex])))
			if want := normalizePanel(item.(string)); got != want {
				t.Errorf("用例 %d 详情 %d: 面板 = %q，期望 %q", index, detailIndex, got, want)
			}
		}
	}
}

// TestCorpusStatusTableShape 锁定 Go 侧状态表的列数与行数（版式差异见文件头）。
func TestCorpusStatusTableShape(t *testing.T) {
	table, ok := statusTable([]servicestatus.Row{{Label: "平台", Value: "x"}}).(tui.Table)
	if !ok {
		t.Fatalf("statusTable 返回类型 = %T，期望 tui.Table", statusTable(nil))
	}
	if len(table.Columns) != 2 || len(table.Rows) != 1 {
		t.Fatalf("表格形状 = %d 列 x %d 行，期望 2 x 1", len(table.Columns), len(table.Rows))
	}
}

// TestCorpusPlatformDispatch 锁定不支持平台的提示（service.py:214-216/358-360）。
func TestCorpusPlatformDispatch(t *testing.T) {
	sections := loadServiceCorpus(t)
	placeholders := newPlaceholders(t, sections)
	for _, goos := range []string{"windows", "linux"} {
		env := newTestEnv(t, placeholders, goos)
		env.Run = newFakeRunner(nil).run
		if text := RenderText(env.SystemServiceStatusPanel(placeholders.configPath())); text == "" {
			t.Errorf("平台 %s 的状态面板为空", goos)
		}
	}
	env := newTestEnv(t, placeholders, "darwin")
	env.Run = newFakeRunner(nil).run
	text := normalizePanel(RenderText(env.SystemServiceStatusPanel(placeholders.configPath())))
	if !strings.Contains(text, "暂不支持当前系统自动注册: Darwin") {
		t.Errorf("darwin 状态面板 = %q", text)
	}
	panel, err := env.ManageSystemService(placeholders.configPath(), "install")
	if err != nil {
		t.Fatalf("ManageSystemService(darwin) 报错: %v", err)
	}
	if text := normalizePanel(RenderText(panel)); !strings.Contains(text, "暂不支持当前系统自动注册: Darwin") {
		t.Errorf("darwin 服务面板 = %q", text)
	}
	if env.IsSystemServiceRegistered(placeholders.configPath()) {
		t.Errorf("darwin 的 IsSystemServiceRegistered 应为假")
	}
}
