package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Sparrived/auto-model-key-router/internal/pricing"
)

// pricingPath 是价格目录在 WebUI 挂载下的文件名。
//
// **为什么挂在 /ui/ 而不是新开一条 /api/ 路由**：管理面的 47 条路由、运维面的 7 条
// 与 app 面的 13 条都被逐字节语料锁定（那些语料由已退役的 Python 脚本生成，是本项目
// 兼容性的唯一凭证）。新增一条 /api 路由就得手写一条语料用例，那等于伪造兼容性证据。
// /ui/ 挂载在所有清单之外，且已经有一套"可用/启用"的判断逻辑，把只读文档放在这里
// 不需要碰任何冻结清单。
//
// 代价是它与 WebUI 同生共死：webui_enabled 为假时 /ui/ 整体不注册，价格目录也
// 不可达。这是可接受的——成本估算本来就是 WebUI 的读数，没有界面时无人消费。
const pricingPath = "/pricing.json"

// pricingMaxAge 是浏览器可以复用的秒数。
//
// 取 0 + must-revalidate：与 models.dev 自己的策略一致，也避免界面显示过期价格。
// 有 ETag 兜底，复验命中时只是一个 304，不会重复传 230 KB。
const pricingCacheControl = "public, must-revalidate, max-age=0"

// handlePricing 提供 models.dev 价格目录（供 WebUI 估算成本）。
//
// 三条语义：
//
//   - **不鉴权**。内容是 models.dev 的公开数据，没有任何本机凭据；WebUI 的静态资源
//     本身也是公开的，价格目录跟着它走才一致。
//   - **目录不可用时回 503**，而不是 200 + 空目录。前端据此区分"还在取"与"确实没有
//     定价"，从而显示"—"而不是把成本算成 $0。
//   - **不在请求路径上取回**。Catalog 只返回当前快照，过期时踢一个后台刷新，因此
//     这个处理器永远不会阻塞三十秒（目录有 4.7 MB，冷取实测 4~35 秒）。
func (a *App) handlePricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// 与 /ui/ 下的静态资源一致（internal/webui 的 serve）。
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	payload, ok := a.pricing.Payload()
	if !ok {
		// 首次启动、或一直连不上 models.dev。**不回空目录**：那会让前端把每个模型
		// 都当成免费，成本列全变 $0——比"没有数据"更糟，因为它看起来像真的。
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "价格目录尚不可用", http.StatusServiceUnavailable)
		return
	}

	// ETag 由内容算出：目录刷新后会自然变化，不需要额外的版本号。
	sum := sha256.Sum256(payload)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", pricingCacheControl)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	// HEAD 的响应体由 net/http 丢弃，但 Content-Length 已经在上面给出（与
	// /ui/ 下静态文件的行为一致）。
	_, _ = w.Write(payload)
}

// newPricingCatalog 构造价格目录。fetch 为 nil 时**完全不出网**（见 Options.PricingFetch）。
//
// 这里刻意不给 nil 配真实的 HTTP 实现：构造 App 的测试有几十处，任何一处漏配都会让
// 测试进程去打 models.dev。缺失目录时 /ui/pricing.json 回 503，前端显示"无定价"。
func newPricingCatalog(fetch pricing.Fetcher) *pricing.Catalog {
	if fetch == nil {
		fetch = func(url, etag string, timeout time.Duration) (pricing.FetchResult, error) {
			return pricing.FetchResult{}, errPricingDisabled
		}
	}
	return pricing.New(fetch)
}

// errPricingDisabled 表示装配时没有注入价格目录的取回实现。
var errPricingDisabled = errors.New("pricing: 未配置价格目录来源（Options.PricingFetch 为 nil）")
