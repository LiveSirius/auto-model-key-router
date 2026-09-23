// Package health 构造 /health 的响应体。
//
// 移植 auto_model_key_router/app.py:153-181 的 health 处理器与 webui.py:48-70 的
// webui_status。单独成包的理由：/health 是**冻结契约**——测试与发布流程都断言它的
// 字段名与语义，调用方（CLI、运维脚本、agent 配置工具）按字段名取值。把它从 HTTP
// 处理器里剥出来，才能不启动服务就逐字段对拍。
//
// 字段顺序即响应字节顺序，不可重排：canonical 编码器按插入顺序输出，而参照实现是
// 一个 dict 字面量 + `**webui_status(app)` 展开。顺序固定为
// status, version, models, config_path, local_auth_enabled, local_api_key_fingerprint,
// unified_model, native_endpoint_states, ops_enabled, webui_available, webui_enabled,
// webui_mounted, webui_path。
//
// 与参照实现相比少了四个字段：`visitor_feature_installed`（产品决策删除，见原 Build
// 说明）以及访客模式整体取消后一并删除的 `visitor_access_enabled` 与
// `visitor_key_count`（还有 Message 里提到的那一个）。其余顺序原样保留，
// 由 TestBuildFieldOrder 锁定。
package health

import (
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/formatting"
)

// webUIPath 是 WebUI 的固定挂载前缀（webui.py:19）。
const webUIPath = "/ui"

// KeyPool 是 /health 需要的 key pool 读数。
//
// 用接口而不是直接依赖 *keypool.KeyPool：一是本包因此不依赖 keypool，二是对拍时
// 可以用固定的假实现覆盖「没有模型」等难以在真实 pool 上构造的组合。
// *keypool.KeyPool 天然满足它。
type KeyPool interface {
	// PublicModelIDs 是 /v1/models 与 /health 对外暴露的模型 ID。
	PublicModelIDs() []string
	// UnifiedRoute 是 unified 路由描述，未配置时为 nil。
	UnifiedRoute() *canonical.Value
	// EndpointCapabilityStates 是各上游原生端点的探测结果。
	EndpointCapabilityStates() *canonical.Value
}

// WebUI 是 WebUI 的当前状态。
type WebUI struct {
	// Available 表示静态资产随当前安装一起发布。
	Available bool
	// Enabled 是配置里的期望状态。
	Enabled bool
	// Mounted 表示当前进程真的在提供 /ui。与 Enabled 不一致说明改了开关但没重启。
	Mounted bool
	// MountPrefix 是嵌入时的挂载前缀（独立运行时为空串）。
	MountPrefix string
}

// path 返回 WebUI 的实际访问路径；未挂载时为 false。
//
// 对齐 webui.py:67-70：前缀先 rstrip("/") 再拼 /ui，所以 "/amkr/" 与 "/amkr" 都得到
// "/amkr/ui"，而空前缀得到 "/ui"。未挂载时返回 false，由 Build 输出 null——**不是**
// 空串，两者在 JSON 里不同。
func (w WebUI) path() (string, bool) {
	if !w.Mounted {
		return "", false
	}
	return strings.TrimRight(w.MountPrefix, "/") + webUIPath, true
}

// Inputs 是构造 /health 响应所需的全部输入。
type Inputs struct {
	// Version 是进程版本号。
	Version string
	// ConfigPath 是当前生效的配置文件路径。
	ConfigPath string
	// LocalAPIKey 为本地 API key；为空表示鉴权整体关闭。
	LocalAPIKey string
	// OpsEnabled 表示 ops 接口是否启用。
	OpsEnabled bool
	// KeyPool 提供模型与端点读数。
	KeyPool KeyPool
	// WebUI 是 WebUI 状态。
	WebUI WebUI
}

// Build 构造 /health 的响应体。
//
// 访客相关的三个字段（`visitor_feature_installed`、`visitor_access_enabled`、
// `visitor_key_count`）已随访客模式一并删除：固定访客 key 不再存在，取而代之的是
// 配置里的**访问密钥**资源，它的数量与管理走 /api/access-keys，不属于存活探针该
// 报的内容（/health 无鉴权，报出「本实例发了几把受限 key」没有用处，只是泄漏面）。
//
// 这是**破坏性变更**：按字段名取值的老调用方（CLI、运维脚本、agent 配置工具）需要
// 同步去掉对这三个字段的处理。
//
// 另注意 ops_enabled 走 bool() 强制转换（app.py:177），空串为 false、非空串为 true；
// Go 侧 Inputs.OpsEnabled 已是 bool，转换在装配处完成。
func Build(in Inputs) *canonical.Value {
	var models *canonical.Value
	var unifiedModel *canonical.Value
	var nativeStates *canonical.Value
	if in.KeyPool != nil {
		ids := in.KeyPool.PublicModelIDs()
		if ids == nil {
			ids = []string{}
		}
		models = canonical.NewStringArray(ids)
		unifiedModel = in.KeyPool.UnifiedRoute()
		nativeStates = in.KeyPool.EndpointCapabilityStates()
	} else {
		models = canonical.NewStringArray([]string{})
	}
	// 参照实现里未配置 unified 路由时该字段是 None，落到 JSON 就是 null。
	if unifiedModel == nil {
		unifiedModel = canonical.NewNull()
	}
	if nativeStates == nil {
		nativeStates = canonical.NewNull()
	}

	webuiPath := canonical.NewNull()
	if path, ok := in.WebUI.path(); ok {
		webuiPath = canonical.NewString(path)
	}

	return canonical.NewObjectOf(
		canonical.ObjectPair{Key: "status", Value: canonical.NewString("ok")},
		canonical.ObjectPair{Key: "version", Value: canonical.NewString(in.Version)},
		canonical.ObjectPair{Key: "models", Value: models},
		canonical.ObjectPair{Key: "config_path", Value: canonical.NewString(in.ConfigPath)},
		canonical.ObjectPair{Key: "local_auth_enabled", Value: canonical.NewBool(in.LocalAPIKey != "")},
		canonical.ObjectPair{Key: "local_api_key_fingerprint", Value: canonical.NewString(formatting.KeyFingerprint(in.LocalAPIKey))},
		canonical.ObjectPair{Key: "unified_model", Value: unifiedModel},
		canonical.ObjectPair{Key: "native_endpoint_states", Value: nativeStates},
		canonical.ObjectPair{Key: "ops_enabled", Value: canonical.NewBool(in.OpsEnabled)},
		canonical.ObjectPair{Key: "webui_available", Value: canonical.NewBool(in.WebUI.Available)},
		canonical.ObjectPair{Key: "webui_enabled", Value: canonical.NewBool(in.WebUI.Enabled)},
		canonical.ObjectPair{Key: "webui_mounted", Value: canonical.NewBool(in.WebUI.Mounted)},
		canonical.ObjectPair{Key: "webui_path", Value: webuiPath},
	)
}
