package servicestatus

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusFile 是 scripts/gen_servicestatus_corpus.py 产出的对拍语料。
type corpusFile struct {
	Cases []corpusCase `json:"cases"`
}

// response 是桩命令执行器的一次返回：带 Error 表示对应 Python 抛出的异常。
type response struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// wantStatus 是采集结果的期望值；行与详情里的临时目录前缀已被换成 <root>。
type wantStatus struct {
	Registered bool       `json:"registered"`
	Rows       [][]string `json:"rows"`
	Details    []struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Style   string `json:"style"`
	} `json:"details"`
}

// corpusCase 是一条用例。不同 op 只用其中一部分字段。
type corpusCase struct {
	Name         string     `json:"name"`
	Op           string     `json:"op"`
	TaskName     string     `json:"task_name"`
	ServiceName  string     `json:"service_name"`
	Python       string     `json:"python"`
	Config       string     `json:"config"`
	ServicePath  string     `json:"service_path"`
	Responses    []response `json:"responses"`
	UnitFileText *string    `json:"unit_file_text"`
	UnitFileHex  string     `json:"unit_file_hex"`
	Result       struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Code   int    `json:"code"`
	} `json:"result"`
	Input     string            `json:"input"`
	Keys      []string          `json:"keys"`
	Values    map[string]string `json:"values"`
	WantCalls [][]string        `json:"want_calls"`

	WantStatus    *wantStatus       `json:"want_status"`
	WantMap       map[string]string `json:"want_map"`
	WantText      string            `json:"want_text"`
	WantValue     *string           `json:"want_value"`
	WantError     bool              `json:"want_error"`
	WantErrorKind string            `json:"want_error_kind"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "servicestatus_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("语料为空")
	}
	return corpus
}

// portable 是生成器里 portable() 的镜像：把临时目录前缀换成 <root>，
// 并把它后面那一段路径的分隔符统一成 "/"。
func portable(value, root string) string {
	if root == "" || !strings.Contains(value, root) {
		return value
	}
	index := strings.Index(value, root)
	head := value[:index]
	tail := value[index+len(root):]
	end := len(tail)
	if cut := strings.IndexAny(tail, "\n\r\t\"'"); cut >= 0 {
		end = cut
	}
	return head + "<root>" + strings.ReplaceAll(tail[:end], `\`, "/") + tail[end:]
}

// portableRows 换算整张表。
func portableRows(rows []Row, root string) [][]string {
	result := make([][]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, []string{
			portable(row.Label, root), portable(row.Value, root),
		})
	}
	return result
}

// materializeUnitFile 按语料铺 systemd unit 文件。
func materializeUnitFile(t *testing.T, testCase corpusCase, root string) string {
	t.Helper()
	servicePath := filepath.Join(root, filepath.FromSlash(testCase.ServicePath))
	if testCase.UnitFileHex == "" && testCase.UnitFileText == nil {
		return servicePath
	}
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o755); err != nil {
		t.Fatalf("建 unit 目录失败: %v", err)
	}
	var content []byte
	if testCase.UnitFileHex != "" {
		decoded, err := hex.DecodeString(testCase.UnitFileHex)
		if err != nil {
			t.Fatalf("unit_file_hex 解析失败: %v", err)
		}
		content = decoded
	} else {
		content = []byte(*testCase.UnitFileText)
	}
	if err := os.WriteFile(servicePath, content, 0o644); err != nil {
		t.Fatalf("写 unit 文件失败: %v", err)
	}
	return servicePath
}

// buildRunner 按响应计划构造桩执行器，并记录真实调用到的命令行。
func buildRunner(t *testing.T, responses []response, calls *[][]string) CommandRunner {
	t.Helper()
	step := 0
	return func(command []string) CommandResult {
		*calls = append(*calls, append([]string(nil), command...))
		if step >= len(responses) {
			t.Errorf("命令被调用 %d 次，超出语料计划 %d 次（命令 %v）",
				step+1, len(responses), command)
			return CommandResult{Code: 1}
		}
		current := responses[step]
		step++
		if current.Error != "" {
			return CommandResult{Err: errors.New(current.Error)}
		}
		return CommandResult{
			Stdout: current.Stdout,
			Stderr: current.Stderr,
			Code:   current.Code,
		}
	}
}

// assertCalls 核对外部命令的调用序列（锁住命令原文与顺序）。
func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("命令调用次数 = %d，期望 %d（实际 %v）", len(got), len(want), got)
	}
	for index := range got {
		if len(got[index]) != len(want[index]) {
			t.Fatalf("第 %d 条命令 = %v，期望 %v", index, got[index], want[index])
		}
		for part := range got[index] {
			if got[index][part] != want[index][part] {
				t.Fatalf("第 %d 条命令 = %v，期望 %v", index, got[index], want[index])
			}
		}
	}
}

// assertStatus 比较采集结果（路径已换算成 <root> 前缀）。
func assertStatus(t *testing.T, got SystemServiceStatus, want *wantStatus, root string) {
	t.Helper()
	if want == nil {
		t.Fatal("语料缺少 want_status")
	}
	if got.Registered != want.Registered {
		t.Fatalf("registered = %v，期望 %v", got.Registered, want.Registered)
	}
	gotRows := portableRows(got.Rows, root)
	if len(gotRows) != len(want.Rows) {
		t.Fatalf("行数 = %d，期望 %d", len(gotRows), len(want.Rows))
	}
	for index, row := range want.Rows {
		if len(row) != 2 {
			t.Fatalf("语料第 %d 行不是二元组: %v", index, row)
		}
		if gotRows[index][0] != row[0] || gotRows[index][1] != row[1] {
			t.Fatalf("第 %d 行 = %v，期望 %v", index, gotRows[index], row)
		}
	}
	if len(got.Details) != len(want.Details) {
		t.Fatalf("详情数 = %d，期望 %d（实际 %v）",
			len(got.Details), len(want.Details), got.Details)
	}
	for index, detail := range want.Details {
		content := portable(got.Details[index].Content, root)
		if got.Details[index].Title != detail.Title || content != detail.Content ||
			got.Details[index].Style != detail.Style {
			t.Fatalf("第 %d 个详情 = {%q, %q, %q}，期望 {%q, %q, %q}",
				index, got.Details[index].Title, content, got.Details[index].Style,
				detail.Title, detail.Content, detail.Style)
		}
	}
}

// TestServiceStatusMatchesPython 重放语料，逐条断言与参照实现一致。
func TestServiceStatusMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	var assertions int

	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			assertions++
			root := t.TempDir()

			switch testCase.Op {
			case "collect_windows_task_status":
				var calls [][]string
				status, err := CollectWindowsTaskStatus(
					filepath.Join(root, filepath.FromSlash(testCase.Python)),
					filepath.Join(root, filepath.FromSlash(testCase.Config)),
					testCase.TaskName,
					buildRunner(t, testCase.Responses, &calls),
				)
				assertCollectResult(t, testCase, status, err, calls, root)

			case "collect_systemd_user_status":
				servicePath := materializeUnitFile(t, testCase, root)
				var calls [][]string
				status, err := CollectSystemdUserStatus(
					filepath.Join(root, filepath.FromSlash(testCase.Python)),
					filepath.Join(root, filepath.FromSlash(testCase.Config)),
					servicePath,
					testCase.ServiceName,
					buildRunner(t, testCase.Responses, &calls),
				)
				assertCollectResult(t, testCase, status, err, calls, root)

			case "parse_windows_task_xml":
				assertMap(t, ParseWindowsTaskXML(testCase.Input), testCase.WantMap)
			case "parse_key_value_lines":
				assertMap(t, ParseKeyValueLines(testCase.Input), testCase.WantMap)
			case "parse_systemctl_properties":
				assertMap(t, ParseSystemctlProperties(testCase.Input), testCase.WantMap)
			case "parse_systemd_unit_file":
				assertMap(t, ParseSystemdUnitFile(testCase.Input), testCase.WantMap)
			case "first_value":
				got := firstValue(testCase.Values, testCase.Keys...)
				want := ""
				if testCase.WantValue != nil {
					want = *testCase.WantValue
				}
				if got != want {
					t.Fatalf("first_value = %q，期望 %q（Python 的 None 对应空串）", got, want)
				}
			case "command_output":
				got := CommandOutput(CommandResult{
					Stdout: testCase.Result.Stdout,
					Stderr: testCase.Result.Stderr,
					Code:   testCase.Result.Code,
				})
				if got != testCase.WantText {
					t.Fatalf("CommandOutput = %q，期望 %q", got, testCase.WantText)
				}
			case "local_xml_name":
				if got := LocalXMLName(testCase.Input); got != testCase.WantText {
					t.Fatalf("LocalXMLName(%q) = %q，期望 %q",
						testCase.Input, got, testCase.WantText)
				}
			default:
				t.Fatalf("未知操作: %s", testCase.Op)
			}
		})
	}
	t.Logf("共重放 %d 个断言", assertions)
}

// assertCollectResult 统一处理采集层的成功/失败断言。
func assertCollectResult(
	t *testing.T,
	testCase corpusCase,
	status SystemServiceStatus,
	err error,
	calls [][]string,
	root string,
) {
	t.Helper()
	assertCalls(t, calls, testCase.WantCalls)
	if testCase.WantError {
		if err == nil {
			t.Fatalf("期望报错（%s），实际注册状态为 %v",
				orDefault(testCase.WantErrorKind, "OSError"), status.Registered)
		}
		if testCase.WantErrorKind == "UnicodeDecodeError" && !errors.Is(err, ErrUnitFileNotUTF8) {
			t.Fatalf("期望 ErrUnitFileNotUTF8，实际 %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("参照成功但 Go 报错: %v", err)
	}
	assertStatus(t, status, testCase.WantStatus, root)
}

// orDefault 在值为空时给一个默认值（用于错误提示文案）。
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// assertMap 比较解析结果（缺失键与空值在 Python 里是两种状态，必须逐键核对）。
func assertMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("键数 = %d，期望 %d（Go=%v，期望=%v）", len(got), len(want), got, want)
	}
	for key, value := range want {
		actual, found := got[key]
		if !found {
			t.Fatalf("缺少键 %q（Go=%v）", key, got)
		}
		if actual != value {
			t.Fatalf("键 %q = %q，期望 %q", key, actual, value)
		}
	}
}
