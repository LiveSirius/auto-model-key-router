package tui

// 本文件提供纯文本/富文本的基础工具：ANSI 剥离、rich 标记剥离、按终端单元格
// 计算显示宽度、截断与折行。
//
// 为什么宽度要自己实现：rich 的 cell_len 按**终端单元格**测量（CJK 宽 2），
// tui.py 里所有居中、截断、折行都以此为准（tui.py:116、tui.py:608-610）。
// Go 标准库没有 East Asian Width 表，而迁移约束不允许再引入第三方依赖，因此
// 这里内置一张常用区段的宽字符表。已知差异：emoji 组合序列（ZWJ/旗帜）的宽度
// 可能与 rich 不一致，见 divergence_test.go；语料刻意只用 ASCII 与 CJK。

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansiEscape 匹配 CSI 与 OSC 转义序列（用于测量前剥离样式）。
//
// 只覆盖这两类：终端 UI 只用得到它们（rich 的颜色、光标控制、OSC 52）。
var ansiSequence = func() func(string) string {
	// 手写扫描比正则更快也更明确：CSI 形如 ESC [ 参数 终字节，OSC 形如 ESC ] ... BEL。
	return func(s string) string {
		if !strings.ContainsRune(s, '\x1b') {
			return s
		}
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); {
			if s[i] != '\x1b' {
				b.WriteByte(s[i])
				i++
				continue
			}
			if i+1 >= len(s) {
				break
			}
			switch s[i+1] {
			case '[': // CSI: 参数字节 0x30-0x3F，中间字节 0x20-0x2F，终字节 0x40-0x7E
				j := i + 2
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3F {
					j++
				}
				if j < len(s) {
					j++
				}
				i = j
			case ']': // OSC: 直到 BEL 或 ST
				j := i + 2
				for j < len(s) && s[j] != '\a' {
					if s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\' {
						j++
						break
					}
					j++
				}
				if j < len(s) {
					j++
				}
				i = j
			default:
				i += 2
			}
		}
		return b.String()
	}
}()

// StripANSI 去掉字符串里的 CSI/OSC 转义序列，便于按纯文本测量与断言。
func StripANSI(s string) string { return ansiSequence(s) }

// wideRanges 是 East Asian Width 为 W/F 的区段（闭区间），值来自 Unicode 15 的
// EastAsianWidth.txt 常用部分。rich 用的是 wcwidth 系列实现，二者在这些区段上一致。
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, // 谚文字母
	{0x2E80, 0x303E}, // 中日韩部首、标点
	{0x3041, 0x33FF}, // 平假名、片假名、注音、中日韩兼容
	{0x3400, 0x4DBF}, // 中日韩扩展 A
	{0x4E00, 0x9FFF}, // 中日韩统一表意文字
	{0xA000, 0xA4CF}, // 彝文
	{0xA960, 0xA97F}, // 谚文字母扩展 A
	{0xAC00, 0xD7A3}, // 谚文音节
	{0xF900, 0xFAFF}, // 中日韩兼容表意文字
	{0xFE10, 0xFE19}, // 竖排标点
	{0xFE30, 0xFE6F}, // 中日韩兼容形式
	{0xFF00, 0xFF60}, // 全角形式
	{0xFFE0, 0xFFE6}, // 全角符号
	{0x1F300, 0x1F64F},
	{0x1F900, 0x1F9FF},
	{0x20000, 0x2FFFD},
	{0x30000, 0x3FFFD},
}

// runeWidth 返回单个码点的显示宽度：0（组合/控制）、1 或 2。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7F && r < 0xA0):
		return 0
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFEFF:
		return 0
	case r >= 0xFE00 && r <= 0xFE0F: // 变体选择符
		return 0
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	}
	for _, rng := range wideRanges {
		if r >= rng[0] && r <= rng[1] {
			return 2
		}
	}
	return 1
}

// DisplayWidth 返回字符串按终端单元格计的显示宽度（先剥离 ANSI 转义）。
func DisplayWidth(s string) int {
	s = StripANSI(s)
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	return width
}

// truncateCells 按显示宽度把字符串截断到 width 个单元格以内，返回截断结果与
// 是否发生了截断。
//
// 宽字符（2 格）落在边界上时整体丢弃该字符，不产生半格——与 rich 的
// `Text.truncate` 一致（rich/text.py 用 cell_len 逐段裁剪）。
func truncateCells(s string, width int) (string, bool) {
	if width <= 0 {
		return "", s != ""
	}
	plain := StripANSI(s)
	if DisplayWidth(plain) <= width {
		return s, false
	}
	used := 0
	end := 0
	for index, r := range plain {
		rw := runeWidth(r)
		if used+rw > width {
			end = index
			return plain[:end], true
		}
		used += rw
		end = index + utf8.RuneLen(r)
	}
	return plain[:end], true
}

// TruncateCells 按显示宽度截断，返回截断后的字符串。
func TruncateCells(s string, width int) string {
	out, _ := truncateCells(s, width)
	return out
}

// cropCells 复刻 rich 的 `set_cell_size`（rich/cells.py）在「裁剪超宽行」时的行为：
//
//   - 未超宽时原样返回（rich 只在行超出宽度时才调整）；
//   - 超宽时裁到 width 个单元格；若最后一个宽字符落在边界上被丢掉，
//     用空格补齐到恰好 width（实测：宽度 5 的「中文内容测试」得到「中文 」）。
//
// 折行与 no_wrap 渲染都要用它，否则语料里 trailing space / 中文截断两条会不一致。
func cropCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if DisplayWidth(s) <= width {
		return s
	}
	head, _ := truncateCells(s, width)
	return PadRight(head, width)
}

// TruncateEllipsis 复刻 rich 的 `overflow="ellipsis"`：超宽时留一格放省略号。
//
// shortcut_text 用它（tui.py:108 的 `overflow="ellipsis"`），语料里有对应的
// 长快捷键提示用例。rich 的实现在 rich/text.py 的 truncate：
// `set_cell_size(plain, max_width - 1) + "…"`，因此被丢掉的宽字符会用空格补齐。
func TruncateEllipsis(s string, width int) string {
	if DisplayWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	return cropCells(s, width-1) + "…"
}

// PadRight 用空格把字符串补到 width 个单元格。
//
// 已经超宽时不截断（rich 的 `_adjust_line_length` 只在 pad 时补齐，裁剪由
// 调用方决定），调用方需自行保证宽度。
func PadRight(s string, width int) string {
	pad := width - DisplayWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// PadLeft 用空格把字符串补到 width 个单元格（右对齐）。
func PadLeft(s string, width int) string {
	pad := width - DisplayWidth(s)
	if pad <= 0 {
		return s
	}
	return strings.Repeat(" ", pad) + s
}

// markupTag 匹配 rich 的标记正则 `(\*)\[([a-z#/@][^[]*?)]`（rich/markup.py）。
//
// 关键点（已用真实 rich 验证）：只有 `[` 后面紧跟小写字母、`#`、`/`、`@` 才当作
// 标签，因此 `[100]`、`[]` 原样保留（tui.py 的文案里出现过这类方括号）。
func isMarkupTagStart(rest string) bool {
	if rest == "" {
		return false
	}
	c := rest[0]
	switch {
	case c >= 'a' && c <= 'z':
	case c == '#' || c == '/' || c == '@':
	default:
		return false
	}
	return true
}

// StripMarkup 把 rich 标记渲染成纯文本（等价于 rich.markup.render 去掉样式）。
//
// 迁移方案已把「富文本降级为等价纯文本」定为不透明字符串，因此这里只保留文字。
// 与 rich 一致的行为：
//   - `\[` 转义为字面量 `[`（rich 的 `\` 转义）；
//   - 标签被丢弃，未知样式同样只丢弃不报错（rich 在渲染阶段才校验样式）；
//   - `[]`、`[100]` 不是标签，原样保留；
//   - `[[escaped]]` → rich 会把第二个 `[` 起的 `[escaped]` 当标签吃掉，剩 `[]`。
func StripMarkup(s string) string {
	if !strings.ContainsRune(s, '[') && !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '[' {
			b.WriteByte('[')
			i += 2
			continue
		}
		if c == '[' {
			end := strings.IndexByte(s[i+1:], ']')
			if end >= 0 && isMarkupTagStart(s[i+1:i+1+end]) {
				i += 1 + end + 1
				continue
			}
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// wrapLine 把单行文本按 width 个单元格贪心折行，等价于 rich 的
// `Text.wrap` + `divide_line(fold=True)` 在纯文本上的结果。
//
// 规则（逐条与真实 rich 对拍过，见 testdata/tui_corpus.json 的 wrap 段）：
//   - 优先在空白处断行，且**断行后去掉行尾空白**（rich 的 divide_line 用
//     `word.rstrip()` 计长，切分点落在空白前）；
//   - 单个词超过一行宽时按单元格硬切（fold=True）；
//   - 行首空白保留（rich 不再剥离前导空格）；
//   - 空行保持为空行。
func wrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if DisplayWidth(line) <= width {
		return []string{line}
	}
	var out []string
	var current strings.Builder
	currentWidth := 0
	flush := func() {
		out = append(out, current.String())
		current.Reset()
		currentWidth = 0
	}
	// 按「空白段」与「非空白段」交替处理，与 rich 的 words() 迭代一致。
	words := splitWords(line)
	for _, word := range words {
		wordWidth := DisplayWidth(strings.TrimRight(word, " \t"))
		if currentWidth > 0 && currentWidth+wordWidth > width {
			flush()
			word = strings.TrimLeft(word, " \t")
			if word == "" {
				continue
			}
			wordWidth = DisplayWidth(word)
		}
		if wordWidth <= width-currentWidth {
			current.WriteString(word)
			currentWidth += DisplayWidth(word)
			continue
		}
		// 需要硬切：先把整词切进剩余空间（可能为 0）。
		rest := word
		for rest != "" {
			space := width - currentWidth
			if space <= 0 {
				flush()
				space = width
			}
			head := TruncateCells(rest, space)
			if head == "" {
				flush()
				continue
			}
			current.WriteString(head)
			currentWidth += DisplayWidth(head)
			rest = rest[len(head):]
			if rest != "" {
				flush()
			}
		}
	}
	if currentWidth > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// splitWords 把一行切成「非空白段 + 其后的空白段」，与 rich.markup 的
// `words()` 正则一致（`\s*\S+\s*`）。
func splitWords(line string) []string {
	var words []string
	i := 0
	n := len(line)
	for i < n {
		start := i
		for i < n && isSpaceByte(line[i]) {
			i++
		}
		for i < n && !isSpaceByte(line[i]) {
			i++
		}
		for i < n && isSpaceByte(line[i]) {
			i++
		}
		if i == start {
			break
		}
		words = append(words, line[start:i])
	}
	return words
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// WrapText 把（可含换行的）纯文本按显示宽度折行。
//
// 折行后每一行都要按 cropCells 裁剪：rich 的 divide_line 会把行尾空白切进上一行
// （offending space），再由 Console.render_lines 把超宽的那一行裁掉。实测：
// 宽度 8 的 "trailing space " → ["trailing", "space "]（第一行 9 格被裁成 8 格）。
func WrapText(text string, width int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, wrapped := range wrapLine(line, width) {
			out = append(out, cropCells(wrapped, width))
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}
