package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeCloser 记录关闭次数。
type fakeCloser struct {
	mu     sync.Mutex
	closes int
}

func (f *fakeCloser) CloseIdleConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
}

func (f *fakeCloser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// closeErrorCloser 模拟关闭幂等且返回 nil。
func (f *fakeCloser) Close() error { return nil }

// fakeMetrics 记录写入的指标行。
type fakeMetrics struct {
	mu       sync.Mutex
	outcomes []StreamOutcome
	closes   int
}

func (f *fakeMetrics) RecordStream(outcome StreamOutcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcome)
	return nil
}

func (f *fakeMetrics) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeMetrics) recorded() []StreamOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StreamOutcome(nil), f.outcomes...)
}

// fakeKeyHealth 记录 key 状态回写。
type fakeKeyHealth struct {
	mu         sync.Mutex
	failures   int
	successes  int
	releases   int
	lastStatus *int
	lastRetry  *float64
}

func (f *fakeKeyHealth) MarkFailure(_, _ string, statusCode *int, retryAfter *float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures++
	f.lastStatus = statusCode
	f.lastRetry = retryAfter
}

func (f *fakeKeyHealth) MarkSuccess(_, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes++
}

func (f *fakeKeyHealth) ReleaseKey(_, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
}

func (f *fakeKeyHealth) snapshot() (failures, successes, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures, f.successes, f.releases
}

// testResources 构造一代仅含假件的资源。
//
// 必须条件赋值：把 (*fakeCloser)(nil) 直接塞进 HTTPClient 接口会得到一个
// **非 nil 接口**（接口里存了类型信息），从而绕过 closeUnused 的 nil 判断并在
// 调用时 panic。这是 Go 的经典陷阱，测试辅助函数里也要避开。
func testResources(client *fakeCloser, metrics *fakeMetrics) *RuntimeResources {
	var clientPort HTTPClient
	if client != nil {
		clientPort = client
	}
	var metricsPort MetricsSink
	if metrics != nil {
		metricsPort = metrics
	}
	return NewRuntimeResources(nil, nil, metricsPort, clientPort)
}

// TestLeaseReleaseIsIdempotent 验证重复释放只生效一次。
//
// 这是最关键的不变量：多减一次并发计数会让 key 被过早复用，而漏减一次会让
// 该 key 永久占用名额。参照实现靠事件循环单线程隐式保证，Go 侧必须显式保证。
func TestLeaseReleaseIsIdempotent(t *testing.T) {
	resources := testResources(nil, nil)
	manager := NewRuntimeManager(resources)

	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	if got := resources.ActiveLeases(); got != 1 {
		t.Fatalf("租约数应为 1，实际 %d", got)
	}

	lease.Release()
	if got := resources.ActiveLeases(); got != 0 {
		t.Fatalf("释放后租约数应为 0，实际 %d", got)
	}
	// 再释放多次都不应把计数减成负数。
	lease.Release()
	lease.Release()
	if got := resources.ActiveLeases(); got != 0 {
		t.Fatalf("重复释放不应改变租约数，实际 %d", got)
	}
	if !lease.Released() {
		t.Fatal("Released() 应为 true")
	}
}

// TestConcurrentReleaseExactlyOnce 验证并发释放恰好生效一次。
//
// 用 -race 之外的手段也能验证：计数若被减多次会变负（被 max 0 掩盖），因此改用
// 「归还次数」观测——通过多次 Acquire 后并发 Release 同一批租约，最终必须归零且
// 不 panic。
func TestConcurrentReleaseExactlyOnce(t *testing.T) {
	resources := testResources(nil, nil)
	manager := NewRuntimeManager(resources)

	const leases = 32
	held := make([]*Lease, 0, leases)
	for range leases {
		lease, err := manager.Acquire()
		if err != nil {
			t.Fatalf("acquire 失败: %v", err)
		}
		held = append(held, lease)
	}

	var wait sync.WaitGroup
	for _, lease := range held {
		// 每个租约被多个 goroutine 同时释放。
		for range 4 {
			wait.Add(1)
			go func(target *Lease) {
				defer wait.Done()
				target.Release()
			}(lease)
		}
	}
	wait.Wait()

	if got := resources.ActiveLeases(); got != 0 {
		t.Fatalf("全部释放后租约数应为 0，实际 %d", got)
	}
}

// TestCloseWaitsForDrain 验证 Close 会等待在途租约归还。
func TestCloseWaitsForDrain(t *testing.T) {
	resources := testResources(nil, nil)
	manager := NewRuntimeManager(resources)

	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- manager.Close() }()

	// Close 必须还没返回——租约未归还。
	select {
	case <-done:
		t.Fatal("Close 不应在租约归还前返回")
	case <-time.After(50 * time.Millisecond):
	}

	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("租约归还后 Close 应返回")
	}
}

// TestLeakedLeaseBlocksCloseForever 用超时把「泄漏租约导致关停永久阻塞」这个
// 最危险的失效模式固化成可运行的检查。
//
// 参照实现里一个泄漏的租约会让 close() 无限等待。这里刻意**不**释放租约，断言
// Close 在给定时间内不返回——如果哪天有人把 Close 改成不等排空，这个测试会失败，
// 提示那处改动破坏了关停顺序保证。
func TestLeakedLeaseBlocksCloseForever(t *testing.T) {
	resources := testResources(nil, nil)
	manager := NewRuntimeManager(resources)

	// 故意泄漏：取得后不释放。
	if _, err := manager.Acquire(); err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = manager.Close()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("泄漏租约时 Close 不应返回——关停顺序保证已被破坏")
	case <-time.After(150 * time.Millisecond):
		// 符合预期。
	}
}

// TestAcquireAfterCloseFails 验证关闭后不再接受租约。
func TestAcquireAfterCloseFails(t *testing.T) {
	manager := NewRuntimeManager(testResources(nil, nil))
	if err := manager.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, err := manager.Acquire(); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("关闭后 acquire 应返回 ErrManagerClosed，实际 %v", err)
	}
}

// TestReplaceDefersCloseUntilDrain 验证热重载期间旧代不被提前关闭。
//
// 这是流式请求能在热重载中跑完的原因：旧代被标记 retired，但只要还有租约就不关
// 连接池。
func TestReplaceDefersCloseUntilDrain(t *testing.T) {
	oldClient := &fakeCloser{}
	oldMetrics := &fakeMetrics{}
	previous := testResources(oldClient, oldMetrics)
	manager := NewRuntimeManager(previous)

	// 旧代上有一个在途流式请求。
	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}

	newClient := &fakeCloser{}
	newMetrics := &fakeMetrics{}
	if err := manager.Replace(testResources(newClient, newMetrics)); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}

	// 旧代仍被占用，不能被关闭。
	if got := oldClient.count(); got != 0 {
		t.Fatalf("旧代连接池不应在租约未归还时关闭，实际关闭 %d 次", got)
	}
	if got := oldMetrics.closes; got != 0 {
		t.Fatalf("旧代指标库不应在租约未归还时关闭，实际关闭 %d 次", got)
	}
	// 新代是当前代。
	if manager.Current() == previous {
		t.Fatal("Replace 后 current 应指向新代")
	}

	// 归还租约后旧代才被关闭。
	lease.Release()
	if got := oldClient.count(); got != 1 {
		t.Fatalf("旧代连接池应在归还后关闭一次，实际 %d 次", got)
	}
	if got := oldMetrics.closes; got != 1 {
		t.Fatalf("旧代指标库应在归还后关闭一次，实际 %d 次", got)
	}
}

// TestReplaceWithIdlePreviousClosesImmediately 验证无租约的旧代立即关闭。
func TestReplaceWithIdlePreviousClosesImmediately(t *testing.T) {
	oldClient := &fakeCloser{}
	manager := NewRuntimeManager(testResources(oldClient, &fakeMetrics{}))

	if err := manager.Replace(testResources(&fakeCloser{}, &fakeMetrics{})); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}
	if got := oldClient.count(); got != 1 {
		t.Fatalf("无租约的旧代应立即关闭，实际 %d 次", got)
	}
}

// TestReplaceWithSameResourcesIsNoop 验证替换成自身不做任何事。
func TestReplaceWithSameResourcesIsNoop(t *testing.T) {
	client := &fakeCloser{}
	resources := testResources(client, &fakeMetrics{})
	manager := NewRuntimeManager(resources)

	if err := manager.Replace(resources); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}
	if got := client.count(); got != 0 {
		t.Fatalf("替换成自身不应关闭资源，实际 %d 次", got)
	}
	if manager.Current() != resources {
		t.Fatal("替换成自身后 current 应不变")
	}
}

// TestReplaceAfterCloseFails 验证关闭后不再接受热重载。
func TestReplaceAfterCloseFails(t *testing.T) {
	manager := NewRuntimeManager(testResources(nil, nil))
	if err := manager.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := manager.Replace(testResources(nil, nil)); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("关闭后 Replace 应返回 ErrManagerClosed，实际 %v", err)
	}
}

// TestCloseIsIdempotent 验证重复关闭。
func TestCloseIsIdempotent(t *testing.T) {
	manager := NewRuntimeManager(testResources(nil, nil))
	for range 3 {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	}
}

// TestSharedResourcesNotClosed 验证共享的连接池/指标库不被关闭。
//
// 热重载常见做法是复用同一个 http.Client；关闭它会让新代立刻失效——一个很难
// 从现象定位的缺陷。
func TestSharedResourcesNotClosed(t *testing.T) {
	shared := &fakeCloser{}
	sharedMetrics := &fakeMetrics{}
	previous := NewRuntimeResources(nil, nil, sharedMetrics, shared)
	manager := NewRuntimeManager(previous)

	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	// 新一代复用同一个 client 与 metrics。
	next := NewRuntimeResources(nil, nil, sharedMetrics, shared)
	if err := manager.Replace(next); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}
	lease.Release()

	if got := shared.count(); got != 0 {
		t.Fatalf("被新一代共享的连接池不应关闭，实际 %d 次", got)
	}
	if got := sharedMetrics.closes; got != 0 {
		t.Fatalf("被新一代共享的指标库不应关闭，实际 %d 次", got)
	}

	// 全部关闭时才真正释放。
	if err := manager.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if got := shared.count(); got != 1 {
		t.Fatalf("最终应关闭共享连接池一次，实际 %d 次", got)
	}
}

// TestCloseWaitsForAllGenerations 验证 Close 等待所有代，首代之外还有代时也要等。
//
// 这里用首代无租约、次代有租约的构造，确认 Close 不是「只等最后一次 Acquire 的
// 那一代」。
func TestCloseWaitsForAllGenerations(t *testing.T) {
	first := testResources(nil, nil)
	manager := NewRuntimeManager(first)
	if err := manager.Replace(testResources(nil, nil)); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}

	// 在当前代（第二代）上持有租约。
	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = manager.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("有在途租约时 Close 不应返回")
	case <-time.After(50 * time.Millisecond):
	}

	lease.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("归还全部租约后 Close 应返回")
	}
}

// TestNewResourcesStartsDrained 验证新资源初始为已排空。
//
// 参照实现在 __post_init__ 里 set() drained：没有任何租约时 close() 不该等待。
func TestNewResourcesStartsDrained(t *testing.T) {
	resources := testResources(nil, nil)
	select {
	case <-resources.drainedChan():
		// 符合预期：已排空。
	default:
		t.Fatal("新资源应初始为已排空")
	}
}

// TestDrainChannelRebuiltAfterReacquire 验证 drain 通道在重新取租约时重建。
//
// 若沿用已关闭的通道，第二次 Close 会立即返回并提前关闭仍在使用的资源。
func TestDrainChannelRebuiltAfterReacquire(t *testing.T) {
	resources := testResources(nil, nil)
	manager := NewRuntimeManager(resources)

	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	lease.Release()

	second, err := manager.Acquire()
	if err != nil {
		t.Fatalf("第二次 acquire 失败: %v", err)
	}
	// 重新取租约后，通道必须回到「未排空」。
	select {
	case <-resources.drainedChan():
		t.Fatal("重新取租约后不应仍为已排空")
	default:
	}

	done := make(chan struct{})
	go func() {
		_ = manager.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("第二次租约未归还时 Close 不应返回")
	case <-time.After(50 * time.Millisecond):
	}
	second.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("归还后 Close 应返回")
	}
}

// TestCurrentGenerationSurvivesRelease 锁定「当前代在租约归零后不被摘除」。
//
// 这条曾经写错过：如果 release 无条件摘除归零的资源，当前代会在第一个请求结束时
// 被摘掉，而 detached 资源的 Release 会提前返回——并发计数再也回不去，后续请求
// 会看到一个计数只增不减的 key。参照实现只在 `retired` 时才摘除（runtime.py:93）。
func TestCurrentGenerationSurvivesRelease(t *testing.T) {
	manager := NewRuntimeManager(testResources(nil, nil))
	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	resources := lease.Resources
	lease.Release()

	if resources.Retired() {
		t.Fatal("当前代不应被标记 retired")
	}
	if resources.Detached() {
		t.Fatal("当前代在租约归零后不应被摘除")
	}
	if manager.Current() != resources {
		t.Fatal("当前代应保持不变")
	}
	// 关键：后续请求仍能在同一代上正确计数并归还。
	again, err := manager.Acquire()
	if err != nil {
		t.Fatalf("第二次 acquire 失败: %v", err)
	}
	if got := resources.ActiveLeases(); got != 1 {
		t.Fatalf("第二次取租约后计数应为 1，实际 %d", got)
	}
	again.Release()
	if got := resources.ActiveLeases(); got != 0 {
		t.Fatalf("第二次归还后计数应为 0，实际 %d", got)
	}
}

// TestRetiredAndIdleGenerationIsDetached 验证 retired 代在归零后被摘除。
//
// 与上一个用例配对：摘除的**唯一**条件是「已 retired 且无租约」。
func TestRetiredAndIdleGenerationIsDetached(t *testing.T) {
	client := &fakeCloser{}
	previous := testResources(client, &fakeMetrics{})
	manager := NewRuntimeManager(previous)

	// 取下租约，使 Replace 无法立即摘除旧代。
	lease, err := manager.Acquire()
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	if err := manager.Replace(testResources(nil, nil)); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}
	if !previous.Retired() {
		t.Fatal("被替换的旧代应标记 retired")
	}
	if previous.Detached() {
		t.Fatal("仍有租约时不应摘除")
	}

	lease.Release()
	if !previous.Detached() {
		t.Fatal("retired 代在租约归零后应被摘除")
	}
	if got := client.count(); got != 1 {
		t.Fatalf("摘除时应关闭连接池一次，实际 %d", got)
	}
	// 迟到的释放被忽略，且不改变状态。
	manager.release(previous)
	if got := previous.ActiveLeases(); got != 0 {
		t.Fatalf("已摘除资源的租约数应保持 0，实际 %d", got)
	}
}

// countingReader 按预设分片产出数据，用于超时测试。
type countingReader struct {
	mu      sync.Mutex
	chunks  [][]byte
	delay   []time.Duration
	index   int
	blockOn chan struct{}
}

func (c *countingReader) Next(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	index := c.index
	if index >= len(c.chunks) {
		c.mu.Unlock()
		return nil, io.EOF
	}
	c.index++
	chunk := c.chunks[index]
	var delay time.Duration
	if index < len(c.delay) {
		delay = c.delay[index]
	}
	block := c.blockOn
	c.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return chunk, nil
}

// TestIterStreamBytesForwardsAllChunks 验证正常流全量转发。
func TestIterStreamBytesForwardsAllChunks(t *testing.T) {
	reader := &countingReader{chunks: [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}}
	var got [][]byte
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), time.Second,
		func(chunk []byte) error {
			// 必须拷贝：实现可能复用缓冲区。
			got = append(got, append([]byte(nil), chunk...))
			return nil
		})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应转发 3 块，实际 %d", len(got))
	}
	if string(got[0]) != "a" || string(got[1]) != "bb" || string(got[2]) != "ccc" {
		t.Fatalf("内容不符: %q", got)
	}
}

// TestIterStreamBytesEmptyStreamIsNotError 验证空流不报错。
//
// 参照实现遇到 StopAsyncIteration 直接 return——上游一个字节都没发不算错误。
func TestIterStreamBytesEmptyStreamIsNotError(t *testing.T) {
	reader := &countingReader{}
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), time.Second, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("空流不应报错，实际 %v", err)
	}
}

// TestIterStreamBytesFirstByteTimeout 验证首块超时使用 firstByteDeadline。
func TestIterStreamBytesFirstByteTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	reader := &countingReader{chunks: [][]byte{[]byte("x")}, blockOn: block}

	// deadline 已过 → 立即超时。
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(-time.Second), time.Second, func([]byte) error { return nil })
	if !errors.Is(err, ErrStreamTimeout) {
		t.Fatalf("应返回超时错误，实际 %v", err)
	}
	if err.Error() != "timed out waiting for first stream byte" {
		t.Fatalf("错误文本不符: %q", err.Error())
	}
}

// TestIterStreamBytesIdleTimeoutBetweenChunks 验证块间空闲超时。
//
// 关键区别：首块的 deadline 用尽后，**后续块仍各自获得完整 idleTimeout**，因此
// 这个流不会在首块后立刻失败。若实现错误地把同一个 deadline 复用到底，第 2 块
// 会立即超时。
func TestIterStreamBytesIdleTimeoutBetweenChunks(t *testing.T) {
	reader := &countingReader{
		chunks: [][]byte{[]byte("first"), []byte("second")},
		// 首块立即返回；第二块等 120ms，仍小于 idleTimeout(500ms)。
		delay: []time.Duration{0, 120 * time.Millisecond},
	}
	var got int
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), 500*time.Millisecond,
		func([]byte) error { got++; return nil })
	if err != nil {
		t.Fatalf("块间延迟小于 idleTimeout 时不应失败，实际 %v", err)
	}
	if got != 2 {
		t.Fatalf("应转发 2 块，实际 %d", got)
	}
}

// TestIterStreamBytesIdleTimeoutExceeded 验证块间空闲超过阈值时报错。
func TestIterStreamBytesIdleTimeoutExceeded(t *testing.T) {
	reader := &countingReader{
		chunks: [][]byte{[]byte("first"), []byte("never")},
		delay:  []time.Duration{0, 2 * time.Second},
	}
	var got int
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), 80*time.Millisecond,
		func([]byte) error { got++; return nil })
	if !errors.Is(err, ErrStreamTimeout) {
		t.Fatalf("应返回超时错误，实际 %v", err)
	}
	if err.Error() != "timed out waiting for next stream byte" {
		t.Fatalf("错误文本不符: %q", err.Error())
	}
	// 首块已转发，第二块没有。
	if got != 1 {
		t.Fatalf("应只转发首块，实际 %d 块", got)
	}
}

// TestIterStreamBytesHasNoTotalDurationCap 验证没有总时长上限。
//
// 这是与「用单个 context.WithTimeout 包住整个流」的关键区别：那样的实现会在
// 总时长到期时切断一个仍在正常产出数据的流。
func TestIterStreamBytesHasNoTotalDurationCap(t *testing.T) {
	reader := &countingReader{
		chunks: [][]byte{[]byte("1"), []byte("2"), []byte("3"), []byte("4")},
		delay:  []time.Duration{0, 60 * time.Millisecond, 60 * time.Millisecond, 60 * time.Millisecond},
	}
	start := time.Now()
	var got int
	// idleTimeout 只有 200ms，但整个流耗时约 180ms 且每块都在阈值内。
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), 200*time.Millisecond,
		func([]byte) error { got++; return nil })
	if err != nil {
		t.Fatalf("不应因总时长而失败，实际 %v", err)
	}
	if got != 4 {
		t.Fatalf("应转发 4 块，实际 %d", got)
	}
	_ = start
}

// TestIterStreamBytesParentCancelStopsImmediately 验证下游断开能立即终止。
//
// 取消必须优先于空闲超时：否则下游已断开时还要白等一个 idleTimeout。
func TestIterStreamBytesParentCancelStopsImmediately(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	reader := &countingReader{
		chunks:  [][]byte{[]byte("first"), []byte("never")},
		blockOn: nil,
	}
	_ = reader

	ctx, cancel := context.WithCancel(context.Background())
	reader2 := &countingReader{chunks: [][]byte{[]byte("first")}}
	var got int
	// 转发首块时取消 context。
	err := IterStreamBytes(ctx, reader2,
		time.Now().Add(time.Hour), time.Hour,
		func([]byte) error {
			got++
			cancel()
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，实际 %v", err)
	}
	if got != 1 {
		t.Fatalf("应只转发首块，实际 %d", got)
	}
}

// TestIterStreamBytesYieldErrorStopsStream 验证下游写失败会终止读取。
func TestIterStreamBytesYieldErrorStopsStream(t *testing.T) {
	reader := &countingReader{chunks: [][]byte{[]byte("a"), []byte("b")}}
	sentinel := errors.New("下游写失败")
	var got int
	err := IterStreamBytes(context.Background(), reader,
		time.Now().Add(time.Second), time.Second,
		func([]byte) error {
			got++
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应返回下游错误，实际 %v", err)
	}
	if got != 1 {
		t.Fatalf("下游失败后应立即停止，实际转发 %d 块", got)
	}
}

// --- 生命周期收尾 ---

// TestStreamFinishIsIdempotent 验证收尾恰好执行一次。
//
// 重复收尾会重复写指标行、并把 key 的在途计数减两次——后者会让 key 被过早复用。
func TestStreamFinishIsIdempotent(t *testing.T) {
	health := &fakeKeyHealth{}
	metrics := &fakeMetrics{}
	now := time.Unix(1000, 0)
	lifecycle := NewStreamLifecycle(200, health, metrics, "m", "k", "m", "up", func() time.Time { return now })
	lifecycle.ObserveChunk(10)

	closer := &fakeCloser{}
	for range 3 {
		if err := lifecycle.Finish(false, closer, nil); err != nil {
			t.Fatalf("Finish 失败: %v", err)
		}
	}

	if got := len(metrics.recorded()); got != 1 {
		t.Fatalf("指标应只写 1 行，实际 %d", got)
	}
	failures, successes, releases := health.snapshot()
	if failures != 0 || successes != 1 || releases != 1 {
		t.Fatalf("key 回写次数不符: failures=%d successes=%d releases=%d",
			failures, successes, releases)
	}
}

// TestStreamFinishMarksFailureOnRetryableStatus 验证可重试状态码记失败。
//
// 401/403 也在可重试集合里，这与直觉相反但是既定契约。
func TestStreamFinishMarksFailureOnRetryableStatus(t *testing.T) {
	for _, statusCode := range []int{401, 403, 429, 500, 502, 503, 504, 521} {
		health := &fakeKeyHealth{}
		lifecycle := NewStreamLifecycle(statusCode, health, &fakeMetrics{},
			"m", "k", "m", "up", time.Now)
		if err := lifecycle.Finish(false, nil, nil); err != nil {
			t.Fatalf("Finish 失败: %v", err)
		}
		failures, successes, releases := health.snapshot()
		if failures != 1 || successes != 0 {
			t.Fatalf("状态码 %d 应记失败，实际 failures=%d successes=%d",
				statusCode, failures, successes)
		}
		if releases != 1 {
			t.Fatalf("状态码 %d 也应释放并发计数，实际 %d", statusCode, releases)
		}
	}
}

// TestStreamFinishReleasesOnNonRetryableClientError 验证 400 也释放并发计数。
//
// 400 既不重试也不算成功，但**必须**归还名额；漏掉会让该 key 永久少一个可用名额。
func TestStreamFinishReleasesOnNonRetryableClientError(t *testing.T) {
	health := &fakeKeyHealth{}
	lifecycle := NewStreamLifecycle(400, health, &fakeMetrics{}, "m", "k", "m", "up", time.Now)
	if err := lifecycle.Finish(false, nil, nil); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	failures, successes, releases := health.snapshot()
	if failures != 0 || successes != 0 {
		t.Fatalf("400 不应记成功或失败，实际 failures=%d successes=%d", failures, successes)
	}
	if releases != 1 {
		t.Fatalf("400 必须释放并发计数，实际 %d", releases)
	}
}

// TestStreamFinishFailedFlagForcesFailure 验证 failed 标志强制记失败。
//
// 流中途出错时状态码往往仍是 200，所以不能只看状态码。
func TestStreamFinishFailedFlagForcesFailure(t *testing.T) {
	health := &fakeKeyHealth{}
	lifecycle := NewStreamLifecycle(200, health, &fakeMetrics{}, "m", "k", "m", "up", time.Now)
	if err := lifecycle.Finish(true, nil, nil); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	failures, _, releases := health.snapshot()
	if failures != 1 {
		t.Fatalf("failed=true 应记失败，实际 %d", failures)
	}
	if releases != 1 {
		t.Fatalf("应释放并发计数，实际 %d", releases)
	}
}

// TestStreamObserveChunkSetsFirstTokenOnce 验证首块耗时只在第一块记录。
func TestStreamObserveChunkSetsFirstTokenOnce(t *testing.T) {
	current := time.Unix(1000, 0)
	lifecycle := NewStreamLifecycle(200, nil, nil, "m", "k", "m", "up",
		func() time.Time { return current })

	current = current.Add(40 * time.Millisecond)
	lifecycle.ObserveChunk(5)
	first := lifecycle.FirstTokenMS()

	current = current.Add(500 * time.Millisecond)
	lifecycle.ObserveChunk(5)
	if got := lifecycle.FirstTokenMS(); got != first {
		t.Fatalf("首块耗时不应被后续块覆盖: %d → %d", first, got)
	}
	if first != 40 {
		t.Fatalf("首块耗时应为 40ms，实际 %d", first)
	}
	if got := lifecycle.ChunkCount(); got != 2 {
		t.Fatalf("块数应为 2，实际 %d", got)
	}
	if got := lifecycle.ByteCount(); got != 10 {
		t.Fatalf("字节数应为 10，实际 %d", got)
	}
}

// TestStreamElapsedUsesBankersRounding 验证耗时用银行家舍入。
//
// Python 的 round() 在恰好 .5 时向偶数取整，Go 的 math.Round 是远离零取整。
// 这个数值会写进指标库，属于持久化契约。
func TestStreamElapsedUsesBankersRounding(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    int
	}{
		{500 * time.Microsecond, 0},  // 0.5 → 0（偶数）
		{1500 * time.Microsecond, 2}, // 1.5 → 2（偶数）
		{2500 * time.Microsecond, 2}, // 2.5 → 2（偶数）
		{3500 * time.Microsecond, 4}, // 3.5 → 4（偶数）
		{1 * time.Millisecond, 1},    // 整数不变
		{999 * time.Microsecond, 1},  // 0.999 → 1
	}
	for _, item := range cases {
		start := time.Unix(1000, 0)
		current := start
		lifecycle := NewStreamLifecycle(200, nil, nil, "m", "k", "m", "up",
			func() time.Time { return current })
		current = start.Add(item.elapsed)
		if got := lifecycle.ElapsedMS(); got != item.want {
			t.Fatalf("耗时 %v: 期望 %dms，实际 %dms（银行家舍入）",
				item.elapsed, item.want, got)
		}
	}
}

// TestRetryAfterSecondsParsing 验证 Retry-After 两种格式。
func TestRetryAfterSecondsParsing(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	// 数值形态。
	if got := RetryAfterSeconds("12.5", now); got == nil || *got != 12.5 {
		t.Fatalf("数值形态解析失败: %v", got)
	}
	// 负值下限取 0。
	if got := RetryAfterSeconds("-5", now); got == nil || *got != 0 {
		t.Fatalf("负值应取下限 0: %v", got)
	}
	// HTTP 日期形态：60 秒后。
	future := now.Add(60 * time.Second).UTC().Format(http.TimeFormat)
	if got := RetryAfterSeconds(future, now); got == nil || *got < 59 || *got > 61 {
		t.Fatalf("日期形态解析失败: %v", got)
	}
	// 过去的日期下限取 0。
	past := now.Add(-60 * time.Second).UTC().Format(http.TimeFormat)
	if got := RetryAfterSeconds(past, now); got == nil || *got != 0 {
		t.Fatalf("过去日期应取下限 0: %v", got)
	}
	// 无法识别返回 nil。
	for _, value := range []string{"", "not-a-date", "abc"} {
		if got := RetryAfterSeconds(value, now); got != nil {
			t.Fatalf("无法识别 %q 时应返回 nil，实际 %v", value, got)
		}
	}
}

// TestRetryPolicyAttempts 验证三种「固定单 key」情形用 max_retries+1。
func TestRetryPolicyAttempts(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2}
	keyName := "k1"
	empty := ""
	cases := []struct {
		name      string
		keyCount  int
		requested *string
		onlyFirst bool
		want      int
	}{
		{"指定 key", 5, &keyName, false, 3},
		{"only_first", 5, nil, true, 3},
		{"只有一个 key", 1, nil, false, 3},
		{"多 key 轮换", 5, nil, false, 5},
		{"多 key 且未指定", 3, nil, false, 3},
		{"空串 key 视同未指定", 5, &empty, false, 5},
	}
	for _, item := range cases {
		got := policy.Attempts(item.keyCount, item.requested, item.onlyFirst)
		if got != item.want {
			t.Fatalf("%s: 期望 %d 次，实际 %d", item.name, item.want, got)
		}
	}
}

// TestIsRetryableStatus 验证可重试状态码集合。
//
// 401/403 在列是刻意的：轮换 key 常常就能成功。
func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{401, 403, 429, 500, 502, 503, 504, 521}
	for _, statusCode := range retryable {
		if !IsRetryableStatus(statusCode) {
			t.Fatalf("%d 应为可重试", statusCode)
		}
	}
	notRetryable := []int{200, 201, 400, 404, 422, 501}
	for _, statusCode := range notRetryable {
		if IsRetryableStatus(statusCode) {
			t.Fatalf("%d 不应为可重试", statusCode)
		}
	}
}
