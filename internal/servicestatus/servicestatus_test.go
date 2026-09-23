package servicestatus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRunner 返回固定的两条命令结果。
func stubRunner(results ...CommandResult) CommandRunner {
	index := 0
	return func([]string) CommandResult {
		if index >= len(results) {
			return CommandResult{Code: 1}
		}
		result := results[index]
		index++
		return result
	}
}

// TestDataclassFieldOrder 用位置字面量锁住字段顺序。
//
// Python 的 dataclass 字段顺序是契约（构造与渲染都按位置取），Go 侧字段顺序
// 一旦被调整，这个测试就编译不过。
func TestDataclassFieldOrder(t *testing.T) {
	detail := StatusDetail{"Windows 原始状态", "内容", "blue"}
	if detail.Title != "Windows 原始状态" || detail.Content != "内容" || detail.Style != "blue" {
		t.Fatal("StatusDetail 字段顺序被改动")
	}
	status := SystemServiceStatus{true, []Row{{"平台", "值"}}, []StatusDetail{detail}}
	if !status.Registered || status.Rows[0].Label != "平台" || status.Details[0].Title != detail.Title {
		t.Fatal("SystemServiceStatus 字段顺序被改动")
	}
	row := Row{"注册状态", ""}
	if row.Label != "注册状态" || row.Value != "" {
		t.Fatal("Row 字段顺序被改动")
	}
}

// TestWindowsRegistrationRowIsAlwaysEmpty 锁定「注册状态」行的值恒为空串
// （service_status.py:45）。
//
// registered 只放在结构体里，渲染层（service.py:288）才把它换成
// 「已注册/未注册」文案。Go 侧不能「顺手」把值填上，否则面板会出现两遍状态。
func TestWindowsRegistrationRowIsAlwaysEmpty(t *testing.T) {
	for _, registered := range []bool{true, false} {
		code := 0
		if !registered {
			code = 1
		}
		status, err := CollectWindowsTaskStatus("python", "config.json", "TaskName",
			stubRunner(CommandResult{Code: code, Stdout: "Status: Ready"},
				CommandResult{Code: code}))
		if err != nil {
			t.Fatalf("采集报错: %v", err)
		}
		if status.Registered != registered {
			t.Fatalf("registered = %v，期望 %v", status.Registered, registered)
		}
		for _, row := range status.Rows {
			if row.Label == "注册状态" && row.Value != "" {
				t.Fatalf("注册状态行的值 = %q，期望空串", row.Value)
			}
		}
	}
}

// TestWindowsTaskKeyPresenceDiffersFromEmptyValue 锁定「键存在但值为空」与
// 「键缺失」被区别对待（service_status.py:53 使用 dict.get 的默认值）。
//
// `<Command>   </Command>` 会留下空串（strip 之后为空），于是「执行程序」行显示
// 空串；`<Arguments/>` 没有文本，键根本不存在，行显示 "-"。
func TestWindowsTaskKeyPresenceDiffersFromEmptyValue(t *testing.T) {
	xml := "<Task><Command>   </Command><Arguments/></Task>"
	status, err := CollectWindowsTaskStatus("python", "config.json", "TaskName",
		stubRunner(CommandResult{Code: 0, Stdout: "Status: Ready"},
			CommandResult{Code: 0, Stdout: xml}))
	if err != nil {
		t.Fatalf("采集报错: %v", err)
	}
	values := map[string]string{}
	for _, row := range status.Rows {
		values[row.Label] = row.Value
	}
	if got := values["执行程序"]; got != "" {
		t.Fatalf("执行程序行 = %q，期望空串（键存在但值为空）", got)
	}
	if got := values["启动参数"]; got != "-" {
		t.Fatalf("启动参数行 = %q，期望 %q（键缺失）", got, "-")
	}
}

// TestParseSystemctlPropertiesKeepsBlankKey 锁定 show 属性解析**不**过滤空键
// （service_status.py:246），与 ParseKeyValueLines 的 `if key:` 形成对比。
func TestParseSystemctlPropertiesKeepsBlankKey(t *testing.T) {
	properties := ParseSystemctlProperties("=abc\nLoadState=loaded")
	if value, found := properties[""]; !found || value != "abc" {
		t.Fatalf("show 属性里的空键应保留: %v", properties)
	}
	lines := ParseKeyValueLines(":abc\nStatus: Ready")
	if _, found := lines[""]; found {
		t.Fatalf("列表解析应丢弃空键: %v", lines)
	}
}

// TestParseSystemdUnitFileIgnoresOnlyHashComments 锁定注释规则
// （service_status.py:254 只认 "#"）。
func TestParseSystemdUnitFileIgnoresOnlyHashComments(t *testing.T) {
	values := ParseSystemdUnitFile("# 注释\n; 分号不是注释\nKey=value")
	if _, found := values["#"]; found {
		t.Fatalf("井号注释不应入表: %v", values)
	}
	// "; 分号不是注释" 不含 "="，同样不入表，但原因是「没有等号」而不是注释。
	if len(values) != 1 || values["Key"] != "value" {
		t.Fatalf("解析结果 = %v", values)
	}
	// 带等号的分号行会被当成键值对，这是 Python 的行为。
	values = ParseSystemdUnitFile(";Key=value")
	if values[";Key"] != "value" {
		t.Fatalf("分号行应被解析成键值对: %v", values)
	}
}

// TestParseWindowsTaskXMLIgnoresDoctypeEntities 记录一处刻意的差异：
// DTD 内部实体。
//
// Python 用 expat，`<!DOCTYPE ... [<!ENTITY e "v">]>` 里的实体会被展开；Go 的
// encoding/xml 不支持 DTD，遇到 `&e;` 直接报错，于是整篇解析失败、返回空 map。
// schtasks 的输出没有 DTD，为这一条引入自定义实体解析不划算，故保留差异。
func TestParseWindowsTaskXMLIgnoresDoctypeEntities(t *testing.T) {
	text := `<!DOCTYPE Task [<!ENTITY e "v">]><Task><Command>&e;</Command></Task>`
	got := ParseWindowsTaskXML(text)
	if len(got) != 0 {
		t.Fatalf("Go 侧应因未知实体而返回空 map，实际 %v", got)
	}
	// 常规实体与数字引用仍然要能解析。
	resolved := ParseWindowsTaskXML("<Command>a&amp;b&#65;</Command>")
	if resolved["Command"] != "a&bA" {
		t.Fatalf("内置实体解析失败: %v", resolved)
	}
}

// TestCollectSystemdUserStatusRejectsNonUTF8UnitFile 锁定非 UTF-8 的 unit 文件。
//
// Python 的 Path.read_text(encoding="utf-8") 抛 UnicodeDecodeError；Go 不会自动
// 校验，这里显式检查并返回 ErrUnitFileNotUTF8。异常类型名不同，但「必须失败」
// 这一点一致。
func TestCollectSystemdUserStatusRejectsNonUTF8UnitFile(t *testing.T) {
	root := t.TempDir()
	servicePath := filepath.Join(root, "unit.service")
	if err := os.WriteFile(servicePath, []byte{'[', 0xff, 0xfe, ']'}, 0o644); err != nil {
		t.Fatalf("写 unit 文件失败: %v", err)
	}
	_, err := CollectSystemdUserStatus("python", "config.json", servicePath, "svc",
		stubRunner(CommandResult{Code: 0, Stdout: "LoadState=loaded"}, CommandResult{Code: 0}))
	if !errors.Is(err, ErrUnitFileNotUTF8) {
		t.Fatalf("错误 = %v，期望 ErrUnitFileNotUTF8", err)
	}
}

// TestCollectSystemdUserStatusUsesUniversalNewlines 锁定 unit 文件的换行翻译。
//
// Python 的 open() 默认开通用换行：\r\n 与 \r 都变成 \n。这既影响「服务文件」
// 详情的显示，也影响解析（孤立 \r 在 Python 里是换行，不翻译就会被当成值的一部分）。
func TestCollectSystemdUserStatusUsesUniversalNewlines(t *testing.T) {
	root := t.TempDir()
	servicePath := filepath.Join(root, "unit.service")
	content := "[Service]\r\nWorkingDirectory=/opt/a\r\nExecStart=/x\rRestart=always\r\n"
	if err := os.WriteFile(servicePath, []byte(content), 0o644); err != nil {
		t.Fatalf("写 unit 文件失败: %v", err)
	}
	// 第二条命令返回非 0 且没有输出：CommandOutput 给空串，因此不产生
	// 「原始状态」详情，只剩「服务文件」这一条。
	status, err := CollectSystemdUserStatus("python", "config.json", servicePath, "svc",
		stubRunner(CommandResult{Code: 0, Stdout: "LoadState=loaded"}, CommandResult{Code: 1}))
	if err != nil {
		t.Fatalf("采集报错: %v", err)
	}
	values := map[string]string{}
	for _, row := range status.Rows {
		values[row.Label] = row.Value
	}
	if values["重启策略"] != "always" {
		t.Fatalf("孤立 \\r 应被当作换行: 重启策略 = %q", values["重启策略"])
	}
	if len(status.Details) != 1 {
		t.Fatalf("详情数 = %d，期望 1（只有服务文件）", len(status.Details))
	}
	if strings.Contains(status.Details[0].Content, "\r") {
		t.Fatalf("详情内容不应残留 \\r: %q", status.Details[0].Content)
	}
	if !strings.Contains(status.Details[0].Content, "\n") {
		t.Fatalf("详情内容应保留 \\n: %q", status.Details[0].Content)
	}
}

// TestPySplitLinesMatchesPython 锁定 str.splitlines() 的换行集合。
//
// Go 的 strings.Split(text, "\n") 在单独的 \r、\v、\f、U+001C..U+001E、U+0085、
// U+2028、U+2029 上都与 Python 不同，而这些字符不会被 TrimSpace 清掉，会直接
// 体现在解析结果里。
func TestPySplitLinesMatchesPython(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"\n", []string{""}},
		{"a\n\n", []string{"a", ""}},
		{"a\r\nb", []string{"a", "b"}},
		{"a\rb", []string{"a", "b"}},
		{"a\vb", []string{"a", "b"}},
		{"a\fb", []string{"a", "b"}},
		{"a\x1cb", []string{"a", "b"}},
		{"a\x1db", []string{"a", "b"}},
		{"a\x1eb", []string{"a", "b"}},
		{"a\u0085b", []string{"a", "b"}},
		{"a\u2028b", []string{"a", "b"}},
		{"a\u2029b", []string{"a", "b"}},
		{"a\r\r\nb", []string{"a", "", "b"}},
	}
	for _, testCase := range cases {
		got := pySplitLines(testCase.input)
		if len(got) != len(testCase.want) {
			t.Errorf("pySplitLines(%q) = %q，期望 %q", testCase.input, got, testCase.want)
			continue
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Errorf("pySplitLines(%q) = %q，期望 %q", testCase.input, got, testCase.want)
				break
			}
		}
	}
}

// TestCommandOutputFallsBackToStderr 锁定 stdout/stderr 的取值与「成功但没有内容」
// 提示（service_status.py:175）。
func TestCommandOutputFallsBackToStderr(t *testing.T) {
	if got := CommandOutput(CommandResult{Stdout: "", Stderr: "err", Code: 1}); got != "err" {
		t.Fatalf("stdout 为空时应退回 stderr，实际 %q", got)
	}
	if got := CommandOutput(CommandResult{Code: 0}); got != "命令执行成功，但没有返回详细内容。" {
		t.Fatalf("返回码 0 且无输出时 = %q", got)
	}
	if got := CommandOutput(CommandResult{Code: 1}); got != "" {
		t.Fatalf("返回码非 0 且无输出时 = %q，期望空串", got)
	}
}

// TestCollectSystemdUserStatusPropagatesRunnerError 锁定执行器异常直接向上传播
// （service_status.py:100 不吞异常），且后续命令不再执行。
func TestCollectSystemdUserStatusPropagatesRunnerError(t *testing.T) {
	calls := 0
	_, err := CollectSystemdUserStatus("python", "config.json", filepath.Join(t.TempDir(), "x"), "svc",
		func([]string) CommandResult {
			calls++
			return CommandResult{Err: errors.New("systemctl 不存在")}
		})
	if err == nil || err.Error() != "systemctl 不存在" {
		t.Fatalf("错误 = %v，期望原样传播执行器错误", err)
	}
	if calls != 1 {
		t.Fatalf("执行次数 = %d，期望 1（第一条失败后不再继续）", calls)
	}
}
