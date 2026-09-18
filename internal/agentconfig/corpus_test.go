package agentconfig

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件重放 gen_agent_config_corpus.py（已随 Python 退役移除） 生成的对拍语料。语料由**真实
// 运行** Python 侧的 auto_model_key_router/agent_config.py 得到——它是参照实现，
// 不是手写的期望值。每条断言失败都指向一处真实的兼容性回归。
//
// 语料里的绝对路径统一写成 "<root>/..." 且剩余部分用 "/" 分隔；测试把 <root>
// 换成自己的 t.TempDir()，并对 Go 侧产出的文本做同样的换算再比对。

const corpusFile = "testdata/agentconfig_corpus.json"

// fileSpec 是一个文件的期望状态：文本、十六进制字节，或不存在。
type fileSpec struct {
	Text    *string `json:"text"`
	Hex     *string `json:"hex"`
	Missing bool    `json:"missing"`
}

// bytes 返回期望的字节内容；Missing 时返回 (nil, false)。
func (f fileSpec) bytes(t *testing.T) ([]byte, bool) {
	t.Helper()
	switch {
	case f.Missing:
		return nil, false
	case f.Text != nil:
		return []byte(*f.Text), true
	case f.Hex != nil:
		raw, err := hex.DecodeString(*f.Hex)
		if err != nil {
			t.Fatalf("语料里的 hex 非法: %v", err)
		}
		return raw, true
	}
	t.Fatal("语料里的文件期望既没有 text/hex 也没有 missing")
	return nil, false
}

// resultSpec 对应 Python 侧的 AgentConfigResult。
type resultSpec struct {
	RouterURL        string   `json:"router_url"`
	ExtraTargetPaths []string `json:"extra_target_paths"`
	Restored         bool     `json:"restored"`
	Mode             *string  `json:"mode"`
}

// statusSpec 对应 Python 侧的 AgentConfigStatus。
type statusSpec struct {
	TargetPath       string  `json:"target_path"`
	BackupPath       string  `json:"backup_path"`
	BackupAvailable  *bool   `json:"backup_available"`
	CurrentIsApplied *bool   `json:"current_is_applied"`
	Mode             *string `json:"mode"`
}

// configureCase 是 op=configure / configure_twice / status 的一条用例。
type configureCase struct {
	Name       string              `json:"name"`
	Op         string              `json:"op"`
	Agent      string              `json:"agent"`
	Mode       string              `json:"mode"`
	Config     string              `json:"config"`
	TargetRel  string              `json:"target_rel"`
	Note       string              `json:"note"`
	Divergence string              `json:"divergence"`
	ErrorMatch string              `json:"error_match"`
	Initial    *fileSpec           `json:"initial"`
	ExtraInit  map[string]fileSpec `json:"extra_initial"`
	WantError  string              `json:"want_error"`
	WantTarget *fileSpec           `json:"want_target"`
	WantExtra  map[string]fileSpec `json:"want_extra"`
	WantBackup *string             `json:"want_backup"`
	WantResult *resultSpec         `json:"want_result"`
	WantStatus *statusSpec         `json:"want_status"`
	Rollback   bool                `json:"rollback"`
	WantRoll   *resultSpec         `json:"want_rollback"`
	WantAfter  map[string]fileSpec `json:"want_after_rollback"`
	WantGone   *bool               `json:"want_backup_gone"`
	SecondMode string              `json:"second_mode"`
	AfterFirst *fileSpec           `json:"after_first"`
	BeforeOK   *bool               `json:"before_backup_available"`
	BeforeCur  *bool               `json:"before_current_is_applied"`
}

// pathCase 是 op=path 的一条用例。
type pathCase struct {
	Name      string            `json:"name"`
	Env       map[string]string `json:"env"`
	Inputs    []string          `json:"inputs"`
	Want      *string           `json:"want"`
	WantError string            `json:"want_error"`
	WantType  string            `json:"want_error_type"`
}

// rollbackCase 是独立回退用例。
type rollbackCase struct {
	Name string `json:"name"`
	// BackupText 是备份文件内容模板，@TARGET@ / @OTHER@ 由测试替换成自己的
	// 临时路径（写死绝对路径会让语料与平台绑定）。
	BackupText string      `json:"backup_text"`
	Agent      string      `json:"agent"`
	TargetRel  string      `json:"target_rel"`
	Note       string      `json:"note"`
	WantError  string      `json:"want_error"`
	ErrorMatch string      `json:"error_match"`
	WantResult *resultSpec `json:"want_result"`
	WantTarget *fileSpec   `json:"want_target"`
	WantGone   *bool       `json:"want_backup_gone"`
}

// envCase 是环境变量覆盖下的落点校验。
type envCase struct {
	Name       string            `json:"name"`
	Agent      string            `json:"agent"`
	Env        map[string]string `json:"env"`
	WantTarget string            `json:"want_target_path"`
	WantFile   *fileSpec         `json:"want_target"`
	WantStatus *bool             `json:"want_status_available"`
}

// corpus 是整份语料。
type corpus struct {
	Configs   map[string]json.RawMessage `json:"configs"`
	Paths     []pathCase                 `json:"paths"`
	Configure []configureCase            `json:"configure"`
	Rollback  []rollbackCase             `json:"rollback"`
	Env       []envCase                  `json:"env"`
}

// loadCorpus 读取并解析语料。
func loadCorpus(t *testing.T) corpus {
	t.Helper()
	content, err := os.ReadFile(corpusFile)
	if err != nil {
		t.Fatalf("读取语料失败（语料已冻结并随仓库提交，生成器已随 Python 退役移除）: %v", err)
	}
	var data corpus
	if err := json.Unmarshal(content, &data); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	if len(data.Configure) == 0 || len(data.Paths) == 0 {
		t.Fatal("语料为空，生成器没有产出用例")
	}
	return data
}

// routerConfig 用 internal/config 解析语料里的原始配置。
//
// 刻意让 Go 侧也走 from_dict：两个实现都从**同一份原始 JSON** 推导配置，
// 这样语料同时覆盖了配置解析的一致性。
func (c corpus) routerConfig(t *testing.T, name string) *config.RouterConfig {
	t.Helper()
	raw, ok := c.Configs[name]
	if !ok {
		t.Fatalf("语料里没有配置 %q", name)
	}
	value, err := canonical.Parse(raw)
	if err != nil {
		t.Fatalf("解析配置 %q 失败: %v", name, err)
	}
	parsed, err := config.FromDict(value)
	if err != nil {
		t.Fatalf("配置 %q 无法通过 Go 侧校验: %v", name, err)
	}
	return parsed
}

// ─────────────────────────── 路径换算辅助 ───────────────────────────

// corpusPath 把 "<root>/a/b" 换算成本次测试的绝对路径。
//
// 直接拼接而不是 filepath.Join：语料里有 "<root>/a/../b/c" 这种刻意保留
// ".." 的输入，Join 会先做词法规范化而抹掉被测量的行为。
func corpusPath(root, portable string) string {
	if !strings.HasPrefix(portable, "<root>") {
		return portable
	}
	return root + filepath.FromSlash(strings.TrimPrefix(portable, "<root>"))
}

// portablePath 是生成器里 portable_path 的 Go 版本。
func portablePath(root, value string) string {
	if !strings.HasPrefix(value, root) {
		return value
	}
	return "<root>" + strings.ReplaceAll(value[len(root):], `\`, "/")
}

// portableText 是生成器里 portable_text 的 Go 版本，用于同时比对备份 JSON 的
// 序列化细节（键序、ensure_ascii、末尾换行）与路径占位。
func portableText(root, text string) string {
	marker := "\x00"
	escapedRoot := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(root)
	out := strings.ReplaceAll(text, escapedRoot, marker)
	out = strings.ReplaceAll(out, root, marker)

	var b strings.Builder
	index := 0
	for {
		found := strings.Index(out[index:], marker)
		if found < 0 {
			b.WriteString(out[index:])
			break
		}
		found += index
		b.WriteString(out[index:found])
		b.WriteString("<root>")
		cursor := found + len(marker)
		for cursor < len(out) && !strings.ContainsRune("\n\r\t\"' ,}]", rune(out[cursor])) {
			char := out[cursor]
			if char == '\\' && cursor+1 < len(out) && out[cursor+1] == '\\' {
				b.WriteByte('/')
				cursor += 2
				continue
			}
			if char == '\\' || char == '/' {
				b.WriteByte('/')
				cursor++
				continue
			}
			b.WriteByte(char)
			cursor++
		}
		index = cursor
	}
	return b.String()
}

// assertFile 断言磁盘上的文件与语料期望一致。
func assertFile(t *testing.T, path string, want fileSpec) {
	t.Helper()
	wantBytes, exists := want.bytes(t)
	got, err := os.ReadFile(path)
	if !exists {
		if err == nil {
			t.Errorf("文件 %s 应不存在，实际内容:\n%s", path, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if string(got) != string(wantBytes) {
		t.Errorf("文件 %s 与 Python 输出不一致\n期望 %d 字节:\n%q\n实际 %d 字节:\n%q",
			path, len(wantBytes), wantBytes, len(got), got)
	}
}

// writeFiles 写入用例的初始文件。
func writeFiles(t *testing.T, workdir, rel string, spec *fileSpec, extras map[string]fileSpec) {
	t.Helper()
	write := func(target string, item fileSpec) {
		content, exists := item.bytes(t)
		if !exists {
			return
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			t.Fatalf("写初始文件失败: %v", err)
		}
	}
	if spec != nil {
		write(filepath.Join(workdir, filepath.FromSlash(rel)), *spec)
	}
	for extraRel, extraSpec := range extras {
		write(filepath.Join(workdir, filepath.FromSlash(extraRel)), extraSpec)
	}
}

// assertResult 比对 AgentConfigResult。
func assertResult(t *testing.T, root string, want *resultSpec, got Result) {
	t.Helper()
	if want == nil {
		return
	}
	if got.RouterURL != want.RouterURL {
		t.Errorf("router_url 期望 %q，实际 %q", want.RouterURL, got.RouterURL)
	}
	if got.Restored != want.Restored {
		t.Errorf("restored 期望 %v，实际 %v", want.Restored, got.Restored)
	}
	wantMode := ""
	if want.Mode != nil {
		wantMode = *want.Mode
	}
	if got.Mode != wantMode {
		t.Errorf("mode 期望 %q，实际 %q", wantMode, got.Mode)
	}
	if len(got.ExtraTargetPaths) != len(want.ExtraTargetPaths) {
		t.Fatalf("附加目标数期望 %d，实际 %d (%v)",
			len(want.ExtraTargetPaths), len(got.ExtraTargetPaths), got.ExtraTargetPaths)
	}
	for i, path := range want.ExtraTargetPaths {
		if portablePath(root, got.ExtraTargetPaths[i]) != path {
			t.Errorf("附加目标[%d] 期望 %q，实际 %q", i, path, got.ExtraTargetPaths[i])
		}
	}
}

// assertStatus 比对 AgentConfigStatus。
func assertStatus(t *testing.T, root string, want *statusSpec, got Status) {
	t.Helper()
	if want == nil {
		return
	}
	if want.TargetPath != "" && portablePath(root, got.TargetPath) != want.TargetPath {
		t.Errorf("status.target_path 期望 %q，实际 %q", want.TargetPath, got.TargetPath)
	}
	if want.BackupPath != "" && portablePath(root, got.BackupPath) != want.BackupPath {
		t.Errorf("status.backup_path 期望 %q，实际 %q", want.BackupPath, got.BackupPath)
	}
	if want.BackupAvailable != nil && got.BackupAvailable != *want.BackupAvailable {
		t.Errorf("status.backup_available 期望 %v，实际 %v", *want.BackupAvailable, got.BackupAvailable)
	}
	if want.CurrentIsApplied != nil && got.CurrentIsApplied != *want.CurrentIsApplied {
		t.Errorf("status.current_is_applied 期望 %v，实际 %v", *want.CurrentIsApplied, got.CurrentIsApplied)
	}
	wantMode := ""
	if want.Mode != nil {
		wantMode = *want.Mode
	}
	if got.Mode != wantMode {
		t.Errorf("status.mode 期望 %q，实际 %q", wantMode, got.Mode)
	}
}

// expectError 按语料的匹配模式校验错误。
func expectError(t *testing.T, c configureCase, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错，实际成功（Python 侧错误: %s）", c.WantError)
	}
	var configErr *ConfigError
	switch c.ErrorMatch {
	case "exact":
		if !errors.As(err, &configErr) {
			t.Fatalf("期望 *ConfigError，实际 %T: %v", err, err)
		}
		if configErr.Message != c.WantError {
			t.Errorf("错误文本与 Python 不一致\n期望 %q\n实际 %q", c.WantError, configErr.Message)
		}
	case "prefix":
		if !errors.As(err, &configErr) {
			t.Fatalf("期望 *ConfigError，实际 %T: %v", err, err)
		}
		prefix := c.WantError
		if idx := strings.Index(c.WantError, ": "); idx >= 0 {
			prefix = c.WantError[:idx+2]
		}
		if !strings.HasPrefix(configErr.Message, prefix) {
			t.Errorf("错误前缀与 Python 不一致\n期望前缀 %q\n实际 %q", prefix, configErr.Message)
		}
	default:
		// "any"：只要求报错，文本可以与 Python 不同（见 doc.go D3）。
	}
}

// ─────────────────────────── 语料重放 ───────────────────────────

// pinBaseEnv 复刻生成器里的 pinned_env：把 home 与各平台缓存目录钉进 root。
//
// 这是「绝不写进开发者真实用户目录」的硬保证：任何一条用例忘了钉，都会在
// path_backup_default 这类用例上暴露出来。
func pinBaseEnv(t *testing.T, root string) {
	t.Helper()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "cache"))
	t.Setenv("APPDATA", filepath.Join(root, "cache-roaming"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg-cache"))
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "CODEX_HOME"} {
		t.Setenv(name, "")
	}
}

// pathCall 描述一条路径用例对应的 Go 调用。
type pathCall struct {
	agent     string
	backup    bool
	backupDir string
}

// pathCalls 把语料用例名映射到 Go 侧的调用；缺少映射会直接失败而不是被静默跳过。
var pathCalls = map[string]pathCall{
	"path_claude_default":       {agent: ClaudeCode},
	"path_claude_env":           {agent: ClaudeCode},
	"path_claude_env_tilde":     {agent: ClaudeCode},
	"path_pi_default":           {agent: PiAgent},
	"path_pi_env":               {agent: PiAgent},
	"path_codex_default":        {agent: Codex},
	"path_codex_env":            {agent: Codex},
	"path_backup_default":       {agent: ClaudeCode, backup: true},
	"path_backup_dir":           {agent: Codex, backup: true, backupDir: "explicit-backups"},
	"path_unknown_agent":        {agent: "nope"},
	"path_unknown_agent_backup": {agent: "nope", backup: true},
}

// TestCorpusPaths 重放路径推导用例（agent_config.py:69-84 / :526）。
func TestCorpusPaths(t *testing.T) {
	data := loadCorpus(t)
	for _, c := range data.Paths {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			pinBaseEnv(t, root)
			for key, value := range c.Env {
				t.Setenv(key, corpusPath(root, value))
			}
			if len(c.Inputs) > 0 {
				got, err := resolvePath(root, corpusPath(root, c.Inputs[0]))
				if err != nil {
					t.Fatalf("resolvePath 失败: %v", err)
				}
				if portablePath(root, got) != *c.Want {
					t.Errorf("解析结果期望 %q，实际 %q", *c.Want, portablePath(root, got))
				}
				return
			}
			call, ok := pathCalls[c.Name]
			if !ok {
				t.Fatalf("语料用例 %q 没有对应的 Go 调用", c.Name)
			}
			opts := Options{BaseDir: root}
			if call.backupDir != "" {
				opts.BackupDir = filepath.Join(root, filepath.FromSlash(call.backupDir))
			}
			var got string
			var err error
			if call.backup {
				got, err = BackupPath(opts, call.agent)
			} else {
				got, err = TargetPath(root, call.agent)
			}
			if c.WantError != "" {
				var configErr *ConfigError
				if !errors.As(err, &configErr) {
					t.Fatalf("期望 *ConfigError %q，实际 %v", c.WantError, err)
				}
				if configErr.Message != c.WantError {
					t.Errorf("错误文本期望 %q，实际 %q", c.WantError, configErr.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("路径推导失败: %v", err)
			}
			if portablePath(root, got) != *c.Want {
				t.Errorf("路径期望 %q，实际 %q", *c.Want, portablePath(root, got))
			}
		})
	}
}

// TestCorpusConfigure 重放全部 configure 用例。
func TestCorpusConfigure(t *testing.T) {
	data := loadCorpus(t)
	for _, c := range data.Configure {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			replayConfigure(t, data, root, c)
		})
	}
}

// replayConfigure 是单个 configure / configure_twice / status 用例的重放。
func replayConfigure(t *testing.T, data corpus, root string, c configureCase) {
	t.Helper()
	workdir := filepath.Join(root, c.Name)
	target := filepath.Join(workdir, filepath.FromSlash(c.TargetRel))
	backup := filepath.Join(workdir, "backup", c.Agent+".json")
	writeFiles(t, workdir, c.TargetRel, c.Initial, c.ExtraInit)

	routerConfig := data.routerConfig(t, c.Config)
	opts := Options{BaseDir: root, TargetPath: target, BackupPath: backup}

	// op=status：先 configure，再按用例的变更函数改文件，最后比对状态。
	if c.Op == "status" {
		before, err := GetStatus(c.Agent, opts)
		if err != nil {
			t.Fatalf("GetStatus 失败: %v", err)
		}
		if c.BeforeOK != nil && before.BackupAvailable != *c.BeforeOK {
			t.Errorf("configure 之前的 backup_available 期望 %v，实际 %v", *c.BeforeOK, before.BackupAvailable)
		}
		if _, err := Configure(c.Agent, routerConfig, c.Mode, opts); err != nil {
			t.Fatalf("Configure 失败: %v", err)
		}
		applyStatusMutation(t, c.Name, workdir, target)
		after, err := GetStatus(c.Agent, opts)
		if err != nil {
			t.Fatalf("GetStatus 失败: %v", err)
		}
		assertStatus(t, root, c.WantStatus, after)
		return
	}

	mode := c.Mode
	result, err := Configure(c.Agent, routerConfig, mode, opts)
	if c.Divergence != "" {
		// 已记录的差异（doc.go D6）：Go 必须显式报错，绝不产生与 Python
		// 不同的文件。这里只断言「报错」，不断言文本。
		if err == nil {
			t.Fatalf("差异用例 %s 期望 Go 报错，实际成功", c.Name)
		}
		return
	}
	if c.WantError != "" {
		expectError(t, c, err)
		return
	}
	if err != nil {
		t.Fatalf("Configure 失败: %v", err)
	}

	if c.Op == "configure_twice" {
		// 先把第一次的结果与语料比对，再做第二次。
		if c.AfterFirst != nil {
			assertFile(t, target, *c.AfterFirst)
		}
		result, err = Configure(c.Agent, routerConfig, c.SecondMode, opts)
		if err != nil {
			t.Fatalf("第二次 Configure 失败: %v", err)
		}
	}

	if c.WantTarget != nil {
		assertFile(t, target, *c.WantTarget)
	}
	for rel, spec := range c.WantExtra {
		assertFile(t, filepath.Join(workdir, filepath.FromSlash(rel)), spec)
	}
	if c.WantBackup != nil {
		raw, readErr := os.ReadFile(backup)
		if readErr != nil {
			t.Fatalf("读取备份失败: %v", readErr)
		}
		if got := portableText(root, string(raw)); got != *c.WantBackup {
			t.Errorf("备份内容与 Python 不一致\n期望:\n%s\n实际:\n%s", *c.WantBackup, got)
		}
	}
	assertResult(t, root, c.WantResult, result)

	status, err := GetStatus(c.Agent, opts)
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	assertStatus(t, root, c.WantStatus, status)

	if !c.Rollback {
		return
	}
	rolled, err := Rollback(c.Agent, opts)
	if err != nil {
		t.Fatalf("Rollback 失败: %v", err)
	}
	assertResult(t, root, c.WantRoll, rolled)
	for rel, spec := range c.WantAfter {
		assertFile(t, filepath.Join(workdir, filepath.FromSlash(rel)), spec)
	}
	if c.WantGone != nil {
		if _, statErr := os.Stat(backup); statErr == nil != !*c.WantGone {
			t.Errorf("回退后备份存在性不符：期望存在=%v", !*c.WantGone)
		}
	}
}

// applyStatusMutation 复现 status_cases 里对文件做的第三方改动。
func applyStatusMutation(t *testing.T, name, workdir, target string) {
	t.Helper()
	auth := filepath.Join(workdir, ".codex", "auth.json")
	backup := filepath.Join(workdir, "backup", Codex+".json")
	switch name {
	case "status_fresh":
	case "status_target_edited":
		appendFile(t, target, "# 用户又改了一行\n")
	case "status_target_deleted":
		if err := os.Remove(target); err != nil {
			t.Fatalf("删除目标失败: %v", err)
		}
	case "status_auth_edited":
		if err := os.WriteFile(auth, []byte(`{"OPENAI_API_KEY":"tampered"}`+"\n"), 0o644); err != nil {
			t.Fatalf("改写 auth.json 失败: %v", err)
		}
	case "status_auth_deleted":
		if err := os.Remove(auth); err != nil {
			t.Fatalf("删除 auth.json 失败: %v", err)
		}
	case "status_backup_deleted":
		if err := os.Remove(backup); err != nil {
			t.Fatalf("删除备份失败: %v", err)
		}
	default:
		t.Fatalf("语料用例 %q 没有对应的状态变更", name)
	}
}

// jsonEscapePath 按 JSON 字符串规则转义路径：
// 生成器用 json.dumps 写出备份，Windows 路径在里面是 "C:\\a\\b" 形式。
func jsonEscapePath(path string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path)
}

// appendFile 追加内容到文件。
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.WriteString(text); err != nil {
		t.Fatalf("追加 %s 失败: %v", path, err)
	}
}

// TestCorpusRollback 重放独立回退用例。
func TestCorpusRollback(t *testing.T) {
	data := loadCorpus(t)
	for _, c := range data.Rollback {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, c.Name)
			target := filepath.Join(workdir, filepath.FromSlash(c.TargetRel))
			backup := filepath.Join(workdir, "backup", c.Agent+".json")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("建目录失败: %v", err)
			}
			if err := os.WriteFile(target, []byte("# 当前内容\nmodel = \"current\"\n"), 0o644); err != nil {
				t.Fatalf("写目标失败: %v", err)
			}
			if c.BackupText != "" {
				if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
					t.Fatalf("建备份目录失败: %v", err)
				}
				// 占位符替换成真实的临时路径，并按 JSON 规则转义反斜杠：语料里
				// 保存的是**模板**，避免把生成机上的绝对路径写进语料。
				text := strings.ReplaceAll(c.BackupText, "@TARGET@", jsonEscapePath(target))
				text = strings.ReplaceAll(text, "@OTHER@", jsonEscapePath(filepath.Join(workdir, "elsewhere.toml")))
				if err := os.WriteFile(backup, []byte(text), 0o644); err != nil {
					t.Fatalf("写备份失败: %v", err)
				}
			}
			opts := Options{BaseDir: root, TargetPath: target, BackupPath: backup}
			result, err := Rollback(c.Agent, opts)
			if c.WantError != "" {
				var configErr *ConfigError
				if !errors.As(err, &configErr) {
					t.Fatalf("期望 *ConfigError %q，实际 %v", c.WantError, err)
				}
				if configErr.Message != c.WantError {
					t.Errorf("错误文本期望 %q，实际 %q", c.WantError, configErr.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rollback 失败: %v", err)
			}
			assertResult(t, root, c.WantResult, result)
			if c.WantTarget != nil {
				assertFile(t, target, *c.WantTarget)
			}
			if c.WantGone != nil {
				if _, statErr := os.Stat(backup); statErr == nil != !*c.WantGone {
					t.Errorf("回退后备份存在性不符：期望存在=%v", !*c.WantGone)
				}
			}
		})
	}
}

// TestCorpusEnv 重放环境变量覆盖下的落点校验（agent_config.py:72-78）。
func TestCorpusEnv(t *testing.T) {
	data := loadCorpus(t)
	for _, c := range data.Env {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, c.Name)
			backup := filepath.Join(workdir, "backup", c.Agent+".json")
			for key, value := range c.Env {
				t.Setenv(key, corpusPath(root, value))
			}
			routerConfig := data.routerConfig(t, "alpha")
			result, err := Configure(c.Agent, routerConfig, ModeUnifiedModel, Options{
				BaseDir: root, BackupPath: backup,
			})
			if err != nil {
				t.Fatalf("Configure 失败: %v", err)
			}
			if got := portablePath(root, result.TargetPath); got != c.WantTarget {
				t.Errorf("目标路径期望 %q，实际 %q", c.WantTarget, got)
			}
			if c.WantFile != nil {
				assertFile(t, result.TargetPath, *c.WantFile)
			}
			status, err := GetStatus(c.Agent, Options{
				BaseDir: root, TargetPath: result.TargetPath, BackupPath: backup,
			})
			if err != nil {
				t.Fatalf("GetStatus 失败: %v", err)
			}
			if c.WantStatus != nil && status.BackupAvailable != *c.WantStatus {
				t.Errorf("backup_available 期望 %v，实际 %v", *c.WantStatus, status.BackupAvailable)
			}
		})
	}
}
