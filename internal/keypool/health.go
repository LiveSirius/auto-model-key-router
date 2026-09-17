// Package keypool 实现可用 key 的选择、冷却与原生端点能力缓存。
package keypool

import "math"

// MaxCooldownSeconds 是冷却时长上限（对齐 key_health.py:9）。
const MaxCooldownSeconds = 300.0

// KeyHealth 是单个 (模型, key) 的健康状态。
type KeyHealth struct {
	Failures      int
	CooldownUntil float64
}

// KeyHealthStore 记录失败计数与冷却截止时间。
//
// 参照实现用注入的 clock 以便测试；这里保持同样做法，因为冷却相关的行为全部
// 依赖「当前时间」，不注入就无法确定性断言。
type KeyHealthStore struct {
	clock  func() float64
	states map[[2]string]*KeyHealth
}

// NewKeyHealthStore 构造健康状态表。clock 为 nil 时使用真实时钟。
func NewKeyHealthStore(clock func() float64) *KeyHealthStore {
	if clock == nil {
		clock = RealClock
	}
	return &KeyHealthStore{clock: clock, states: map[[2]string]*KeyHealth{}}
}

// IsCoolingDown 报告该 key 是否仍在冷却中。
//
// 注意用「取值」而非「取或建」：查询不该产生状态条目（Python 侧用 dict.get）。
func (s *KeyHealthStore) IsCoolingDown(modelID, keyName string) bool {
	state, found := s.states[[2]string{modelID, keyName}]
	return found && state.CooldownUntil > s.clock()
}

// MarkSuccess 清除健康状态。
//
// 参照实现的判断是「状态存在且失败计数与冷却时间都为零时直接返回」，因此一个
// 全零状态会**留在表里**。行为上等价（查询仍返回不冷却），这里保持同样的删除
// 条件以免维护期出现无意义的状态差异。
func (s *KeyHealthStore) MarkSuccess(modelID, keyName string) {
	key := [2]string{modelID, keyName}
	state, found := s.states[key]
	if !found || (state.Failures == 0 && state.CooldownUntil == 0) {
		return
	}
	delete(s.states, key)
}

// MarkFailure 记录一次失败，必要时进入冷却。
//
// 规则（对齐 key_health.py:36）：
//   - 429 立即冷却，不看阈值；
//   - 其他状态码要累计到 failure_threshold 才开始冷却；
//   - 有 retry_after 时直接用它，否则用 cooldown_seconds × 失败次数；
//   - 两者都截断到 MaxCooldownSeconds，且取下限 0；
//   - 冷却截止时间只延后不提前（取 max）。
func (s *KeyHealthStore) MarkFailure(
	modelID, keyName string,
	statusCode *int,
	retryAfter *float64,
	failureThreshold int,
	cooldownSeconds float64,
) {
	key := [2]string{modelID, keyName}
	state, found := s.states[key]
	if !found {
		state = &KeyHealth{}
		s.states[key] = state
	}
	state.Failures++
	// statusCode 为 nil 时「不是 429」，因此走阈值判断（与 Python 的 None != 429 一致）。
	if (statusCode == nil || *statusCode != 429) && state.Failures < failureThreshold {
		return
	}
	var cooldown float64
	if retryAfter != nil {
		cooldown = math.Min(MaxCooldownSeconds, math.Max(0, *retryAfter))
	} else {
		cooldown = math.Min(
			MaxCooldownSeconds,
			math.Max(0, cooldownSeconds)*math.Max(1, float64(state.Failures)),
		)
	}
	if cooldown <= 0 {
		return
	}
	state.CooldownUntil = math.Max(state.CooldownUntil, s.clock()+cooldown)
}

// Reset 清空全部状态（配置热重载时使用）。
func (s *KeyHealthStore) Reset() { s.states = map[[2]string]*KeyHealth{} }

// RealClock 返回秒级 Unix 时间（对齐 Python 的 time()，含小数）。
func RealClock() float64 {
	return float64(nowUnixNano()) / 1e9
}
