package server

import (
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/auth"
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/metrics"
	"github.com/Sparrived/auto-model-key-router/internal/runtime"
)

// accessKeyUsagePath 是**访客看板**的读数端点（访客 = 访问密钥的持有者）。
//
// 与 /ui/workspace-panel.json 的分工：那个给面板 key 看它自己那一个**工作空间**，
// 这个给访问密钥看它**自己**的用量。两者都是「一条路由一个范围」，读代码就能确定
// 边界，而不是让一条路由按凭据种类分支、把「谁能看到多少」藏进一串判断。
//
// **为什么挂在 /ui/ 而不是新开 /api/ 路由**：与 workspaces.go、pricing.go、
// update.go、workspace_usage.go 同一条理由——本项目自己的响应形状不该混进被冻结的
// /api 清单，挂在 /ui/ 之下则落在所有清单之外。
const accessKeyUsagePath = "/access-key-usage.json"

// handleAccessKeyUsage 返回**请求凭据那一把**访问密钥的用量、拆分与最近调用明细。
//
// 鉴权只认访问密钥，其它三种凭据都被拒：
//
//   - 完整权限走 /metrics 与 /metrics/requests（那里能看全部流量），没必要也不应该
//     从这条受限路径拿数据；
//   - 工作空间面板 key 与推理 key 各有自己的读数（/ui/workspace-panel.json），
//     它们的归属在另一张旁挂表里，在这条路由上本来也查不到任何行——但那样会返回
//     一份「全是 0」的合法响应，让人以为自己的流量丢了，因此这里显式拒绝。
//
// 密钥身份**完全由 Bearer 凭据决定**，不接受任何请求参数或请求头指定要查哪一把：
// 这正是这个端点存在的意义。加一个 ?key_id= 参数就等于让任何一把 key 读别人的用量。
func (a *App) handleAccessKeyUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	// hours 与 /metrics、workspace-usage 同一套取值方式（含 all_history），前端的时间
	// 选择器对三者语义一致。
	result, errs := validateQuery(r.URL.Query(), accessKeyUsageParams)
	if len(errs) > 0 {
		writeQueryErrors(w, errs)
		return
	}
	a.withLease(w, func(resources *runtime.RuntimeResources) {
		accessKey, status := accessKeyForRequest(resources.Config, r)
		if accessKey == nil {
			writeErrorEnvelope(w, status, accessKeyFailureMessage(status))
			return
		}
		store := metricsStoreOf(resources)
		if store == nil {
			writeInternalError(w)
			return
		}
		body, err := store.AccessKeyUsage(metrics.AccessKeyUsageParams{
			Hours:       result.hoursOrNil("hours", "all_history"),
			AccessKeyID: accessKey.ID,
		})
		if err != nil {
			// 参数已由 validateQuery 挡住，这里只可能是数据库错误；与 /metrics 一致
			// 冒到 500。
			writeInternalError(w)
			return
		}
		// 看板页要在标题里显示「这是哪把 key」，而 key 是它唯一的输入。把名字放在
		// 响应里，前端就不必再猜或另开一个接口。
		//
		// 只放 id 与 name：**明文 key 绝不回显**，它只出现在创建与轮换的响应里
		// （见 internal/api/handlers_accesskeys.go）。
		body.SetKey("access_key_name", canonical.NewString(accessKey.Name))
		writeJSON(w, http.StatusOK, body)
	})
}

// accessKeyFailureMessage 给出凭据解析失败的文案。
//
// 停用与「不是访问密钥」分开：停用是可恢复的已知身份（403，与 proxy 的文案一致），
// 而凭据不对是 401。两者的排查方向完全不同——前者去管理面把开关打开，后者是拿错了
// key——混成一条会让人白找半天。
func accessKeyFailureMessage(status int) string {
	if status == http.StatusForbidden {
		return "访问密钥已被停用"
	}
	return authFailureMessage
}

// accessKeyForRequest 解析请求凭据对应的访问密钥，返回 nil 与应回的状态码。
//
// 与 proxy 的 authorize 用**同一个**查表（config.AccessKeyFor），否则同一把 key 在
// 代理侧与看板侧的判定可能不一致（一个放行、一个说停用）。
//
// 停用的 key 也要认出来（AccessKeyFor 刻意会匹配停用的 key），否则无法给出「已停用」
// 这个比「凭据不对」有用得多的结论。
func accessKeyForRequest(cfg *config.RouterConfig, r *http.Request) (*config.AccessKeyConfig, int) {
	if cfg == nil {
		return nil, http.StatusUnauthorized
	}
	key := auth.RequestAPIKey(r.Header)
	if key == "" {
		return nil, http.StatusUnauthorized
	}
	accessKey := cfg.AccessKeyFor(key)
	if accessKey == nil {
		return nil, http.StatusUnauthorized
	}
	if !accessKey.Enabled {
		return nil, http.StatusForbidden
	}
	return accessKey, http.StatusOK
}
