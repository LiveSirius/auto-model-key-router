package clipboard

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// corpusFile 是 gen_clipboard_corpus.py（已随 Python 退役移除） 产出的对拍语料。
type corpusFile struct {
	Cases []corpusCase `json:"cases"`
}

// planStep 是桩 Runner 的一次返回：带 Error 表示对应 Python 抛出的异常。
type planStep struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// recordedCall 是一次外部命令调用，用来核对**命令原文**与回退顺序。
type recordedCall struct {
	Command []string `json:"command"`
	Stdin   string   `json:"stdin"`
}

// corpusCase 是一条用例。
type corpusCase struct {
	Name       string            `json:"name"`
	Op         string            `json:"op"`
	System     string            `json:"system"`
	Which      []string          `json:"which"`
	Env        map[string]string `json:"env"`
	StdoutMode string            `json:"stdout_mode"`
	Text       string            `json:"text"`
	Plan       []planStep        `json:"plan"`
	Note       string            `json:"note"`

	WantCommands [][]string     `json:"want_commands"`
	WantBool     bool           `json:"want_bool"`
	WantOK       bool           `json:"want_ok"`
	WantDetail   string         `json:"want_detail"`
	WantCalls    []recordedCall `json:"want_calls"`
	WantWritten  string         `json:"want_written"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "clipboard_corpus.json"))
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

// failingWriter 对应 Python 里会抛异常的 sys.stdout。
type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }

// buildEnv 按语料构造注入环境，并返回记录调用的容器。
//
// 这里的桩与生成器里的桩一一对应：which 只认名单里的名字、环境变量是一个定值表、
// stdout 有「可用 / None / 抛异常」三种形态、Runner 按 plan 逐个返回结果。
func buildEnv(t *testing.T, testCase corpusCase, calls *[]recordedCall) Env {
	t.Helper()
	available := make(map[string]bool, len(testCase.Which))
	for _, name := range testCase.Which {
		available[name] = true
	}
	var stdout io.Writer
	switch testCase.StdoutMode {
	case "nil":
		stdout = nil
	case "oserror":
		stdout = failingWriter{err: errors.New("模拟写入失败")}
	case "valueerror":
		// Python 的 write 还会抛 ValueError（流已关闭）；Go 用同一个「写入失败」
		// 形态覆盖——CopyToTerminalClipboard 对两者的处理一致。
		stdout = failingWriter{err: errors.New("模拟已关闭的流")}
	default:
		stdout = &recordingWriter{}
	}

	step := 0
	return Env{
		System: testCase.System,
		Which: func(name string) (string, bool) {
			if available[name] {
				return "/usr/bin/" + name, true
			}
			return "", false
		},
		LookupEnv: func(key string) (string, bool) {
			value, found := testCase.Env[key]
			return value, found
		},
		Stdout: stdout,
		Run: func(command []string, stdin string) CommandResult {
			*calls = append(*calls, recordedCall{
				Command: append([]string(nil), command...),
				Stdin:   stdin,
			})
			if step >= len(testCase.Plan) {
				t.Errorf("Runner 被调用 %d 次，超出语料计划 %d 次（命令 %v）",
					step+1, len(testCase.Plan), command)
				return CommandResult{Code: 1}
			}
			current := testCase.Plan[step]
			step++
			if current.Error != "" {
				return CommandResult{Err: errors.New(current.Error)}
			}
			return CommandResult{
				Stdout: current.Stdout,
				Stderr: current.Stderr,
				Code:   current.Code,
			}
		},
	}
}

// recordingWriter 记录写入内容（对应生成器里的 RecordingWriter）。
type recordingWriter struct{ written string }

func (writer *recordingWriter) Write(data []byte) (int, error) {
	writer.written += string(data)
	return len(data), nil
}

// equalCommands 比较命令表；nil 与空表视为等价（Python 返回空列表，Go 返回 nil）。
func equalCommands(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if len(got[index]) != len(want[index]) {
			return false
		}
		for part := range got[index] {
			if got[index][part] != want[index][part] {
				return false
			}
		}
	}
	return true
}

// TestClipboardMatchesPython 重放语料，逐条断言与参照实现一致。
func TestClipboardMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	var assertions int

	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			var calls []recordedCall
			env := buildEnv(t, testCase, &calls)
			assertions++

			switch testCase.Op {
			case "clipboard_commands":
				got := ClipboardCommands(env)
				if !equalCommands(got, testCase.WantCommands) {
					t.Fatalf("写入命令 = %v，期望 %v", got, testCase.WantCommands)
				}
			case "paste_commands":
				got := PasteCommands(env)
				if !equalCommands(got, testCase.WantCommands) {
					t.Fatalf("读取命令 = %v，期望 %v", got, testCase.WantCommands)
				}
			case "is_remote_terminal":
				if got := IsRemoteTerminal(env); got != testCase.WantBool {
					t.Fatalf("IsRemoteTerminal = %v，期望 %v", got, testCase.WantBool)
				}
			case "copy_to_terminal_clipboard":
				got := CopyToTerminalClipboard(env.Stdout, testCase.Text)
				if got != testCase.WantOK {
					t.Fatalf("CopyToTerminalClipboard = %v，期望 %v", got, testCase.WantOK)
				}
				assertWritten(t, env.Stdout, testCase.WantWritten)
			case "copy_to_clipboard":
				ok, detail := CopyToClipboard(env, testCase.Text)
				if ok != testCase.WantOK || detail != testCase.WantDetail {
					t.Fatalf("CopyToClipboard = (%v, %q)，期望 (%v, %q)",
						ok, detail, testCase.WantOK, testCase.WantDetail)
				}
				assertCalls(t, calls, testCase.WantCalls)
				assertWritten(t, env.Stdout, testCase.WantWritten)
			case "paste_from_clipboard":
				ok, detail := PasteFromClipboard(env)
				if ok != testCase.WantOK || detail != testCase.WantDetail {
					t.Fatalf("PasteFromClipboard = (%v, %q)，期望 (%v, %q)",
						ok, detail, testCase.WantOK, testCase.WantDetail)
				}
				assertCalls(t, calls, testCase.WantCalls)
			default:
				t.Fatalf("未知操作: %s", testCase.Op)
			}
		})
	}
	t.Logf("共重放 %d 个断言", assertions)
}

// assertWritten 核对写到 stdout 的内容（只在用例记录该字段时比较）。
func assertWritten(t *testing.T, stdout io.Writer, want string) {
	t.Helper()
	writer, ok := stdout.(*recordingWriter)
	if !ok {
		// stdout 为 nil 或会抛异常：此时不该有任何成功写入。
		if want != "" {
			t.Fatalf("stdout 不可用却期望写入 %q", want)
		}
		return
	}
	if writer.written != want {
		t.Fatalf("stdout 内容 = %q，期望 %q", writer.written, want)
	}
}

// assertCalls 核对外部命令的调用序列（命令原文 + stdin）。
func assertCalls(t *testing.T, got []recordedCall, want []recordedCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("命令调用次数 = %d，期望 %d（实际 %v）", len(got), len(want), got)
	}
	for index := range got {
		if !equalCommands([][]string{got[index].Command}, [][]string{want[index].Command}) {
			t.Fatalf("第 %d 次命令 = %v，期望 %v",
				index, got[index].Command, want[index].Command)
		}
		if got[index].Stdin != want[index].Stdin {
			t.Fatalf("第 %d 次 stdin = %q，期望 %q",
				index, got[index].Stdin, want[index].Stdin)
		}
	}
}
