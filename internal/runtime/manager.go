// Package runtime 管理可热重载的运行时资源，以及流式请求的租约与超时。
//
// 本包移植 runtime.py / routing.py / streaming.py 三个模块。这里最关键、也最容易
// 写错的是**租约语义**：
//
//   - 租约覆盖整条流式响应（不只是建立连接），因此必须靠 defer 在所有退出路径上
//     释放；
//   - 热重载只能把旧一代标记为 retired，无法关闭仍被流式请求持有的连接池；
//   - close() 会等待**每一代**的租约归零，所以只要泄漏一个租约，关停就会永久阻塞。
//
// 参照实现靠 async generator 的 finally 保证释放；Go 侧没有等价机制，改为显式
// Release() 并以 atomic 保证恰好执行一次（见 Lease.Release）。
package runtime

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// ErrManagerClosed 表示运行管理器已关闭，不再接受新租约或热重载。
var ErrManagerClosed = errors.New("runtime manager is closed")

// HTTPClient 是运行时持有的 HTTP 客户端。
//
// 只要求 CloseIdleConnections，因此 *http.Client 天然满足，无需适配层。
// 参照实现调用 httpx 的 aclose() 关闭整个连接池；Go 的 http.Client 没有等价
// 方法，关闭空闲连接是最接近的语义（在途请求不受影响，这正是租约要保护的）。
type HTTPClient interface {
	CloseIdleConnections()
}

// MetricsSink 是收尾时写入指标的最小接缝。
//
// 真正的实现由 internal/metrics 提供（另一个包，尚未落地）。这里只声明迁移期
// 需要的接缝，让 finish() 的调用语义（写一次、关一次）在 metrics 就绪前就能
// 被测试锁定。参数用本包定义的 StreamOutcome，避免与 metrics 的写入结构耦合。
type MetricsSink interface {
	RecordStream(outcome StreamOutcome) error
	Close() error
}

// KeyHealth 是收尾时需要回写的 key 健康状态。
//
// *keypool.KeyPool 已实现这三个方法（签名一致），因此无需适配层。用接口而非
// 具体类型是为了让本包的测试不依赖真实的 key 选择逻辑。
type KeyHealth interface {
	MarkFailure(modelID, keyName string, statusCode *int, retryAfter *float64)
	MarkSuccess(modelID, keyName string)
	ReleaseKey(modelID, keyName string)
}

// RuntimeResources 是一代运行时资源。
//
// 并发字段由自身的 mu 保护；管理器总是在持有自己的锁时再取这把锁，锁序固定为
// manager.mu → resources.mu，避免死锁。
type RuntimeResources struct {
	Config     *config.RouterConfig
	KeyPool    KeyHealth
	Metrics    MetricsSink
	HTTPClient HTTPClient

	mu           sync.Mutex
	activeLeases int
	retired      bool
	detached     bool
	// drainCh 在 activeLeases 归零时关闭；从 0 起新租约会重建它。
	//
	// 用「关闭通道」而不是 sync.Cond，是因为 close() 要同时等待多代，而
	// <-chan struct{} 可以直接交给 select 与超时并存，测试里也能设上限，
	// 避免「关停永久阻塞」这类缺陷把测试挂死。
	drainCh     chan struct{}
	drainClosed bool
}

// NewRuntimeResources 构造一代资源，初始状态为「已排空」。
//
// 初始 drained 是参照实现的语义（runtime.py:26 在 __post_init__ 里 set()）：
// 还没有任何租约时，close() 不该等待。
func NewRuntimeResources(
	routerConfig *config.RouterConfig,
	keyPool KeyHealth,
	metrics MetricsSink,
	client HTTPClient,
) *RuntimeResources {
	empty := make(chan struct{})
	close(empty)
	return &RuntimeResources{
		Config:      routerConfig,
		KeyPool:     keyPool,
		Metrics:     metrics,
		HTTPClient:  client,
		drainCh:     empty,
		drainClosed: true,
	}
}

// takeLease 记入一个租约。
//
// 从 0 起新租约时必须重建 drainCh，否则第二次 close() 会在一个已关闭的通道上
// 等待并立即返回，从而提前关闭仍在使用的资源。
func (r *RuntimeResources) takeLease() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeLeases == 0 {
		r.drainCh = make(chan struct{})
		r.drainClosed = false
	}
	r.activeLeases++
}

// releaseLease 归还一个租约，并在归零时标记已排空。
func (r *RuntimeResources) releaseLease() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeLeases > 0 {
		r.activeLeases--
	}
	if r.activeLeases == 0 && !r.drainClosed {
		r.drainClosed = true
		close(r.drainCh)
	}
}

// markRetired 标记该代已被热重载替换。
func (r *RuntimeResources) markRetired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retired = true
}

// detachIfIdle 在该代已停用且无租约时将其摘除；返回是否成功摘除。
func (r *RuntimeResources) detachIfIdle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.detached || r.activeLeases != 0 {
		return false
	}
	r.detached = true
	return true
}

// isDetached 报告该代是否已摘除。
func (r *RuntimeResources) isDetached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detached
}

// isRetired 报告该代是否已被热重载替换。
//
// 只有 retired 代才会在租约归零后被摘除；当前代必须留着继续服务。
func (r *RuntimeResources) isRetired() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retired
}

// Retired 报告该代是否已被替换（供测试与观测）。
func (r *RuntimeResources) Retired() bool { return r.isRetired() }

// drainedChan 返回当前轮的排空通道。
func (r *RuntimeResources) drainedChan() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drainCh
}

// ActiveLeases 返回当前未归还的租约数（供测试与观测）。
func (r *RuntimeResources) ActiveLeases() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLeases
}

// Detached 报告该代是否已被摘除（供测试与观测）。
func (r *RuntimeResources) Detached() bool { return r.isDetached() }

// RuntimeManager 管理当前代与历史代的资源生命周期。
type RuntimeManager struct {
	mu          sync.Mutex
	current     *RuntimeResources
	generations map[*RuntimeResources]struct{}
	closed      bool
}

// NewRuntimeManager 以给定资源作为当前代构造管理器。
func NewRuntimeManager(resources *RuntimeResources) *RuntimeManager {
	return &RuntimeManager{
		current:     resources,
		generations: map[*RuntimeResources]struct{}{resources: {}},
	}
}

// Current 返回当前代资源。
func (m *RuntimeManager) Current() *RuntimeResources {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Lease 是运行时资源的持有凭证。
type Lease struct {
	manager   *RuntimeManager
	Resources *RuntimeResources
	released  atomic.Bool
}

// Acquire 取得一个租约。
//
// 调用方**必须**保证释放：defer lease.Release() 或 wrap_stream 的等价写法。
// 漏掉一次就会让 Close() 永久等待（参照实现的 drained 会一直不被 set）。
func (m *RuntimeManager) Acquire() (*Lease, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	resources := m.current
	resources.takeLease()
	m.mu.Unlock()
	return &Lease{manager: m, Resources: resources}, nil
}

// Release 归还租约，幂等。
//
// 用 CompareAndSwap 而非 bool 标志，是为了让并发的重复释放也恰好生效一次——
// 参照实现靠 asyncio 单线程事件循环隐式保证，Go 侧必须显式保证。
func (l *Lease) Release() {
	if l.released.CompareAndSwap(false, true) {
		l.manager.release(l.Resources)
	}
}

// Released 报告租约是否已归还。
func (l *Lease) Released() bool { return l.released.Load() }

// Replace 切换到新一代资源（热重载）。
//
// 旧代只在**没有未归还租约**时才真正摘除并关闭；否则留待最后一次 Release 处理。
// 这是流式请求能在热重载期间继续跑完的原因。
func (m *RuntimeManager) Replace(resources *RuntimeResources) error {
	var toDetach *RuntimeResources
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	previous := m.current
	if previous == resources {
		m.mu.Unlock()
		return nil
	}
	previous.markRetired()
	m.current = resources
	m.generations[resources] = struct{}{}
	if previous.detachIfIdle() {
		toDetach = previous
	}
	if toDetach != nil {
		delete(m.generations, toDetach)
	}
	m.mu.Unlock()

	if toDetach != nil {
		m.closeUnused(toDetach)
	}
	return nil
}

// release 处理一次租约归还。
//
// **只有 retired 代才会被摘除**（对齐 runtime.py:93）。这是关键：当前代在租约
// 归零后仍然要继续服务后续请求，若在此处摘除它，下一次 Acquire 会拿到一个
// detached 的资源，而 Release 又会因 detached 提前返回 —— 并发计数再也回不去。
func (m *RuntimeManager) release(resources *RuntimeResources) {
	var toDetach *RuntimeResources
	m.mu.Lock()
	if resources.isDetached() {
		m.mu.Unlock()
		return
	}
	resources.releaseLease()
	if resources.isRetired() && resources.detachIfIdle() {
		toDetach = resources
	}
	if toDetach != nil {
		delete(m.generations, toDetach)
	}
	m.mu.Unlock()

	if toDetach != nil {
		m.closeUnused(toDetach)
	}
}

// Close 关闭管理器并等待所有代的租约归零。
//
// 关停顺序不可交换：必须先标记 closed（挡住新租约），再等待排空，最后才关闭
// 连接池。反过来会让在途流式请求读到已关闭的连接池。
func (m *RuntimeManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	generations := make([]*RuntimeResources, 0, len(m.generations))
	for resources := range m.generations {
		generations = append(generations, resources)
	}
	m.mu.Unlock()

	for _, resources := range generations {
		resources.markRetired()
	}
	// 逐代等待排空。用循环而非 WaitGroup：这里要的是「等待每一代都归零」，
	// 且各代之间没有顺序要求，但闭包捕获在 Go 1.22 之前是经典陷阱，循环更直白。
	for _, resources := range generations {
		<-resources.drainedChan()
	}

	var firstErr error
	for _, resources := range generations {
		m.mu.Lock()
		detached := false
		if resources.detachIfIdle() {
			delete(m.generations, resources)
			detached = true
		}
		m.mu.Unlock()
		if detached {
			if err := m.closeUnused(resources); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// closeUnused 关闭某代独占的连接池与指标库。
//
// 共享检测是必需的：热重载常见做法是复用同一个 http.Client 或 metrics，此时
// 关闭它会打断新代。参照实现用 `is` 比较对象身份（runtime.py:123-130），Go 侧
// 依赖接口值的 == 比较，因此实现必须是可比较的（实践中都是指针）。
func (m *RuntimeManager) closeUnused(resources *RuntimeResources) error {
	m.mu.Lock()
	closeClient := true
	closeMetrics := true
	for generation := range m.generations {
		if generation.HTTPClient == resources.HTTPClient {
			closeClient = false
		}
		if generation.Metrics == resources.Metrics {
			closeMetrics = false
		}
	}
	m.mu.Unlock()

	var firstErr error
	if closeClient && resources.HTTPClient != nil {
		resources.HTTPClient.CloseIdleConnections()
	}
	if closeMetrics && resources.Metrics != nil {
		if err := resources.Metrics.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
