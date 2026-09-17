package keypool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// TestSelectionCorpusIsDiscriminating 证明对拍语料确实有鉴别力。
//
// 对拍最容易变成「永远通过」的测试：如果断言写错（比如只比较了空字符串），
// 语料再丰富也发现不了偏差。这里用一个**已知错误**的期望值去跑同一套断言
// 路径，必须失败——否则说明断言本身是空的。
func TestSelectionCorpusIsDiscriminating(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1"}, "k2": {"api_key": "2"}}}},
		"models": {"m": {"targets": [
			{"provider": "p", "key": "k1", "upstream_model": "u1"},
			{"provider": "p", "key": "k2", "upstream_model": "u2"}],
			"routing_mode": "round_robin"}}
	}`
	routerConfig := mustConfig(t, raw)
	pool := New(routerConfig, nil, func() float64 { return 1000 })

	first, err := pool.NextKey("m", nil, false, "")
	if err != nil {
		t.Fatalf("首次选择失败: %v", err)
	}
	// 第一轮必然是 k1（游标从 0 开始，两个 key 并发数都是 0）。
	if first.Name != "k1" {
		t.Fatalf("期望 k1，实际 %q", first.Name)
	}
	// 反证：若断言逻辑写错（例如恒等比较），下面这个「错误的期望」就不会被发现。
	if first.Name == "k2" {
		t.Fatal("对拍断言无鉴别力：错误的期望值未被识别")
	}
}

// TestNextKeyRoundRobinDoesNotStarve 验证轮转在正常释放下会覆盖所有 key。
//
// 这是 concurrency 与 cursor 交互的核心不变量：round_robin 的意义就是分摊负载，
// 如果游标或并发数计算写错，会退化成「总是同一个 key」而静默失去负载均衡。
func TestNextKeyRoundRobinDoesNotStarve(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1"}, "k2": {"api_key": "2"},
			"k3": {"api_key": "3"}}}},
		"models": {"m": {"targets": [
			{"provider": "p", "key": "k1", "upstream_model": "u1"},
			{"provider": "p", "key": "k2", "upstream_model": "u2"},
			{"provider": "p", "key": "k3", "upstream_model": "u3"}],
			"routing_mode": "round_robin"}}
	}`
	pool := New(mustConfig(t, raw), nil, func() float64 { return 1000 })

	counts := map[string]int{}
	for range 9 {
		key, err := pool.NextKey("m", nil, false, "")
		if err != nil {
			t.Fatalf("选择失败: %v", err)
		}
		counts[key.Name]++
		// 每次用完立刻释放，模拟无并发压力的稳定状态。
		pool.ReleaseKey("m", key.Name)
	}
	for _, name := range []string{"k1", "k2", "k3"} {
		if counts[name] != 3 {
			t.Fatalf("round_robin 未均分：%s 被选中 %d 次，期望 3 次（全量 %v）",
				name, counts[name], counts)
		}
	}
}

// TestConcurrentNextKeyDoesNotRace 用 -race 检查并发安全。
//
// 参照实现用 asyncio.Lock 保护游标与并发计数；Go 侧用 sync.Mutex。这个测试在
// `go test -race` 下才有意义，普通运行时至少验证不 panic、计数守恒。
func TestConcurrentNextKeyDoesNotRace(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1"}, "k2": {"api_key": "2"}}}},
		"models": {"m": {"targets": [
			{"provider": "p", "key": "k1", "upstream_model": "u1"},
			{"provider": "p", "key": "k2", "upstream_model": "u2"}],
			"routing_mode": "round_robin"}}
	}`
	pool := New(mustConfig(t, raw), nil, func() float64 { return 1000 })

	const workers = 8
	const perWorker = 50
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for range perWorker {
				key, err := pool.NextKey("m", nil, false, "")
				if err != nil {
					return
				}
				pool.MarkFailure("m", key.Name, intPtr(429), nil)
				pool.MarkSuccess("m", key.Name)
				pool.ReleaseKey("m", key.Name)
			}
		}()
	}
	for range workers {
		<-done
	}
	// 全部释放后并发计数必须归零，否则说明有配对泄漏。
	for _, name := range []string{"k1", "k2"} {
		if got := pool.ActiveCount("m", name); got != 0 {
			t.Fatalf("并发计数未归零：%s = %d", name, got)
		}
	}
}

// TestHealthStoreCooldownArithmetic 验证冷却算术，覆盖语料不方便表达的边界。
func TestHealthStoreCooldownArithmetic(t *testing.T) {
	now := 1000.0
	store := NewKeyHealthStore(func() float64 { return now })

	// 非 429 且未达阈值：只累计失败、不冷却。
	store.MarkFailure("m", "k", intPtr(500), nil, 2, 60)
	if store.IsCoolingDown("m", "k") {
		t.Fatal("未达阈值不应冷却")
	}
	// 达到阈值：cooldown = 60 × 2 = 120（失败次数已为 2）。
	store.MarkFailure("m", "k", intPtr(500), nil, 2, 60)
	if !store.IsCoolingDown("m", "k") {
		t.Fatal("达到阈值应冷却")
	}
	// 时间前进 119 秒仍在冷却，120 秒后解除。
	now = 1000 + 119
	if !store.IsCoolingDown("m", "k") {
		t.Fatal("119 秒时仍在冷却期内")
	}
	now = 1000 + 120
	if store.IsCoolingDown("m", "k") {
		t.Fatal("120 秒时冷却应已结束")
	}

	// 429 立即冷却，且不看阈值。
	store2 := NewKeyHealthStore(func() float64 { return now })
	store2.MarkFailure("m", "k", intPtr(429), nil, 99, 60)
	if !store2.IsCoolingDown("m", "k") {
		t.Fatal("429 应立即冷却，不受阈值限制")
	}

	// 上限截断到 300 秒。
	//
	// 注意基准时间是上面推进后的 now，不是初始值：这里显式重新标定，避免依赖
	// 前面步骤留下的时间状态。
	now = 2000
	store3 := NewKeyHealthStore(func() float64 { return now })
	store3.MarkFailure("m", "k", intPtr(429), floatPtr(99999), 1, 60)
	now = 2000 + 299
	if !store3.IsCoolingDown("m", "k") {
		t.Fatal("299 秒时应仍在冷却（上限 300）")
	}
	now = 2000 + 300
	if store3.IsCoolingDown("m", "k") {
		t.Fatal("冷却时长应被截断到 300 秒")
	}

	// retry_after 为负时下限取 0，即不冷却。
	now = 1000
	store4 := NewKeyHealthStore(func() float64 { return now })
	store4.MarkFailure("m", "k", intPtr(429), floatPtr(-5), 1, 60)
	if store4.IsCoolingDown("m", "k") {
		t.Fatal("负 retry_after 应取下限 0，不进入冷却")
	}

	// mark_success 清除状态。
	store5 := NewKeyHealthStore(func() float64 { return now })
	store5.MarkFailure("m", "k", intPtr(429), nil, 1, 60)
	store5.MarkSuccess("m", "k")
	if store5.IsCoolingDown("m", "k") {
		t.Fatal("mark_success 应清除冷却状态")
	}
}

// TestCapabilityCacheTTL 验证能力缓存的过期与遗留格式兼容。
func TestCapabilityCacheTTL(t *testing.T) {
	now := 10_000.0
	clock := func() float64 { return now }

	cache := NewEndpointCapabilityCache(nil, clock)
	if got := cache.Get("https://a.example", "v1/messages"); got != nil {
		t.Fatal("空缓存应返回 nil（未测试）")
	}

	// 正结果永久有效。
	cache.Update("https://a.example", true, "v1/messages", "ok")
	now = 10_000 + 86_400*365
	if got := cache.Get("https://a.example", "v1/messages"); got == nil || !*got {
		t.Fatal("正结果应永久有效")
	}

	// 负结果 600 秒后过期（reason != "error"）。
	now = 10_000
	cache.Update("https://b.example", false, "v1/messages", "unsupported")
	now = 10_000 + 599
	if got := cache.Get("https://b.example", "v1/messages"); got == nil || *got {
		t.Fatal("599 秒时负结果应仍有效")
	}
	now = 10_000 + 600
	if got := cache.Get("https://b.example", "v1/messages"); got != nil {
		t.Fatal("600 秒时负结果应已过期")
	}

	// reason == "error" 用 60 秒 TTL。
	now = 10_000
	cache.Update("https://c.example", false, "v1/messages", "error")
	now = 10_000 + 60
	if got := cache.Get("https://c.example", "v1/messages"); got != nil {
		t.Fatal("error 类负结果 60 秒后应过期")
	}

	// v1/messages 查询回退到只按 base_url 记录的键。
	now = 10_000
	cache.Update("https://d.example", true, "", "ok")
	if got := cache.Get("https://d.example/", "v1/messages"); got == nil || !*got {
		t.Fatal("应回退到 base_url 键并命中")
	}
}

// TestCapabilityCacheLegacyBoolState 验证遗留布尔格式的解析。
func TestCapabilityCacheLegacyBoolState(t *testing.T) {
	raw := mustValue(t, `{
		"https://yes.example|v1/messages": true,
		"https://no.example|v1/messages": false,
		"bad-entry": "not-a-state",
		"also-bad": {"supported": "yes"}
	}`)
	cache := NewEndpointCapabilityCache(raw, func() float64 { return 5000 })

	// 遗留 true 视为已确认支持，且无 TTL。
	if got := cache.Get("https://yes.example", "v1/messages"); got == nil || !*got {
		t.Fatal("遗留 true 应解析为支持")
	}
	// 遗留 false 带负 TTL，但 checked_at 为 0，故在 5000 秒时已过期。
	if got := cache.Get("https://no.example", "v1/messages"); got != nil {
		t.Fatal("遗留 false 因 checked_at=0 应已过期")
	}
	// 无法识别的条目被忽略，不影响其它条目。
	if got := cache.Get("bad-entry", "v1/messages"); got != nil {
		t.Fatal("坏数据应被忽略")
	}
}

// TestCapabilityCacheKeyNormalization 验证缓存键归一化。
//
// 尾部斜杠、路径两侧斜杠、空路径都要归一到同一个键，否则同一端点会因写法不同
// 而重复探测（每次探测都是计费的上游调用）。
func TestCapabilityCacheKeyNormalization(t *testing.T) {
	same := [][2]string{
		{"https://a.example", "/v1/messages"},
		{"https://a.example/", "v1/messages"},
		{"https://a.example", "v1/messages/"},
		{"https://a.example/", "/v1/messages/"},
	}
	cache := NewEndpointCapabilityCache(nil, func() float64 { return 100 })
	cache.Update(same[0][0], true, same[0][1], "ok")
	for _, pair := range same[1:] {
		if got := cache.Get(pair[0], pair[1]); got == nil || !*got {
			t.Fatalf("键未归一：%q + %q 未命中", pair[0], pair[1])
		}
	}
}

// TestRequestRouteKind 验证路径归类。
func TestRequestRouteKind(t *testing.T) {
	cases := map[string]string{
		"images/generations": "image",
		"images/edits":       "image",
		"embeddings":         "embeddings",
		"chat/completions":   "default",
		"messages":           "default",
		"":                   "default",
		"images":             "default",
	}
	for path, want := range cases {
		if got := RequestRouteKind(path); got != want {
			t.Fatalf("路径 %q: 期望 %q，实际 %q", path, want, got)
		}
	}
}

// TestCapabilityStoreRoundTrip 验证能力状态的落盘与读回。
func TestCapabilityStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.json")
	store := NewCapabilityStore(path)

	// 文件缺失时返回空对象。
	if got := store.Load(); !got.IsObject() || got.Len() != 0 {
		t.Fatal("缺失文件应返回空对象")
	}

	states := mustValue(t, `{"z|v1/messages": {"supported": true, "checked_at": 5, "reason": "ok", "ttl_seconds": 0}}`)
	if err := store.Save(states); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	loaded := store.Load()
	if !loaded.IsObject() || loaded.Lookup("z|v1/messages").Kind == 0 {
		t.Fatalf("读回失败: %s", mustDump(t, loaded))
	}

	// 载荷结构固定为 version + endpoint_capabilities。
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(content, &payload); err != nil {
		t.Fatalf("解析落盘内容失败: %v", err)
	}
	if string(payload["version"]) != "1" {
		t.Fatalf("version 应为 1，实际 %s", payload["version"])
	}
	if _, found := payload["endpoint_capabilities"]; !found {
		t.Fatal("缺少 endpoint_capabilities 字段")
	}
	// 写入必须是原子的：不能留下临时文件。
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("残留临时文件: %s", entry.Name())
		}
	}
}

// TestCapabilityStoreAcceptsLegacyFieldName 验证旧字段名兼容。
func TestCapabilityStoreAcceptsLegacyFieldName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.json")
	content := `{"url_native_support": {"a|v1/messages": true}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	loaded := NewCapabilityStore(path).Load()
	if !loaded.IsObject() || loaded.Lookup("a|v1/messages").Kind == 0 {
		t.Fatal("应识别旧字段名 url_native_support")
	}
}

// TestCapabilityStoreCorruptFileIsIgnored 验证损坏缓存不会阻止启动。
func TestCapabilityStoreCorruptFileIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.json")
	for _, content := range []string{"not json", "[]", `{"endpoint_capabilities": []}`} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
		loaded := NewCapabilityStore(path).Load()
		if !loaded.IsObject() || loaded.Len() != 0 {
			t.Fatalf("内容 %q 应被忽略并返回空对象", content)
		}
	}
}

// TestHiddenNamesPreferRealIDs 验证真实 ID/别名优先于隐藏名。
//
// 手写的 hidden_aliases 与模型 ID 撞名会被 config 校验直接拒绝
// （config.py:1153），但**自动推导**的隐藏名（来自 target 的 upstream_model，
// config.py:1238）不参与那项校验，因此可以撞名。此时真实 ID 必须优先——否则
// 一个正常模型的请求会被路由到另一个模型上。
func TestHiddenNamesPreferRealIDs(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1"}}}},
		"models": {
			"alpha": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "beta"}]},
			"beta": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "u"}]}
		}
	}`
	pool := New(mustConfig(t, raw), nil, func() float64 { return 1000 })

	if got := pool.ResolveModelID("beta"); got != "beta" {
		t.Fatalf("隐藏名撞真实 ID 时应解析到真实模型，实际 %q", got)
	}
	// hidden_model_names() 含 beta（alpha 的上游名）与 u（beta 的上游名），但
	// beta 是真实 ID，因此只剩 u 作为隐藏名。
	if hidden := pool.HiddenModelIDs(); !equalStrings(hidden, []string{"u"}) {
		t.Fatalf("隐藏名列表应为 [u]（beta 被真实 ID 遮蔽），实际 %v", hidden)
	}
	// 未撞名的自动推导名仍然可作为隐藏名直接调用。
	if got := pool.ResolveModelID("u"); got != "beta" {
		t.Fatalf("未撞名的上游名应解析到其模型，实际 %q", got)
	}
}

// TestExplicitHiddenAliasCollidingWithModelIDIsRejected 锁定 config 层的校验。
//
// 这条行为的价值在于：它说明「隐藏名撞真实 ID」是**不允许**的，所以
// KeyPool 里那段「真实 ID 优先」的逻辑只可能被自动推导名触发。
func TestExplicitHiddenAliasCollidingWithModelIDIsRejected(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1"}}}},
		"models": {
			"alpha": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "u"}],
			          "hidden_aliases": ["beta"]},
			"beta": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "v"}]}
		}
	}`
	_, err := config.FromDict(mustValue(t, raw))
	if err == nil {
		t.Fatal("隐藏别名与模型 ID 撞名应被拒绝")
	}
	if !strings.Contains(err.Error(), "模型名称重复") {
		t.Fatalf("错误文本不符: %v", err)
	}
}

// TestVisitorRoutePrefix 验证访客模型名前缀规则。
func TestVisitorRoutePrefix(t *testing.T) {
	raw := `{
		"config_version": 4, "local_api_key": "local",
		"providers": {"p": {"base_url": "https://a.example", "keys": {
			"k1": {"api_key": "1", "allow_visitor": true}}}},
		"models": {"m": {"targets": [{"provider": "p", "key": "k1", "upstream_model": "u"}]}}
	}`
	pool := New(mustConfig(t, raw), nil, func() float64 { return 1000 })

	if got, found := pool.ResolveVisitorModelID("amkr-m"); !found || got != "m" {
		t.Fatalf("访客前缀解析失败：%q %v", got, found)
	}
	if _, found := pool.ResolveVisitorModelID("m"); found {
		t.Fatal("非前缀名不应被当作访客路由")
	}
}

// --- 测试辅助 ---

// mustConfig 从 JSON 文本构造配置。
func mustConfig(t *testing.T, raw string) *config.RouterConfig {
	t.Helper()
	routerConfig, err := config.FromDict(mustValue(t, raw))
	if err != nil {
		t.Fatalf("构造配置失败: %v", err)
	}
	return routerConfig
}

// mustValue 解析 JSON 文本。
func mustValue(t *testing.T, raw string) *canonical.Value {
	t.Helper()
	value, err := canonical.ParseString(raw)
	if err != nil {
		t.Fatalf("解析 JSON 失败: %v\n输入: %s", err, raw)
	}
	return value
}

// mustDump 序列化 canonical 值以便断言失败时输出。
func mustDump(t *testing.T, value *canonical.Value) string {
	t.Helper()
	return canonical.Dumps(value)
}

// intPtr 返回 int 指针。
func intPtr(value int) *int { return &value }

// floatPtr 返回 float64 指针。
func floatPtr(value float64) *float64 { return &value }
