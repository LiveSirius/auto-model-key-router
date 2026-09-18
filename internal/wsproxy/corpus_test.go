package wsproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/proxysupport"
)

// testdataDir 是差分语料目录；proxy.jsonl 由 gen_websocket_corpus.py（已随 Python 退役移除） 驱动
// **真实的** websocket_proxy.py 产出：synthesize 用例直接调 _websocket_http_request，
// frames 用例跑真实最小 FastAPI 应用 + TestClient。
const testdataDir = "testdata"

// corpusLine 是 proxy.jsonl 的通用外壳。
type corpusLine struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Note  string `json:"note"`
	Scope *struct {
		Scheme   string     `json:"scheme"`
		Path     string     `json:"path"`
		QueryB64 string     `json:"query_b64"`
		Headers  [][]string `json:"headers"`
	} `json:"scope"`
	Body   string `json:"body"`
	APIKey string `json:"api_key"`

	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Send    *struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		B64  string `json:"b64"`
	} `json:"send"`
	Response *struct {
		Status      int     `json:"status"`
		ContentType *string `json:"content_type"`
		Streaming   bool    `json:"streaming"`
		Chunks      []struct {
			Text string `json:"text"`
			B64  string `json:"b64"`
		} `json:"chunks"`
	} `json:"response"`
	Fail   string `json:"fail"`
	Status int    `json:"status"`
	Expect string `json:"expect"`
}

// loadCorpus 逐行读取 JSONL 语料。
func loadCorpus(t *testing.T, name string) []corpusLine {
	t.Helper()
	path := filepath.Join(testdataDir, name)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开语料 %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []corpusLine
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var line corpusLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("%s: 解析语料行失败: %v", path, err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s: 读取语料失败: %v", path, err)
	}
	if len(lines) == 0 {
		t.Fatalf("%s: 语料为空", path)
	}
	return lines
}

// decodeExpect 把 expect（紧凑 JSON 文本）解成给定类型。
func decodeExpect(t *testing.T, expect string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(expect), target); err != nil {
		t.Fatalf("解析 expect 失败: %v (%s)", err, expect)
	}
}

// assertSame 比较期望与实际。
func assertSame(t *testing.T, name string, want, got any) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	t.Errorf("%s 不一致\n期望: %s\n实际: %s", name, wantJSON, gotJSON)
}

// decodeB64 解语料里的 base64 字段。
func decodeB64(t *testing.T, raw string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("base64 解码失败: %v (%s)", err, raw)
	}
	return data
}

// synthesizeExpectation 与 synthesize 用例的 expect 同形。
type synthesizeExpectation struct {
	Method          string            `json:"method"`
	ScopeScheme     string            `json:"scope_scheme"`
	HTTPVersion     string            `json:"http_version"`
	URLPath         string            `json:"url_path"`
	Query           string            `json:"query"`
	Headers         [][]string        `json:"headers"`
	UpstreamHeaders map[string]string `json:"upstream_headers"`
}

// TestSynthesizeCorpus 重放「WS 握手 → HTTP 请求」的折算语料。
//
// 覆盖三件事：路径参数怎么算（含 `/v1/` 与百分号解码）、哪些握手头被剔除、以及
// 折算出来的头部再经 proxysupport.UpstreamHeaders 过滤后的上游头部。
func TestSynthesizeCorpus(t *testing.T) {
	seen := 0
	for _, line := range loadCorpus(t, "proxy.jsonl") {
		if line.Kind != "synthesize" {
			continue
		}
		seen++
		line := line
		t.Run(line.Name, func(t *testing.T) {
			var want synthesizeExpectation
			decodeExpect(t, line.Expect, &want)

			target, err := url.Parse(line.Scope.Path)
			if err != nil {
				t.Fatalf("解析 path 失败: %v", err)
			}
			target.RawQuery = string(decodeB64(t, line.Scope.QueryB64))

			handshake := http.Header{}
			for _, pair := range line.Scope.Headers {
				handshake.Add(string(decodeB64(t, pair[0])), string(decodeB64(t, pair[1])))
			}

			// scope.scheme 是 ASGI 的 "ws"/"wss"，Go 侧对应的折算规则是
			// SchemeFromWebSocket（"wss" → "https"，见 websocket_proxy.py:56）。
			request := SynthesizeRequest(SchemeFromWebSocket(line.Scope.Scheme), target,
				handshake, decodeB64(t, line.Body))

			got := synthesizeExpectation{
				Method:      request.Method,
				ScopeScheme: request.URL.Scheme,
				HTTPVersion: strings.TrimPrefix(request.Proto, "HTTP/"),
				URLPath:     request.URL.Path,
				Query:       request.URL.RawQuery,
				Headers:     encodeRawPairs(request.Header),
			}
			// 期望里的 headers 是 Python 的**原始字节对**（base64 存字节、大小写与
			// 重复都保留），Go 的 http.Header 会规范化键并合并同名头——差异由
			// TestCanonicalisedHeaderNamesDivergeFromPythonRawBytes 单独记录。
			// Host 也不在 Go 的 Header 里（net/http 把它摘到 Request.Host），
			// 而 proxysupport.UpstreamHeaders 本来就会剔除 host。
			wantPairs := make([][]string, 0, len(want.Headers))
			for _, pair := range want.Headers {
				wantPairs = append(wantPairs, []string{
					string(decodeB64(t, pair[0])), string(decodeB64(t, pair[1]))})
			}
			assertSame(t, line.Name+"/headers",
				normalizePairs(wantPairs), normalizePairs(got.Headers))

			upstream := proxysupport.UpstreamHeaders(map[string][]string(request.Header), line.APIKey)
			// 上游头部按**键小写后逐键**比较，但重复键要特殊处理：Python 的
			// `request.headers.items()` 会把「仅大小写不同的同名头」当成两个键
			// （X-Multi / x-multi 各自存活），Go 的 http.Header 会先把它们合并成
			// 一个键。因此这里比较的是「小写键 → 该键在期望里的最后一个值」，
			// 与 Go 的后者胜语义对齐；被丢掉的那个值正是**已知分歧**，
			// 见 TestCanonicalisedHeaderNamesDivergeFromPythonRawBytes。
			assertSame(t, line.Name+"/upstream_headers",
				lastWinsByLowercase(t, line.Expect), lowercaseMap(upstream))

			if want.Method != got.Method || want.ScopeScheme != got.ScopeScheme ||
				want.URLPath != got.URLPath || want.Query != got.Query ||
				want.HTTPVersion != got.HTTPVersion {
				t.Fatalf("折算请求的骨架不一致: %+v vs 期望 %+v", got, want)
			}
		})
	}
	if seen == 0 {
		t.Fatal("语料里没有 synthesize 用例")
	}
}

// encodeRawPairs 把 http.Header 展成 [名, 值] 列表。
func encodeRawPairs(header http.Header) [][]string {
	pairs := [][]string{}
	for name, values := range header {
		for _, value := range values {
			pairs = append(pairs, []string{name, value})
		}
	}
	return pairs
}

// normalizePairs 归一化头部对：键转小写、去掉 host、排序后比较。
func normalizePairs(pairs [][]string) []string {
	result := []string{}
	for _, pair := range pairs {
		name := strings.ToLower(pair[0])
		if name == "host" {
			continue
		}
		result = append(result, name+": "+pair[1])
	}
	sort.Strings(result)
	return result
}

// lowercaseMap 归一化头部映射的键。
func lowercaseMap(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers))
	for name, value := range headers {
		result[strings.ToLower(name)] = value
	}
	return result
}

// lastWinsByLowercase 从 expect 文本里取出 upstream_headers，按**文件里的键顺序**
// 折叠成「小写键 → 最后一个值」。
//
// 不能把 upstream_headers 直接解成 Go 的 map：那样键顺序就丢了，而「后者胜」恰恰
// 依赖顺序。语料的键顺序是生成时排序过的（render 用 sort_keys=True），因此 canonical
// 的保序解析能还原出一份确定的顺序。
func lastWinsByLowercase(t *testing.T, expect string) map[string]string {
	t.Helper()
	parsed, err := canonical.ParseString(expect)
	if err != nil {
		t.Fatalf("解析 expect 失败: %v", err)
	}
	headers := parsed.Lookup("upstream_headers")
	if !headers.IsObject() {
		t.Fatalf("expect 里没有 upstream_headers 对象: %s", expect)
	}
	result := make(map[string]string, headers.Len())
	for _, key := range headers.Obj.Keys() {
		value, _ := headers.Obj.Get(key)
		result[strings.ToLower(key)] = value.Str
	}
	return result
}

// recordedRequest 是 ProxyHandler 实际看到的请求（frames 用例的比较对象）。
type recordedRequest struct {
	Path                    string `json:"path"`
	Method                  string `json:"method"`
	ScopeScheme             string `json:"scope_scheme"`
	URLPath                 string `json:"url_path"`
	Query                   string `json:"query"`
	Body                    string `json:"body"`
	DisconnectedBeforeFrame bool   `json:"disconnected_before_frame"`
}

type frameExpectation struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	B64  string `json:"b64"`
}

type closeExpectation struct {
	Code            int    `json:"code"`
	Reason          string `json:"reason"`
	ClientInitiated bool   `json:"client_initiated"`
	Raised          string `json:"raised"`
}

type framesExpectation struct {
	Recorded []recordedRequest  `json:"recorded"`
	Frames   []frameExpectation `json:"frames"`
	Close    *closeExpectation  `json:"close"`
}

// TestFramesCorpus 用真实进程内服务器重放帧序列语料。
//
// 每个用例起一个 httptest 服务器挂上真实的 Handler；ProxyHandler 用脚本化的响应
// （流式就是「每块写一次并 Flush」）。客户端用 coder/websocket 发一帧、读到关闭为止
// ——帧的顺序与类型、以及关闭码都是真跑出来的，不是模拟的。
func TestFramesCorpus(t *testing.T) {
	seen := 0
	for _, line := range loadCorpus(t, "proxy.jsonl") {
		if line.Kind != "frames" {
			continue
		}
		seen++
		line := line
		t.Run(line.Name, func(t *testing.T) {
			var want framesExpectation
			decodeExpect(t, line.Expect, &want)

			var recorded []recordedRequest
			handler := &Handler{Proxy: func(w http.ResponseWriter, request *http.Request, path string) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("读取请求体失败: %v", err)
				}
				recorded = append(recorded, recordedRequest{
					Path:        path,
					Method:      request.Method,
					ScopeScheme: request.URL.Scheme,
					URLPath:     request.URL.Path,
					Query:       request.URL.RawQuery,
					Body:        base64.StdEncoding.EncodeToString(body),
				})
				if line.Fail == "raise" {
					panic("boom")
				}
				writeScriptedResponse(t, w, line)
			}}
			server := httptest.NewServer(handler)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			headers := http.Header{}
			for name, value := range line.Headers {
				headers.Set(name, value)
			}
			conn, _, err := websocket.Dial(ctx, server.URL+line.URL, &websocket.DialOptions{
				HTTPHeader: headers,
			})
			if err != nil {
				t.Fatalf("拨号失败: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()

			if line.Send == nil {
				// 不发消息直接断开：服务器应直接返回，不调用 ProxyHandler。
				_ = conn.Close(websocket.StatusNormalClosure, "")
			} else if line.Send.Kind == "bytes" {
				if err := conn.Write(ctx, websocket.MessageBinary, decodeB64(t, line.Send.B64)); err != nil {
					t.Fatalf("发送失败: %v", err)
				}
			} else if err := conn.Write(ctx, websocket.MessageText, []byte(line.Send.Text)); err != nil {
				t.Fatalf("发送失败: %v", err)
			}

			gotFrames := []frameExpectation{}
			var closeErr *websocket.CloseError
			for {
				messageType, data, readErr := conn.Read(ctx)
				if readErr != nil {
					var parsed websocket.CloseError
					if errors.As(readErr, &parsed) {
						closeErr = &parsed
					}
					break
				}
				if messageType == websocket.MessageText {
					gotFrames = append(gotFrames, frameExpectation{Kind: "text", Text: string(data)})
				} else {
					gotFrames = append(gotFrames, frameExpectation{
						Kind: "bytes", B64: base64.StdEncoding.EncodeToString(data)})
				}
			}

			if want.Frames == nil {
				want.Frames = []frameExpectation{}
			}
			assertSame(t, line.Name+"/frames", want.Frames, gotFrames)

			if line.Send == nil {
				if len(recorded) != 0 {
					t.Fatalf("对端在首帧前断开，不该调用 ProxyHandler: %+v", recorded)
				}
			} else {
				if want.Recorded == nil {
					want.Recorded = []recordedRequest{}
				}
				if recorded == nil {
					recorded = []recordedRequest{}
				}
				assertSame(t, line.Name+"/recorded", want.Recorded, recorded)
			}

			if want.Close != nil && want.Close.Raised == "" && !want.Close.ClientInitiated {
				if closeErr == nil {
					t.Fatalf("期望关闭码 %d，实际没有关闭帧", want.Close.Code)
				}
				if int(closeErr.Code) != want.Close.Code {
					t.Fatalf("关闭码 = %d（reason %q），期望 %d",
						closeErr.Code, closeErr.Reason, want.Close.Code)
				}
			}
		})
	}
	if seen == 0 {
		t.Fatal("语料里没有 frames 用例")
	}
}

// writeScriptedResponse 按脚本写响应。
//
// 「流式」在 Go 侧的表现就是**调用 Flush**：每 Flush 一次发一帧（见 capture 的说明），
// 因此语料里 streaming=true 的 chunk 序列对应「每个 chunk 写一次并 Flush」。
func writeScriptedResponse(t *testing.T, w http.ResponseWriter, line corpusLine) {
	t.Helper()
	if line.Response == nil {
		t.Error("用例缺少 response 脚本")
		return
	}
	if line.Response.ContentType != nil {
		w.Header().Set("Content-Type", *line.Response.ContentType)
	}
	w.WriteHeader(line.Response.Status)
	for _, chunk := range line.Response.Chunks {
		data := []byte(chunk.Text)
		if chunk.B64 != "" {
			data = decodeB64(t, chunk.B64)
		}
		if len(data) > 0 {
			if _, err := w.Write(data); err != nil {
				return
			}
		}
		if line.Response.Streaming {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

// TestCloseCodeCorpus 重放「状态码 → 关闭码」全表。
func TestCloseCodeCorpus(t *testing.T) {
	seen := 0
	for _, line := range loadCorpus(t, "proxy.jsonl") {
		if line.Kind != "close_code" {
			continue
		}
		seen++
		var want int
		decodeExpect(t, line.Expect, &want)
		if got := CloseCode(line.Status); got != want {
			t.Errorf("CloseCode(%d) = %d，期望 %d", line.Status, got, want)
		}
	}
	if seen == 0 {
		t.Fatal("语料里没有 close_code 用例")
	}
}
