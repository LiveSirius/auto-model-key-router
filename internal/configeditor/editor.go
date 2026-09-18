package configeditor

import (
	"errors"
	"sort"
	"strconv"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
	"github.com/Sparrived/auto-model-key-router/internal/configservice"
	"github.com/Sparrived/auto-model-key-router/internal/formatting"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// RestartServiceFunc 是 service.restart_service_after_config_change（service.py:150）
// 的注入接缝：返回用于结果页展示的「配置热重载」面板。
//
// 与参照实现的差异：Python 的签名多一个 path 参数，但那个参数在函数体里从未被使用，
// 而 internal/service 的移植同样去掉了它。这里对齐成两参数，装配层可以直接赋值：
//
//	editor.RestartServiceAfterConfigChange = service.RestartServiceAfterConfigChange
//
// **nil 表示未接线**：调用点返回错误（见 restartAfterConfigChange），而不是伪造一个
// 「已热重载」的成功面板。
type RestartServiceFunc func(oldConfig, newConfig *config.RouterConfig) tui.Renderable

// UI 是交互原语的注入接缝。
//
// 每个字段为 nil 时回落到 internal/tui 的真实实现；测试注入脚本化实现来驱动整条
// 菜单/表单流程——与 Python 测试里的 `monkeypatch.setattr(config_editor,
// "select_option", ...)` 等价。
type UI struct {
	// SelectOption 对应 config_editor 里的 tui.select_option。
	SelectOption func(title string, options []tui.Option, opts tui.SelectOptions) string
	// SelectMultiple 对应 tui.select_multiple。
	SelectMultiple func(title string, options []tui.Option, content any, checked []string) []string
	// PromptText 对应 tui.prompt_text；取消时返回 tui.ErrCancelled。
	PromptText func(title, prompt string, opts tui.PromptOptions) (string, error)
	// ConfirmChoice 对应 tui.confirm_choice。
	ConfirmChoice func(message string, defaultValue bool) bool
	// ShowResultPage 对应 tui.show_result_page。
	ShowResultPage func(title string, content any)
	// ClearTerminalHistory 对应 tui.clear_terminal_history。
	ClearTerminalHistory func()
	// OpenConfigFile 对应 tui.open_config_file。
	OpenConfigFile func(configPath string) string
	// RunSubmodule 对应 tui.run_submodule：执行子流程并把「取消/出错」收敛成面板。
	RunSubmodule func(action func() (any, error)) any
	// Status 对应 `console.status(...)` 上下文：显示提示后执行 action。
	Status func(message string, action func())
}

// Editor 是交互式配置编辑器的宿主状态。
//
// 对应 config_editor.py 的模块级状态与注入依赖：
//   - Path 是配置文件路径（Python 里是每个函数的第一个参数）；
//   - UI / Prober / RestartServiceAfterConfigChange 是接缝；
//   - drafts 对应模块级字典 FORM_DRAFTS。参照实现把它做成进程级全局，Go 侧放在
//     Editor 上：一个 TUI 会话一个 Editor，行为一致，但测试之间不再互相污染。
type Editor struct {
	// Path 是配置文件路径。
	Path string
	// UI 是交互原语接缝（零值即真实实现）。
	UI UI
	// Prober 是探测逻辑（HTTP 客户端与时钟接缝）。
	Prober Prober
	// RestartServiceAfterConfigChange 见 RestartServiceFunc。
	RestartServiceAfterConfigChange RestartServiceFunc
	// GenerateLocalAPIKey 对应 config.generate_local_api_key。nil 表示用 config 包的
	// 实现；注入它是为了让「重新生成本地鉴权密钥」这条流程能确定性对拍（Python 侧
	// 把它 monkeypatch 成固定值）。
	GenerateLocalAPIKey func() (string, error)

	drafts map[string]map[string]string
}

// New 返回只填了配置文件路径的编辑器；接缝字段按需赋值。
func New(path string) *Editor { return &Editor{Path: path} }

// —— 交互原语：nil 字段回落到 internal/tui ——

func (e *Editor) selectOption(title string, options []tui.Option, opts tui.SelectOptions) string {
	if e.UI.SelectOption != nil {
		return e.UI.SelectOption(title, options, opts)
	}
	return tui.SelectOption(title, options, opts)
}

func (e *Editor) selectMultiple(title string, options []tui.Option, content any, checked []string) []string {
	if e.UI.SelectMultiple != nil {
		return e.UI.SelectMultiple(title, options, content, checked)
	}
	return tui.SelectMultiple(title, options, content, checked)
}

func (e *Editor) promptText(title, prompt string, opts tui.PromptOptions) (string, error) {
	if e.UI.PromptText != nil {
		return e.UI.PromptText(title, prompt, opts)
	}
	return tui.PromptText(title, prompt, opts)
}

func (e *Editor) confirmChoice(message string, defaultValue bool) bool {
	if e.UI.ConfirmChoice != nil {
		return e.UI.ConfirmChoice(message, defaultValue)
	}
	return tui.ConfirmChoice(message, defaultValue)
}

func (e *Editor) showResultPage(title string, content any) {
	if e.UI.ShowResultPage != nil {
		e.UI.ShowResultPage(title, content)
		return
	}
	tui.ShowResultPage(title, content)
}

func (e *Editor) clearTerminalHistory() {
	if e.UI.ClearTerminalHistory != nil {
		e.UI.ClearTerminalHistory()
		return
	}
	tui.ClearTerminalHistory()
}

func (e *Editor) openConfigFile() string {
	if e.UI.OpenConfigFile != nil {
		return e.UI.OpenConfigFile(e.Path)
	}
	return tui.OpenConfigFile(e.Path)
}

func (e *Editor) runSubmodule(action func() (any, error)) any {
	if e.UI.RunSubmodule != nil {
		return e.UI.RunSubmodule(action)
	}
	return tui.RunSubmodule(action)
}

func (e *Editor) withStatus(message string, action func()) {
	if e.UI.Status != nil {
		e.UI.Status(message, action)
		return
	}
	// 参照实现用 `console.status(...)` 显示一个转圈提示；internal/tui 的 Terminal
	// 没有该 API，且迁移方案不要求复刻动画，因此这里直接执行——提示文案丢失，
	// 进度语义（同步执行完再继续）一致。
	action()
}

// —— 配置读写 ——

// loadConfigData 对应 config_editor.py:212 的 load_v2_config_data。
//
// 参照实现里 load_v2_config_data 只是 load_config_data 的别名，Go 侧因此只有一个
// 方法；文件不存在时 config.LoadConfigData 会创建一份空配置并落盘（对齐
// config.py:279-284）。
func (e *Editor) loadConfigData() (*canonical.Value, error) {
	return config.LoadConfigData(e.Path)
}

// loadRouterConfig 对应 `RouterConfig.from_dict(data)`。
func loadRouterConfig(data *canonical.Value) (*config.RouterConfig, error) {
	return config.FromDict(data)
}

// generateLocalAPIKey 走注入接缝，nil 时用 config 包的实现。
func (e *Editor) generateLocalAPIKey() (string, error) {
	if e.GenerateLocalAPIKey != nil {
		return e.GenerateLocalAPIKey()
	}
	return config.GenerateLocalAPIKey()
}

// restartAfterConfigChange 调用热重载提示接缝。
//
// 接缝未接线时返回错误而不是 nil：调用方（以及最终的用户）必须看到「未接入」，
// 而不是一个看起来成功的静默结果——这与 internal/api 对未接线接缝的处理一致。
func (e *Editor) restartAfterConfigChange(oldConfig, newConfig *config.RouterConfig) (tui.Renderable, error) {
	if e.RestartServiceAfterConfigChange == nil {
		return nil, errNotWired("重启服务")
	}
	return e.RestartServiceAfterConfigChange(oldConfig, newConfig), nil
}

// commitV2Config 对应 config_editor.py:370 的 commit_v2_config：落盘 + 热重载提示。
func (e *Editor) commitV2Config(data *canonical.Value, oldConfig *config.RouterConfig) (any, error) {
	change, err := configservice.CommitConfigData(e.Path, data, oldConfig)
	if err != nil {
		return nil, err
	}
	return e.restartAfterConfigChange(oldConfig, change.NewConfig)
}

// configOperationsError 判断错误是否是 configops 的契约错误（Python 的
// ConfigOperationError）。用于「捕获后换成面板」的分支。
func isConfigOperationError(err error) bool {
	var opErr *configops.ConfigOperationError
	return errors.As(err, &opErr)
}

// —— 配置结构访问器（= config_editor.py:216-226 的三个别名） ——

func rawProviders(data *canonical.Value) (*canonical.Value, error) {
	return configops.Providers(data)
}

func rawModels(data *canonical.Value) (*canonical.Value, error) {
	return configops.Models(data)
}

func providerKeys(provider *canonical.Value) (*canonical.Value, error) {
	return configops.ProviderKeys(provider)
}

func modelTargets(model *canonical.Value) (*canonical.Value, error) {
	return configops.ModelTargets(model)
}

// —— 展示用文本（formatting 包的薄包装，保持调用点与参照实现同名） ——

func shortText(value string, limit int) string { return formatting.ShortText(value, limit) }

func compactURL(value string, limit int) string { return formatting.CompactURL(value, limit) }

func keyFingerprint(apiKey string) string { return formatting.KeyFingerprint(apiKey) }

// sortedKeysOf 返回对象键的排序副本（等价于 Python 的 sorted(dict)）。
func sortedKeysOf(value *canonical.Value) []string {
	if !value.IsObject() {
		return nil
	}
	keys := value.Obj.Keys()
	sort.Strings(keys)
	return keys
}

// optionIndex 把菜单返回值折算成 0 基下标。
//
// 参照实现直接写 `int(choice) - 1`；select_option 只会返回合法选项值，所以这里
// 解析失败时报 false 而不是 panic（Go 没有 ValueError 可以冒泡）。
func optionIndex(choice string) (int, bool) {
	index, err := strconv.Atoi(choice)
	if err != nil || index < 1 {
		return 0, false
	}
	return index - 1, true
}

// formDraft 对应 config_editor.py:66 的 form_draft：按名字取（并惰性创建）草稿。
func (e *Editor) formDraft(name string) map[string]string {
	if e.drafts == nil {
		e.drafts = map[string]map[string]string{}
	}
	draft, ok := e.drafts[name]
	if !ok {
		draft = map[string]string{}
		e.drafts[name] = draft
	}
	return draft
}

// dropFormDraft 对应 `FORM_DRAFTS.pop(name, None)`。
func (e *Editor) dropFormDraft(name string) {
	delete(e.drafts, name)
}

// visitorAvailable 对应 visitor.visitor_feature_available()。
//
// Go 侧访客功能常驻（见 doc.go 与 internal/auth），因此恒为真。
func visitorAvailable() bool { return true }

// stringOrEmpty 返回对象的字符串成员，等价于 `str(value.get(key) or "")`。
func stringOrEmpty(value *canonical.Value, key string) string {
	return value.Lookup(key).StringValue()
}
