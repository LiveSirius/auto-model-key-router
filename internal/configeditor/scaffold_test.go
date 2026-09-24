package configeditor

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// 本文件是探测相关的测试脚手架：脚本化 HTTP 传输与固定时钟。
//
// 它们原先住在差分语料文件里；那批语料已随 Python 参照实现退役删除，但被测用例
// 仍然需要这些桩。

// corpusFixedEpoch 是桩时钟的 Unix 秒（固定值，避免用例受真实时间影响）。
const corpusFixedEpoch = 1_800_000_000.0

// scriptedClock 是确定性时钟：单调钟每次调用递增 0.25 秒（取值沿用迁移期生成脚本的
// 桩，该脚本已随 Python 参照实现退役删除）。
type scriptedClock struct{ value float64 }

// scriptedClockStart / scriptedClockStep 是上面那套桩的起始值与步长。
const (
	scriptedClockStart = 1000.0
	scriptedClockStep  = 0.25
)

func newScriptedClock() *scriptedClock { return &scriptedClock{value: scriptedClockStart} }

func (c *scriptedClock) Now() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func (c *scriptedClock) EpochSeconds() float64 { return corpusFixedEpoch }

func (c *scriptedClock) MonotonicSeconds() float64 {
	current := c.value
	c.value += scriptedClockStep
	return current
}

// assertValue 用有序紧凑 JSON 比较两个 canonical 值（连带锁定键顺序）。
func assertValue(t *testing.T, label string, got, want *canonical.Value) {
	t.Helper()
	gotText := canonical.DumpsOrdered(got)
	wantText := canonical.DumpsOrdered(want)
	if gotText != wantText {
		t.Errorf("%s 不一致:\n got = %s\nwant = %s", label, gotText, wantText)
	}
}

// recordedRequest 是一条被记录的请求。
type recordedRequest struct {
	method        string
	url           string
	authorization string
	body          *canonical.Value
	hasBody       bool
}

// scriptedTransport 是脚本化传输：按脚本应答并记录请求。
type scriptedTransport struct {
	t        *testing.T
	steps    []*canonical.Value
	pos      int
	requests []recordedRequest
}

// RoundTrip 实现 http.RoundTripper。
func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorded := recordedRequest{method: request.Method, url: request.URL.String()}
	recorded.authorization = request.Header.Get("Authorization")
	if request.Body != nil {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			s.t.Fatalf("读取请求体失败: %v", err)
		}
		_ = request.Body.Close()
		if len(raw) > 0 {
			value, err := canonical.Parse(raw)
			if err != nil {
				s.t.Fatalf("请求体不是合法 JSON: %v（%s）", err, raw)
			}
			recorded.body = value
			recorded.hasBody = true
		}
	}
	s.requests = append(s.requests, recorded)

	if s.pos >= len(s.steps) {
		s.t.Fatalf("脚本已用尽，但仍有请求: %s %s", request.Method, request.URL)
	}
	step := s.steps[s.pos]
	s.pos++
	if errorStep := step.Lookup("error"); errorStep.Truthy() {
		// Python 侧抛 httpx.ReadError/ConnectError(msg)，Go 侧返回等价的 error；
		// 两边 str(exc) 都是 msg，所以错误文本可以逐字比较。
		return nil, errors.New(step.Lookup("message").StringValue())
	}
	status, _ := canonical.ToInt(step.Lookup("status"))
	body := step.Lookup("body").StringValue()
	return &http.Response{
		StatusCode: int(status),
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Request:    request,
	}, nil
}
