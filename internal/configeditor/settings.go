package configeditor

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
	"github.com/Sparrived/auto-model-key-router/internal/configservice"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// ManageConfigTransferInteractively 对应 config_editor.py:1435 的
// manage_config_transfer_interactively：配置迁移菜单。
//
// 「o」键随时可以打开配置文件（参照实现的 on_key 钩子，tui.select_option 在**所有**
// 按键分支之前调用它）。
func (e *Editor) ManageConfigTransferInteractively() error {
	for {
		choice := e.selectOption("配置迁移", []tui.Option{
			{Value: "1", Label: "复制 Key 配置"},
			{Value: "2", Label: "粘贴并应用"},
			{Value: "0", Label: "返回"},
		}, tui.SelectOptions{OnKey: func(key string) (string, bool) {
			// 对应 config_editor.py:206 的 _open_config_on_key：只有 o/O 有动作，
			// 且永不结束菜单。
			if key == "o" || key == "O" {
				e.openConfigFile()
			}
			return "", false
		}})
		if choice == "0" {
			return nil
		}
		e.clearTerminalHistory()
		var result any
		var err error
		if choice == "1" {
			result, err = e.ExportConfigInteractively()
		} else {
			result, err = e.PasteConfigInteractively()
		}
		if err != nil {
			return err
		}
		if result != nil {
			e.showResultPage("配置迁移", result)
		}
	}
}

// TransferableKeyConfig 对应 config_editor.py:1455 的 transferable_key_config。
func TransferableKeyConfig(data *canonical.Value) (*canonical.Value, error) {
	return configops.TransferableConfig(data)
}

// MergeTransferableKeyConfig 对应 config_editor.py:1463 的
// merge_transferable_key_config。
func MergeTransferableKeyConfig(currentData, transferData *canonical.Value) (configops.MergeResult, error) {
	return configops.MergeTransferableConfig(currentData, transferData)
}

// ExportConfigInteractively 对应 config_editor.py:1469 的
// export_config_interactively：生成可复制的单行 Key 配置。
func (e *Editor) ExportConfigInteractively() (tui.ResultPage, error) {
	data, err := e.loadConfigData()
	if err != nil {
		return tui.ResultPage{}, err
	}
	transferData, err := TransferableKeyConfig(data)
	if err != nil {
		return tui.ResultPage{}, err
	}
	// 参照实现用 `json.dumps(..., ensure_ascii=False, separators=(",", ":"))`：
	// 紧凑、不排序、不转义非 ASCII——正是 canonical.DumpsOrdered 的语义。
	configText := canonical.DumpsOrdered(transferData)
	modelCount := transferData.Lookup("models").Len()
	keyCount := 0
	if providers := transferData.Lookup("providers"); providers.IsObject() {
		for _, providerID := range providers.Obj.Keys() {
			keys, err := providerKeys(providers.Lookup(providerID))
			if err != nil {
				return tui.ResultPage{}, err
			}
			keyCount += keys.Obj.Len()
		}
	}
	content := tui.SectionPanel(
		"配置文件: [bold]"+absolutePath(e.Path)+"[/bold]\n模型数量: [bold]"+strconv.Itoa(modelCount)+"[/bold]\n"+
			"Key 数量: [bold]"+strconv.Itoa(keyCount)+"[/bold]\n\n"+
			"[bold yellow]复制内容仅包含模型与上游 API key，请仅粘贴到可信终端。[/bold yellow]\n\n"+
			"复制内容为单行 JSON。本地鉴权、监听地址、端口及其他 CLI 设置不会复制。\n\n"+
			"在另一台机器或另一个 TUI 中进入“CLI 设置 → 配置迁移 → 粘贴并应用”即可导入。",
		"复制 Key 配置", "green",
	)
	return tui.CopyResultPage(content, configText, "复制 Key 配置"), nil
}

// PasteConfigInteractively 对应 config_editor.py:1489 的 paste_config_interactively。
func (e *Editor) PasteConfigInteractively() (any, error) {
	text, err := e.promptText("粘贴并应用", "请粘贴单行 Key 配置 JSON，然后按 Enter", tui.PromptOptions{})
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return tui.SectionPanel("未输入配置内容。", "应用取消", "yellow"), nil
	}
	pasted, parseErr := canonical.ParseString(trimmed)
	if parseErr != nil {
		return tui.SectionPanel("粘贴内容不是有效 JSON: "+parseErr.Error(), "应用失败", "red"), nil
	}
	if !pasted.IsObject() {
		return tui.SectionPanel("粘贴内容必须是配置对象。", "应用失败", "red"), nil
	}
	transferData, currentData, oldConfig, mergedData, mergeResult, err := e.mergePasted(pasted)
	if err != nil {
		if isValidationError(err) {
			return tui.SectionPanel("配置校验失败: "+err.Error(), "应用失败", "red"), nil
		}
		return nil, err
	}
	_ = transferData
	if !e.confirmChoice(
		"将追加粘贴的 Key 配置，并保留现有模型和本机 CLI 设置："+absolutePath(e.Path)+"，是否继续？",
		false,
	) {
		return tui.SectionPanel("配置未变化。", "应用取消", "yellow"), nil
	}
	change, err := configservice.CommitConfigData(e.Path, mergedData, oldConfig)
	if err != nil {
		if isValidationError(err) {
			return tui.SectionPanel("配置校验失败: "+err.Error(), "应用失败", "red"), nil
		}
		return nil, err
	}
	_ = currentData
	newConfig := change.NewConfig
	modelCount := len(newConfig.Models)
	keyCount := 0
	for _, model := range newConfig.Models {
		keyCount += len(model.Keys)
	}
	content := tui.SectionPanel(
		"已追加粘贴的 Key 配置，并保留现有模型和本机 CLI 设置。\n配置文件: [bold]"+absolutePath(e.Path)+"[/bold]\n"+
			"新增模型: [bold]"+strconv.Itoa(mergeResult.AddedModels)+"[/bold]\n新增 Key: [bold]"+strconv.Itoa(mergeResult.AddedKeys)+"[/bold]\n"+
			"跳过重复 Key: [bold]"+strconv.Itoa(mergeResult.SkippedKeys)+"[/bold]\n"+
			"当前模型数量: [bold]"+strconv.Itoa(modelCount)+"[/bold]\n当前 Key 数量: [bold]"+strconv.Itoa(keyCount)+"[/bold]",
		"应用完成", "green",
	)
	restart, err := e.restartAfterConfigChange(oldConfig, newConfig)
	if err != nil {
		return nil, err
	}
	return tui.Group{Items: []tui.Renderable{content, asRenderable(restart)}}, nil
}

// mergePasted 是 paste_config_interactively 里那一串「校验并合并」的步骤。
//
// 参照实现把它们放在一个 try 里捕获 KeyError/TypeError/ValueError；Go 侧由调用方
// 判断 isValidationError 后显示同样的「配置校验失败」面板。
func (e *Editor) mergePasted(pasted *canonical.Value) (
	transferData *canonical.Value,
	currentData *canonical.Value,
	oldConfig *config.RouterConfig,
	mergedData *canonical.Value,
	mergeResult configops.MergeResult,
	err error,
) {
	transferData, err = TransferableKeyConfig(pasted)
	if err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	if _, err = loadRouterConfig(transferData); err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	currentData, err = e.loadConfigData()
	if err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	oldConfig, err = loadRouterConfig(currentData)
	if err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	mergeResult, err = MergeTransferableKeyConfig(currentData, transferData)
	if err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	mergedData = mergeResult.Config
	if _, err = loadRouterConfig(mergedData); err != nil {
		return nil, nil, nil, nil, mergeResult, err
	}
	return transferData, currentData, oldConfig, mergedData, mergeResult, nil
}

// SetLocalAPIKeyInteractively 对应 config_editor.py:1727 的
// set_local_api_key_interactively。
func (e *Editor) SetLocalAPIKeyInteractively() (any, error) {
	data, err := e.loadConfigData()
	if err != nil {
		return nil, err
	}
	oldConfig, err := loadRouterConfig(data)
	if err != nil {
		return nil, err
	}
	if data.Lookup("local_api_key").Truthy() && !e.confirmChoice("是否重置本地鉴权密钥？", true) {
		return tui.SectionPanel("[yellow]配置未变化。[/yellow]", "本地鉴权", "yellow"), nil
	}
	localAPIKey, err := e.generateLocalAPIKey()
	if err != nil {
		return nil, err
	}
	if err := configops.RegenerateLocalAPIKey(data, localAPIKey); err != nil {
		return nil, err
	}
	restart, err := e.commitV2Config(data, oldConfig)
	if err != nil {
		return nil, err
	}
	content := tui.Group{Items: []tui.Renderable{
		tui.SectionPanel(
			"已生成新密钥。\n\n[bold]"+localAPIKey+"[/bold]\n\n请求时添加：\nAuthorization: Bearer <key>",
			"本地鉴权", "green",
		),
		asRenderable(restart),
	}}
	return tui.CopyResultPage(content, localAPIKey, "复制本地鉴权 key"), nil
}

// SetWebUIInteractively 对应 config_editor.py:1748 的 set_webui_interactively。
//
// WebUI 资产随安装包发布，这里只切换 webui_enabled；挂载发生在服务启动时，因此变更
// 需要重启服务才会真正释放/占用 /ui。
func (e *Editor) SetWebUIInteractively() (any, error) {
	data, err := e.loadConfigData()
	if err != nil {
		return nil, err
	}
	oldConfig, err := loadRouterConfig(data)
	if err != nil {
		return nil, err
	}
	current := boolOr(data, "webui_enabled", false)
	action := "启用"
	if current {
		action = "关闭"
	}
	state := "已关闭"
	if current {
		state = "已启用"
	}
	if !e.confirmChoice("WebUI 当前"+state+"，是否"+action+"？", !current) {
		return tui.SectionPanel("[yellow]配置未变化。[/yellow]", "WebUI", "yellow"), nil
	}
	updates := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "webui_enabled", Value: canonical.NewBool(!current)},
	)
	if err := configops.UpdateSettings(data, updates); err != nil {
		return nil, err
	}
	change, err := configservice.CommitConfigData(e.Path, data, oldConfig)
	if err != nil {
		return nil, err
	}
	restart, err := e.restartAfterConfigChange(oldConfig, change.NewConfig)
	if err != nil {
		return nil, err
	}
	newState := "关闭"
	if change.NewConfig.WebUIEnabled {
		newState = "启用"
	}
	return tui.Group{Items: []tui.Renderable{
		tui.SectionPanel(
			"已"+newState+" WebUI。\n\n"+
				"访问地址: [bold]http://"+change.NewConfig.Host+":"+strconv.Itoa(change.NewConfig.Port)+"/ui/[/bold]\n"+
				"资产随软件包一起安装，无需额外安装步骤。",
			"WebUI", "green",
		),
		asRenderable(restart),
	}}, nil
}

// SetTimeoutsInteractively 对应 config_editor.py:1778 的 set_timeouts_interactively。
func (e *Editor) SetTimeoutsInteractively() (any, error) {
	data, err := e.loadConfigData()
	if err != nil {
		return nil, err
	}
	oldConfig, err := loadRouterConfig(data)
	if err != nil {
		return nil, err
	}
	fields := []struct {
		field   string
		label   string
		current float64
	}{
		{"request_timeout", "普通请求超时（秒）", oldConfig.RequestTimeout},
		{"stream_first_byte_timeout", "流式首字节超时（秒）", oldConfig.StreamFirstByteTimeout},
		{"stream_idle_timeout", "流式空闲超时（秒）", oldConfig.StreamIdleTimeout},
	}
	values := make([]float64, 0, len(fields))
	for _, field := range fields {
		defaultValue := pythonFloatStr(field.current)
		text, err := e.promptText("超时配置", field.label, tui.PromptOptions{Default: &defaultValue})
		if err != nil {
			return nil, err
		}
		value, parseErr := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if parseErr != nil {
			return tui.SectionPanel("[red]超时必须是数字。[/red]", "超时配置", "red"), nil
		}
		if value <= 0 {
			return tui.SectionPanel("[red]超时必须大于 0。[/red]", "超时配置", "red"), nil
		}
		values = append(values, value)
	}
	updates := canonical.NewObject()
	for index, field := range fields {
		updates.SetKey(field.field, canonical.NewFloat(values[index]))
	}
	if err := configops.UpdateSettings(data, updates); err != nil {
		return nil, err
	}
	restart, err := e.commitV2Config(data, oldConfig)
	if err != nil {
		return nil, err
	}
	return tui.Group{Items: []tui.Renderable{
		tui.SectionPanel(
			"已更新超时配置。\n"+
				"普通请求: [bold]"+formatPythonG(values[0])+" 秒[/bold]\n"+
				"流式首字节: [bold]"+formatPythonG(values[1])+" 秒[/bold]\n"+
				"流式空闲: [bold]"+formatPythonG(values[2])+" 秒[/bold]",
			"超时配置", "green",
		),
		asRenderable(restart),
	}}, nil
}

// SetListenInteractively 对应 config_editor.py:1825 的 set_listen_interactively。
func (e *Editor) SetListenInteractively() (any, error) {
	data, err := e.loadConfigData()
	if err != nil {
		return nil, err
	}
	oldConfig, err := loadRouterConfig(data)
	if err != nil {
		return nil, err
	}
	currentHost := stringOrEmpty(data, "host")
	if currentHost == "" {
		currentHost = "127.0.0.1"
	}
	currentPort := int64(8000)
	if rawPort := data.Lookup("port"); rawPort.Truthy() {
		if parsed, ok := rawPort.AsInt(); ok {
			currentPort = parsed
		}
	}
	hostDefault := currentHost
	hostText, err := e.promptText("监听配置", "监听 IP/地址", tui.PromptOptions{Default: &hostDefault})
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(hostText)
	if host == "" {
		return tui.SectionPanel("[red]监听 IP/地址不能为空。[/red]", "监听配置", "red"), nil
	}
	if strings.Contains(host, "://") || strings.Contains(host, "/") {
		return tui.SectionPanel(
			"[red]监听地址只填写 IP 或主机名，不要包含协议或路径。[/red]",
			"监听配置", "red",
		), nil
	}
	portDefault := strconv.FormatInt(currentPort, 10)
	portText, err := e.promptText("监听配置", "监听端口", tui.PromptOptions{Default: &portDefault})
	if err != nil {
		return nil, err
	}
	port, parseErr := strconv.Atoi(strings.TrimSpace(portText))
	if parseErr != nil {
		return tui.SectionPanel("[red]端口必须是数字。[/red]", "监听配置", "red"), nil
	}
	if port < 1 || port > 65535 {
		return tui.SectionPanel("[red]端口范围必须是 1-65535。[/red]", "监听配置", "red"), nil
	}
	if host == currentHost && int64(port) == currentPort {
		return tui.SectionPanel(
			"监听配置未变化: [bold]"+host+":"+strconv.Itoa(port)+"[/bold]",
			"监听配置", "yellow",
		), nil
	}
	if host == "0.0.0.0" && host != currentHost &&
		!e.confirmChoice("0.0.0.0 会允许局域网/公网访问，确认继续？", false) {
		return tui.SectionPanel("[yellow]配置未变化。[/yellow]", "监听配置", "yellow"), nil
	}
	updates := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "host", Value: canonical.NewString(host)},
		canonical.ObjectPair{Key: "port", Value: canonical.NewIntValue(int64(port))},
	)
	if err := configops.UpdateSettings(data, updates); err != nil {
		return nil, err
	}
	restart, err := e.commitV2Config(data, oldConfig)
	if err != nil {
		return nil, err
	}
	warning := ""
	if host == "0.0.0.0" {
		warning = "\n[bold red]风险提示: 0.0.0.0 会暴露到所有可达网络，请确保防火墙和本地鉴权已正确配置。[/bold red]"
	}
	return tui.Group{Items: []tui.Renderable{
		tui.SectionPanel(
			"已更新监听配置。\n配置文件: [bold]"+e.Path+"[/bold]\n旧配置: [bold]"+
				currentHost+":"+strconv.FormatInt(currentPort, 10)+"[/bold]\n新配置: [bold]"+
				host+":"+strconv.Itoa(port)+"[/bold]"+warning,
			"监听配置", "green",
		),
		asRenderable(restart),
	}}, nil
}

// absolutePath 对应 Python 的 `path.resolve()`。
//
// 差异：Python 的 resolve() 还会解析符号链接与 `..`，Go 的 filepath.Abs 只做
// 「相对转绝对 + Clean」。展示用文本，且解析符号链接在 Windows 上语义不一致
// （需要文件存在），因此只做到 Abs。
func absolutePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

// formatPythonG 复刻 Python 的 `format(value, "g")`：6 位有效数字。
func formatPythonG(value float64) string {
	return strconv.FormatFloat(value, 'g', 6, 64)
}

// pythonFloatStr 复刻 Python 的 `str(float)`（= repr）。
//
// 不能直接用 Go 的 %v 或 'g'：Go 的 'g' 在 1234567.0 这类值上会切到指数形式
// （"1.234567e+06"），而 Python 的 repr 只在 decpt <= -4 或 decpt > 16 时用指数
// 形式（"1234567.0"、"1000000000000000.0"、"1e+16"）。超时配置虽然取不到极端值，
// 但这里照 repr 的规则实现，省得留下「看起来一样其实不同」的差异。
func pythonFloatStr(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	}
	// 最短往返的指数形式：由它的指数推出 Python 的 decpt（小数点位置）。
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	exponent, err := strconv.Atoi(scientific[strings.IndexByte(scientific, 'e')+1:])
	if err != nil {
		return scientific
	}
	if decpt := exponent + 1; decpt > -4 && decpt <= 16 {
		fixed := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(fixed, ".") {
			fixed += ".0"
		}
		return fixed
	}
	return scientific
}

// isValidationError 判断错误是否属于参照实现里被 `except (KeyError, TypeError,
// ValueError)` 捕获的那一类（配置内容不合法），用于决定「显示红色失败面板」还是
// 「向上抛出」。
func isValidationError(err error) bool {
	var configErr *config.ConfigError
	var internalErr *config.InternalError
	var opErr *configops.ConfigOperationError
	var pyErr *configops.PyError
	var valueErr *canonical.ValueError
	var typeErr *canonical.TypeError
	return errors.As(err, &configErr) || errors.As(err, &internalErr) ||
		errors.As(err, &opErr) || errors.As(err, &pyErr) ||
		errors.As(err, &valueErr) || errors.As(err, &typeErr)
}
