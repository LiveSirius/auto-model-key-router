package agentconfig

import (
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// 迷你 tomlkit：格式保真的 TOML 读取 / 编辑 / 回写
//
// 参照实现用 tomlkit 处理 ~/.codex/config.toml（agent_config.py:318 解析、
// agent_config.py:352 回写）。tomlkit 是「格式保真」库：注释、空行、键序、
// 键与 = 之间的空白全部原样保留，因此 AMKR 只改动自己拥有的那几个键，用户手
// 写的其余内容逐字节不变。
//
// Go 生态没有等价物。pelletier/go-toml/v2 的 Unmarshal→Marshal 往返会丢掉
// **全部注释**（实测：3 行注释 → 0 行，键序与表结构也被重排，见 doc.go 的
// 记录），用它在用户手工维护的配置上属于不可接受的数据丢失。
//
// 所以这里实现一个只覆盖 Codex 配置所需子集的迷你 tomlkit：解析成与 tomlkit
// 同构的 body / trivia 模型，编辑与渲染逐条照抄 tomlkit 的 container.py 与
// items.py（引用处标注 tomlkit 源码位置），从而与 Python 输出逐字节一致。
// 一致性由 testdata/agentconfig_corpus.json 对拍锁定，oracle 是真实 tomlkit。
//
// 刻意不支持、遇到即显式报错的形状（绝不退化成有损重写）：
//   - 点号键（`a.b = 1`）：tomlkit 走 _handle_dotted_key，重建整棵隐式表；
//   - 数组表（`[[x]]`）：AMKR 的三个写入点都不可能落在 AoT 上；
//   - 行内表（`x = { ... }`）出现在 model_providers / model_providers.OpenAI。
//
// 这三类在真实 Codex 配置里都不出现；报错而不是丢数据是本迁移的硬约束。
// ─────────────────────────────────────────────────────────────────────────────

// tkTrivia 对应 tomlkit.items.Trivia（items.py:318）。零值 trail 为 "\n"，
// 与 tomlkit 的 dataclass 默认值一致：**新建**条目的行尾自带一个换行。
type tkTrivia struct {
	indent    string
	commentWS string
	comment   string
	trail     string
}

// newTrivia 返回 tomlkit 新建条目的 Trivia 默认值。
func newTrivia() tkTrivia { return tkTrivia{trail: "\n"} }

// tkKey 对应 tomlkit.items.SingleKey（items.py:393）。
type tkKey struct {
	// original 是 as_string()：裸键含尾部空白，带引号的键含引号与尾部空白。
	original string
	// name 是解析后的键名（去空白、去引号、反转义）。
	name string
	// sep 是键与值之间的原文，例如 "= "、"=   "、" ="。
	sep string
	// dotted 对应 Key.is_dotted()：点号键为 true。
	dotted bool
}

// tkValueClass 是值的粗分类，只用于判断能否当作 TOML 表处理。
type tkValueClass int

const (
	// tkScalar 是字符串 / 数字 / 布尔 / 日期时间。
	tkScalar tkValueClass = iota
	// tkArray 是行内数组。
	tkArray
	// tkInlineTable 是行内表 `{ ... }`。
	tkInlineTable
	// tkAoT 是数组表表体。
	tkAoT
)

// tkValue 是键值对的值：原文 + 粗分类。
//
// 只保留原文就够了——AMKR 从不修改用户已有的值文本，只在整体替换该键时写入
// 新渲染的值；不改动的值原样输出。
type tkValue struct {
	class tkValueClass
	raw   string
}

// tkEntryKind 区分 body 中的四类条目。
type tkEntryKind int

const (
	tkEntryWhitespace tkEntryKind = iota
	tkEntryComment
	tkEntryKeyValue
	tkEntryTable
)

// tkEntry 对应 tomlkit body 里的一项：(key, item)。
//
// key 为 nil 的条目标志着「空白行」或「整行注释」，对应 tomlkit 中
// _raw_append(None, Whitespace/Comment) 留下的 (None, item)。
type tkEntry struct {
	kind tkEntryKind
	// raw 是空白行 / 注释行的原文（Whitespace.as_string / Comment.as_string）。
	raw string
	// trivia 是键值对与表头的装饰信息。
	trivia tkTrivia
	// key 为 nil 表示该条目没有键（空白行、整行注释）。
	key   *tkKey
	value *tkValue
	table *tkTable
	// removed 对应 tomlkit 把 body 项替换成 (None, Null())：保留位置但不渲染。
	removed bool
}

// tkTable 对应 tomlkit.items.Table（items.py:1847）。
type tkTable struct {
	key *tkKey
	// displayName 对应 Table.display_name：非空时表头按它整段输出（保留用户
	// 写的原始大小写与引号），否则用 prefix + key 拼出。空串等价于 None。
	displayName string
	trivia      tkTrivia
	// superSet / super 对应 Table._is_super_table：解析出来的表恒有显式取值，
	// 新建的表为 None（渲染时按子项推断）。
	superSet bool
	super    bool
	// aot 表示这是 `[[...]]` 数组表的一个元素。
	aot  bool
	body *tkContainer
}

// tkContainer 对应 tomlkit.container.Container。
type tkContainer struct {
	entries []*tkEntry
	// parsed 对应 Container._parsed。解析出的表体为 true——此时 tomlkit 的
	// 「插入到最后一个非表项之后」分支被跳过，新键一律 _raw_append。
	// 解析出的**文档根**在 parse() 末尾被 parsing(False)，是唯一为 false 的
	// 解析容器（parser.py:173）。
	parsed bool
}

// ───────────────────────────────────────── 解析 ─────────────────────────────

// tomlParseError 是迷你 tomlkit 的解析失败。
type tomlParseError struct {
	offset int
	line   int
	column int
	reason string
}

func (e *tomlParseError) Error() string {
	return fmt.Sprintf("%s at line %d col %d", e.reason, e.line, e.column)
}

// tomlParser 是逐字符扫描器，位置语义与 tomlkit 的 _Source 一致。
type tomlParser struct {
	src string
	pos int
}

// parseTOMLDocument 解析文档并返回根容器。
//
// 与 tomlkit 一样：解析期间容器处于 parsed=true（TOMLDocument(True) /
// Container(True)），解析结束调用 parsing(False)（parser.py:173）。注意
// Container.parsing 会**递归**清掉所有嵌套表体的 _parsed（container.py:90），
// 因此解析完成后文档里的每一个容器都处于「可变」状态——这决定了新增键走
// 「插入到最后一个非表项之后」还是单纯追加，是本实现与参照实现对齐的关键。
func parseTOMLDocument(src string) (*tkContainer, error) {
	root := &tkContainer{parsed: true}
	p := &tomlParser{src: src}
	if err := p.parseBody(root); err != nil {
		return nil, err
	}
	clearParsed(root)
	return root, nil
}

// clearParsed 复刻 Container.parsing(False) 的递归效果。
func clearParsed(c *tkContainer) {
	c.parsed = false
	for _, e := range c.entries {
		switch {
		case e.kind == tkEntryTable && e.table != nil:
			clearParsed(e.table.body)
		}
	}
}

// parseBody 解析从当前位置到文件末尾的全部内容，把条目写入 root。
//
// 结构上复刻 tomlkit 的 parser.parse()（parser.py:137）：
//  1. 先收集所有不属于任何表的条目，遇到第一个 `[` 停止；
//  2. 之后每个 `[a.b]` 头都会把 a、b 逐级挂到根容器下，KV 写进最内层表体。
func (p *tomlParser) parseBody(root *tkContainer) error {
	current := root
	for p.pos < len(p.src) {
		start := p.pos
		ws := p.skipSpaces()
		if p.pos >= len(p.src) {
			// 文件末尾只剩空白：tomlkit 的 _parse_item 会返回 Whitespace。
			appendWhitespace(current, ws)
			return nil
		}
		switch p.src[p.pos] {
		case '\n':
			p.pos++
			appendWhitespace(current, p.src[start:p.pos])
			continue
		case '\r':
			if err := p.expectCRLF(); err != nil {
				return err
			}
			appendWhitespace(current, p.src[start:p.pos])
			continue
		case '#':
			current.entries = append(current.entries, p.parseComment(ws))
			continue
		case '[':
			// tomlkit 的 parser.parse() 遇到第一个表头后就不再往**根**容器收集
			// KV，后续 KV 一律写进当前表体（本循环的 current）。
			table, err := p.parseTableHeader(ws)
			if err != nil {
				return err
			}
			body, err := p.attachTable(root, table)
			if err != nil {
				return err
			}
			current = body
			continue
		}
		key, value, trivia, err := p.parseKeyValue(ws)
		if err != nil {
			return err
		}
		// TOML 禁止同一张表里出现重复键。go-toml/v2 的 unstable parser 是**语法**
		// 解析器，不查这一条（实测：`model = "a"` 后紧跟 `model = "b"` 它接受、
		// tomlkit 拒绝），所以必须在这里自己拦：否则 Go 会去改写一个 Python 会拒收
		// 的文件，并把重复键原样留在用户配置里。见 doc.go 的 D8。
		if current.findEntry(key.name) != nil {
			return p.errf("键重复: " + key.name)
		}
		current.entries = append(current.entries, &tkEntry{
			kind: tkEntryKeyValue, key: key, value: value, trivia: trivia,
		})
	}
	return nil
}

// skipSpaces 消费行首的水平空白并返回原文（tomlkit 的 is_spaces 为 " \t"）。
func (p *tomlParser) skipSpaces() string {
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	return p.src[start:p.pos]
}

// parseComment 解析一整行注释（调用时位置在 '#'）。
func (p *tomlParser) parseComment(indent string) *tkEntry {
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '\n' && p.src[p.pos] != '\r' {
		p.pos++
	}
	text := p.src[start:p.pos]
	trail := p.consumeNewline()
	return &tkEntry{
		kind: tkEntryComment,
		raw:  indent + text + trail,
		trivia: tkTrivia{
			indent: indent, comment: text, trail: trail,
		},
	}
}

// consumeNewline 消费一个换行（LF 或 CRLF）并返回其原文。
func (p *tomlParser) consumeNewline() string {
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '\r' {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '\n' {
		p.pos++
	}
	return p.src[start:p.pos]
}

// expectCRLF 校验孤立的 \r（tomlkit 会报 InvalidControlChar）。
func (p *tomlParser) expectCRLF() error {
	if p.pos+1 >= len(p.src) || p.src[p.pos+1] != '\n' {
		return p.errf("非法的控制字符 \\r")
	}
	p.pos += 2
	return nil
}

// appendWhitespace 追加一个空白条目，并复刻 tomlkit 的 _merge_ws（parser.py:177）：
// 相邻空白条目会合并成一项。合并只影响渲染以外的语义（_get_last_index_before_table
// 会跳过全部非 fixed 的 Whitespace），合并后原文是简单拼接。
func appendWhitespace(c *tkContainer, text string) {
	if text == "" {
		return
	}
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == tkEntryWhitespace {
		c.entries[n-1].raw += text
		return
	}
	c.entries = append(c.entries, &tkEntry{kind: tkEntryWhitespace, raw: text})
}

// parseKey 解析一个键（裸键或带引号的键），复刻 parser.py:371-439。
//
// 返回 (key, 是否点号键)。裸键的 original 含键名之后的空白；带引号的键的
// original 含引号与之后的空白。
func (p *tomlParser) parseKey() (*tkKey, error) {
	leading := p.skipSpaces()
	if p.pos < len(p.src) && (p.src[p.pos] == '"' || p.src[p.pos] == '\'') {
		return p.parseQuotedKey(leading)
	}
	return p.parseBareKey(leading)
}

// parseBareKey 解析裸键（parser.py:414）。
func (p *tomlParser) parseBareKey(leading string) (*tkKey, error) {
	start := p.pos
	for p.pos < len(p.src) && (isBareKeyChar(p.src[p.pos]) || p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	original := leading + p.src[start:p.pos]
	name := strings.TrimSpace(original)
	if name == "" {
		return nil, p.errf("空键名")
	}
	if strings.Contains(name, " ") || strings.Contains(name, "\t") {
		return nil, p.errf("裸键中不能含空白: " + name)
	}
	return &tkKey{original: original, name: name, sep: "", dotted: false}, nil
}

// parseQuotedKey 解析带引号的键（parser.py:385）。引号内是单行字符串，
// 支持 TOML 的转义规则，但键名本身不参与格式化保留（只保留 original 原文）。
func (p *tomlParser) parseQuotedKey(leading string) (*tkKey, error) {
	quote := p.src[p.pos]
	start := p.pos
	value, err := p.scanQuotedSingle(quote == '"')
	if err != nil {
		return nil, err
	}
	original := leading + p.src[start:p.pos]
	trailingStart := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	original += p.src[trailingStart:p.pos]
	return &tkKey{original: original, name: value, sep: "", dotted: false}, nil
}

// isBareKeyChar 对应 toml_char.py:18：字母、数字、'-'、'_'。
func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// parseKeyValue 解析一行键值对（parser.py:324）。
//
// 返回的 trivia 里 indent 是行首空白，commentWS/comment/trail 是值之后的
// 注释与换行；key.sep 是键名与值之间的原文。
func (p *tomlParser) parseKeyValue(indent string) (*tkKey, *tkValue, tkTrivia, error) {
	key, err := p.parseKey()
	if err != nil {
		return nil, nil, tkTrivia{}, err
	}
	if err := p.parseKeySeparator(key); err != nil {
		return nil, nil, tkTrivia{}, err
	}
	value, err := p.parseValue()
	if err != nil {
		return nil, nil, tkTrivia{}, err
	}
	cws, comment, trail := p.parseCommentTrail()
	return key, value, tkTrivia{
		indent: indent, commentWS: cws, comment: comment, trail: trail,
	}, nil
}

// parseKeySeparator 复刻 parser.py:336-351：消费 "= \t" 直到 `=`，随后把
// 「值之前的空白」并入 sep（tomlkit 用 extract() 从键名末尾开始取）。
func (p *tomlParser) parseKeySeparator(key *tkKey) error {
	start := p.pos
	foundEquals := false
	for p.pos < len(p.src) && (p.src[p.pos] == '=' || p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		if p.src[p.pos] == '=' {
			if foundEquals {
				return p.errf("重复的 '='")
			}
			foundEquals = true
		}
		p.pos++
	}
	if !foundEquals {
		return p.errf("缺少 '='")
	}
	key.sep = p.src[start:p.pos]
	return nil
}

// parseCommentTrail 复刻 parser.py:275-322。
//
// 返回 (值后注释前空白, 注释原文, 行尾原文)。没有注释时 marker 停在函数入口，
// 因此「行尾空白 + 换行」会整体进入 trail，从而原样保留。
func (p *tomlParser) parseCommentTrail() (commentWS, comment, trail string) {
	marker := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\n' || c == '\r' {
			break
		}
		if c == '#' {
			commentWS = p.src[marker:p.pos]
			commentStart := p.pos
			for p.pos < len(p.src) && p.src[p.pos] != '\n' && p.src[p.pos] != '\r' {
				p.pos++
			}
			comment = p.src[commentStart:p.pos]
			marker = p.pos
			break
		}
		if c == ' ' || c == '\t' {
			p.pos++
			continue
		}
		break
	}
	// tomlkit 在入口处 mark，行尾空白与换行都从那个位置 extract()，因此值之后的
	// 空白必须算进 trail（否则 `model = "x"   \n` 会丢掉三个空格）。
	trailStart := marker
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '\r' {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '\n' {
		p.pos++
	}
	if p.pos != marker || (p.pos < len(p.src) && isTOMLWS(p.src[p.pos])) {
		trail = p.src[trailStart:p.pos]
	}
	return commentWS, comment, trail
}

// isTOMLWS 对应 toml_char.py:16：空白与换行。
func isTOMLWS(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// parseValue 解析一个值，只保留它的原文与粗分类。
func (p *tomlParser) parseValue() (*tkValue, error) {
	if p.pos >= len(p.src) {
		return nil, p.errf("缺少值")
	}
	switch c := p.src[p.pos]; {
	case c == '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			raw, err := p.scanMultilineString('"')
			if err != nil {
				return nil, err
			}
			return &tkValue{class: tkScalar, raw: raw}, nil
		}
		start := p.pos
		if _, err := p.scanQuotedSingle(true); err != nil {
			return nil, err
		}
		return &tkValue{class: tkScalar, raw: p.src[start:p.pos]}, nil
	case c == '\'':
		if strings.HasPrefix(p.src[p.pos:], "'''") {
			raw, err := p.scanMultilineString('\'')
			if err != nil {
				return nil, err
			}
			return &tkValue{class: tkScalar, raw: raw}, nil
		}
		start := p.pos
		if _, err := p.scanQuotedSingle(false); err != nil {
			return nil, err
		}
		return &tkValue{class: tkScalar, raw: p.src[start:p.pos]}, nil
	case c == '[':
		raw, err := p.scanBalanced('[', ']')
		if err != nil {
			return nil, err
		}
		return &tkValue{class: tkArray, raw: raw}, nil
	case c == '{':
		raw, err := p.scanBalanced('{', '}')
		if err != nil {
			return nil, err
		}
		return &tkValue{class: tkInlineTable, raw: raw}, nil
	default:
		start := p.pos
		for p.pos < len(p.src) && !isValueTerminator(p.src[p.pos]) {
			p.pos++
		}
		if p.pos == start {
			return nil, p.errf(fmt.Sprintf("非法的值起始字符 %q", p.src[p.pos]))
		}
		return &tkValue{class: tkScalar, raw: p.src[start:p.pos]}, nil
	}
}

// isValueTerminator 对应 parser.py:470 的停止集合：" \t\n\r#,]}"。
func isValueTerminator(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '#', ',', ']', '}':
		return true
	}
	return false
}

// scanQuotedSingle 扫描一个单行字符串（含引号），返回其内容。
// basic 为 true 时按基本字符串处理转义。
func (p *tomlParser) scanQuotedSingle(basic bool) (string, error) {
	quote := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == quote {
			p.pos++
			return b.String(), nil
		}
		if c == '\n' || c == '\r' {
			return "", p.errf("单行字符串中不能换行")
		}
		if basic && c == '\\' {
			r, n, err := p.decodeEscape()
			if err != nil {
				return "", err
			}
			b.WriteString(r)
			p.pos += n
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", p.errf("字符串没有闭合")
}

// scanMultilineString 扫描 """ / ”' 字符串，返回含定界符的原文。
func (p *tomlParser) scanMultilineString(quote byte) (string, error) {
	start := p.pos
	delim := strings.Repeat(string(quote), 3)
	p.pos += 3
	for p.pos < len(p.src) {
		if p.src[p.pos] == '\\' && quote == '"' {
			_, n, err := p.decodeEscape()
			if err != nil {
				return "", err
			}
			p.pos += n
			continue
		}
		if strings.HasPrefix(p.src[p.pos:], delim) {
			// TOML 允许 """..."""" 这类「内容以引号结尾」的写法，最多吃掉两个。
			p.pos += 3
			for quote == '"' && p.pos < len(p.src) && p.src[p.pos] == '"' && p.pos-start < len(p.src) {
				if p.pos+1 < len(p.src) && p.src[p.pos+1] == '"' {
					p.pos++
					continue
				}
				break
			}
			return p.src[start:p.pos], nil
		}
		p.pos++
	}
	return "", p.errf("多行字符串没有闭合")
}

// scanBalanced 扫描一个可跨行的括号结构，返回含括号的原文。
// 数组里允许出现注释与换行（TOML 1.0），因此必须识别字符串与注释。
func (p *tomlParser) scanBalanced(open, close byte) (string, error) {
	start := p.pos
	depth := 0
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '"' || c == '\'':
			if strings.HasPrefix(p.src[p.pos:], strings.Repeat(string(c), 3)) {
				if _, err := p.scanMultilineString(c); err != nil {
					return "", err
				}
				continue
			}
			if _, err := p.scanQuotedSingle(c == '"'); err != nil {
				return "", err
			}
			continue
		case c == '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				p.pos++
				return p.src[start:p.pos], nil
			}
		}
		p.pos++
	}
	return "", p.errf("括号没有闭合")
}

// decodeEscape 解码一个 TOML 转义序列，返回 (文本, 消耗字节数)。
func (p *tomlParser) decodeEscape() (string, int, error) {
	if p.pos+1 >= len(p.src) {
		return "", 0, p.errf("转义序列不完整")
	}
	c := p.src[p.pos+1]
	switch c {
	case 'b':
		return "\b", 2, nil
	case 't':
		return "\t", 2, nil
	case 'n':
		return "\n", 2, nil
	case 'f':
		return "\f", 2, nil
	case 'r':
		return "\r", 2, nil
	case '"':
		return "\"", 2, nil
	case '\\':
		return "\\", 2, nil
	case 'u', 'U':
		width := 4
		if c == 'U' {
			width = 8
		}
		if p.pos+2+width > len(p.src) {
			return "", 0, p.errf("\\u 转义不完整")
		}
		code := 0
		for i := 0; i < width; i++ {
			digit := hexDigit(p.src[p.pos+2+i])
			if digit < 0 {
				return "", 0, p.errf("非法的 \\u 转义")
			}
			code = code*16 + digit
		}
		return string(rune(code)), 2 + width, nil
	}
	return "", 0, p.errf("未知的转义序列")
}

// hexDigit 返回十六进制数字的值，非法时返回 -1。
func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseTableHeader 解析 `[a.b]` / `[[a.b]]`，返回最外层表（container.py 的
// parser._parse_table，parser.py:921）。
func (p *tomlParser) parseTableHeader(indent string) (*tkTable, error) {
	if p.pos >= len(p.src) || p.src[p.pos] != '[' {
		return nil, p.errf("表头缺少 '['")
	}
	p.pos++
	aot := false
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		aot = true
		p.pos++
	}
	var parts []*tkKey
	for {
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		key.sep = ""
		parts = append(parts, key)
		if p.pos < len(p.src) && p.src[p.pos] == '.' {
			p.pos++
			continue
		}
		break
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ']' {
		return nil, p.errf("表头缺少 ']'")
	}
	p.pos++
	if aot {
		if p.pos >= len(p.src) || p.src[p.pos] != ']' {
			return nil, p.errf("数组表头缺少 ']]'")
		}
		p.pos++
	}
	cws, comment, trail := p.parseCommentTrail()
	full := joinKeyOriginals(parts)

	outer := &tkTable{
		key:   parts[0],
		body:  &tkContainer{parsed: true},
		super: true, superSet: true, aot: aot,
	}
	if len(parts) == 1 {
		// 只有一级：这张表本身就是用户写的那张表，表头原文保留在 displayName。
		outer.displayName = full
		outer.super = false
		outer.trivia = tkTrivia{indent: indent, commentWS: cws, comment: comment, trail: trail}
		return outer, nil
	}
	// 多级：最外层是隐式父表（不渲染表头），逐级挂下去。
	// parser.py:998-1004 给隐式父表的是 Trivia("", cws, comment, trail)。
	outer.trivia = tkTrivia{indent: "", commentWS: cws, comment: comment, trail: trail}
	outer.displayName = ""
	cur := outer
	for i := 1; i < len(parts); i++ {
		child := &tkTable{
			key:   parts[i],
			body:  &tkContainer{parsed: true},
			super: i < len(parts)-1, superSet: true,
			aot: aot && i == len(parts)-1,
		}
		if i == len(parts)-1 {
			child.displayName = full
			child.trivia = tkTrivia{indent: indent, commentWS: cws, comment: comment, trail: trail}
		}
		appendTable(cur.body, parts[i], child)
		cur = child
	}
	return outer, nil
}

// joinKeyOriginals 复刻 DottedKey 的 original（items.py:456）：用 "." 拼接各级原文。
func joinKeyOriginals(parts []*tkKey) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.original)
	}
	return strings.Join(out, ".")
}

// attachTable 把表头解析结果挂到根容器下，返回应该继续写入的容器。
//
// 复刻 tomlkit parser.parse() 的 `body.append(key, value)`（parser.py:169）：
// 解析期根容器的 _parsed 为 true，因此不会做任何缩进 / display_name 调整；
// 同名父表已存在时走 Container.append 的 super table 合并分支
// （container.py:235-273），把新子表并进已有的隐式父表。
//
// 同时补上 tomlkit 会做、而 go-toml/v2 的语法解析器不做的合法性检查：显式表不能
// 重复定义，表也不能撞上已有的键值对（见 doc.go 的 D8）。
func (p *tomlParser) attachTable(root *tkContainer, table *tkTable) (*tkContainer, error) {
	existing := root.findEntry(table.key.name)
	if existing == nil {
		appendTable(root, table.key, table)
		return innermostBody(table), nil
	}
	if existing.kind != tkEntryTable || existing.table == nil {
		return nil, p.errf("表与已有的键冲突: " + table.key.name)
	}
	// 两次显式写同一个表头（[a] 之后又 [a]）在 TOML 里非法；而 [a.b] 之后写 [a]
	// 合法——那时已有的 a 是隐式父表，displayName 为空。
	if table.displayName != "" && existing.table.displayName != "" {
		return nil, p.errf("表重复定义: " + table.key.name)
	}
	return mergeInto(existing.table, table)
}

// mergeInto 把 src 的子表并入 dst，返回应该继续写入的容器。
func mergeInto(dst *tkTable, src *tkTable) (*tkContainer, error) {
	body := dst.body
	for _, e := range src.body.entries {
		if e.kind != tkEntryTable || e.table == nil {
			continue
		}
		existing := dst.body.findEntry(e.key.name)
		if existing != nil {
			if existing.kind != tkEntryTable || existing.table == nil {
				return nil, errTableConflict(e.key.name)
			}
			if existing.table.isSuper() && e.table.isSuper() {
				merged, err := mergeInto(existing.table, e.table)
				if err != nil {
					return nil, err
				}
				body = merged
				continue
			}
			if existing.table.displayName != "" && e.table.displayName != "" {
				return nil, errTableRedefined(e.key.name)
			}
		}
		appendTable(dst.body, e.key, e.table)
		body = innermostBody(e.table)
	}
	return body, nil
}

// errTableConflict / errTableRedefined 让 mergeInto 不必依赖解析器位置就能报出
// 与 attachTable 同形的错误。
func errTableConflict(name string) error {
	return errf("表与已有的键冲突: %s", name)
}

func errTableRedefined(name string) error {
	return errf("表重复定义: %s", name)
}

// innermostBody 返回一张表中真正承载 KV 的容器（多级表头时是最内层）。
func innermostBody(t *tkTable) *tkContainer {
	cur := t
	for {
		var next *tkTable
		for _, e := range cur.body.entries {
			if e.kind == tkEntryTable && e.table != nil && !e.removed {
				next = e.table
			}
		}
		if next == nil {
			return cur.body
		}
		cur = next
	}
}

// appendTable 复刻 Container.append 处理 Table 的分支（container.py:181-196）。
func appendTable(c *tkContainer, key *tkKey, table *tkTable) {
	table.key = key
	if !c.parsed {
		table.displayName = ""
	}
	prev := c.previousItem()
	prevWS := prev != nil && (prev.kind == tkEntryWhitespace || endsWithWhitespace(prev))
	if len(c.entries) > 0 && !c.parsed && table.trivia.indent == "" && !prevWS && key != nil && !key.dotted {
		table.trivia.indent = "\n"
	}
	c.entries = append(c.entries, &tkEntry{kind: tkEntryTable, key: key, table: table})
}

// errf 构造带位置信息的解析错误。
func (p *tomlParser) errf(reason string) error {
	line, column := 1, 1
	for i := 0; i < p.pos && i < len(p.src); i++ {
		if p.src[i] == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return &tomlParseError{offset: p.pos, line: line, column: column, reason: reason}
}

// previousItem 对应 Container._previous_item：最后一个未被删除的条目。
func (c *tkContainer) previousItem() *tkEntry {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if !c.entries[i].removed {
			return c.entries[i]
		}
	}
	return nil
}

// endsWithWhitespace 对应 container.py:1010：表（或数组表）的最后一个子项是空白。
func endsWithWhitespace(e *tkEntry) bool {
	if e.kind != tkEntryTable || e.table == nil {
		return false
	}
	last := e.table.body.previousItem()
	return last != nil && last.kind == tkEntryWhitespace
}

// ───────────────────────────────────────── 渲染 ─────────────────────────────

// Render 复刻 tomlkit Container.as_string()（container.py:527）。
func (c *tkContainer) Render() string {
	var b strings.Builder
	for _, e := range c.entries {
		if e.removed {
			continue
		}
		switch e.kind {
		case tkEntryTable:
			rendered := b.String()
			trimmed := strings.Trim(rendered, " ")
			if trimmed != "" && !strings.HasSuffix(trimmed, "\n") && !strings.Contains(e.table.trivia.indent, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(renderTable(e.key, e.table, ""))
		case tkEntryKeyValue:
			b.WriteString(renderSimpleItem(e.key, e, ""))
		case tkEntryComment:
			// Comment.as_string() 是 f"{indent}{comment}{trail}"（items.py:600）
			// ——必须从 trivia 现场渲染：插入逻辑会给它补前导换行。
			b.WriteString(e.trivia.indent + e.trivia.comment + e.trivia.trail)
		default:
			// 空白行：Whitespace.as_string() 直接返回原文。
			b.WriteString(e.raw)
		}
	}
	return b.String()
}

// renderSimpleItem 复刻 container.py:683 的 _render_simple_item。
func renderSimpleItem(key *tkKey, e *tkEntry, prefix string) string {
	renderedKey := key.original
	if prefix != "" {
		renderedKey = prefix + "." + renderedKey
	}
	return e.trivia.indent + renderedKey + key.sep + e.value.raw +
		e.trivia.commentWS + e.trivia.comment + e.trivia.trail
}

// renderTable 复刻 container.py:555 的 _render_table。
func renderTable(key *tkKey, t *tkTable, prefix string) string {
	renderedKey := t.displayName
	if renderedKey == "" {
		renderedKey = key.original
		if prefix != "" {
			renderedKey = prefix + "." + renderedKey
		}
	}
	var b strings.Builder
	if !t.isSuper() || hasNonTableChild(t) || hasDottedTableChild(t) {
		open, close := "[", "]"
		if t.aot {
			open, close = "[[", "]]"
		}
		newlineInTrivia := ""
		if !strings.Contains(t.trivia.trail, "\n") && t.body.dictLen() > 0 {
			newlineInTrivia = "\n"
		}
		b.WriteString(t.trivia.indent + open + renderedKey + close +
			t.trivia.commentWS + t.trivia.comment + t.trivia.trail + newlineInTrivia)
	} else if t.trivia.indent == "\n" {
		b.WriteString(t.trivia.indent)
	}
	for _, e := range t.body.entries {
		if e.removed {
			continue
		}
		switch e.kind {
		case tkEntryTable:
			current := b.String()
			trimmed := strings.Trim(current, " ")
			if trimmed != "" && !strings.HasSuffix(trimmed, "\n") && !strings.Contains(e.table.trivia.indent, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(renderTable(e.key, e.table, renderedKey))
		case tkEntryKeyValue:
			nested := ""
			if key.dotted {
				nested = renderedKey
			}
			b.WriteString(renderSimpleItem(e.key, e, nested))
		case tkEntryComment:
			b.WriteString(e.trivia.indent + e.trivia.comment + e.trivia.trail)
		default:
			b.WriteString(e.raw)
		}
	}
	return b.String()
}

// isSuper 复刻 Table.is_super_table（items.py:1933）。
func (t *tkTable) isSuper() bool {
	if t.superSet {
		return t.super
	}
	if t.body.dictLen() == 0 {
		return false
	}
	for _, e := range t.body.entries {
		if e.removed || e.key == nil {
			continue
		}
		if e.kind != tkEntryTable || e.table == nil || e.key.dotted {
			return false
		}
	}
	return true
}

// hasNonTableChild 复刻 _render_table 的第二个条件：表体里存在既不是表、也不是
// 空白/Null 的子项。注意 Comment 属于这一类（它不在 (Table, AoT, Whitespace,
// Null) 里），因此「只有子表 + 注释」的表仍会渲染自己的表头。
func hasNonTableChild(t *tkTable) bool {
	for _, e := range t.body.entries {
		switch e.kind {
		case tkEntryTable:
		case tkEntryWhitespace:
		default:
			return true
		}
	}
	return false
}

// hasDottedTableChild 复刻 _render_table 的第三个条件。
func hasDottedTableChild(t *tkTable) bool {
	for _, e := range t.body.entries {
		if e.kind == tkEntryTable && e.key != nil && e.key.dotted {
			return true
		}
	}
	return false
}

// dictLen 对应 Container.__len__：只数有键的存活条目（dict 视图的长度）。
func (c *tkContainer) dictLen() int {
	n := 0
	for _, e := range c.entries {
		if !e.removed && e.key != nil {
			n++
		}
	}
	return n
}

// ───────────────────────────────────────── 编辑 ─────────────────────────────

// findEntry 按键名查找存活条目（键值对或表）。
func (c *tkContainer) findEntry(name string) *tkEntry {
	for _, e := range c.entries {
		if e.removed || e.key == nil {
			continue
		}
		if e.key.name == name && !e.key.dotted {
			return e
		}
	}
	return nil
}

// setScalar 复刻 Container.__setitem__ → _replace_at（container.py:717 与 :742）。
//
// 已存在同类型条目时只换值，**复用原 trivia 与键原文**，因此缩进、`=` 两侧空白、
// 行尾注释与换行都保持原样；不存在时走 append 的新增分支。
func (c *tkContainer) setScalar(name, raw string) {
	if e := c.findEntry(name); e != nil && e.kind == tkEntryKeyValue {
		e.value = &tkValue{class: tkScalar, raw: raw}
		return
	}
	key := &tkKey{original: name, name: name, sep: " = "}
	c.appendKeyValue(&tkEntry{
		kind: tkEntryKeyValue, key: key,
		value:  &tkValue{class: tkScalar, raw: raw},
		trivia: newTrivia(),
	})
}

// itemTrivia 返回条目所承载 **item** 的 trivia。
//
// tomlkit 的 body 存的是 (key, item)，表条目的装饰信息在 Table 自己身上
// （Table.trivia），键值对/注释则在自己的 trivia 里。插入逻辑与渲染都必须按
// item 取，混用会让「给表补一个前导换行」这类调整落到一个永远不会被读到的地方。
func (e *tkEntry) itemTrivia() *tkTrivia {
	if e.kind == tkEntryTable && e.table != nil {
		return &e.table.trivia
	}
	return &e.trivia
}

// appendKeyValue 复刻 Container.append 的非表分支（container.py:299-330）。
func (c *tkContainer) appendKeyValue(entry *tkEntry) {
	if len(c.entries) > 0 && !c.parsed {
		lastIndex := c.lastIndexBeforeTable()
		if lastIndex < len(c.entries) {
			afterItem := c.entries[lastIndex]
			if afterItem.kind != tkEntryWhitespace {
				trivia := afterItem.itemTrivia()
				if !strings.Contains(trivia.indent, "\n") {
					trivia.indent = "\n" + trivia.indent
				}
			}
			c.insertAt(lastIndex, entry)
			return
		}
		previous := c.previousItem()
		if previous != nil && previous.kind != tkEntryWhitespace && !endsWithWhitespace(previous) &&
			!strings.Contains(previous.itemTrivia().trail, "\n") {
			previous.itemTrivia().trail += "\n"
		}
	}
	c.entries = append(c.entries, entry)
}

// lastIndexBeforeTable 复刻 container.py:140 的 _get_last_index_before_table。
func (c *tkContainer) lastIndexBeforeTable() int {
	last := -1
	for i, e := range c.entries {
		if e.removed {
			continue
		}
		if e.kind == tkEntryWhitespace {
			continue
		}
		if e.kind == tkEntryTable && e.key != nil && !e.key.dotted {
			break
		}
		last = i
	}
	return last + 1
}

// insertAt 复刻 container.py:458 的 _insert_at。
func (c *tkContainer) insertAt(index int, entry *tkEntry) {
	if index > 0 {
		previous := c.entries[index-1]
		if previous.kind != tkEntryWhitespace && !endsWithWhitespace(previous) &&
			entry.kind != tkEntryTable && !strings.Contains(previous.itemTrivia().trail, "\n") {
			previous.itemTrivia().trail += "\n"
		}
	}
	c.entries = append(c.entries, nil)
	copy(c.entries[index+1:], c.entries[index:])
	c.entries[index] = entry
}

// remove 复刻 Container.remove（container.py:392）：条目保留在 body 里但内容
// 变成 Null，渲染时输出空串，因此整行（含行尾注释）消失。
func (c *tkContainer) remove(name string) {
	if e := c.findEntry(name); e != nil {
		e.removed = true
	}
}

// newTable 对应 tomlkit.table()：一张新建的表，_is_super_table 为 None。
func newTable(key *tkKey) *tkTable {
	return &tkTable{key: key, body: &tkContainer{}}
}

// quoteTOMLString 复刻 tomlkit 新建字符串值的渲染
// （items.py String.as_string + _utils.escape_string，_utils.py:128）。
func quoteTOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
