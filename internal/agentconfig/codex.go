package agentconfig

import (
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// configureCodex 改写 ~/.codex/config.toml（agent_config.py:310）。
//
// 这是整个模块唯一需要**格式保真**的地方：config.toml 由用户手工维护，里面
// 通常有注释与自定义表，而 tomlkit 会把这些原样保留。Go 侧用 tomlkit.go 里的
// 迷你 tomlkit 复刻同一套 body/trivia 模型，只动 AMKR 拥有的 7 个键：
//
//	model_provider / model / review_model / model_reasoning_effort
//	model_providers.OpenAI.{name,base_url,wire_api,requires_openai_auth}
//
// 其余字节（注释、空行、键序、`=` 两侧空白、行尾注释、CRLF）逐字节不变。
//
// 注意 provider 名是大小写敏感的 "OpenAI"：用户自己的
// `[model_providers.openai]` 不会被覆盖，两者在 Codex 里是两个 provider。
func configureCodex(current []byte, cfg *config.RouterConfig, mode string, removeUnifiedModelOverrides bool) ([]byte, error) {
	// 空文档对应 tomlkit.document()。**实测**它的根容器 _parsed 是 false，
	// 因此新增键会走「插入到最后一个非表项之后」，新建表也会拿到一个前导换行
	// （这正是 `model = "v"\n\n[model_providers.OpenAI]` 里空行的来源）。
	root := &tkContainer{}
	if len(current) > 0 {
		text, err := decodeUTF8Sig("Codex", current)
		if err != nil {
			return nil, err
		}
		// 先用 go-toml/v2 的 unstable parser 走一遍做校验：迷你 tomlkit 为了
		// 保真而刻意宽松（值只做原文扫描），不校验就可能把 Python 会拒绝的
		// 坏文件「修好」再回写。
		if err := validateTOML(text); err != nil {
			return nil, errf("Codex 配置不是有效的 UTF-8 TOML: %v", err)
		}
		parsed, parseErr := parseTOMLDocument(text)
		if parseErr != nil {
			return nil, errf("Codex 配置不是有效的 UTF-8 TOML: %v", parseErr)
		}
		root = parsed
	}

	document := &codexTable{container: root}
	document.setScalar("model_provider", quoteTOMLString(codexProviderID))
	if mode == ModeUnifiedModel {
		document.setScalar("model", quoteTOMLString(config.UNIFIED_MODEL_ID))
		document.setScalar("review_model", quoteTOMLString(config.UNIFIED_MODEL_ID))
		document.setScalar("model_reasoning_effort", quoteTOMLString(codexReasoningEffort(cfg)))
	} else if removeUnifiedModelOverrides {
		document.remove("model")
		document.remove("review_model")
		document.remove("model_reasoning_effort")
	}

	providers, err := document.subTable("model_providers", "model_providers")
	if err != nil {
		return nil, err
	}
	provider, err := providers.subTable(codexProviderID, "model_providers."+codexProviderID)
	if err != nil {
		return nil, err
	}
	provider.setScalar("name", quoteTOMLString(codexProviderID))
	provider.setScalar("base_url", quoteTOMLString(RouterOrigin(cfg)+"/v1"))
	provider.setScalar("wire_api", quoteTOMLString("responses"))
	provider.setScalar("requires_openai_auth", "true")
	return []byte(root.Render()), nil
}

// codexReasoningEffort 取出 unified-model 主模型配置的 reasoning_effort，
// 缺省 xhigh（agent_config.py:328-330）。
//
// 参照实现是 `config.reasoning_effort_by_model.get(model) or "xhigh"`：空串与
// 缺失一样回落 xhigh。
func codexReasoningEffort(cfg *config.RouterConfig) string {
	modelName := cfg.UnifiedModel.Default.Primary.Model
	if effort := cfg.ReasoningEffortByModel[modelName]; effort != "" {
		return effort
	}
	return "xhigh"
}

// validateTOML 用 pelletier/go-toml/v2 的 unstable parser 校验文档。
//
// 只做遍历与取错误，不消费 AST：保真回写靠迷你 tomlkit，而 go-toml/v2 的
// Unmarshal→Marshal 往返会丢掉全部注释（见 doc.go 的实测数据），不能用于写入。
func validateTOML(text string) error {
	parser := &unstable.Parser{}
	parser.Reset([]byte(text))
	for parser.NextExpression() {
		// 遍历即校验：任何语法错误都会让 NextExpression 返回 false 并使
		// parser.Error() 非空。
	}
	return parser.Error()
}

// codexTable 是配置里的一张 TOML 表（顶层或嵌套）。
type codexTable struct {
	container *tkContainer
}

// setScalar 设置一个字符串 / 布尔字面量；raw 已经是渲染好的 TOML 值文本。
func (t *codexTable) setScalar(name, raw string) {
	t.container.setScalar(name, raw)
}

// remove 删除一个键（整行消失，含行尾注释），对应 tomlkit 的 dict.pop。
func (t *codexTable) remove(name string) {
	t.container.remove(name)
}

// subTable 取出或创建一张子表，对应参照实现的
// `document.get(key)` 与 `providers[CODEX_PROVIDER_ID] = tomlkit.table()`。
// label 是报错时用的完整点号路径（参照实现的错误文本就是这个形状）。
func (t *codexTable) subTable(name, label string) (*codexTable, error) {
	entry := t.container.findEntry(name)
	if entry == nil {
		// 新建表的 key.original 用裸键名；表头的 sep 是空串（不渲染 sep）。
		table := newTable(&tkKey{original: name, name: name, sep: ""})
		appendTable(t.container, table.key, table)
		return &codexTable{container: table.body}, nil
	}
	switch entry.kind {
	case tkEntryTable:
		if entry.table.aot {
			return nil, errf("Codex 配置中的 %s 是数组表（[[...]]），本实现不支持", label)
		}
		return &codexTable{container: entry.table.body}, nil
	case tkEntryKeyValue:
		switch entry.value.class {
		case tkScalar:
			// 与参照实现逐字一致（agent_config.py:339 / :346）。
			return nil, errf("Codex 配置中的 %s 必须是 TOML 表", label)
		default:
			// 数组 / 行内表在 Python 侧会通过 isinstance(MutableMapping) 检查，
			// 然后在写键时抛 TypeError 或产出「部分行内、部分多行」的古怪结果。
			// Go 侧选择显式报错而不是默默改写（见 doc.go 的差异清单）。
			return nil, errf("Codex 配置中的 %s 是数组或行内表，本实现不支持", label)
		}
	}
	return nil, errf("Codex 配置中的 %s 必须是 TOML 表", label)
}
