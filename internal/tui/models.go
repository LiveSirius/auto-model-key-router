package tui

// 本文件是交互式界面的 Bubble Tea 实现：单/多选菜单与文本输入框。
//
// 设计原则：**状态机照抄 Python，渲染复用本包的版式助手**。
// select_option / select_multiple / prompt_text 的按键分支在 tui.py:697-733、
// tui.py:813-853、tui.py:631-667，这里逐条对应，因此行为测试可以直接对
// Update 断言，而不需要真终端。
//
// 渲染复用 RenderOptionMenuState / RenderMultiSelectState，所以交互界面与
// 「其他模块打印出来的窗体」用的是同一套版式（同一份对拍语料覆盖）。

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// KeyHandler 对应 select_option 的 on_key 回调（tui.py:676、699-702）：
// 返回 (值, true) 表示立即以该值结束。
type KeyHandler func(key string) (string, bool)

// KeyName 把 Bubble Tea 的按键消息映射成 tui.py 的按键名（read_key 的返回值）。
//
// 未识别的按键返回空串，调用方按「忽略」处理。
func KeyName(msg tea.KeyMsg) string {
	switch msg.Type {
	case tea.KeyEnter:
		return KeyEnter
	case tea.KeyUp:
		return KeyUp
	case tea.KeyDown:
		return KeyDown
	case tea.KeyLeft:
		return KeyLeft
	case tea.KeyRight:
		return KeyRight
	case tea.KeyPgUp:
		return KeyPageUp
	case tea.KeyPgDown:
		return KeyPageDown
	case tea.KeyHome:
		return KeyHome
	case tea.KeyEnd:
		return KeyEnd
	case tea.KeyEsc, tea.KeyCtrlC:
		return KeyCancel
	case tea.KeyBackspace:
		return KeyBackspace
	case tea.KeyDelete:
		return KeyDelete
	case tea.KeyCtrlU:
		return KeyCtrlU
	case tea.KeyCtrlW:
		return KeyCtrlW
	case tea.KeyRunes:
		return string(msg.Runes)
	}
	return ""
}

// SelectState 是菜单模型的可观测状态。
//
// 导出是为了让调用方（以及行为测试）不必碰内部字段就能判断结局：
// Done 为真后 Value（单选）或 Values（多选）就是结果。
type SelectState struct {
	// Selected 是当前选中项下标。
	Selected int
	// Checked 是多选状态。
	Checked map[int]bool
	// Offset 是窗体正文偏移。
	Offset int
	// MaxOffset 是窗体正文最大偏移。
	MaxOffset int
	// ViewportHeight 是正文视口高度。
	ViewportHeight int
	// Done 表示已经结束（有结果或已取消）。
	Done bool
	// Cancelled 表示通过取消键结束。
	Cancelled bool
	// Value 是单选结果。
	Value string
	// Values 是多选结果（按选项顺序）。
	Values []string
}

// SelectModel 是单/多选菜单的 Bubble Tea 模型。
//
// 单选对应 select_option（tui.py:670-733），多选对应 select_multiple
// （tui.py:781-853）；两者共用一套滚动与按键逻辑，靠 Multiple 区分。
type SelectModel struct {
	// Title 是窗体标题。
	Title string
	// Options 是菜单项。
	Options []Option
	// Multiple 为真表示多选。
	Multiple bool
	// Content 是正文（可为 nil）。
	Content any
	// OnKey 是按键钩子（仅单选使用）。
	OnKey KeyHandler
	// OnKeyAlways 对应 Python 里 on_key 在**所有**按键前调用（tui.py:699）的语义。
	OnKeyAlways bool

	selected     int
	checked      map[int]bool
	frameOffset  int
	lastWheelKey string
	lastWheelAt  float64
	// Now 提供单调时钟；nil 表示用进程时钟。测试注入它来固定滚轮去抖。
	Now func() float64
	// frameState 是最近一次渲染的窗体状态（对应 Python 的 frame_state 变量）。
	frameState FrameState
	state      SelectState
	width      int
	height     int
}

// NewSelectModel 构造菜单模型。
func NewSelectModel(title string, options []Option, selected int) SelectModel {
	return SelectModel{
		Title:    title,
		Options:  options,
		selected: selected,
		checked:  map[int]bool{},
	}
}

// Init 实现 tea.Model。
func (m SelectModel) Init() tea.Cmd { return nil }

// State 返回当前可观测状态。
func (m SelectModel) State() SelectState {
	state := m.state
	state.Selected = m.selected
	state.Checked = map[int]bool{}
	for index, value := range m.checked {
		state.Checked[index] = value
	}
	frame := m.frameState
	if frame.Renderable == nil {
		frame = m.frame(true)
	}
	state.Offset = frame.Offset
	state.MaxOffset = frame.MaxOffset
	state.ViewportHeight = frame.ViewportHeight
	return state
}

// Update 实现 tea.Model。
func (m SelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		Console.SetSize(message.Width, message.Height)
		return m, nil
	case tea.MouseMsg:
		if message.Action == tea.MouseActionPress {
			switch message.Button {
			case tea.MouseButtonWheelUp:
				return m.handleKey(KeyScrollUp)
			case tea.MouseButtonWheelDown:
				return m.handleKey(KeyScrollDown)
			}
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(KeyName(message))
	}
	return m, nil
}

// View 实现 tea.Model：渲染当前窗体。
//
// 与 Python 一样，View 用的是**最近一次 refresh 得到的版面**（live.update 的内容），
// 而不是每次重新计算，否则滚动正文后选中行又会被拉回视口（tui.py:683-694）。
func (m SelectModel) View() string {
	state := m.frameState
	if state.Renderable == nil {
		state = m.frame(true)
	}
	return strings.Join(state.Renderable.RenderLines(maxInt(Console.Width(), MinRenderWidth)), "\n")
}

// frame 渲染当前窗体状态，ensureSelectedVisible 对应 Python 的
// `refresh(ensure_selected_visible=...)`：滚动正文时传 false，
// 这样焦点行不会被强行拉回视口（tui.py:717）。
func (m SelectModel) frame(ensureSelectedVisible bool) FrameState {
	highlight := func(cell string) string { return selectedRowStyle.Render(cell) }
	if m.Multiple {
		return RenderMultiSelectStateWithTable(
			m.Title,
			CheckboxMenuTableStyled(m.Options, m.selected, m.checked, highlight),
			m.Content, m.frameOffset, ensureSelectedVisible,
		)
	}
	return RenderOptionMenuStateWithTable(
		m.Title,
		MenuTableStyled(m.Options, m.selected, highlight),
		m.Content, m.frameOffset, ensureSelectedVisible,
	)
}

// refresh 重新渲染并同步偏移（对应 Python 的 refresh 闭包，tui.py:683-694）：
// 无论是否保证选中行可见，都把 frame_offset 收敛到新的 offset。
func (m *SelectModel) refresh(ensureSelectedVisible bool) {
	state := m.frame(ensureSelectedVisible)
	m.frameOffset = state.Offset
	m.frameState = state
}

// selectedRowStyle 是选中行的高亮样式。
var selectedRowStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))

// now 返回单调秒数。
func (m SelectModel) now() float64 {
	if m.Now != nil {
		return m.Now()
	}
	return MonotonicSeconds()
}

// finish 结束模型并记录结果。
func (m SelectModel) finish(value string, values []string, cancelled bool) (tea.Model, tea.Cmd) {
	state := m.State()
	state.Done = true
	state.Cancelled = cancelled
	state.Value = value
	state.Values = values
	m.state = state
	return m, tea.Quit
}

// scrollKeys 返回当前生效的滚动按键集合（tui.py:712-716）。
func (m SelectModel) scrollKeys() map[string]bool {
	keys := map[string]bool{KeyPageUp: true, KeyPageDown: true, KeyHome: true, KeyEnd: true}
	if m.Content != nil {
		for _, wheelKey := range WheelKeys {
			keys[wheelKey] = true
		}
	}
	return keys
}

// handleKey 是菜单的按键状态机（tui.py:697-733 / tui.py:813-853）。
func (m SelectModel) handleKey(key string) (tea.Model, tea.Cmd) {
	if m.OnKey != nil && (m.OnKeyAlways || !m.Multiple) {
		if result, ok := m.OnKey(key); ok {
			return m.finish(result, nil, false)
		}
	}
	optionValues := map[string]bool{}
	for _, option := range m.Options {
		optionValues[option.Value] = true
	}
	if key == KeyCancel {
		if m.Multiple {
			return m.finish("", nil, true)
		}
		return m.finish(m.Options[len(m.Options)-1].Value, nil, true)
	}
	if key == "q" || key == "Q" {
		if m.Multiple {
			return m.finish("", nil, true)
		}
		if optionValues["0"] {
			return m.finish("0", nil, false)
		}
		return m.finish(m.Options[len(m.Options)-1].Value, nil, true)
	}
	if key == KeyScrollUp || key == KeyScrollDown {
		handled, newKey, newAt := ShouldHandleWheel(key, m.lastWheelKey, m.lastWheelAt, m.now())
		m.lastWheelKey, m.lastWheelAt = newKey, newAt
		if !handled {
			return m, nil
		}
	}
	state := m.frame(true)
	if m.scrollKeys()[key] && state.MaxOffset > 0 {
		m.frameOffset = ContentScrollOffset(key, m.frameOffset, state.MaxOffset, state.ViewportHeight)
		// 滚动正文时不再保证选中行可见（tui.py:717）。
		m.refresh(false)
		return m, nil
	}
	switch key {
	case KeyUp, KeyScrollUp:
		m.selected = (m.selected - 1 + len(m.Options)) % len(m.Options)
		m.refresh(true)
		return m, nil
	case KeyDown, KeyScrollDown:
		m.selected = (m.selected + 1) % len(m.Options)
		m.refresh(true)
		return m, nil
	}
	if m.Multiple {
		switch key {
		case " ":
			if m.checked[m.selected] {
				delete(m.checked, m.selected)
			} else {
				m.checked[m.selected] = true
			}
			m.refresh(true)
			return m, nil
		case "a", "A":
			if len(m.checked) == len(m.Options) {
				m.checked = map[int]bool{}
			} else {
				m.checked = map[int]bool{}
				for index := range m.Options {
					m.checked[index] = true
				}
			}
			// Python 在这里传 ensure_selected_visible=False（tui.py:843、850），
			// 勾选本身不改变选中行位置，这里同样用 false 保持偏移不变。
			m.refresh(false)
			return m, nil
		case KeyEnter:
			values := make([]string, 0, len(m.checked))
			for index, option := range m.Options {
				if m.checked[index] {
					values = append(values, option.Value)
				}
			}
			return m.finish("", values, false)
		}
		return m, nil
	}
	if key == KeyEnter {
		return m.finish(m.Options[m.selected].Value, nil, false)
	}
	if optionValues[key] {
		return m.finish(key, nil, false)
	}
	if key != "" && optionValues[strings.ToLower(key)] {
		return m.finish(strings.ToLower(key), nil, false)
	}
	return m, nil
}

// PromptState 是输入框模型的可观测状态。
type PromptState struct {
	// Done 表示已经结束。
	Done bool
	// Cancelled 表示用户取消。
	Cancelled bool
	// Value 是最终输入值。
	Value string
	// Status 是校验失败时的提示文案。
	Status string
}

// PromptModel 是文本输入框的 Bubble Tea 模型（对应 prompt_text，tui.py:584-667）。
//
// 与 Python 的差异（刻意）：编辑状态交给 bubbles/textinput，因此多了光标移动、
// Home/End、Delete 等能力；Python 只有尾部追加、退格、Ctrl+U、Ctrl+W。
// 显示规则仍然严格照抄 Python：可见值由 VisiblePromptValue 计算（含密码掩码与
// "…" 截断），所以语料 prompt_visible 段锁定的显示结果完全一致。
type PromptModel struct {
	// Title 是窗体标题。
	Title string
	// Prompt 是输入提示。
	Prompt string
	// Default 是默认值（nil 表示没有）。
	Default *string
	// Password 为真时回显星号。
	Password bool
	// Choices 非空时，回车后必须命中其中之一，否则显示 Status。
	Choices []string

	input  textinput.Model
	status string
	state  PromptState
	width  int
}

// NewPromptModel 构造输入框模型。
func NewPromptModel(title, prompt string, options PromptOptions) PromptModel {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 0
	input.Focus()
	if options.Default != nil {
		input.SetValue(*options.Default)
	}
	return PromptModel{
		Title:    title,
		Prompt:   prompt,
		Default:  options.Default,
		Password: options.Password,
		Choices:  options.Choices,
		input:    input,
	}
}

// Init 实现 tea.Model。
func (m PromptModel) Init() tea.Cmd { return nil }

// State 返回当前可观测状态。
func (m PromptModel) State() PromptState {
	state := m.state
	state.Status = m.status
	if !state.Done {
		state.Value = m.input.Value()
	}
	return state
}

// VisibleValue 返回输入框当前的可见值（按 Python 的规则截断/掩码）。
func (m PromptModel) VisibleValue() string {
	return VisiblePromptValue(m.input.Value(), m.Password, m.maxVisible())
}

// maxVisible 对应 tui.py:608 的 `max(console.size.width - 14, 8)`。
func (m PromptModel) maxVisible() int {
	width := m.width
	if width <= 0 {
		width = Console.Width()
	}
	return maxInt(width-14, 8)
}

// Update 实现 tea.Model。
func (m PromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		Console.SetSize(message.Width, message.Height)
		m.width = message.Width
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(message)
	}
	return m, nil
}

// handleKey 是输入框的按键状态机（tui.py:631-667）。
func (m PromptModel) handleKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := KeyName(message)
	value := m.input.Value()
	switch {
	case key == KeyCancel:
		return m.finish(value, true)
	case (key == "q" || key == "Q") && value == "":
		// tui.py:636-637：空输入时 q 视作取消（因此首字符不能是 q）。
		return m.finish(value, true)
	case key == KeyEnter:
		result := value
		if result == "" && m.Default != nil {
			result = *m.Default
		}
		if len(m.Choices) > 0 && !containsString(m.Choices, result) {
			m.status = "请输入以下值之一: " + strings.Join(m.Choices, " / ")
			return m, nil
		}
		return m.finish(result, false)
	}
	// 其余按键交给 textinput 做编辑（退格、Ctrl+U、Ctrl+W、光标移动等）。
	var command tea.Cmd
	m.input, command = m.input.Update(message)
	if m.input.Value() != value {
		m.status = ""
	}
	return m, command
}

// finish 结束模型。
func (m PromptModel) finish(value string, cancelled bool) (tea.Model, tea.Cmd) {
	m.state = PromptState{Done: true, Cancelled: cancelled, Value: value}
	return m, tea.Quit
}

// View 实现 tea.Model：标题 + 输入框 +（可选）提示，再套上窗体。
func (m PromptModel) View() string {
	renderables := []Renderable{m.inputPanel()}
	if m.status != "" {
		renderables = append(renderables, SectionPanel(m.status, "提示", "yellow"))
	}
	renderable := TerminalFrame(renderables, ShortcutText("输入内容  ·  Backspace 删除  ·  Ctrl+U 清空  ·  Enter 确认  ·  q/Esc/Ctrl+C 返回"), true, FrameOptions{})
	return strings.Join(renderable.RenderLines(maxInt(Console.Width(), MinRenderWidth)), "\n")
}

// inputPanel 组装「输入」面板（tui.py:611-619）。
func (m PromptModel) inputPanel() Renderable {
	text := m.Prompt + "\n" + m.VisibleValue() + "▌"
	if m.input.Value() == "" && m.Default != nil {
		text += "\n默认值: " + *m.Default
	}
	if len(m.Choices) > 0 {
		text += "\n可选值: " + strings.Join(m.Choices, " / ")
	}
	return SectionPanel(NewText(text), "输入", "cyan")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------- #
// 对外入口（与 Python 同名同语义）
// --------------------------------------------------------------------------- #

// SelectOption 显示单/多选菜单并返回用户选择（tui.py:670-733）。
//
// 返回值语义与 Python 完全一致：取消或 q 返回最后一项的值（`0` 存在时返回 `0`）；
// 数字/字母快捷键直接返回该键；回车返回当前项的值。
func SelectOption(title string, options []Option, opts SelectOptions) string {
	model := NewSelectModel(title, options, opts.Selected)
	model.Content = opts.Content
	model.OnKey = opts.OnKey
	model.OnKeyAlways = true
	final, _ := runProgram(model)
	if selectModel, ok := final.(SelectModel); ok {
		return selectModel.State().Value
	}
	if model, ok := final.(*SelectModel); ok {
		return model.State().Value
	}
	return ""
}

// SelectMultiple 显示多选菜单并返回勾选的值（按选项顺序，tui.py:781-853）。
func SelectMultiple(title string, options []Option, content any, checkedValues []string) []string {
	model := NewSelectModel(title, options, 0)
	model.Multiple = true
	model.Content = content
	for _, value := range checkedValues {
		for index, option := range options {
			if option.Value == value {
				model.checked[index] = true
			}
		}
	}
	final, _ := runProgram(model)
	if selectModel, ok := final.(SelectModel); ok {
		return selectModel.State().Values
	}
	return nil
}

// PromptText 读取一行文本（tui.py:584-667）。
//
// choices 非空时退化成单选菜单（与 Python 同一分支），取消返回 ErrCancelled。
func PromptText(title, prompt string, options PromptOptions) (string, error) {
	if len(options.Choices) > 0 {
		selected := 0
		if options.Default != nil {
			for index, choice := range options.Choices {
				if choice == *options.Default {
					selected = index
					break
				}
			}
		}
		menu := make([]Option, 0, len(options.Choices)+1)
		for _, choice := range options.Choices {
			menu = append(menu, Option{Value: choice, Label: choice})
		}
		menu = append(menu, Option{Value: "0", Label: "返回"})
		choice := SelectOption(title, menu, SelectOptions{Selected: selected})
		if choice == "0" {
			return "", ErrCancelled
		}
		return choice, nil
	}
	final, _ := runProgram(NewPromptModel(title, prompt, options))
	model, ok := final.(PromptModel)
	if !ok {
		return "", ErrCancelled
	}
	state := model.State()
	if state.Cancelled {
		return "", ErrCancelled
	}
	return state.Value, nil
}

// ConfirmChoice 让用户确认一次操作（tui.py:454-457）。
//
// default 决定初始选中项（默认选中「是」还是「否」），回车即返回当前项。
func ConfirmChoice(message string, defaultValue bool) bool {
	options := []Option{{Value: "y", Label: "是"}, {Value: "n", Label: "否"}}
	selected := 1
	if defaultValue {
		selected = 0
	}
	content := SectionPanel("[bold]"+message+"[/bold]", "请确认", "yellow")
	return SelectOption("确认操作", options, SelectOptions{Selected: selected, Content: content}) == "y"
}

// ShowResultPage 展示结果页，并在可复制时提供复制入口（tui.py:883-896）。
//
// 复制走 internal/clipboard 的注入式实现（与 clipboard.py 的语义一致）。
func ShowResultPage(title string, content any) {
	result, ok := content.(ResultPage)
	if !ok {
		result = ResultPage{Content: content, CopyLabel: "复制 key"}
	}
	options := []Option{{Value: "0", Label: "返回"}}
	if result.CopyText != "" {
		options = append([]Option{{Value: "c", Label: result.CopyLabel}}, options...)
	}
	var status Renderable
	for {
		pageContent := coerceRenderable(result.Content)
		if status != nil {
			pageContent = Group{Items: []Renderable{pageContent, status}}
		}
		choice := SelectOption(title, options, SelectOptions{Content: pageContent})
		if choice == "c" && result.CopyText != "" {
			copied, message := copyToClipboard(result.CopyText)
			border := "yellow"
			if copied {
				border = "green"
			}
			status = SectionPanel(message, "复制结果", border)
			continue
		}
		return
	}
}

// styleFor 辅助已移除；交互界面的着色集中在 selectedRowStyle 与各 Model 的 View。
