package configeditor

import (
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// SelectProvider 对应 config_editor.py:375 的 select_provider：列出供应商并返回
// 选中的 id。第二个返回值为假表示「没有供应商」或用户选了返回。
func (e *Editor) SelectProvider(data *canonical.Value, title string) (string, bool, error) {
	providers, err := rawProviders(data)
	if err != nil {
		return "", false, err
	}
	if providers.Obj.Len() == 0 {
		return "", false, nil
	}
	providerIDs := sortedKeysOf(providers)
	options := make([]tui.Option, 0, len(providerIDs)+1)
	for index, providerID := range providerIDs {
		provider := providers.Lookup(providerID)
		keys, err := providerKeys(provider)
		if err != nil {
			return "", false, err
		}
		options = append(options, tui.Option{
			Value: strconv.Itoa(index + 1),
			Label: shortText(providerID, 22) + " · " +
				compactURL(stringOrEmpty(provider, "base_url"), 42) + " · " +
				strconv.Itoa(keys.Obj.Len()) + " Key",
		})
	}
	options = append(options, tui.Option{Value: "0", Label: "返回"})
	choice := e.selectOption(title, options, tui.SelectOptions{})
	if choice == "0" {
		return "", false, nil
	}
	index, ok := optionIndex(choice)
	if !ok || index >= len(providerIDs) {
		return "", false, nil
	}
	return providerIDs[index], true, nil
}

// SelectProviderKey 对应 config_editor.py:392 的 select_provider_key。
//
// providerID 为空串表示「先让用户选供应商」（Python 的 None）。返回的 providerID 与
// keyName 只有在第三个返回值为真时有效。
func (e *Editor) SelectProviderKey(data *canonical.Value, title, providerID string) (string, string, bool, error) {
	if providerID == "" {
		selected, found, err := e.SelectProvider(data, "选择供应商")
		if err != nil || !found {
			return "", "", false, err
		}
		providerID = selected
	}
	providers, err := rawProviders(data)
	if err != nil {
		return "", "", false, err
	}
	keys, err := providerKeys(providers.Lookup(providerID))
	if err != nil {
		return "", "", false, err
	}
	if keys.Obj.Len() == 0 {
		return "", "", false, nil
	}
	keyNames := sortedKeysOf(keys)
	options := make([]tui.Option, 0, len(keyNames)+1)
	for index, keyName := range keyNames {
		key := keys.Lookup(keyName)
		enabledText := "禁用"
		if boolOr(key, "enabled", true) {
			enabledText = "启用"
		}
		options = append(options, tui.Option{
			Value: strconv.Itoa(index + 1),
			Label: shortText(keyName, 26) + " · " + enabledText + " · " +
				keyFingerprint(stringOrEmpty(key, "api_key")),
		})
	}
	options = append(options, tui.Option{Value: "0", Label: "返回"})
	choice := e.selectOption(title, options, tui.SelectOptions{})
	if choice == "0" {
		return "", "", false, nil
	}
	index, ok := optionIndex(choice)
	if !ok || index >= len(keyNames) {
		return "", "", false, nil
	}
	return providerID, keyNames[index], true, nil
}

// SelectV2Model 对应 config_editor.py:417 的 select_v2_model。
func (e *Editor) SelectV2Model(data *canonical.Value, title string) (string, bool, error) {
	models, err := rawModels(data)
	if err != nil {
		return "", false, err
	}
	if models.Obj.Len() == 0 {
		return "", false, nil
	}
	modelIDs := sortedKeysOf(models)
	options := make([]tui.Option, 0, len(modelIDs)+1)
	for index, modelID := range modelIDs {
		model := models.Lookup(modelID)
		targets, err := modelTargets(model)
		if err != nil {
			return "", false, err
		}
		routingMode := stringOrEmpty(model, "routing_mode")
		if routingMode == "" {
			routingMode = "round_robin"
		}
		options = append(options, tui.Option{
			Value: strconv.Itoa(index + 1),
			Label: shortText(modelID, 30) + " · " + strconv.Itoa(len(targets.Arr)) +
				" Target · " + routingMode,
		})
	}
	options = append(options, tui.Option{Value: "0", Label: "返回"})
	choice := e.selectOption(title, options, tui.SelectOptions{})
	if choice == "0" {
		return "", false, nil
	}
	index, ok := optionIndex(choice)
	if !ok || index >= len(modelIDs) {
		return "", false, nil
	}
	return modelIDs[index], true, nil
}

// SelectOrEnterModelID 对应 config_editor.py:434 的 select_or_enter_model_id：
// 从已有模型里挑，或选「自定义输入」。返回空串表示用户选了返回。
func (e *Editor) SelectOrEnterModelID(title, prompt string, models *canonical.Value, defaultValue string) (string, error) {
	modelIDs := sortedKeysOf(models)
	if len(modelIDs) == 0 {
		text, err := e.promptText(title, prompt, tui.PromptOptions{Default: &defaultValue})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(text), nil
	}
	options := make([]tui.Option, 0, len(modelIDs)+2)
	selected := 0
	for index, modelID := range modelIDs {
		options = append(options, tui.Option{Value: modelID, Label: shortText(modelID, 48)})
		if modelID == defaultValue {
			selected = index
		}
	}
	options = append(options,
		tui.Option{Value: "__custom__", Label: "自定义输入"},
		tui.Option{Value: "0", Label: "返回"},
	)
	choice := e.selectOption(title, options, tui.SelectOptions{Selected: selected})
	switch choice {
	case "0":
		return "", nil
	case "__custom__":
		text, err := e.promptText(title, prompt, tui.PromptOptions{Default: &defaultValue})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(text), nil
	}
	return choice, nil
}

// SelectModelsToServe 对应 config_editor.py:584 的 select_models_to_serve：
// 多选该 Key 服务哪些模型。返回 nil 表示「用户放弃/没有输入」，返回空的非 nil 切片
// 表示「一个都不选」——参照实现用 None 与 [] 区分这两者。
// providerID/keyName 只是与参照实现的签名对齐（那边也没用到它们），故匿名。
func (e *Editor) SelectModelsToServe(_, _ string, models []string, extraTitle string) ([]string, error) {
	unique := map[string]bool{}
	for _, modelID := range models {
		if modelID != "" {
			unique[modelID] = true
		}
	}
	allIDs := sortedKeys(unique)
	if len(allIDs) == 0 {
		text, err := e.promptText(extraTitle, "上游未发现模型，请手动填写可用模型（逗号分隔）", tui.PromptOptions{})
		if err != nil {
			return nil, err
		}
		manual := strings.TrimSpace(text)
		if manual == "" {
			return nil, nil
		}
		out := make([]string, 0)
		for _, item := range strings.Split(manual, ",") {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out, nil
	}
	options := make([]tui.Option, 0, len(allIDs))
	for _, modelID := range allIDs {
		options = append(options, tui.Option{Value: modelID, Label: modelID})
	}
	content := tui.SectionPanel(
		"选择后这些模型会创建/更新为本地模型，并把该 Key 绑定到它们。\n"+
			"取消勾选表示该 Key 不服务该模型。",
		"说明", "cyan",
	)
	selected := e.selectMultiple(extraTitle, options, content, nil)
	// tui.SelectMultiple 取消时返回空切片；参照实现的 select_multiple 也返回 []，
	// 因此这里把 nil 归一成非 nil，避免与「None」混淆。
	if selected == nil {
		selected = []string{}
	}
	return selected, nil
}
