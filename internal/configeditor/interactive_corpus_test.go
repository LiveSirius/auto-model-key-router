package configeditor

// 交互流程的对拍：回放语料 interactive 段——每个场景都是「初始配置 + 脚本化回答」，
// Python 侧把 select_option/prompt_text/confirm_choice/select_multiple/
// show_result_page/restart_service_after_config_change 全部打桩（见生成脚本的
// install_ui），记录每次菜单、每次提问、每次结果页与落盘后的配置。
//
// Go 侧用同一套脚本化 UI 驱动 Editor，逐项比较。这样菜单结构、默认值、校验分支、
// 子流程分发与最终落盘内容都被真实 Python 锁定，而不是靠手抄。

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/tui"
)

// scriptedUI 是语料驱动的交互接缝实现。
type scriptedUI struct {
	t         *testing.T
	answers   []*canonical.Value
	position  int
	pressKey  string
	path      string
	transport *scriptedTransport

	menus     []*canonical.Value
	multiples []*canonical.Value
	prompts   []*canonical.Value
	confirms  []*canonical.Value
	results   []*canonical.Value
	restarts  []*canonical.Value
	opened    int
}

// next 按顺序取一条回答，并校验种类（错位时立即失败，避免把偏差记成通过）。
func (u *scriptedUI) next(kind string) *canonical.Value {
	u.t.Helper()
	if u.position >= len(u.answers) {
		u.t.Fatalf("回答已用尽，仍在请求 %s", kind)
	}
	answer := u.answers[u.position]
	u.position++
	if got := answer.Lookup("kind").StringValue(); got != kind {
		u.t.Fatalf("回答种类不匹配: 请求 %s，脚本给的是 %s", kind, got)
	}
	return answer.Lookup("value")
}

// text 把可能含临时配置路径的文本折成占位符，与生成脚本的 scrub 一致：
// 两边的临时目录必然不同，路径不是被对拍的语义。
func (u *scriptedUI) text(value string) *canonical.Value {
	for _, placeholder := range []string{u.path, absolutePath(u.path)} {
		if placeholder != "" {
			value = strings.ReplaceAll(value, placeholder, "<config-path>")
		}
	}
	return canonical.NewString(value)
}

func (u *scriptedUI) optionsValue(options []tui.Option) *canonical.Value {
	items := make([]*canonical.Value, 0, len(options))
	for _, option := range options {
		items = append(items, canonical.NewArray(
			canonical.NewString(option.Value),
			canonical.NewString(option.Label),
		))
	}
	return canonical.NewArray(items...)
}

func (u *scriptedUI) ui() UI {
	return UI{
		SelectOption: func(title string, options []tui.Option, opts tui.SelectOptions) string {
			record := canonical.NewObjectOf(
				canonical.ObjectPair{Key: "title", Value: u.text(title)},
				canonical.ObjectPair{Key: "options", Value: u.optionsValue(options)},
				canonical.ObjectPair{Key: "selected", Value: canonical.NewIntValue(int64(opts.Selected))},
				canonical.ObjectPair{Key: "on_key", Value: canonical.NewBool(opts.OnKey != nil)},
			)
			u.menus = append(u.menus, record)
			if u.pressKey != "" && opts.OnKey != nil {
				opts.OnKey(u.pressKey)
			}
			return u.next("select").StringValue()
		},
		SelectMultiple: func(title string, options []tui.Option, content any, checked []string) []string {
			u.multiples = append(u.multiples, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "title", Value: u.text(title)},
				canonical.ObjectPair{Key: "options", Value: u.optionsValue(options)},
			))
			answer := u.next("multiple")
			out := make([]string, 0)
			for _, item := range answer.Items() {
				out = append(out, item.StringValue())
			}
			return out
		},
		PromptText: func(title, prompt string, opts tui.PromptOptions) (string, error) {
			defaultValue := canonical.NewNull()
			if opts.Default != nil {
				defaultValue = canonical.NewString(*opts.Default)
			}
			choices := canonical.NewNull()
			if len(opts.Choices) > 0 {
				choices = canonical.NewStringArray(opts.Choices)
			}
			u.prompts = append(u.prompts, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "title", Value: u.text(title)},
				canonical.ObjectPair{Key: "prompt", Value: u.text(prompt)},
				canonical.ObjectPair{Key: "default", Value: defaultValue},
				canonical.ObjectPair{Key: "password", Value: canonical.NewBool(opts.Password)},
				canonical.ObjectPair{Key: "choices", Value: choices},
			))
			return u.next("prompt").PyStr(), nil
		},
		ConfirmChoice: func(message string, defaultValue bool) bool {
			u.confirms = append(u.confirms, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "message", Value: u.text(message)},
				canonical.ObjectPair{Key: "default", Value: canonical.NewBool(defaultValue)},
			))
			return u.next("confirm").Truthy()
		},
		ShowResultPage: func(title string, content any) {
			copyText := canonical.NewNull()
			copyLabel := canonical.NewNull()
			if page, ok := content.(tui.ResultPage); ok {
				if page.CopyText != "" {
					copyText = canonical.NewString(page.CopyText)
				}
				if page.CopyLabel != "" {
					copyLabel = canonical.NewString(page.CopyLabel)
				}
			}
			u.results = append(u.results, canonical.NewObjectOf(
				canonical.ObjectPair{Key: "title", Value: canonical.NewString(title)},
				canonical.ObjectPair{Key: "copy_text", Value: copyText},
				canonical.ObjectPair{Key: "copy_label", Value: copyLabel},
			))
		},
		ClearTerminalHistory: func() {},
		OpenConfigFile: func(string) string {
			u.opened++
			return "已打开"
		},
		RunSubmodule: func(action func() (any, error)) any {
			value, err := action()
			if err != nil {
				u.t.Fatalf("子流程出错: %v", err)
			}
			return value
		},
		Status: func(_ string, action func()) { action() },
	}
}

// TestCorpusInteractiveFlows 回放 interactive 段。
func TestCorpusInteractiveFlows(t *testing.T) {
	corpus := loadCorpus(t)
	for _, item := range section(t, corpus, "interactive") {
		item := item
		t.Run(item.Lookup("name").StringValue(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "router-config.json")
			// 初始配置按**插入顺序**落盘：参照实现用 json.dumps(indent=2) 写测试配置，
			// 而 canonical.Dumps 会排序键——那会让 providers 的遍历顺序与 Python 不同，
			// 导致迁移结果里的键顺序对不上（实测踩过）。
			if err := os.WriteFile(path, []byte(canonical.DumpsOrdered(item.Lookup("config"))), 0o600); err != nil {
				t.Fatalf("写初始配置失败: %v", err)
			}
			ui := &scriptedUI{t: t, answers: item.Lookup("answers").Items(), path: path,
				pressKey: item.Lookup("press_on_key").StringValue()}
			transport := &scriptedTransport{t: t, steps: item.Lookup("steps").Items()}
			if transport.steps == nil {
				transport.steps = []*canonical.Value{}
			}
			ui.transport = transport
			fixedAPIKey := corpus.Lookup("fixed_local_api_key").StringValue()
			editor := New(path)
			editor.UI = ui.ui()
			editor.Prober = Prober{
				Client: &http.Client{Transport: transport},
				Clock:  newScriptedClock(),
			}
			editor.GenerateLocalAPIKey = func() (string, error) { return fixedAPIKey, nil }
			editor.RestartServiceAfterConfigChange = func(oldConfig, newConfig *config.RouterConfig) tui.Renderable {
				ui.restarts = append(ui.restarts, canonical.NewArray(
					canonical.NewString(oldConfig.Host),
					canonical.NewIntValue(int64(oldConfig.Port)),
					canonical.NewString(newConfig.Host),
					canonical.NewIntValue(int64(newConfig.Port)),
				))
				return tui.NewText("reloaded")
			}

			if err := runInteractiveCase(editor, item); err != nil {
				t.Fatalf("流程失败: %v", err)
			}
			if ui.position != len(ui.answers) {
				t.Fatalf("回答未用尽（剩余 %d 条）", len(ui.answers)-ui.position)
			}
			if transport.pos != len(transport.steps) {
				t.Fatalf("网络脚本未用尽：已用 %d / 共 %d", transport.pos, len(transport.steps))
			}

			assertValue(t, "menus", canonical.NewArray(ui.menus...), item.Lookup("menus"))
			assertValue(t, "multiples", canonical.NewArray(ui.multiples...), item.Lookup("multiples"))
			assertValue(t, "prompts", canonical.NewArray(ui.prompts...), item.Lookup("prompts"))
			assertValue(t, "confirms", canonical.NewArray(ui.confirms...), item.Lookup("confirms"))
			assertValue(t, "results", canonical.NewArray(ui.results...), item.Lookup("results"))
			assertValue(t, "restarts", canonical.NewArray(ui.restarts...), item.Lookup("restarts"))
			if want, _ := canonical.ToInt(item.Lookup("opened_count")); int64(ui.opened) != want {
				t.Errorf("打开配置文件次数: got %d want %d", ui.opened, want)
			}
			// 落盘内容按**键排序**比较：配置文件的键顺序已由 config / configops 的
			// 语料锁定，这里只需要确认本次改动写进去的字段与值一致。
			saved, err := config.LoadConfigData(path)
			if err != nil {
				t.Fatalf("读回配置失败: %v", err)
			}
			got := canonical.Dumps(saved)
			want := canonical.Dumps(item.Lookup("config_after"))
			if got != want {
				t.Errorf("落盘配置不一致:\n got = %s\nwant = %s", got, want)
			}
		})
	}
}

// runInteractiveCase 按场景的 run 字段分派到对应的编辑器入口。
//
// 入口名与生成脚本的 INTERACTIVE_RUNNERS 一一对应；场景需要的额外参数（provider_id /
// model_id）从语料里取。
func runInteractiveCase(editor *Editor, item *canonical.Value) error {
	name := item.Lookup("name").StringValue()
	providerID := item.Lookup("provider_id").StringValue()
	modelID := item.Lookup("model_id").StringValue()
	switch {
	case name == "manage_providers_returns_immediately" || name == "empty_config_provider_menu" ||
		name == "manage_providers_opens_add_provider_key_with_probe":
		return editor.ManageProvidersInteractively()
	case name == "add_provider_key_existing_provider_no_models_selected" ||
		name == "add_provider_key_saves_without_binding_when_manual_input_empty" ||
		name == "add_provider_key_rejects_duplicate_key_name" ||
		name == "add_provider_key_rejects_empty_api_key":
		_, err := editor.AddProviderKeyInteractively(providerID, false)
		return err
	case name == "manage_provider_keys_toggle_then_return" || name == "manage_provider_keys_rename" ||
		name == "manage_provider_keys_delete_last_key_confirmed":
		return editor.ManageProviderKeysInteractively(providerID)
	case name == "manage_provider_routes_set_and_clear" || name == "manage_provider_routes_restore_default":
		return editor.ManageProviderRoutesInteractively(providerID)
	case name == "refresh_provider_capability_all_keys" ||
		name == "refresh_provider_capability_single_key_one_mode":
		_, err := editor.RefreshProviderCapabilityInteractively(providerID)
		return err
	case name == "delete_provider_confirmed" || name == "delete_provider_declined":
		_, err := editor.DeleteProviderInteractively(providerID)
		return err
	case name == "manage_models_update_aliases_then_return" ||
		name == "manage_models_update_hidden_aliases_and_routing_mode" ||
		name == "manage_models_delete_model_confirmed":
		return editor.ManageV2ModelSettingsInteractively()
	case name == "delete_model_missing":
		_, err := editor.DeleteV2ModelInteractively(modelID)
		return err
	case name == "manage_model_routes_unbind_last_key_declined" ||
		name == "manage_model_routes_unbind_last_key_confirmed":
		return editor.ManageModelRoutesInteractively(modelID)
	case name == "update_model_target_upstream":
		_, err := editor.UpdateModelTargetUpstreamInteractively(modelID)
		return err
	case name == "add_model_route_new_model" || name == "add_model_route_provider_without_keys" ||
		name == "add_model_route_without_providers":
		_, err := editor.AddModelRouteInteractively(modelID)
		return err
	case name == "export_config_transfer" || name == "paste_config_transfer_apply" ||
		name == "paste_config_transfer_invalid_json":
		return editor.ManageConfigTransferInteractively()
	case name == "set_local_api_key_regenerate" || name == "set_local_api_key_declined":
		_, err := editor.SetLocalAPIKeyInteractively()
		return err
	case name == "set_webui_enable":
		_, err := editor.SetWebUIInteractively()
		return err
	case name == "set_timeouts_ok" || name == "set_timeouts_rejects_non_numeric" ||
		name == "set_timeouts_rejects_zero":
		_, err := editor.SetTimeoutsInteractively()
		return err
	case name == "set_listen_updates_port" || name == "set_listen_unchanged" ||
		name == "set_listen_rejects_scheme" || name == "set_listen_rejects_bad_port" ||
		name == "set_listen_wildcard_confirmed" || name == "set_listen_wildcard_declined":
		_, err := editor.SetListenInteractively()
		return err
	}
	return errors.New("语料里有未映射的交互场景: " + name)
}
