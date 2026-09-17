// Package configservice 提供配置的「读-改-写」服务，把并发提交串行化。
//
// 移植 auto_model_key_router/config_service.py。它是管理 API 与 CLI 改动配置的唯一
// 入口：先迁移、再校验、再原子落盘，并把变更前后的配置一并返回，供调用方决定是否
// 需要重启服务或刷新运行时。
package configservice

import (
	"path/filepath"
	"sync"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// ConfigChange 描述一次已落盘的配置变更。
type ConfigChange struct {
	// Path 是落盘的配置文件路径（未做解析，即构造 ConfigService 时给的那个）。
	Path string
	// OldConfig 是变更前的配置。
	OldConfig *config.RouterConfig
	// NewConfig 是变更后的配置。
	NewConfig *config.RouterConfig
	// Data 是**迁移后**的原始数据快照（深拷贝），供调用方直接使用而不必重新读盘。
	Data *canonical.Value
}

// locks 是「按绝对路径共享」的互斥锁表。
//
// 必须跨实例共享：同一个配置文件可能被管理 API 与 CLI 各自构造一个 ConfigService，
// 若锁是每实例一个，两条路径的并发提交就会互相覆盖（后写者赢，前一次变更静默丢失）。
// 参照实现用模块级的 _CONFIG_LOCKS 达到同样效果（config_service.py:20-25）。
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

// lockFor 返回某路径对应的共享互斥锁，首次调用时创建。
func lockFor(path string) *sync.Mutex {
	// 参照实现用 path.absolute() 归一化（config_service.py:24）。Go 用
	// filepath.Abs；它失败时退回原字符串而不是 panic——此时最坏情况是退化成
	// 「每实例一把锁」，与不共享的旧行为一致，不会造成数据损坏。
	normalized, err := filepath.Abs(path)
	if err != nil {
		normalized = path
	}
	locksMu.Lock()
	defer locksMu.Unlock()
	if existing, ok := locks[normalized]; ok {
		return existing
	}
	created := &sync.Mutex{}
	locks[normalized] = created
	return created
}

// ConfigService 串行化对某个配置文件的读改写。
type ConfigService struct {
	path string
	mu   *sync.Mutex
}

// New 构造针对 path 的服务；同一路径的多个实例共享同一把锁。
func New(path string) *ConfigService {
	return &ConfigService{path: path, mu: lockFor(path)}
}

// Path 返回构造时给定的配置文件路径。
func (s *ConfigService) Path() string { return s.path }

// Commit 迁移并落盘一份完整配置，返回变更详情。
//
// oldConfig 为 nil 时从磁盘读取变更前的配置（对应参照实现里
// `old_config or RouterConfig.load(self.path)`）。
//
// 参照实现的 commit 用的是 **RLock**（可重入），因为 update 会持锁再调 commit
// （config_service.py:39、:50）。Go 的 sync.Mutex **不可重入**，照搬
// `Update 持锁 -> 调 Commit 再取锁` 会直接死锁。因此这里拆出 commitLocked：
// 公开的 Commit 负责加锁，Update 持锁后直接调 commitLocked，绝不重入。
func (s *ConfigService) Commit(data *canonical.Value, oldConfig *config.RouterConfig) (ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(data, oldConfig)
}

// commitLocked 在**已持锁**的前提下完成迁移与落盘。
func (s *ConfigService) commitLocked(data *canonical.Value, oldConfig *config.RouterConfig) (ConfigChange, error) {
	previous := oldConfig
	if previous == nil {
		loaded, err := config.Load(s.path)
		if err != nil {
			return ConfigChange{}, err
		}
		previous = loaded
	}
	// 落盘的是**迁移后**的数据（config_service.py:41-43），不是调用方给的原始数据；
	// 否则一份 v3 配置会被原样写回，迁移永远不生效。
	migrated, err := config.MigrateConfigData(data)
	if err != nil {
		return ConfigChange{}, err
	}
	newConfig, err := config.FromDict(migrated)
	if err != nil {
		return ConfigChange{}, err
	}
	// 先校验（FromDict）再落盘：校验失败时磁盘必须保持原样，否则会写入一份
	// 连自己都加载不了的配置。
	if err := config.SaveConfigData(s.path, migrated); err != nil {
		return ConfigChange{}, err
	}
	return ConfigChange{
		Path:      s.path,
		OldConfig: previous,
		NewConfig: newConfig,
		// 深拷贝：落盘已完成，快照不应再与任何后续修改共享底层对象
		// （config_service.py:44 的 deepcopy）。
		Data: migrated.Clone(),
	}, nil
}

// Update 读取当前配置、就地施加 mutation、再迁移落盘。
//
// mutation 返回错误时中止且**不落盘**，磁盘保持原样。
func (s *ConfigService) Update(mutation func(data *canonical.Value) error) (ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := config.LoadConfigData(s.path)
	if err != nil {
		return ConfigChange{}, err
	}
	// 变更前的配置按**未迁移**的数据解析：参照实现这里用的也是 from_dict(data)
	// （config_service.py:52），而 from_dict 内部会先迁移，所以两者一致。
	oldConfig, err := config.FromDict(data)
	if err != nil {
		return ConfigChange{}, err
	}
	updated := data.Clone()
	if err := mutation(updated); err != nil {
		return ConfigChange{}, err
	}
	// 直接调 commitLocked：已持锁，不能再走公开的 Commit。
	return s.commitLocked(updated, oldConfig)
}

// CommitConfigData 是无状态便捷入口，对应 config_service.py:58-63。
func CommitConfigData(path string, data *canonical.Value, oldConfig *config.RouterConfig) (ConfigChange, error) {
	return New(path).Commit(data, oldConfig)
}
