package configeditor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// numericError 把 canonical 的数值转换错误折算成 config 包的错误类型，沿用该包的
// 约定：Python 的 ValueError（文本不是数字）是对外契约错误，TypeError（类型不对）
// 是内部错误。
func numericError(err error) error {
	var valueErr *canonical.ValueError
	if errors.As(err, &valueErr) {
		return &config.ConfigError{Message: valueErr.Message}
	}
	return &config.InternalError{Message: err.Error()}
}

// ServiceManagementBaseURL 对应 config_editor.py:86 的 service_management_base_url：
// 把配置里的 host/port 折算成「本机可达」的管理地址。
//
// 三个细节照抄参照实现：通配地址（0.0.0.0 / ::）换成 127.0.0.1；含冒号且未加方括号的
// IPv6 字面量补上方括号；host/port 是 Python 的 `or` 语义（空串、0、None 都用默认值）。
//
// 与参照实现的差异：`int(data.get("port"))` 对非数字端口抛 TypeError，Go 侧返回
// config.InternalError（与 config 包对 TypeError 的处理一致），不静默取默认值。
func ServiceManagementBaseURL(data *canonical.Value) (string, error) {
	host := data.Lookup("host").StringValue()
	if host == "" {
		host = "127.0.0.1"
	}
	rawPort := data.Lookup("port")
	port := int64(8000)
	if rawPort.Truthy() {
		parsed, err := canonical.ToInt(rawPort)
		if err != nil {
			return "", numericError(err)
		}
		port = parsed
	}
	connectHost := host
	if connectHost == "0.0.0.0" || connectHost == "::" {
		connectHost = "127.0.0.1"
	}
	if strings.Contains(connectHost, ":") && !strings.HasPrefix(connectHost, "[") {
		connectHost = "[" + connectHost + "]"
	}
	return "http://" + connectHost + ":" + strconv.FormatInt(port, 10), nil
}

// NativeEndpointSupportText 对应 config_editor.py:193 的 native_endpoint_support_text。
func NativeEndpointSupportText(state *canonical.Value) string {
	if !state.Truthy() {
		return "\n[dim]探测: 未测试[/dim]"
	}
	reasonValue := state.Lookup("reason")
	if !reasonValue.Truthy() {
		reasonValue = canonical.NewString("-")
	}
	reason := reasonValue.PyStr()
	if supported, ok := state.Lookup("supported").AsBool(); ok && supported {
		return "\n[green]探测: 支持[/green] [dim]" + reason + "[/dim]"
	}
	retry := ""
	// Python 的 isinstance(expires, int) 对 bool 也为真（bool 是 int 的子类），
	// 且对 30.0 这样的浮点为假；因此要区分整数字面量与浮点字面量，不能用 AsInt 的
	// 「能转成整数」来判断。
	expires := state.Lookup("expires_in_seconds")
	switch {
	case expires.IsBool():
		seconds := int64(0)
		if expires.Bool {
			seconds = 1
		}
		retry = "，" + strconv.FormatInt(seconds, 10) + "s 后重试"
	case expires.IsNumber() && isIntLiteral(expires.Num):
		seconds, _ := expires.AsInt()
		retry = "，" + strconv.FormatInt(seconds, 10) + "s 后重试"
	}
	return "\n[yellow]探测: 回退缓存[/yellow] [dim]" + reason + retry + "[/dim]"
}

// isIntLiteral 报告数字字面量是否为整数，对应 Python 的 isinstance(x, int)。
//
// canonical 的数字都保存成 Python repr 之后的字面量，因此整数的字面量里不会出现
// 小数点或指数（见 internal/canonical 的 isFloatLiteral）。
func isIntLiteral(literal string) bool {
	return !strings.ContainsAny(literal, ".eE") &&
		!strings.Contains(literal, "Infinity") && literal != "NaN"
}

// NativeEndpointStatePayload 对应 config_editor.py:138 的
// _native_endpoint_state_payload。
//
// now 是 `time.time()`（epoch 秒，注入以便测试）。返回 nil 表示「这个值不是一份
// 可用的状态」——对应 Python 的 None。
func NativeEndpointStatePayload(value *canonical.Value, now float64) (*canonical.Value, error) {
	if value.IsBool() {
		return canonical.NewObjectOf(
			canonical.ObjectPair{Key: "supported", Value: canonical.NewBool(value.Bool)},
			canonical.ObjectPair{Key: "reason", Value: canonical.NewString("legacy")},
			canonical.ObjectPair{Key: "expires_in_seconds", Value: canonical.NewNull()},
		), nil
	}
	if !value.IsObject() {
		return nil, nil
	}
	if _, ok := value.Lookup("supported").AsBool(); !ok {
		return nil, nil
	}
	payload := value.Clone()
	expiresAt := payload.Lookup("expires_at")
	if expiresAt.IsNull() && payload.Lookup("ttl_seconds").Truthy() {
		checkedAt := 0.0
		if raw := payload.Lookup("checked_at"); raw.Truthy() {
			parsed, err := canonical.ToFloat(raw)
			if err != nil {
				return nil, numericError(err)
			}
			checkedAt = parsed
		}
		ttl, err := canonical.ToFloat(payload.Lookup("ttl_seconds"))
		if err != nil {
			return nil, numericError(err)
		}
		expiresAt = canonical.NewFloat(checkedAt + ttl)
	}
	if !expiresAt.IsNull() {
		value, err := canonical.ToFloat(expiresAt)
		if err != nil {
			return nil, numericError(err)
		}
		remaining := int64(value - now)
		if remaining < 0 {
			remaining = 0
		}
		payload.SetKey("expires_in_seconds", canonical.NewIntValue(remaining))
	}
	return payload, nil
}

// LoadNativeEndpointStatesFromFile 对应 config_editor.py:97 的
// load_native_endpoint_states_from_file：从 endpoint_capabilities_path
// （兼容旧键 key_state_path）读取原生端点能力缓存。
//
// 做成 Prober 的方法（而不是自由函数）只为一件事：过期换算要用 `time.time()`，
// 而时钟是 Prober 的接缝。文件读不到或不是合法 JSON 时返回空对象（参照实现的
// `except (OSError, ValueError)`）；但**条目本身**不合法（expires_at 不是数字）
// 时会失败——参照实现里那个 ValueError 在 try 之外，同样会冒出去。
func (p Prober) LoadNativeEndpointStatesFromFile(data *canonical.Value) (*canonical.Value, error) {
	rawPath := data.Lookup("endpoint_capabilities_path")
	if !rawPath.Truthy() {
		rawPath = data.Lookup("key_state_path")
	}
	path := rawPath.StringValue()
	if path == "" {
		defaultPath, err := config.DefaultEndpointCapabilitiesPath()
		if err != nil {
			return canonical.NewObject(), nil
		}
		path = defaultPath
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return canonical.NewObject(), nil
	}
	raw, err := canonical.Parse(content)
	if err != nil {
		return canonical.NewObject(), nil
	}
	states := raw.Lookup("endpoint_capabilities")
	if !states.Truthy() {
		// Python 的 `raw.get("endpoint_capabilities", raw.get("url_native_support", {}))`：
		// 键存在但值为假（None/{}）时**不会**回退到 url_native_support。
		if _, present := raw.LookupOK("endpoint_capabilities"); !present {
			states = raw.Lookup("url_native_support")
		}
	}
	if !states.IsObject() {
		return canonical.NewObject(), nil
	}
	out := canonical.NewObject()
	now := p.clock().EpochSeconds()
	for _, key := range states.Obj.Keys() {
		child, _ := states.Obj.Get(key)
		payload, err := NativeEndpointStatePayload(child, now)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			out.SetKey(key, payload)
		}
	}
	return out, nil
}

// FetchNativeEndpointStates 对应 config_editor.py:119 的 fetch_native_endpoint_states：
// 先读文件缓存，再用 `GET {管理地址}/health` 的 native_endpoint_states 覆盖。
//
// 参照实现用模块级 `httpx.get(...)`（无 Authorization），Go 侧走 Prober 的客户端接缝，
// 因此可以脚本化测试。失败（网络错误、非 2xx、响应不是 JSON 对象）时**原样返回文件状态**，
// 与参照实现把这几类异常折算成「只用文件缓存」一致。
func (p Prober) FetchNativeEndpointStates(ctx context.Context, data *canonical.Value, timeout float64) (*canonical.Value, error) {
	states, err := p.LoadNativeEndpointStatesFromFile(data)
	if err != nil {
		return nil, err
	}
	baseURL, err := ServiceManagementBaseURL(data)
	if err != nil {
		return states, nil
	}
	result, requestErr := p.doRequest(ctx, http.MethodGet, baseURL+"/health", "", nil, timeout, false)
	if requestErr != nil {
		return states, nil
	}
	if result.status < 200 || result.status >= 300 {
		return states, nil
	}
	raw, parseErr := canonical.Parse(result.body)
	if parseErr != nil {
		return states, nil
	}
	// 参照实现在这里会因 AttributeError 直接崩溃（list 没有 .get）；Go 侧折叠成
	// 「只用文件缓存」而不是崩溃，避免一条坏响应把整个 dashboard 打挂。
	if !raw.IsObject() {
		return states, nil
	}
	serviceStates := raw.Lookup("native_endpoint_states")
	if !serviceStates.IsObject() {
		return states, nil
	}
	merged := states.Clone()
	for _, key := range serviceStates.Obj.Keys() {
		child, _ := serviceStates.Obj.Get(key)
		if child.IsObject() {
			merged.SetKey(key, child)
		}
	}
	return merged, nil
}
