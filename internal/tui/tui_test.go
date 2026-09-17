package tui

// 本文件是交互式界面的**行为测试**：直接把按键消息喂给 Bubble Tea 模型的
// Update，断言模型状态与 View 内容，不启动真终端。
//
// 这样做的理由：tui.py 的交互逻辑（select_option / select_multiple / prompt_text）
// 是「状态机 + 渲染」，状态机完全可以确定性测试；终端本身不是被测对象。
// 断言的按键分支与 tui.py:697-733、tui.py:813-853、tui.py:631-667 一一对应。

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sparrived/auto-model-key-router/internal/clipboard"
)

func testOptions() []Option {
	return []Option{
		{Value: "1", Label: "启动服务"},
		{Value: "2", Label: "停止服务"},
		{Value: "0", Label: "返回"},
	}
}

func keyMsg(keyType tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: keyType} }

// runes 把字符串变成一次按键消息（Bubble Tea 的 KeyRunes）。
func runes(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}

// update 驱动模型并返回新模型。
func update(t *testing.T, model SelectModel, msg tea.Msg) SelectModel {
	t.Helper()
	next, _ := model.Update(msg)
	selectModel, ok := next.(SelectModel)
	if !ok {
		t.Fatalf("Update 返回了非 SelectModel: %T", next)
	}
	return selectModel
}

func updatePrompt(t *testing.T, model PromptModel, msg tea.Msg) PromptModel {
	t.Helper()
	next, _ := model.Update(msg)
	promptModel, ok := next.(PromptModel)
	if !ok {
		t.Fatalf("Update 返回了非 PromptModel: %T", next)
	}
	return promptModel
}

// --------------------------------------------------------------------------- #
// 单选菜单
// --------------------------------------------------------------------------- #

func TestSelectModelArrowWrapsSelection(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 0)
	model = update(t, model, keyMsg(tea.KeyDown))
	if got := model.State().Selected; got != 1 {
		t.Fatalf("down 后选中 = %d, 期望 1", got)
	}
	model = update(t, model, keyMsg(tea.KeyUp))
	if got := model.State().Selected; got != 0 {
		t.Fatalf("up 后选中 = %d, 期望 0", got)
	}
	// 从第一项往上应回绕到最后一项（tui.py:720 的取模）。
	model = update(t, model, keyMsg(tea.KeyUp))
	if got := model.State().Selected; got != len(testOptions())-1 {
		t.Fatalf("回绕选中 = %d, 期望 %d", got, len(testOptions())-1)
	}
}

func TestSelectModelNumberShortcutReturnsValue(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 0)
	next, _ := model.Update(runes("2"))
	final := next.(SelectModel)
	state := final.State()
	if !state.Done || state.Value != "2" {
		t.Fatalf("数字快捷键结果 = %+v, 期望 Done=true Value=2", state)
	}
	if state.Cancelled {
		t.Fatal("数字快捷键不应算取消")
	}
}

func TestSelectModelEnterReturnsSelected(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 1)
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	state := next.(SelectModel).State()
	if !state.Done || state.Value != "2" {
		t.Fatalf("回车结果 = %+v, 期望 Value=2", state)
	}
}

func TestSelectModelCancelAndQuitSemantics(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	// cancel → 最后一项（tui.py:703-704）
	model := NewSelectModel("主菜单", testOptions(), 0)
	state := update(t, model, keyMsg(tea.KeyEsc)).State()
	if !state.Cancelled || state.Value != "0" {
		t.Fatalf("cancel 结果 = %+v, 期望 Cancelled=true Value=0", state)
	}
	// q → 有 "0" 选项时返回 "0"（tui.py:705-707）
	model = NewSelectModel("主菜单", testOptions(), 0)
	state = update(t, model, runes("q")).State()
	if state.Value != "0" || state.Cancelled {
		t.Fatalf("q 结果 = %+v, 期望 Value=0 且不算取消", state)
	}
	// 没有 "0" 选项时 q → 最后一项
	options := []Option{{Value: "y", Label: "是"}, {Value: "n", Label: "否"}}
	model = NewSelectModel("确认操作", options, 0)
	if got := update(t, model, runes("q")).State().Value; got != "n" {
		t.Fatalf("无 0 选项时 q 应返回最后一项, 得到 %q", got)
	}
}

func TestSelectModelOnKeyHookIntercepts(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 0)
	model.OnKey = func(key string) (string, bool) {
		if key == "x" {
			return "hooked", true
		}
		return "", false
	}
	state := update(t, model, runes("x")).State()
	if !state.Done || state.Value != "hooked" {
		t.Fatalf("on_key 钩子结果 = %+v", state)
	}
	// 未被钩子接管的按键仍然走默认分支。
	state = update(t, model, keyMsg(tea.KeyEnter)).State()
	if state.Value != "1" {
		t.Fatalf("钩子返回 false 后回车结果 = %q, 期望 1", state.Value)
	}
}

func TestSelectModelViewContainsTitleAndOptions(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 1)
	view := model.View()
	for _, want := range []string{"主菜单", "操作菜单", "启动服务", "停止服务", "返回", SelectedRowMarker} {
		if !strings.Contains(view, want) {
			t.Fatalf("视图缺少 %q:\n%s", want, view)
		}
	}
	if lines := strings.Split(view, "\n"); len(lines) != 20 {
		t.Fatalf("视图行数 = %d, 期望与终端高度一致(20)", len(lines))
	}
}

func TestSelectModelPageDownScrollsFrame(t *testing.T) {
	Console.SetSize(30, 12)
	defer Console.SetSize(80, 25)

	// 正文很长时，PgDn 应该滚动正文而不是移动选中项（tui.py:712-718）。
	longContent := NewText(strings.Repeat("正文行\n", 60))
	model := NewSelectModel("主菜单", testOptions(), 0)
	model.Content = longContent
	model = update(t, model, keyMsg(tea.KeyPgDown))
	state := model.State()
	if state.Offset == 0 {
		t.Fatalf("PgDn 后偏移仍为 0（MaxOffset=%d）", state.MaxOffset)
	}
	if state.Selected != 0 {
		t.Fatalf("PgDn 不应改变选中项, 得到 %d", state.Selected)
	}
	model = update(t, model, keyMsg(tea.KeyHome))
	if got := model.State().Offset; got != 0 {
		t.Fatalf("Home 后偏移 = %d, 期望 0", got)
	}
}

func TestSelectModelWheelThrottledByInterval(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	now := 100.0
	model := NewSelectModel("主菜单", testOptions(), 0)
	model.Now = func() float64 { return now }

	// 没有正文时滚轮等同于上下移动（tui.py:719-726）。
	model = update(t, model, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if got := model.State().Selected; got != 1 {
		t.Fatalf("首次滚轮后选中 = %d, 期望 1", got)
	}
	// 0.16 秒内的同方向事件被去抖（tui.py:287-293）。
	now += 0.05
	model = update(t, model, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if got := model.State().Selected; got != 1 {
		t.Fatalf("去抖窗口内选中被改变为 %d", got)
	}
	// 超过窗口后生效。
	now += WheelEventIntervalSeconds + 0.01
	model = update(t, model, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if got := model.State().Selected; got != 2 {
		t.Fatalf("去抖窗口外选中 = %d, 期望 2", got)
	}
}

func TestSelectModelWindowSizeUpdatesConsole(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("主菜单", testOptions(), 0)
	update(t, model, tea.WindowSizeMsg{Width: 44, Height: 18})
	width, height := Console.Size()
	if width != 44 || height != 18 {
		t.Fatalf("终端尺寸 = (%d,%d), 期望 (44,18)", width, height)
	}
}

// --------------------------------------------------------------------------- #
// 多选菜单
// --------------------------------------------------------------------------- #

func TestSelectMultipleSpaceToggles(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("选择 Key", testOptions(), 0)
	model.Multiple = true
	model = update(t, model, runes(" "))
	model = update(t, model, keyMsg(tea.KeyDown))
	model = update(t, model, runes(" "))
	if state := model.State(); !state.Checked[0] || !state.Checked[1] {
		t.Fatalf("勾选状态 = %v, 期望 0 与 1", state.Checked)
	}
	// 再按一次取消勾选。
	model = update(t, model, runes(" "))
	if state := model.State(); state.Checked[1] {
		t.Fatalf("再次按空格应取消勾选: %v", state.Checked)
	}
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	state := next.(SelectModel).State()
	if !state.Done || len(state.Values) != 1 || state.Values[0] != "1" {
		t.Fatalf("多选结果 = %+v, 期望 [1]", state)
	}
}

func TestSelectMultipleToggleAllAndOrder(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("选择 Key", testOptions(), 0)
	model.Multiple = true
	model = update(t, model, runes("a"))
	if state := model.State(); len(state.Checked) != 3 {
		t.Fatalf("A 全选后 = %v", state.Checked)
	}
	// 全选状态下再按 A 清空。
	model = update(t, model, runes("A"))
	if state := model.State(); len(state.Checked) != 0 {
		t.Fatalf("A 再按一次应清空: %v", state.Checked)
	}
	// 勾选顺序应不影响返回值顺序（tui.py:853 按选项下标排序）。
	model = update(t, model, keyMsg(tea.KeyDown))
	model = update(t, model, runes(" "))
	model = update(t, model, keyMsg(tea.KeyUp))
	model = update(t, model, runes(" "))
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	values := next.(SelectModel).State().Values
	if len(values) != 2 || values[0] != "1" || values[1] != "2" {
		t.Fatalf("返回值顺序 = %v, 期望 [1 2]", values)
	}
}

func TestSelectMultipleCancelReturnsEmpty(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("选择 Key", testOptions(), 0)
	model.Multiple = true
	next, _ := model.Update(keyMsg(tea.KeyCtrlC))
	state := next.(SelectModel).State()
	if !state.Done || !state.Cancelled || len(state.Values) != 0 {
		t.Fatalf("取消结果 = %+v, 期望 Cancelled 且无值", state)
	}
}

func TestSelectMultipleViewShowsCheckMarks(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewSelectModel("选择 Key", testOptions(), 0)
	model.Multiple = true
	model = update(t, model, runes(" "))
	if view := model.View(); !strings.Contains(view, "✓") {
		t.Fatalf("多选视图应显示勾选标记:\n%s", view)
	}
}

// --------------------------------------------------------------------------- #
// 输入框
// --------------------------------------------------------------------------- #

func TestPromptModelTypingAndEnter(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewPromptModel("输入标题", "输入", PromptOptions{})
	for _, char := range "abc" {
		model = updatePrompt(t, model, runes(string(char)))
	}
	if got := model.State().Value; got != "abc" {
		t.Fatalf("输入值 = %q, 期望 abc", got)
	}
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	state := next.(PromptModel).State()
	if !state.Done || state.Value != "abc" {
		t.Fatalf("回车结果 = %+v", state)
	}
}

func TestPromptModelBackspaceAndCtrlU(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewPromptModel("输入标题", "输入", PromptOptions{})
	for _, char := range "abcd" {
		model = updatePrompt(t, model, runes(string(char)))
	}
	model = updatePrompt(t, model, keyMsg(tea.KeyBackspace))
	if got := model.State().Value; got != "abc" {
		t.Fatalf("退格后 = %q, 期望 abc", got)
	}
	model = updatePrompt(t, model, keyMsg(tea.KeyCtrlU))
	if got := model.State().Value; got != "" {
		t.Fatalf("Ctrl+U 后 = %q, 期望空串", got)
	}
}

func TestPromptModelQCancelsOnlyWhenEmpty(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	// 空输入时 q 视作取消（tui.py:636-637）。
	model := NewPromptModel("输入标题", "输入", PromptOptions{})
	next, _ := model.Update(runes("q"))
	if state := next.(PromptModel).State(); !state.Cancelled {
		t.Fatalf("空输入按 q 应取消: %+v", state)
	}
	// 已有内容时 q 是普通字符。
	model = NewPromptModel("输入标题", "输入", PromptOptions{})
	model = updatePrompt(t, model, runes("a"))
	model = updatePrompt(t, model, runes("q"))
	if got := model.State().Value; got != "aq" {
		t.Fatalf("有内容时 q 应为普通字符, 得到 %q", got)
	}
}

func TestPromptModelDefaultValueAndEsc(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	def := "8765"
	model := NewPromptModel("监听配置", "监听端口", PromptOptions{Default: &def})
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	state := next.(PromptModel).State()
	if state.Value != "8765" {
		t.Fatalf("空输入回车应取默认值, 得到 %q", state.Value)
	}
	model = NewPromptModel("监听配置", "监听端口", PromptOptions{})
	next, _ = model.Update(keyMsg(tea.KeyEsc))
	if state := next.(PromptModel).State(); !state.Cancelled {
		t.Fatalf("Esc 应取消: %+v", state)
	}
}

func TestPromptModelChoicesValidation(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewPromptModel("模式", "routing", PromptOptions{Choices: []string{"openai", "anthropic"}})
	model = updatePrompt(t, model, runes("x"))
	next, _ := model.Update(keyMsg(tea.KeyEnter))
	state := next.(PromptModel).State()
	if state.Done {
		t.Fatalf("非法取值不应结束: %+v", state)
	}
	if !strings.Contains(state.Status, "请输入以下值之一") {
		t.Fatalf("应给出可选值提示, 得到 %q", state.Status)
	}
	// 输入合法值后可以结束。
	model = updatePrompt(t, next.(PromptModel), keyMsg(tea.KeyCtrlU))
	model = updatePrompt(t, model, runes("openai"))
	next, _ = model.Update(keyMsg(tea.KeyEnter))
	if state := next.(PromptModel).State(); !state.Done || state.Value != "openai" {
		t.Fatalf("合法取值结果 = %+v", state)
	}
}

func TestPromptModelPasswordMaskedInView(t *testing.T) {
	Console.SetSize(60, 20)
	defer Console.SetSize(80, 25)

	model := NewPromptModel("添加供应商 Key", "API key", PromptOptions{Password: true})
	for _, char := range "secret" {
		model = updatePrompt(t, model, runes(string(char)))
	}
	view := model.View()
	if strings.Contains(view, "secret") {
		t.Fatalf("密码不应明文出现在视图里:\n%s", view)
	}
	if !strings.Contains(view, "******") {
		t.Fatalf("密码应显示为 6 个星号:\n%s", view)
	}
}

func TestPromptModelVisibleValueTruncatesWithEllipsis(t *testing.T) {
	Console.SetSize(40, 20)
	defer Console.SetSize(80, 25)

	model := NewPromptModel("输入标题", "输入", PromptOptions{})
	for _, char := range strings.Repeat("x", 40) {
		model = updatePrompt(t, model, runes(string(char)))
	}
	// maxVisible = max(40-14, 8) = 26 → 省略号 + 末 25 个字符（tui.py:608-610）。
	want := "…" + strings.Repeat("x", 25)
	if got := model.VisibleValue(); got != want {
		t.Fatalf("可见值 = %q, 期望 %q", got, want)
	}
}

// --------------------------------------------------------------------------- #
// 按键映射与剪贴板接线
// --------------------------------------------------------------------------- #

func TestKeyNameMapping(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		want string
	}{
		{keyMsg(tea.KeyEnter), KeyEnter},
		{keyMsg(tea.KeyUp), KeyUp},
		{keyMsg(tea.KeyPgUp), KeyPageUp},
		{keyMsg(tea.KeyHome), KeyHome},
		{keyMsg(tea.KeyEnd), KeyEnd},
		{keyMsg(tea.KeyEsc), KeyCancel},
		{keyMsg(tea.KeyCtrlC), KeyCancel},
		{keyMsg(tea.KeyBackspace), KeyBackspace},
		{keyMsg(tea.KeyCtrlU), KeyCtrlU},
		{keyMsg(tea.KeyCtrlW), KeyCtrlW},
		{runes(" "), " "},
		{runes("中"), "中"},
	}
	for _, testCase := range cases {
		if got := KeyName(testCase.msg); got != testCase.want {
			t.Fatalf("KeyName(%v) = %q, 期望 %q", testCase.msg, got, testCase.want)
		}
	}
}

// TestCopyToClipboardSeam 确认结果页的复制走的是 internal/clipboard：
// 空文本被拒绝这条文案由 clipboard 包给出（clipboard.py:67）。
func TestCopyToClipboardSeam(t *testing.T) {
	copied, message := clipboard.CopyToClipboard(clipboard.Env{
		System:    "Linux",
		Which:     func(string) (string, bool) { return "", false },
		LookupEnv: func(string) (string, bool) { return "", false },
	}, "")
	if copied || message != "没有可复制的内容。" {
		t.Fatalf("空文本复制结果 = (%v, %q)", copied, message)
	}
}

// TestPosixInputModeNestedIsIdempotent 钉住 Python 的模块级标志语义
// （tui.py:412、418-420）：重复进入不再切换，退出时只恢复一次。
func TestPosixInputModeNestedIsIdempotent(t *testing.T) {
	outer := PosixInputMode()
	inner := PosixInputMode()
	inner()
	outer()
	if posixInputModeActive {
		t.Fatal("退出后不应仍标记为激活")
	}
}
