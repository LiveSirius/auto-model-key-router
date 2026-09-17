package tui

// 本文件是「富文本降级为等价纯文本」的渲染模型。
//
// 迁移方案已明确：不追求与 rich 逐像素对齐，只对齐**语义与信息**。因此这里用
// 一个极小的 Renderable 接口替代 rich 的富文本对象树，样式信息（颜色、加粗）
// 在构造时就被丢弃，只保留文字与版式。这样：
//
//  1. 其他待移植模块（service.py / main.py / config_editor.py / dashboard.py）
//     调用 section_panel、page_title、menu_table 时拿到的东西可以直接打印；
//  2. 对拍语料只比较**纯文本行**，不受颜色系统影响。
//
// 刻意保留的差异（都在 divergence_test.go 里具名钉住）：
//   - 面板边框固定用 ROUNDED（╭╮╰╯）。rich 在 safe_box 生效时会替换成 SQUARE
//     （┌┐└┘），safe_box 又取决于终端探测；Go 侧恒为 ROUNDED，等于真实支持
//     Unicode 的终端下的表现（tui.py:80/104 都传 box.ROUNDED）。
//   - menu_table / checkbox_menu_table 用简单的固定列宽布局，不复制 rich 的
//     表格列宽分配算法（非目标）。
//   - Segment/Segments 的样式被丢弃，只保留文本行。
//   - rich 对未闭合/多余闭合标签抛 MarkupError，StripMarkup 永不抛异常。

import (
	"strings"
)

// Renderable 是可在给定宽度下渲染为纯文本行的富文本节点。
//
// width 是可用单元格数；实现必须把内容折行/裁剪到该宽度内（可以更短，
// 外层面板会补空格）。返回的行不带换行符。
type Renderable interface {
	RenderLines(width int) []string
}

// Text 是一段纯文本（不做标记解析，对应 rich.text.Text）。
type Text struct {
	// Value 是已经解析好的纯文本（构造时由 StripMarkup 或原样给出）。
	Value string
	// NoWrap 为真时不折行（对应 rich 的 no_wrap=True）。
	NoWrap bool
	// OverflowEllipsis 为真时超宽用省略号截断（对应 overflow="ellipsis"）。
	OverflowEllipsis bool
}

// NewText 构造不解析 rich 标记的纯文本节点。
func NewText(value string) Text { return Text{Value: value} }

// MarkupText 是一段**按 rich 标记解析**的文本，对应 Python 里直接把 str 交给
// rich 渲染的写法（tui.py 的 content 参数几乎都是这种）。
type MarkupText struct {
	// Source 是带标记的原文。
	Source string
}

// NewMarkup 构造会解析 rich 标记的文本节点。
func NewMarkup(source string) MarkupText { return MarkupText{Source: source} }

// RenderLines 实现 Renderable。
func (t MarkupText) RenderLines(width int) []string {
	return textLines(StripMarkup(t.Source), false, false, width)
}

// RenderLines 实现 Renderable。
//
// no_wrap 时始终返回一行；否则按宽度折行（与 rich 的 Text.wrap 一致）。
func (t Text) RenderLines(width int) []string {
	return textLines(t.Value, t.NoWrap, t.OverflowEllipsis, width)
}

func textLines(value string, noWrap, ellipsis bool, width int) []string {
	if width <= 0 {
		return nil
	}
	if noWrap {
		if ellipsis {
			return []string{TruncateEllipsis(value, width)}
		}
		return []string{cropCells(value, width)}
	}
	return WrapText(value, width)
}

// Lines 是已经成型的文本行，对应 rich.segment.Segments。
//
// 它不做任何折行：rich 的 Segments 是「已经渲染完的段」，放进面板只会被裁剪
// （tui.py:119-125 segment_lines_renderable）。
type Lines struct {
	// Text 是逐行的纯文本。
	Text []string
}

// RenderLines 实现 Renderable：原样返回，只做宽度裁剪。
func (l Lines) RenderLines(width int) []string {
	out := make([]string, 0, len(l.Text))
	for _, line := range l.Text {
		out = append(out, TruncateCells(line, width))
	}
	return out
}

// Group 把多个 Renderable 纵向拼接，对应 rich.console.Group。
type Group struct {
	// Items 是按顺序渲染的子节点。
	Items []Renderable
}

// RenderLines 实现 Renderable。
func (g Group) RenderLines(width int) []string {
	var out []string
	for _, item := range g.Items {
		if item == nil {
			continue
		}
		out = append(out, item.RenderLines(width)...)
	}
	return out
}

// Align 是水平居中包装器，对应 rich.align.Align。
//
// 与 rich 一致：先按可用宽度渲染子节点，再把每一行居中；子节点行超过可用宽度
// 时裁剪（rich 的 Align 同样不折行）。
type Align struct {
	// Child 是待居中的子节点。
	Child Renderable
}

// RenderLines 实现 Renderable。
//
// 复刻 rich align.py 的 __rich_console__：先量出**子树的最大行宽**作为整块宽度
// （不是逐行居中！），再把每一行补齐到这块宽度，最后统一左右平分剩余空间。
// 实测差异：`Align.center("居中\nsecond")` 在 40 列下首行左边距是 17 而不是 18
// ——因为块宽度取的是 6（"second" 的宽度），excess=34，left=17。
func (a Align) RenderLines(width int) []string {
	if width <= 0 || a.Child == nil {
		return nil
	}
	lines := a.Child.RenderLines(width)
	blockWidth := 0
	for _, line := range lines {
		if lineWidth := DisplayWidth(line); lineWidth > blockWidth {
			blockWidth = lineWidth
		}
	}
	excess := width - blockWidth
	left := 0
	if excess > 0 {
		left = excess / 2
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		padded := PadRight(line, blockWidth)
		if excess > 0 {
			out = append(out, strings.Repeat(" ", left)+padded+strings.Repeat(" ", excess-left))
			continue
		}
		out = append(out, padded)
	}
	return out
}

// AlignCenter 把节点水平居中，等价于 rich 的 `Align.center(renderable)`。
func AlignCenter(child Renderable) Renderable { return Align{Child: child} }

// Padding 是上下左右留白（逻辑行数/单元格数），对应 rich.padding.Padding。
type Padding struct {
	// Top、Right、Bottom、Left 分别是四边的留白量。
	Top, Right, Bottom, Left int
	// Child 是被包裹的节点。
	Child Renderable
}

// RenderLines 实现 Renderable。
func (p Padding) RenderLines(width int) []string {
	if p.Child == nil {
		return nil
	}
	inner := width - p.Left - p.Right
	if inner < 1 {
		inner = 1
	}
	lines := p.Child.RenderLines(inner)
	out := make([]string, 0, len(lines)+p.Top+p.Bottom)
	blank := strings.Repeat(" ", width)
	for i := 0; i < p.Top; i++ {
		out = append(out, blank)
	}
	prefix := strings.Repeat(" ", p.Left)
	for _, line := range lines {
		out = append(out, PadRight(prefix+line, width))
	}
	for i := 0; i < p.Bottom; i++ {
		out = append(out, blank)
	}
	return out
}

// PanelBorder 是面板边框用的四个字符组。
type PanelBorder struct {
	TopLeft, TopRight, BottomLeft, BottomRight rune
	Top, Bottom, Left, Right                   rune
}

// roundedBorder 对应 rich.box.ROUNDED。
var roundedBorder = PanelBorder{
	TopLeft: '╭', TopRight: '╮', BottomLeft: '╰', BottomRight: '╯',
	Top: '─', Bottom: '─', Left: '│', Right: '│',
}

// Panel 是带标题/副标题的边框面板，对应 rich.panel.Panel。
type Panel struct {
	// Content 是面板内容；nil 表示空面板。
	Content Renderable
	// Title 是顶部标题（会按 rich 标记解析后作为纯文本），空串表示没有标题。
	Title string
	// Subtitle 是底部副标题，空串表示没有副标题。
	Subtitle string
	// BorderStyle 记录边框样式名（如 "cyan"、"red"）。
	//
	// Go 侧不渲染颜色，字段保留是为了与 Python 调用点一一对应、便于未来接入
	// 真正的着色（届时只需在 RenderLines 里读取它）。
	BorderStyle string
	// Padding 是内容留白，零值表示 rich 的默认 (0,1,0,1)。
	Padding Padding
	// Width 是面板总宽；0 表示撑满可用宽度（rich 的 expand=True）。
	Width int
	// Height 是面板总高；0 表示由内容决定。
	Height int
	// Border 是边框字符组；零值用 ROUNDED。
	Border PanelBorder
}

// RenderLines 实现 Renderable，逐条复刻 rich panel.py 的排版：
//
//	width = min(可用宽度, self.width)
//	child_width = width - 2
//	内容宽 = child_width - 左右留白
//	标题行 = 边框角 + 边框线 + 居中(「 标题 」, width-4) + 边框线 + 边框角
//
// 标题/副标题的居中规则（rich panel.py 的 align_text）：先把「 标题 」截断到
// width-4，再把剩余空间按 excess//2 分到左边、其余分到右边。width<=4 时 rich
// 直接不画标题，这里保持一致。
func (p Panel) RenderLines(width int) []string {
	border := p.Border
	if border.Top == 0 {
		border = roundedBorder
	}
	panelWidth := width
	if p.Width > 0 && p.Width < panelWidth {
		panelWidth = p.Width
	}
	if panelWidth < 1 {
		return nil
	}
	if panelWidth == 1 {
		return []string{string(border.TopLeft)}
	}
	pad := p.Padding
	if pad == (Padding{}) {
		pad = Padding{Right: 1, Left: 1}
	}
	childWidth := panelWidth - 2
	contentWidth := childWidth - pad.Left - pad.Right

	var body []string
	if p.Content != nil && contentWidth >= 1 {
		for _, line := range p.Content.RenderLines(contentWidth) {
			body = append(body, PadRight(TruncateCells(line, contentWidth), contentWidth))
		}
	}

	top := string(border.TopLeft) + strings.Repeat(string(border.Top), panelWidth-2) + string(border.TopRight)
	if title := StripMarkup(p.Title); p.Title != "" && panelWidth > 4 {
		top = string(border.TopLeft) + string(border.Top) +
			alignBorderText(title, panelWidth-4, border.Top) +
			string(border.Top) + string(border.TopRight)
	}
	bottom := string(border.BottomLeft) + strings.Repeat(string(border.Bottom), panelWidth-2) + string(border.BottomRight)
	if subtitle := StripMarkup(p.Subtitle); p.Subtitle != "" && panelWidth > 4 {
		bottom = string(border.BottomLeft) + string(border.Bottom) +
			alignBorderText(subtitle, panelWidth-4, border.Bottom) +
			string(border.Bottom) + string(border.BottomRight)
	}

	inner := make([]string, 0, len(body)+2)
	side := strings.Repeat(" ", pad.Left)
	for _, line := range body {
		inner = append(inner, PadRight(TruncateCells(side+line, childWidth), childWidth))
	}
	if p.Height > 0 {
		inner = fitLines(inner, p.Height-2, false)
	}
	out := make([]string, 0, len(inner)+2)
	out = append(out, top)
	for _, line := range inner {
		out = append(out, string(border.Left)+line+string(border.Right))
	}
	out = append(out, bottom)
	return out
}

// alignBorderText 把边框上的标题文字排版到 width 个单元格：文字两侧各留一个
// 空格，剩余空间左右平分（左边少一个），不足时按单元格截断。复刻 rich
// panel.py 的 align_text(title_text, width, "center", box.top)。
func alignBorderText(text string, width int, fill rune) string {
	text = " " + text + " "
	if width <= 0 {
		return ""
	}
	// rich 用 Text.truncate → set_cell_size，宽字符被边界截断时用空格补齐，
	// 所以这里必须用 cropCells 而不是纯截断。
	text = cropCells(text, width)
	excess := width - DisplayWidth(text)
	if excess <= 0 {
		return text
	}
	left := excess / 2
	return strings.Repeat(string(fill), left) + text + strings.Repeat(string(fill), excess-left)
}

// TableColumn 描述一个表格列。为控制复杂度，Go 侧的列宽是固定值，不做 rich
// 的列宽分配（见文件头「刻意保留的差异」）。
type TableColumn struct {
	// Width 是列的内容宽度（不含列间的一个空格）。
	Width int
	// Align 取 "left" / "center" / "right"。
	Align string
}

// Table 是简化版表格，对应 rich.table.Table 在 tui.py 里的两种用法。
type Table struct {
	// Columns 是列定义，顺序与 Rows 中单元格一致。
	Columns []TableColumn
	// Rows 是行内容（会按 rich 标记解析）。
	Rows [][]string
}

// RenderLines 实现 Renderable：每行按列宽补齐，列间用一个空格分隔。
//
// Width 为 0 表示「不固定宽度」：不截断也不补齐（MenuTable 的文案列就用它）。
func (t Table) RenderLines(width int) []string {
	out := make([]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		var b strings.Builder
		for index, cell := range row {
			column := TableColumn{Align: "left"}
			if index < len(t.Columns) {
				column = t.Columns[index]
			}
			value := StripMarkup(cell)
			if column.Width > 0 {
				value = TruncateCells(value, column.Width)
				switch column.Align {
				case "center":
					pad := column.Width - DisplayWidth(value)
					if pad > 0 {
						left := pad / 2
						value = strings.Repeat(" ", left) + value + strings.Repeat(" ", pad-left)
					}
				case "right":
					value = PadLeft(value, column.Width)
				default:
					value = PadRight(value, column.Width)
				}
			}
			if index > 0 {
				b.WriteString(" ")
			}
			b.WriteString(value)
		}
		out = append(out, TruncateCells(b.String(), width))
	}
	return out
}

// fitLines 把 lines 裁剪或补空到 height 行，对应 rich 的 `lines[:height]` 与
// 面板的 pad 行为。
func fitLines(lines []string, height int, preserveBottom bool) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) > height {
		if preserveBottom {
			return lines[len(lines)-height:]
		}
		return lines[:height]
	}
	out := make([]string, 0, height)
	out = append(out, lines...)
	for len(out) < height {
		out = append(out, "")
	}
	return out
}

// coerceRenderable 把 Python 风格的「Any」参数收敛成 Renderable。
//
// 支持的形态与 rich 一致：Renderable 原样、string 按标记解析、[]string 视为
// 多个子节点、nil 视为空。其他类型会被转成其字符串形式（fail-open，避免
// 未来移植时因为类型不符而崩）。
func coerceRenderable(value any) Renderable {
	switch typed := value.(type) {
	case nil:
		return nil
	case Renderable:
		return typed
	case string:
		return NewMarkup(typed)
	case []string:
		items := make([]Renderable, 0, len(typed))
		for _, item := range typed {
			items = append(items, NewMarkup(item))
		}
		return Group{Items: items}
	case []Renderable:
		return Group{Items: typed}
	default:
		return nil
	}
}

// Renderables 把混合参数收敛成 Renderable 切片，方便移植时照抄 Python 的
// `[page_title(...), content, section_panel(...)]` 写法。
func Renderables(values ...any) []Renderable {
	out := make([]Renderable, 0, len(values))
	for _, value := range values {
		out = append(out, coerceRenderable(value))
	}
	return out
}
