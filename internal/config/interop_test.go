package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestPythonWrittenConfigRoundTripsByteIdentical 是「完全兼容」承诺的核心证据。
//
// 夹具 testdata/python_written_config.json 由参照实现的 save_config_data **真实
// 写出**（见 gen_config_model_corpus.py（已随 Python 退役移除） 的 persist_fixture_text），因此它
// 包含了 Python 侧的全部落盘细节：indent=2、键序不排序、嵌套缩进、末尾换行、
// 非 ASCII 原样保留、以及 45.5 这类浮点保持 "45.5"。
//
// 断言的是三件事：
//  1. Go 能解析这个由 Python 写出的文件；
//  2. Go 能把它解析成有效配置（不会因未知字段 unicode_note 而失败）；
//  3. Go 再写回时**逐字节一致**——这是关键，任何一处格式漂移都会让用户在
//     两种实现之间来回切换时产生无意义的文件改动。
func TestPythonWrittenConfigRoundTripsByteIdentical(t *testing.T) {
	fixturePath := filepath.Join("testdata", "python_written_config.json")
	original, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("读取夹具失败（语料已冻结并随仓库提交，生成器已随 Python 退役移除）: %v", err)
	}

	// 1 & 2：Go 读得懂，且能解析成有效配置。
	data, err := canonical.ParseString(string(original))
	if err != nil {
		t.Fatalf("Go 无法解析 Python 写出的配置: %v", err)
	}
	config, err := FromDict(data)
	if err != nil {
		t.Fatalf("Go 无法把 Python 写出的配置解析为有效配置: %v", err)
	}
	if config.Port != 8000 {
		t.Errorf("port 期望 8000，实际 %d", config.Port)
	}
	if config.StreamFirstByteTimeout != 45.5 {
		t.Errorf("stream_first_byte_timeout 期望 45.5，实际 %v", config.StreamFirstByteTimeout)
	}
	if config.LocalAPIKey != "amkr_fixture_key_for_golden_file" {
		t.Errorf("local_api_key 不符: %q", config.LocalAPIKey)
	}
	if len(config.Providers) != 1 || config.Providers[0].ID != "openai" {
		t.Fatalf("providers 解析异常: %+v", config.Providers)
	}
	// provider 级 routes 是唯一真正生效的路由来源。
	routes := config.UpstreamRoutesForBaseURL("https://api.openai.com")
	if routes["anthropic"] != "anthropic/v1/messages" {
		t.Errorf("provider routes 未生效: %v", routes)
	}
	if len(config.Models) != 1 || len(config.Models[0].Keys) != 1 {
		t.Fatalf("models 解析异常: %+v", config.Models)
	}

	// 3：写回逐字节一致。
	//
	// 换行按平台归一后再比对。注意先把夹具自身归一成 LF 再翻译：git 可能按
	// core.autocrlf 把夹具检出成 CRLF，若直接翻译会把已有的 \r\n 变成 \r\r\n，
	// 造成与 git 配置相关的假失败。
	dir := t.TempDir()
	rewritten := filepath.Join(dir, "router-config.json")
	if err := SaveConfigData(rewritten, data); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	got, err := os.ReadFile(rewritten)
	if err != nil {
		t.Fatalf("读取写回结果失败: %v", err)
	}
	fixtureLF := strings.ReplaceAll(string(original), "\r\n", "\n")
	want := translateNewlines(fixtureLF)
	if string(got) != want {
		t.Errorf("写回内容与 Python 输出不一致\n期望 %d 字节:\n%s\n实际 %d 字节:\n%s",
			len(want), want, len(got), string(got))
	}
}

// TestSaveNewlineMatchesPythonTextMode 固化 Python write_text 的换行行为。
//
// 实测（本机 Windows + Python 3.12）：
//
//	save_config_data 写出的字节为 b'{\r\n  "config_version": 4,\r\n...\r\n}\r\n'
//
// 即 Python 以文本模式打开文件时把 "\n" 翻译成 os.linesep。Go 的 os.WriteFile
// 不翻译，故必须显式补上，否则 Windows 下每个换行少一个 \r——文件功能上等价，
// 但「字节级兼容」的承诺就破了，且用户在两版之间切换时 git 会一直看到改动。
//
// 同时断言字符串**内部**的换行不受影响：JSON 里它是转义的两个字节（\ 与 n），
// 不是真实换行，因此不该被翻译。
func TestSaveNewlineMatchesPythonTextMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	// 值里带一个真实换行，用来验证它被转义而非被翻译。
	data := mustParse(t, `{"config_version":4,"multiline":"line1\nline2"}`)
	if err := SaveConfigData(path, data); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	text := string(raw)

	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(text, "{\r\n") {
			t.Errorf("Windows 下应以 CRLF 换行，实际开头: %q", text)
		}
		if strings.Contains(text, "\n") && !strings.Contains(text, "\r\n") {
			t.Error("Windows 下应全部使用 CRLF")
		}
	} else if strings.Contains(text, "\r\n") {
		t.Errorf("类 Unix 下不应出现 CRLF，实际: %q", text)
	}

	// 字符串内部的换行必须是转义形式（\ 后跟 n），且不被换行翻译影响。
	if !strings.Contains(text, `"line1\nline2"`) {
		t.Errorf("字符串内部的换行应保持 JSON 转义形式，实际:\n%s", text)
	}
	// 重新读取后值应完好。
	reloaded, err := LoadConfigData(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	got, _ := reloaded.Lookup("multiline").AsString()
	if got != "line1\nline2" {
		t.Errorf("往返后多行字符串损坏: %q", got)
	}
}

// TestPythonFixtureIsRealImplementationOutput 是一道测试有效性防线。
//
// 它断言夹具具备「真由 Python 写出」的特征，而不是人手敲的近似文本。若夹具被
// 换成手写样例，本测试会失败——否则上面的往返断言可能只是自证。
func TestPythonFixtureIsRealImplementationOutput(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "python_written_config.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	text := string(content)

	// json.dumps(indent=2) 的特征：两空格缩进 + ": " 分隔 + 末尾换行。
	if !strings.Contains(text, "\n  \"config_version\": 4,") {
		t.Error("缺少 indent=2 的两空格缩进特征")
	}
	if !strings.HasSuffix(text, "}\n") {
		t.Error("缺少末尾换行（Python 侧显式补的 \"\\n\"）")
	}
	if strings.Contains(text, "\\u") {
		t.Error("出现了 \\u 转义，说明没有用 ensure_ascii=False")
	}
	if !strings.Contains(text, "中文注释与 emoji 🙂") {
		t.Error("缺少非 ASCII 内容，无法验证 ensure_ascii=False")
	}
	// 浮点必须保持 ".5" 而非被规整成整数。
	if !strings.Contains(text, "\"stream_first_byte_timeout\": 45.5") {
		t.Error("浮点未按 Python 形式写出")
	}
	// 空容器在 indent 模式下仍是紧凑形式（Python 的行为，容易被写错成多行）。
	if !strings.Contains(text, "\"upstream_routes\": {}") {
		t.Error("空对象应写成紧凑的 {}")
	}
}

// TestUnknownFieldsSurviveRoundTrip 断言未知字段不会被丢弃。
//
// 后果很实际：若 Go 在保存时丢掉不认识的字段（例如未来版本新增的配置项），
// 用户用旧版 Go 打开一次配置就会永久丢失数据。解析时可忽略，写回时必须保留。
func TestUnknownFieldsSurviveRoundTrip(t *testing.T) {
	raw := mustParse(t, `{"config_version":4,"local_api_key":"k","future_feature":{"nested":[1,2]},"models":{},"providers":{}}`)
	text := canonical.DumpsIndent(raw, 2) + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	loaded, err := LoadConfigData(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !loaded.Obj.Has("future_feature") {
		t.Fatal("未知字段 future_feature 在加载后丢失")
	}
	if got := canonical.Dumps(loaded.Lookup("future_feature")); got != `{"nested":[1,2]}` {
		t.Errorf("未知字段内容改变: %s", got)
	}
	// 解析为配置对象同样不应因为存在未知字段而失败。
	if _, err := FromDict(loaded); err != nil {
		t.Errorf("未知字段导致解析失败: %v", err)
	}
}
