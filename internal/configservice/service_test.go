package configservice

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// writeBaseline 写一份合法的 v4 基线配置，local_api_key 固定为 old-key。
//
// 用 config.EmptyConfigDict() 而不是手写字段：参照实现的
// RouterConfig.load 会拒绝 config_version 不是 4 的文件（实测：手工写一份缺
// config_version 的最小配置会让 load 抛 "配置文件必须是 config_version 4"）。
func writeBaseline(t *testing.T, path string) {
	t.Helper()
	data, err := config.EmptyConfigDict()
	if err != nil {
		t.Fatalf("构造空配置失败: %v", err)
	}
	data.Obj.Set("local_api_key", canonical.NewString("old-key"))
	if err := config.SaveConfigData(path, data); err != nil {
		t.Fatalf("写基线失败: %v", err)
	}
}

// newData 构造一份 local_api_key 为 key 的完整配置。
func newData(t *testing.T, key string) *canonical.Value {
	t.Helper()
	data, err := config.EmptyConfigDict()
	if err != nil {
		t.Fatalf("构造空配置失败: %v", err)
	}
	data.Obj.Set("local_api_key", canonical.NewString(key))
	return data
}

// readKey 读回磁盘配置里的 local_api_key。
func readKey(t *testing.T, path string) string {
	t.Helper()
	data, err := config.LoadConfigData(path)
	if err != nil {
		t.Fatalf("读回配置失败: %v", err)
	}
	return data.Lookup("local_api_key").PyStr()
}

// TestCommitPersistsAndReportsChange 锁定 commit 的落盘与返回内容。
//
// 期望值来自对参照实现的实测：commit 之后磁盘上是新数据；old_config 是变更**前**
// 的配置（oldConfig 为 nil 时从磁盘读）；change.data 与磁盘内容相等但**不是**同一个
// 对象（config_service.py:44 的 deepcopy）。
func TestCommitPersistsAndReportsChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)

	change, err := New(path).Commit(newData(t, "new-key"), nil)
	if err != nil {
		t.Fatalf("commit 失败: %v", err)
	}
	if got := readKey(t, path); got != "new-key" {
		t.Fatalf("磁盘 local_api_key = %q，期望 new-key", got)
	}
	if change.OldConfig == nil || change.OldConfig.LocalAPIKey != "old-key" {
		t.Errorf("old_config 应为变更前的 old-key，实际 %+v", change.OldConfig)
	}
	if change.NewConfig == nil || change.NewConfig.LocalAPIKey != "new-key" {
		t.Errorf("new_config 应为 new-key，实际 %+v", change.NewConfig)
	}
	if change.Path != path {
		t.Errorf("path 应为 %q，实际 %q", path, change.Path)
	}
	// Data 与磁盘内容相等。
	onDisk, err := config.LoadConfigData(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if canonical.DumpsOrdered(change.Data) != canonical.DumpsOrdered(onDisk) {
		t.Error("change.data 应与磁盘内容一致")
	}
	// 但是深拷贝：改动 Data 不应影响磁盘。
	change.Data.Obj.Set("local_api_key", canonical.NewString("tampered"))
	if got := readKey(t, path); got != "new-key" {
		t.Errorf("改动 change.data 影响了磁盘（不是深拷贝）: %q", got)
	}
}

// TestCommitUsesProvidedOldConfig 验证显式传入 oldConfig 时不再从磁盘读取。
//
// 判据：故意传一个与磁盘**不同**的 oldConfig，返回值必须是传入的那个。若实现忽略
// 入参去读磁盘，这里会失败。
func TestCommitUsesProvidedOldConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)
	provided, err := config.FromDict(newData(t, "explicit-old"))
	if err != nil {
		t.Fatalf("构造 provided 失败: %v", err)
	}
	change, err := New(path).Commit(newData(t, "new-key"), provided)
	if err != nil {
		t.Fatalf("commit 失败: %v", err)
	}
	if change.OldConfig.LocalAPIKey != "explicit-old" {
		t.Fatalf("应使用传入的 oldConfig，实际 %q", change.OldConfig.LocalAPIKey)
	}
}

// TestUpdateAppliesMutationAndPersists 锁定 update 的读改写。
//
// 同时是**死锁回归测试**：参照实现用可重入的 RLock，update 持锁后调 commit 再取同
// 一把锁；Go 的 sync.Mutex 不可重入，若把 update 写成「持锁后调公开的 Commit」，
// 本测试会永久挂起（而不是失败）。因此这里显式设了超时意识——挂起即回归。
func TestUpdateAppliesMutationAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)

	change, err := New(path).Update(func(data *canonical.Value) error {
		data.Obj.Set("local_api_key", canonical.NewString("mutated"))
		return nil
	})
	if err != nil {
		t.Fatalf("update 失败: %v", err)
	}
	if got := readKey(t, path); got != "mutated" {
		t.Fatalf("磁盘 local_api_key = %q，期望 mutated", got)
	}
	if change.NewConfig.LocalAPIKey != "mutated" {
		t.Errorf("new_config 应为 mutated，实际 %q", change.NewConfig.LocalAPIKey)
	}
	// update 里 old_config 来自读取磁盘（参照实现 config_service.py:52），
	// 因此是变更前的 old-key。
	if change.OldConfig.LocalAPIKey != "old-key" {
		t.Errorf("old_config 应为 old-key，实际 %q", change.OldConfig.LocalAPIKey)
	}
}

// TestUpdateMutationErrorDoesNotPersist 验证 mutation 报错时磁盘不动。
//
// 期望来自实测：mutation 抛错时文件字节完全不变，已落盘的值保持原样。
func TestUpdateMutationErrorDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}

	sentinel := errForTest("boom")
	_, err = New(path).Update(func(data *canonical.Value) error {
		// 先改再报错：确认"改了"也不会被写下去。
		data.Obj.Set("local_api_key", canonical.NewString("SHOULD-NOT-PERSIST"))
		return sentinel
	})
	if err == nil {
		t.Fatal("mutation 报错时 update 应返回错误")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("mutation 报错时不应改动配置文件")
	}
	if got := readKey(t, path); got != "old-key" {
		t.Fatalf("磁盘应保持 old-key，实际 %q", got)
	}
}

// TestCommitValidationFailureDoesNotPersist 验证解析失败时磁盘不动。
//
// 这是「先校验再落盘」这一顺序的回归测试。期望来自实测：commit 的
// {"models": []} 让 RouterConfig.from_dict 抛
// "config_version 4 的 models 必须是对象"，且文件字节完全不变。若实现把
// save 放到 from_dict 之前，磁盘会被写坏。
//
// 注意 providers 不是这种情形：实测 providers 为列表/字符串时 from_dict **不**报错，
// 配置会被照常写入。因此这里用 models 作为判据，不要误以为 providers 也受保护。
func TestCommitValidationFailureDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}

	bad := newData(t, "bad")
	bad.Obj.Set("models", canonical.NewArray())

	if _, err := New(path).Commit(bad, nil); err == nil {
		t.Fatal("models 非对象时 commit 应返回错误")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("校验失败时不应改动配置文件")
	}
}

// TestCommitHighConfigVersionRejected 对照实测的高版本拒绝文案。
func TestCommitHighConfigVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)
	future := newData(t, "k")
	future.Obj.Set("config_version", canonical.NewIntValue(99))
	_, err := New(path).Commit(future, nil)
	if err == nil {
		t.Fatal("config_version 99 应被拒绝")
	}
	if got := err.Error(); got != "配置文件版本 99 高于当前支持的 4，请升级软件" {
		t.Fatalf("错误文案不符: %q", got)
	}
}

// TestLocksSharedPerPath 锁定「同一路径共享一把锁」。
//
// 这是跨实例的正确性前提：管理 API 与 CLI 各建一个 ConfigService 时，若锁不共享，
// 并发提交会互相覆盖，前一次变更静默丢失（config_service.py:20-25 用模块级
// _CONFIG_LOCKS 达到同样效果）。
func TestLocksSharedPerPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	first, second := New(path), New(path)
	if first.mu != second.mu {
		t.Fatal("同一路径的两个 ConfigService 必须共享同一把锁")
	}
	// 不同路径应各自独立（否则所有配置文件的提交会被串行化）。
	other := New(filepath.Join(t.TempDir(), "other.json"))
	if other.mu == first.mu {
		t.Fatal("不同路径不应共享同一把锁")
	}
}

// TestCommitConfigDataHelper 覆盖无状态便捷入口。
func TestCommitConfigDataHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	writeBaseline(t, path)
	if _, err := CommitConfigData(path, newData(t, "helper"), nil); err != nil {
		t.Fatalf("CommitConfigData 失败: %v", err)
	}
	if got := readKey(t, path); got != "helper" {
		t.Fatalf("磁盘 local_api_key = %q，期望 helper", got)
	}
}

// errForTest 是测试用的哨兵错误。
type errForTest string

func (e errForTest) Error() string { return string(e) }
