package agentconfig

import (
	"bytes"
	"unicode/utf8"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// configureClaudeCode 改写 ~/.claude/settings.json（agent_config.py:260）。
//
// 与 Codex / Pi 不同，这里**可以**用整份 JSON 重写：JSON 不允许注释，参照实现
// 本身就是 `json.loads` → 改 dict → `json.dumps(indent=2, ensure_ascii=False)`，
// 因此不存在需要保真的格式。canonical 的 DumpsIndent 正是这个形式
// （保留键插入顺序、不排序、ensure_ascii=False），所以直接复用。
//
// removeUnifiedModelOverrides 只在 native 模式下生效：从 unified-model 回退到
// native 时，把 AMKR 之前塞进去的四个 ANTHROPIC_*_MODEL 键删掉，否则用户切回
// 原生模式后模型名仍会被钉在 unified-model 上。
func configureClaudeCode(current []byte, cfg *config.RouterConfig, mode string, removeUnifiedModelOverrides bool) ([]byte, error) {
	var data *canonical.Value
	if len(current) > 0 {
		text, err := decodeUTF8Sig("Claude Code", current)
		if err != nil {
			return nil, err
		}
		parsed, parseErr := canonical.ParseString(text)
		if parseErr != nil {
			return nil, errf("Claude Code 配置不是有效的 UTF-8 JSON: %v", parseErr)
		}
		if !parsed.IsObject() {
			return nil, errf("Claude Code 配置根节点必须是 JSON 对象")
		}
		data = parsed
	} else {
		data = canonical.NewObject()
	}

	env := data.Lookup("env")
	if env == nil || env.IsNull() {
		env = canonical.NewObject()
		data.SetKey("env", env)
	}
	if !env.IsObject() {
		return nil, errf("Claude Code 配置中的 env 必须是 JSON 对象")
	}

	// env.update({...})：已有键保持位置只换值，新键追加到末尾。
	env.SetKey("ANTHROPIC_BASE_URL", canonical.NewString(RouterOrigin(cfg)))
	env.SetKey("ANTHROPIC_AUTH_TOKEN", canonical.NewString(cfg.LocalAPIKey))
	env.SetKey("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", canonical.NewString("1"))
	env.SetKey("CLAUDE_CODE_ATTRIBUTION_HEADER", canonical.NewString("false"))
	switch {
	case mode == ModeUnifiedModel:
		env.SetKey("ANTHROPIC_MODEL", canonical.NewString(config.UNIFIED_MODEL_ID))
		env.SetKey("ANTHROPIC_DEFAULT_HAIKU_MODEL", canonical.NewString(config.UNIFIED_MODEL_ID))
		env.SetKey("ANTHROPIC_DEFAULT_SONNET_MODEL", canonical.NewString(config.UNIFIED_MODEL_ID))
		env.SetKey("ANTHROPIC_DEFAULT_OPUS_MODEL", canonical.NewString(config.UNIFIED_MODEL_ID))
	case removeUnifiedModelOverrides:
		for _, name := range []string{
			"ANTHROPIC_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL",
			"ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL",
		} {
			env.DeleteKey(name)
		}
	}
	// 关闭 Claude Code 的 Git 归属署名。整体替换（而不是 update），因此用户
	// 写在 attribution 里的其它字段会被丢弃——这是参照实现的既有行为。
	data.SetKey("attribution", canonical.NewObjectOf(
		canonical.ObjectPair{Key: "commit", Value: canonical.NewString("")},
		canonical.ObjectPair{Key: "pr", Value: canonical.NewString("")},
	))
	return []byte(canonical.DumpsIndent(data, 2) + "\n"), nil
}

// configurePiAgent 改写 ~/.pi/agent/models.json（agent_config.py:355）。
//
// 与 Claude Code 同理，整份 JSON 重写。providers.amkr 是**整体覆盖**：用户写在
// 该 provider 下的自定义字段会丢，这是参照实现的既有行为（Pi agent 的 provider
// 由 AMKR 独占）。
func configurePiAgent(current []byte, cfg *config.RouterConfig) ([]byte, error) {
	var data *canonical.Value
	if len(current) > 0 {
		text, err := decodeUTF8Sig("Pi Agent", current)
		if err != nil {
			return nil, err
		}
		parsed, parseErr := canonical.ParseString(text)
		if parseErr != nil {
			return nil, errf("Pi Agent 配置不是有效的 UTF-8 JSON: %v", parseErr)
		}
		if !parsed.IsObject() {
			return nil, errf("Pi Agent 配置根节点必须是 JSON 对象")
		}
		data = parsed
	} else {
		data = canonical.NewObject()
	}

	providers := data.Lookup("providers")
	if providers == nil || providers.IsNull() {
		providers = canonical.NewObject()
		data.SetKey("providers", providers)
	}
	if !providers.IsObject() {
		return nil, errf("Pi Agent 配置中的 providers 必须是 JSON 对象")
	}

	// 模型清单 = unified-model 本身 + 所有「至少有一个启用 key」的模型 id 与别名。
	// 禁用全部 key 的模型不进清单：Pi agent 侧看不到它，用户也就不会选中一个
	// 必然失败的模型（agent_config.py:372-378）。
	modelNames := []string{config.UNIFIED_MODEL_ID}
	seen := map[string]bool{config.UNIFIED_MODEL_ID: true}
	for _, model := range cfg.Models {
		enabled := false
		for _, key := range model.Keys {
			if key.Enabled {
				enabled = true
				break
			}
		}
		if !enabled {
			continue
		}
		for _, name := range append([]string{model.ID}, model.Aliases...) {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			modelNames = append(modelNames, name)
		}
	}
	models := make([]*canonical.Value, 0, len(modelNames))
	for _, name := range modelNames {
		models = append(models, canonical.NewObjectOf(
			canonical.ObjectPair{Key: "id", Value: canonical.NewString(name)},
			canonical.ObjectPair{Key: "contextWindow", Value: canonical.NewIntValue(piContextWindow)},
		))
	}
	providers.SetKey(piProviderID, canonical.NewObjectOf(
		canonical.ObjectPair{Key: "baseUrl", Value: canonical.NewString(RouterOrigin(cfg) + "/v1")},
		canonical.ObjectPair{Key: "api", Value: canonical.NewString("openai-completions")},
		canonical.ObjectPair{Key: "apiKey", Value: canonical.NewString(cfg.LocalAPIKey)},
		canonical.ObjectPair{Key: "authHeader", Value: canonical.NewBool(true)},
		canonical.ObjectPair{Key: "models", Value: canonical.NewArray(models...)},
	))
	return []byte(canonical.DumpsIndent(data, 2) + "\n"), nil
}

// configureCodexAuth 改写 ~/.codex/auth.json（agent_config.py:398）。
//
// Codex 的 OpenAI provider 需要 requires_openai_auth=true，此时它从 auth.json
// 读 OPENAI_API_KEY；只改 config.toml 而不改这里，Codex 会拿旧 key 去打 AMKR
// 并被本地鉴权拒绝。
func configureCodexAuth(current []byte, cfg *config.RouterConfig) ([]byte, error) {
	var data *canonical.Value
	if len(current) > 0 {
		text, err := decodeUTF8Sig("Codex 鉴权", current)
		if err != nil {
			return nil, err
		}
		parsed, parseErr := canonical.ParseString(text)
		if parseErr != nil {
			return nil, errf("Codex 鉴权配置不是有效的 UTF-8 JSON: %v", parseErr)
		}
		data = parsed
	} else {
		data = canonical.NewObject()
	}
	// 空文件走 {} 分支（data 是新建对象，必定通过）；非空文件解析出的根节点
	// 必须是对象，否则对应参照实现的 "必须是 JSON 对象" 报错。
	if !data.IsObject() {
		return nil, errf("Codex 鉴权配置必须是 JSON 对象")
	}
	data.SetKey("OPENAI_API_KEY", canonical.NewString(cfg.LocalAPIKey))
	return []byte(canonical.DumpsIndent(data, 2) + "\n"), nil
}

// decodeUTF8Sig 复刻 Python 的 `content.decode("utf-8-sig")`。
//
// 两件事：剥掉开头的一个 UTF-8 BOM，以及拒绝非法 UTF-8（Python 抛
// UnicodeDecodeError，Go 的 string 可以承载非法字节，不校验就会把坏字节原样
// 写回去）。label 用于拼出与参照实现同形的错误前缀。
func decodeUTF8Sig(label string, content []byte) (string, error) {
	trimmed := bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(trimmed) {
		return "", errf("%s 配置不是有效的 UTF-8 JSON: 输入不是合法的 UTF-8", label)
	}
	return string(trimmed), nil
}
