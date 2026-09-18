package tui

// 本文件重放 gen_tui_corpus.py（已随 Python 退役移除） 产出的对拍语料。
//
// 语料的每一条都是真实 Python（auto_model_key_router/tui.py + rich）在同一台
// 机器上跑出来的**纯文本行**（渲染后剥掉 ANSI），Go 侧用同样的输入重放并逐字比较。
// 刻意不同的版式（菜单表格、高度<3 的窗体）在语料里带 diverges 字段，测试改成
// 语义断言并单独具名，见 divergence_test.go。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --------------------------------------------------------------------------- #
// 语料结构
// --------------------------------------------------------------------------- #

type tuiCorpus struct {
	Markup      []markupCase      `json:"markup"`
	MarkupError []markupErrorCase `json:"markup_error"`
	Width       []widthCase       `json:"width"`
	Truncate    []truncateCase    `json:"truncate"`
	Wrap        []wrapCase        `json:"wrap"`
	Panel       []panelCase       `json:"panel"`
	AppFlag     []appFlagCase     `json:"app_flag"`
	Menu        []menuCase        `json:"menu"`
	Fit         []fitCase         `json:"fit"`
	Frame       []frameCase       `json:"frame"`
	Scroll      []scrollCase      `json:"scroll"`
	Wheel       []wheelCase       `json:"wheel"`
	Mouse       []mouseCase       `json:"mouse"`
	Viewport    []viewportCase    `json:"viewport"`
	KeyWindows  []keyWindowsCase  `json:"key_windows"`
	KeyPosix    []keyPosixCase    `json:"key_posix"`
	Prompt      []promptCase      `json:"prompt_visible"`
}

type markupCase struct {
	Input string `json:"input"`
	Want  string `json:"want"`
}

type markupErrorCase struct {
	Input       string `json:"input"`
	PythonError string `json:"python_error"`
	Note        string `json:"note"`
}

type widthCase struct {
	Input string `json:"input"`
	Want  int    `json:"want"`
}

type truncateCase struct {
	Input    string   `json:"input"`
	Width    int      `json:"width"`
	Ellipsis bool     `json:"ellipsis"`
	Want     []string `json:"want"`
}

type wrapCase struct {
	Input string   `json:"input"`
	Width int      `json:"width"`
	Want  []string `json:"want"`
}

type panelCase struct {
	Name  string     `json:"name"`
	Width int        `json:"width"`
	Spec  renderSpec `json:"spec"`
	Want  []string   `json:"want"`
}

type appFlagCase struct {
	Name     string   `json:"name"`
	Width    int      `json:"width"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle"`
	Version  string   `json:"version"`
	Want     []string `json:"want"`
}

type menuCase struct {
	Name     string     `json:"name"`
	Width    int        `json:"width"`
	Checkbox bool       `json:"checkbox"`
	Options  [][]string `json:"options"`
	Selected int        `json:"selected"`
	Checked  []int      `json:"checked"`
	Diverges string     `json:"diverges"`
	Want     []string   `json:"want_python"`
}

type fitCase struct {
	Lines          []string `json:"lines"`
	Height         int      `json:"height"`
	PreserveBottom bool     `json:"preserve_bottom"`
	Want           []string `json:"want"`
}

type frameCase struct {
	Name           string       `json:"name"`
	Width          int          `json:"width"`
	Height         int          `json:"height"`
	Offset         int          `json:"offset"`
	FocusText      *string      `json:"focus_text"`
	PreserveBottom bool         `json:"preserve_bottom"`
	FrameTitle     string       `json:"frame_title"`
	Renderables    []renderSpec `json:"renderables"`
	Footer         *renderSpec  `json:"footer"`
	WantLines      []string     `json:"want_lines"`
	WantOffset     int          `json:"want_offset"`
	WantMaxOffset  int          `json:"want_max_offset"`
	WantViewport   int          `json:"want_viewport_height"`
}

type scrollCase struct {
	Key            string `json:"key"`
	Offset         int    `json:"offset"`
	MaxOffset      int    `json:"max_offset"`
	ViewportHeight int    `json:"viewport_height"`
	Want           int    `json:"want"`
}

type wheelCase struct {
	Key         string  `json:"key"`
	LastKey     *string `json:"last_key"`
	LastAt      float64 `json:"last_at"`
	Now         float64 `json:"now"`
	WantHandled bool    `json:"want_handled"`
	WantKey     *string `json:"want_key"`
	WantAt      float64 `json:"want_at"`
}

type mouseCase struct {
	Sequence string  `json:"sequence"`
	WantKey  *string `json:"want_key"`
}

type viewportCase struct {
	Height      int `json:"height"`
	OptionCount int `json:"option_count"`
	Want        int `json:"want"`
}

type keyWindowsCase struct {
	Name         string   `json:"name"`
	Script       []string `json:"script"`
	WantKey      string   `json:"want_key"`
	WantConsumed int      `json:"want_consumed"`
}

type keyPosixCase struct {
	Name         string `json:"name"`
	Script       []int  `json:"script"`
	WantKey      string `json:"want_key"`
	WantConsumed int    `json:"want_consumed"`
}

type promptCase struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Password    bool     `json:"password"`
	Keys        []string `json:"keys"`
	MaxVisible  int      `json:"max_visible"`
	WantVisible string   `json:"want_visible"`
	WantValue   *string  `json:"want_value"`
}

// renderSpec 是语料里对渲染节点的中立描述，与生成器的 build_renderable 一一对应。
type renderSpec struct {
	Kind     string       `json:"kind"`
	Value    string       `json:"value"`
	Title    string       `json:"title"`
	Subtitle string       `json:"subtitle"`
	Border   string       `json:"border"`
	Version  string       `json:"version"`
	Content  *renderSpec  `json:"content"`
	Child    *renderSpec  `json:"child"`
	Items    []renderSpec `json:"items"`
	Options  [][]string   `json:"options"`
	Selected int          `json:"selected"`
	Checked  []int        `json:"checked"`
}

func loadTUICorpus(t *testing.T) tuiCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tui_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus tuiCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	return corpus
}

// buildRenderable 把语料里的描述还原成 Renderable。
func buildRenderable(t *testing.T, spec renderSpec) Renderable {
	t.Helper()
	switch spec.Kind {
	case "string":
		return NewMarkup(spec.Value)
	case "text":
		return NewText(spec.Value)
	case "page_title":
		if spec.Subtitle == "" {
			return PageTitle(spec.Title)
		}
		return PageTitle(spec.Title, spec.Subtitle)
	case "panel":
		var content any = ""
		if spec.Content != nil {
			content = buildRenderable(t, *spec.Content)
		}
		border := spec.Border
		if border == "" {
			border = "cyan"
		}
		if spec.Subtitle == "" {
			return SectionPanel(content, spec.Title, border)
		}
		return SectionPanel(content, spec.Title, border, spec.Subtitle)
	case "shortcut":
		return ShortcutText(spec.Value)
	case "group":
		items := make([]Renderable, 0, len(spec.Items))
		for _, item := range spec.Items {
			items = append(items, buildRenderable(t, item))
		}
		return Group{Items: items}
	case "menu":
		return MenuTable(optionsFromSpec(spec.Options), spec.Selected)
	case "checkbox":
		return CheckboxMenuTable(optionsFromSpec(spec.Options), spec.Selected, checkedSet(spec.Checked))
	case "app_flag":
		return AppFlagTitle(spec.Title, spec.Subtitle, spec.Version)
	case "align":
		return AlignCenter(buildRenderable(t, *spec.Child))
	}
	t.Fatalf("未知渲染节点: %s", spec.Kind)
	return nil
}

func optionsFromSpec(pairs [][]string) []Option {
	options := make([]Option, 0, len(pairs))
	for _, pair := range pairs {
		option := Option{}
		if len(pair) > 0 {
			option.Value = pair[0]
		}
		if len(pair) > 1 {
			option.Label = pair[1]
		}
		options = append(options, option)
	}
	return options
}

func checkedSet(indexes []int) map[int]bool {
	checked := map[int]bool{}
	for _, index := range indexes {
		checked[index] = true
	}
	return checked
}

// --------------------------------------------------------------------------- #
// 重放
// --------------------------------------------------------------------------- #

func TestCorpusMarkup(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Markup) == 0 {
		t.Fatal("语料缺少 markup 段")
	}
	for _, testCase := range corpus.Markup {
		t.Run(testCase.Input, func(t *testing.T) {
			if got := StripMarkup(testCase.Input); got != testCase.Want {
				t.Fatalf("StripMarkup(%q) = %q, 期望 %q", testCase.Input, got, testCase.Want)
			}
		})
	}
}

// TestCorpusMarkupErrorsDocumentedDivergence 记录刻意差异：rich 对未闭合/多余的
// 闭合标签抛 MarkupError，Go 侧不抛异常（移植后各模块的内容都是常量文案，
// 让 StripMarkup 抛异常只会把版面问题变成崩溃）。
// 这里只要求「不 panic、尽力给出文本」。
func TestCorpusMarkupErrorsDocumentedDivergence(t *testing.T) {
	corpus := loadTUICorpus(t)
	for _, testCase := range corpus.MarkupError {
		t.Run(testCase.Input, func(t *testing.T) {
			if got := StripMarkup(testCase.Input); strings.Contains(got, "[/") {
				t.Fatalf("StripMarkup(%q) = %q，不应残留闭合标签", testCase.Input, got)
			}
		})
	}
}

func TestCorpusDisplayWidth(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Width) == 0 {
		t.Fatal("语料缺少 width 段")
	}
	for _, testCase := range corpus.Width {
		t.Run(testCase.Input, func(t *testing.T) {
			if got := DisplayWidth(testCase.Input); got != testCase.Want {
				t.Fatalf("DisplayWidth(%q) = %d, 期望 %d", testCase.Input, got, testCase.Want)
			}
		})
	}
}

func TestCorpusTruncate(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Truncate) == 0 {
		t.Fatal("语料缺少 truncate 段")
	}
	for _, testCase := range corpus.Truncate {
		name := testCase.Input
		if testCase.Ellipsis {
			name += "/ellipsis"
		}
		t.Run(name, func(t *testing.T) {
			width := testCase.Width
			if width < 1 {
				width = 1
			}
			node := Text{Value: testCase.Input, NoWrap: true, OverflowEllipsis: testCase.Ellipsis}
			got := node.RenderLines(width)
			if !equalLines(got, testCase.Want) {
				t.Fatalf("截断 %q 到 %d = %q, 期望 %q", testCase.Input, width, got, testCase.Want)
			}
		})
	}
}

func TestCorpusWrap(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Wrap) == 0 {
		t.Fatal("语料缺少 wrap 段")
	}
	for _, testCase := range corpus.Wrap {
		t.Run(testCase.Input, func(t *testing.T) {
			got := WrapText(testCase.Input, testCase.Width)
			if !equalLines(got, testCase.Want) {
				t.Fatalf("WrapText(%q, %d) = %q, 期望 %q", testCase.Input, testCase.Width, got, testCase.Want)
			}
		})
	}
}

func TestCorpusPanel(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Panel) == 0 {
		t.Fatal("语料缺少 panel 段")
	}
	for _, testCase := range corpus.Panel {
		t.Run(testCase.Name, func(t *testing.T) {
			node := buildRenderable(t, testCase.Spec)
			got := node.RenderLines(testCase.Width)
			if !equalLines(got, testCase.Want) {
				t.Fatalf("面板 %s 渲染不一致\n得到:\n%s\n期望:\n%s",
					testCase.Name, numbered(got), numbered(testCase.Want))
			}
		})
	}
}

func TestCorpusAppFlag(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.AppFlag) == 0 {
		t.Fatal("语料缺少 app_flag 段")
	}
	for _, testCase := range corpus.AppFlag {
		t.Run(testCase.Name, func(t *testing.T) {
			got := AppFlagTitle(testCase.Title, testCase.Subtitle, testCase.Version).RenderLines(testCase.Width)
			if !equalLines(got, testCase.Want) {
				t.Fatalf("旗标 %s 渲染不一致\n得到:\n%s\n期望:\n%s",
					testCase.Name, numbered(got), numbered(testCase.Want))
			}
		})
	}
}

// TestCorpusMenuSemantics 断言菜单表格的**语义**（版式刻意不同）：
// 行数、每行的编号与文案、以及选中标记只出现在选中行。
func TestCorpusMenuSemantics(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Menu) == 0 {
		t.Fatal("语料缺少 menu 段")
	}
	for _, testCase := range corpus.Menu {
		t.Run(testCase.Name, func(t *testing.T) {
			options := optionsFromSpec(testCase.Options)
			var node Renderable
			if testCase.Checkbox {
				node = CheckboxMenuTable(options, testCase.Selected, checkedSet(testCase.Checked))
			} else {
				node = MenuTable(options, testCase.Selected)
			}
			lines := node.RenderLines(testCase.Width)
			if len(lines) != len(options) {
				t.Fatalf("菜单行数 = %d, 期望 %d（Python 版式: %q）", len(lines), len(options), testCase.Want)
			}
			for index, option := range options {
				if !strings.Contains(lines[index], option.Value) {
					t.Errorf("第 %d 行 %q 缺少编号 %q", index, lines[index], option.Value)
				}
				if !strings.Contains(lines[index], option.Label) {
					t.Errorf("第 %d 行 %q 缺少文案 %q", index, lines[index], option.Label)
				}
				hasMarker := strings.Contains(lines[index], SelectedRowMarker)
				if hasMarker != (index == testCase.Selected) {
					t.Errorf("第 %d 行选中标记 = %v, 期望 %v (%q)", index, hasMarker, index == testCase.Selected, lines[index])
				}
				if testCase.Checkbox {
					hasCheck := strings.Contains(lines[index], "✓")
					_, wantCheck := checkedSet(testCase.Checked)[index]
					if hasCheck != wantCheck {
						t.Errorf("第 %d 行勾选 = %v, 期望 %v (%q)", index, hasCheck, wantCheck, lines[index])
					}
				}
			}
			// 与 Python 版式做**内容**对拍：Python 那一行里的每个可见词元都必须
			// 出现在 Go 的对应行里（版式不同，信息必须相同）。这条让语料里的
			// want_python 也有牙齿——改坏它就会失败。
			for index, pythonLine := range testCase.Want {
				if index >= len(lines) {
					break
				}
				for _, token := range strings.Fields(pythonLine) {
					if !strings.Contains(lines[index], token) {
						t.Errorf("第 %d 行缺少 Python 版式中的词元 %q\nGo    : %q\nPython: %q",
							index, token, lines[index], pythonLine)
					}
				}
			}
		})
	}
}

func TestCorpusFitTerminalLines(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Fit) == 0 {
		t.Fatal("语料缺少 fit 段")
	}
	for _, testCase := range corpus.Fit {
		t.Run(testCase.HeightLabel(), func(t *testing.T) {
			got := FitTerminalLines(testCase.Lines, testCase.Height, testCase.PreserveBottom)
			if !equalLines(got, testCase.Want) {
				t.Fatalf("FitTerminalLines(%q, %d, %v) = %q, 期望 %q",
					testCase.Lines, testCase.Height, testCase.PreserveBottom, got, testCase.Want)
			}
		})
	}
}

func TestCorpusScrollOffset(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Scroll) == 0 {
		t.Fatal("语料缺少 scroll 段")
	}
	for _, testCase := range corpus.Scroll {
		t.Run(testCase.Key, func(t *testing.T) {
			got := ContentScrollOffset(testCase.Key, testCase.Offset, testCase.MaxOffset, testCase.ViewportHeight)
			if got != testCase.Want {
				t.Fatalf("ContentScrollOffset(%q, %d, %d, %d) = %d, 期望 %d",
					testCase.Key, testCase.Offset, testCase.MaxOffset, testCase.ViewportHeight, got, testCase.Want)
			}
		})
	}
}

func TestCorpusWheel(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Wheel) == 0 {
		t.Fatal("语料缺少 wheel 段")
	}
	for _, testCase := range corpus.Wheel {
		t.Run(testCase.Key, func(t *testing.T) {
			lastKey := ""
			if testCase.LastKey != nil {
				lastKey = *testCase.LastKey
			}
			handled, newKey, newAt := ShouldHandleWheel(testCase.Key, lastKey, testCase.LastAt, testCase.Now)
			if handled != testCase.WantHandled {
				t.Fatalf("handled = %v, 期望 %v", handled, testCase.WantHandled)
			}
			wantKey := ""
			if testCase.WantKey != nil {
				wantKey = *testCase.WantKey
			}
			if newKey != wantKey {
				t.Fatalf("lastKey = %q, 期望 %q", newKey, wantKey)
			}
			if newAt != testCase.WantAt {
				t.Fatalf("lastAt = %v, 期望 %v", newAt, testCase.WantAt)
			}
		})
	}
}

func TestCorpusMouse(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Mouse) == 0 {
		t.Fatal("语料缺少 mouse 段")
	}
	for _, testCase := range corpus.Mouse {
		t.Run(testCase.Sequence, func(t *testing.T) {
			got, ok := ParseSGRMouseSequence(testCase.Sequence)
			want := ""
			if testCase.WantKey != nil {
				want = *testCase.WantKey
			}
			if want == "" {
				if ok {
					t.Fatalf("ParseSGRMouseSequence(%q) = %q, 期望无结果", testCase.Sequence, got)
				}
				return
			}
			if !ok || got != want {
				t.Fatalf("ParseSGRMouseSequence(%q) = (%q, %v), 期望 %q", testCase.Sequence, got, ok, want)
			}
		})
	}
}

func TestCorpusViewportHeight(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Viewport) == 0 {
		t.Fatal("语料缺少 viewport 段")
	}
	for _, testCase := range corpus.Viewport {
		Console.SetSize(40, testCase.Height)
		if got := ContentViewportHeight(testCase.OptionCount); got != testCase.Want {
			t.Fatalf("高度 %d / 选项 %d: ContentViewportHeight = %d, 期望 %d",
				testCase.Height, testCase.OptionCount, got, testCase.Want)
		}
	}
}

func TestCorpusFrame(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Frame) == 0 {
		t.Fatal("语料缺少 frame 段")
	}
	for _, testCase := range corpus.Frame {
		t.Run(testCase.Name, func(t *testing.T) {
			Console.SetSize(testCase.Width, testCase.Height)
			defer Console.SetSize(80, 25)

			renderables := make([]Renderable, 0, len(testCase.Renderables))
			for _, spec := range testCase.Renderables {
				renderables = append(renderables, buildRenderable(t, spec))
			}
			var footer Renderable
			if testCase.Footer != nil {
				footer = buildRenderable(t, *testCase.Footer)
			}
			options := FrameOptions{
				Offset:         testCase.Offset,
				FrameTitle:     testCase.FrameTitle,
				PreserveBottom: testCase.PreserveBottom,
			}
			if testCase.FocusText != nil {
				options.FocusText = *testCase.FocusText
			}
			state := TerminalFrameState(renderables, footer, options)
			if state.Offset != testCase.WantOffset {
				t.Errorf("offset = %d, 期望 %d", state.Offset, testCase.WantOffset)
			}
			if state.MaxOffset != testCase.WantMaxOffset {
				t.Errorf("max_offset = %d, 期望 %d", state.MaxOffset, testCase.WantMaxOffset)
			}
			if state.ViewportHeight != testCase.WantViewport {
				t.Errorf("viewport_height = %d, 期望 %d", state.ViewportHeight, testCase.WantViewport)
			}
			got := state.Renderable.RenderLines(testCase.Width)
			if !equalLines(got, testCase.WantLines) {
				t.Fatalf("窗体 %s 渲染不一致\n得到:\n%s\n期望:\n%s",
					testCase.Name, numbered(got), numbered(testCase.WantLines))
			}
		})
	}
}

func TestCorpusKeyWindows(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.KeyWindows) == 0 {
		t.Fatal("语料缺少 key_windows 段")
	}
	for _, testCase := range corpus.KeyWindows {
		t.Run(testCase.Name, func(t *testing.T) {
			reader := &scriptedRunReader{script: []rune(strings.Join(testCase.Script, ""))}
			got := ReadKeyFromRunReader(reader, &fakeClock{})
			if got != testCase.WantKey {
				t.Fatalf("read_key = %q, 期望 %q", got, testCase.WantKey)
			}
			if reader.index != testCase.WantConsumed {
				t.Fatalf("消耗字符数 = %d, 期望 %d", reader.index, testCase.WantConsumed)
			}
		})
	}
}

func TestCorpusKeyPosix(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.KeyPosix) == 0 {
		t.Fatal("语料缺少 key_posix 段")
	}
	for _, testCase := range corpus.KeyPosix {
		t.Run(testCase.Name, func(t *testing.T) {
			script := make([]byte, 0, len(testCase.Script))
			for _, value := range testCase.Script {
				script = append(script, byte(value))
			}
			reader := &scriptedByteReader{script: script}
			got := ReadKeyFromByteReader(reader)
			if got != testCase.WantKey {
				t.Fatalf("read_key(posix) = %q, 期望 %q", got, testCase.WantKey)
			}
			if reader.index != testCase.WantConsumed {
				t.Fatalf("消耗字节数 = %d, 期望 %d", reader.index, testCase.WantConsumed)
			}
		})
	}
}

func TestCorpusPromptVisible(t *testing.T) {
	corpus := loadTUICorpus(t)
	if len(corpus.Prompt) == 0 {
		t.Fatal("语料缺少 prompt_visible 段")
	}
	for _, testCase := range corpus.Prompt {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.WantValue == nil {
				t.Skip("该用例以取消结束，没有可比较的输入值")
			}
			got := VisiblePromptValue(*testCase.WantValue, testCase.Password, testCase.MaxVisible)
			if got != testCase.WantVisible {
				t.Fatalf("VisiblePromptValue(%q, password=%v, %d) = %q, 期望 %q",
					*testCase.WantValue, testCase.Password, testCase.MaxVisible, got, testCase.WantVisible)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// 桩与断言助手
// --------------------------------------------------------------------------- #

// scriptedRunReader 按脚本吐宽字符，脚本耗尽后 kbhit 为假。
type scriptedRunReader struct {
	script []rune
	index  int
}

func (r *scriptedRunReader) Getwch() (rune, bool) {
	if r.index >= len(r.script) {
		return 0, false
	}
	char := r.script[r.index]
	r.index++
	return char, true
}

func (r *scriptedRunReader) Kbhit() bool { return r.index < len(r.script) }

// fakeClock 每次读数 +1 秒、Sleep 为空操作：与生成器里的 FakeTime 行为一致。
//
// 必须每次递增：read_windows_char_if_available 的 `for now < deadline` 循环
// 依赖时间前进才能退出（否则空脚本的 ESC 用例会死循环）。
type fakeClock struct{ now float64 }

func (c *fakeClock) Monotonic() float64 {
	value := c.now
	c.now += 1.0
	return value
}

func (c *fakeClock) Sleep(time.Duration) {}

// scriptedByteReader 按脚本吐字节，脚本耗尽后不可读。
type scriptedByteReader struct {
	script []byte
	index  int
}

func (r *scriptedByteReader) NextByte() (byte, bool) {
	if r.index >= len(r.script) {
		return 0, false
	}
	value := r.script[r.index]
	r.index++
	return value, true
}

func (r *scriptedByteReader) Readable(timeout time.Duration) bool {
	return r.index < len(r.script)
}

// HeightLabel 给子测试起个可读的名字。
func (c fitCase) HeightLabel() string {
	return "h" + strconv.Itoa(c.Height) + "-bottom" + strconv.FormatBool(c.PreserveBottom)
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func numbered(lines []string) string {
	var b strings.Builder
	for index, line := range lines {
		b.WriteString(strconv.Itoa(index))
		b.WriteString("| ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
